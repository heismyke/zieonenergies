// Package app contains the platform's business rules. HTTP handlers stay thin
// and delegate here; every tenant-owned read or write runs inside
// db.Store.Tenant so PostgreSQL RLS backs up the explicit tenant predicates.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/config"
	"github.com/heismyke/zieonenergies/internal/db"
	"github.com/heismyke/zieonenergies/internal/messaging"
	"github.com/heismyke/zieonenergies/internal/payments"
	"github.com/heismyke/zieonenergies/internal/secure"
	"github.com/heismyke/zieonenergies/internal/vending"
)

type Service struct {
	DB     *db.Store
	Keys   *secure.Keyring
	Signer *auth.Signer
	Cfg    config.Config
	Log    *slog.Logger
	Now    func() time.Time
	HTTP   *http.Client

	// Vending routes by name. "sandbox" is always present outside production;
	// "live" is nil until an authorised STS / DisCo vending partner is integrated.
	Vending map[string]vending.Provider
	// Payment providers by name: "sandbox" (non-production), "paystack".
	Payments map[string]payments.Provider
	// Message senders by channel (email, sms, whatsapp); a missing channel is off.
	Messengers map[string]messaging.Sender
}

// Error is a business error safe to show to API clients.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

var (
	ErrNotFound      = errf(http.StatusNotFound, "not_found", "resource not found")
	ErrForbidden     = errf(http.StatusForbidden, "forbidden", "you do not have permission for this action")
	ErrInsufficient  = errf(http.StatusPaymentRequired, "insufficient_funds", "balance is too low for this transaction")
	ErrFeatureOff    = errf(http.StatusForbidden, "feature_disabled", "this feature is not enabled for your organisation")
	errDuplicatePost = errors.New("duplicate ledger posting")
)

func notFoundIfNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ---------------------------------------------------------------------------
// Request metadata carried through context for audit logging.
// ---------------------------------------------------------------------------

type ctxKey int

const metaKey ctxKey = 1

type RequestMeta struct {
	IP        string
	RequestID string
	Principal auth.Principal
}

func WithMeta(ctx context.Context, m RequestMeta) context.Context {
	return context.WithValue(ctx, metaKey, m)
}

func Meta(ctx context.Context) RequestMeta {
	m, _ := ctx.Value(metaKey).(RequestMeta)
	return m
}

type AuditEntry struct {
	TenantID   *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	Metadata   map[string]any
}

func (s *Service) audit(ctx context.Context, tx pgx.Tx, e AuditEntry) error {
	m := Meta(ctx)
	actorType := m.Principal.Kind
	if actorType == "" {
		actorType = "system"
	}
	if m.Principal.Kind == "user" {
		actorType = m.Principal.Role
	}
	var actor *uuid.UUID
	switch m.Principal.Kind {
	case "user":
		actor = &m.Principal.UserID
	case "api_key":
		actor = &m.Principal.KeyID
	}
	md, _ := json.Marshal(e.Metadata)
	if e.Metadata == nil {
		md = []byte("{}")
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_logs (tenant_id, actor_user_id, actor_type, action, target_type, target_id, metadata, ip, request_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, e.TenantID, actor, actorType, e.Action, e.TargetType, e.TargetID, md, m.IP, m.RequestID)
	return err
}

// ---------------------------------------------------------------------------
// Ledger
// ---------------------------------------------------------------------------

const (
	AcctWallet     = "wallet"
	AcctReward     = "reward"
	AcctRewardPool = "reward_pool"
	AcctClearing   = "clearing"
)

// post applies one signed movement to an account balance and appends the
// immutable ledger entry. Non-clearing balances may not go negative; the
// (account, kind, reference) uniqueness makes every posting exactly-once.
func post(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, acctType string, acctID uuid.UUID, amount int64, kind, ref, memo string) (int64, error) {
	if amount == 0 {
		return 0, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO balances (tenant_id, account_type, account_id) VALUES ($1,$2,$3)
		ON CONFLICT (account_type, account_id) DO NOTHING`, tenant, acctType, acctID); err != nil {
		return 0, err
	}
	var after int64
	err := tx.QueryRow(ctx, `UPDATE balances SET balance_kobo = balance_kobo + $1, updated_at = now()
		WHERE account_type=$2 AND account_id=$3 AND tenant_id=$4 RETURNING balance_kobo`, amount, acctType, acctID, tenant).Scan(&after)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		return 0, ErrInsufficient
	}
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ledger_entries (tenant_id, account_type, account_id, amount_kobo, balance_after, kind, reference, memo)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, tenant, acctType, acctID, amount, after, kind, ref, memo)
	if db.IsUniqueViolation(err) {
		return 0, errDuplicatePost
	}
	return after, err
}

// transfer moves amount from one account to another as two balanced postings.
func transfer(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, fromType string, fromID uuid.UUID, toType string, toID uuid.UUID, amount int64, kind, ref, memo string) error {
	if _, err := post(ctx, tx, tenant, fromType, fromID, -amount, kind, ref, memo); err != nil {
		return err
	}
	_, err := post(ctx, tx, tenant, toType, toID, amount, kind, ref, memo)
	return err
}

func balance(ctx context.Context, tx pgx.Tx, acctType string, acctID uuid.UUID) (int64, error) {
	var b int64
	err := tx.QueryRow(ctx, `SELECT balance_kobo FROM balances WHERE account_type=$1 AND account_id=$2`, acctType, acctID).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return b, err
}

func postEnergy(ctx context.Context, tx pgx.Tx, tenant, meter uuid.UUID, wh int64, kind, ref, memo string) error {
	_, err := tx.Exec(ctx, `INSERT INTO energy_entries (tenant_id, meter_id, wh, kind, reference, memo) VALUES ($1,$2,$3,$4,$5,$6)`,
		tenant, meter, wh, kind, ref, memo)
	if db.IsUniqueViolation(err) {
		return errDuplicatePost
	}
	return err
}

// ---------------------------------------------------------------------------
// Notifications and outbound partner events (transactional outbox)
// ---------------------------------------------------------------------------

func notify(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, user *uuid.UUID, title, message, kind string, critical bool) error {
	_, err := tx.Exec(ctx, `INSERT INTO notifications (tenant_id, user_id, title, message, kind, critical) VALUES ($1,$2,$3,$4,$5,$6)`,
		tenant, user, title, message, kind, critical)
	return err
}

// emit queues a signed webhook delivery to every subscribed partner endpoint
// in the same transaction as the state change that produced the event.
func emit(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, eventType string, data any) error {
	eventID := uuid.New()
	payload, err := json.Marshal(map[string]any{
		"id": eventID, "type": eventType, "created_at": time.Now().UTC(), "api_version": "v1", "data": data,
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO webhook_deliveries (tenant_id, endpoint_id, event_id, event_type, payload)
		SELECT tenant_id, id, $2, $3, $4 FROM webhook_endpoints
		WHERE tenant_id=$1 AND active AND ($3 = ANY(events) OR '*' = ANY(events))`, tenant, eventID, eventType, payload)
	return err
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

// Idempotent executes fn at most once per (tenant, principal, key). A replay
// with the same body returns the stored response; a different body is rejected.
// Server errors release the key so the client can safely retry.
func (s *Service) Idempotent(ctx context.Context, tenant uuid.UUID, principal, key string, body []byte,
	fn func() (int, any, error)) (int, json.RawMessage, error) {
	if key == "" || len(key) > 128 {
		return 0, nil, errf(http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header (1-128 chars) is required")
	}
	h := sha256.Sum256(body)
	var inserted bool
	var storedHash []byte
	var storedStatus *int
	var storedResp json.RawMessage
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (tenant_id, principal, key, request_hash) VALUES ($1,$2,$3,$4)
			ON CONFLICT DO NOTHING`, tenant, principal, key, h[:])
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			inserted = true
			return nil
		}
		return tx.QueryRow(ctx, `SELECT request_hash, status_code, response FROM idempotency_keys
			WHERE tenant_id=$1 AND principal=$2 AND key=$3`, tenant, principal, key).Scan(&storedHash, &storedStatus, &storedResp)
	})
	if err != nil {
		return 0, nil, err
	}
	if !inserted {
		if string(storedHash) != string(h[:]) {
			return 0, nil, errf(http.StatusUnprocessableEntity, "idempotency_key_reused", "this Idempotency-Key was used with a different request")
		}
		if storedStatus == nil {
			return 0, nil, errf(http.StatusConflict, "request_in_progress", "a request with this Idempotency-Key is still processing")
		}
		return *storedStatus, storedResp, nil
	}

	status, out, ferr := fn()
	var appErr *Error
	if ferr != nil && !errors.As(ferr, &appErr) {
		// Unexpected failure: release the key so a retry is possible.
		_ = s.DB.Tenant(context.WithoutCancel(ctx), tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE tenant_id=$1 AND principal=$2 AND key=$3`, tenant, principal, key)
			return err
		})
		return 0, nil, ferr
	}
	if appErr != nil {
		status, out = appErr.Status, map[string]any{"error": appErr}
	}
	resp, _ := json.Marshal(out)
	err = s.DB.Tenant(context.WithoutCancel(ctx), tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE idempotency_keys SET status_code=$4, response=$5
			WHERE tenant_id=$1 AND principal=$2 AND key=$3`, tenant, principal, key, status, resp)
		return err
	})
	return status, resp, err
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func WhToKwh(wh int64) float64 { return float64(wh) / 1000 }

func KwhToWh(kwh float64) int64 { return int64(kwh*1000 + 0.5) }

func ptr[T any](v T) *T { return &v }
