package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/secure"
)

// ---------------------------------------------------------------------------
// One-time token links (shared as a QR code)
// ---------------------------------------------------------------------------

const tokenShareTTL = 24 * time.Hour

type TokenShare struct {
	Code      string    `json:"code"` // shown once; only its hash is stored
	ExpiresAt time.Time `json:"expires_at"`
}

// ShareToken makes a one-time link for an order's token. A new link replaces
// any earlier one for the same order that has not been used.
func (s *Service) ShareToken(ctx context.Context, p auth.Principal, t Tenant, orderID uuid.UUID) (TokenShare, error) {
	o, err := s.Order(ctx, p, t, orderID)
	if err != nil {
		return TokenShare{}, err
	}
	if o.Token == nil || *o.Token == "" {
		return TokenShare{}, errf(http.StatusConflict, "no_token", "this order has no token to share yet")
	}
	code := secure.RandomToken(18)
	share := TokenShare{Code: code, ExpiresAt: s.Now().Add(tokenShareTTL).UTC()}
	err = s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE token_shares SET expires_at=now() WHERE vend_transaction_id=$1 AND revealed_at IS NULL AND expires_at > now()`,
			orderID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO token_shares (tenant_id, vend_transaction_id, created_by, code_hash, expires_at) VALUES ($1,$2,$3,$4,$5)`,
			t.ID, orderID, p.UserID, secure.HashSecret(code), share.ExpiresAt)
		return err
	})
	return share, err
}

// SharedToken is what the person who scans the QR sees.
type SharedToken struct {
	Status       string     `json:"status"` // ready | used | expired
	ProviderName string     `json:"provider_name"`
	MeterNumber  string     `json:"meter_number"`
	EnergyKwh    float64    `json:"energy_kwh"`
	AmountKobo   int64      `json:"amount_kobo"`
	Token        *string    `json:"token,omitempty"` // only from Reveal
	RevealedAt   *time.Time `json:"revealed_at,omitempty"`
	ExpiresAt    time.Time  `json:"expires_at"`
}

var errShareNotFound = errf(http.StatusNotFound, "share_not_found", "this token link is not valid")

// SharedTokenInfo describes a one-time link without using it up, so link
// previews (WhatsApp, browsers) cannot consume it.
func (s *Service) SharedTokenInfo(ctx context.Context, t Tenant, code string) (SharedToken, error) {
	var out SharedToken
	err := s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		var err error
		_, out, err = loadShare(ctx, tx, t, code)
		return err
	})
	return out, err
}

// RevealSharedToken returns the token once. The first caller gets it; later
// calls see status "used".
func (s *Service) RevealSharedToken(ctx context.Context, t Tenant, code string) (SharedToken, error) {
	var out SharedToken
	err := s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		var vend uuid.UUID
		tag, err := tx.Exec(ctx, `UPDATE token_shares SET revealed_at=now()
			WHERE tenant_id=$1 AND code_hash=$2 AND revealed_at IS NULL AND expires_at > now()`, t.ID, secure.HashSecret(code))
		if err != nil {
			return err
		}
		if vend, out, err = loadShare(ctx, tx, t, code); err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // already used or expired: status says which
		}
		var enc []byte
		if err := tx.QueryRow(ctx, `SELECT token_enc FROM energy_tokens WHERE vend_transaction_id=$1 ORDER BY created_at LIMIT 1`, vend).Scan(&enc); err != nil {
			return err
		}
		plain, err := s.Keys.Decrypt(enc)
		if err != nil {
			return err
		}
		tok := string(plain)
		out.Token = &tok
		out.Status = "revealed"
		return nil
	})
	return out, err
}

func loadShare(ctx context.Context, tx pgx.Tx, t Tenant, code string) (uuid.UUID, SharedToken, error) {
	var out SharedToken
	var vend uuid.UUID
	var wh int64
	err := tx.QueryRow(ctx, `SELECT sh.vend_transaction_id, sh.expires_at, sh.revealed_at, m.meter_number,
			coalesce(tr.provider_name, m.provider_code), v.energy_wh, v.amount_kobo - v.fee_kobo
		FROM token_shares sh JOIN vend_transactions v ON v.id = sh.vend_transaction_id JOIN meters m ON m.id = v.meter_id
		LEFT JOIN tariffs tr ON tr.tenant_id = sh.tenant_id AND tr.provider_code = m.provider_code
		WHERE sh.tenant_id=$1 AND sh.code_hash=$2`, t.ID, secure.HashSecret(code)).Scan(
		&vend, &out.ExpiresAt, &out.RevealedAt, &out.MeterNumber, &out.ProviderName, &wh, &out.AmountKobo)
	if errors.Is(err, pgx.ErrNoRows) {
		return vend, out, errShareNotFound
	}
	if err != nil {
		return vend, out, err
	}
	out.EnergyKwh = WhToKwh(wh)
	switch {
	case out.RevealedAt != nil:
		out.Status = "used"
	case !out.ExpiresAt.After(time.Now()):
		out.Status = "expired"
	default:
		out.Status = "ready"
	}
	return vend, out, nil
}

// ---------------------------------------------------------------------------
// Account statement
// ---------------------------------------------------------------------------

type StatementLine struct {
	ID           uuid.UUID `json:"id"`
	Date         time.Time `json:"date"`
	ProviderName string    `json:"provider_name"`
	MeterNumber  string    `json:"meter_number"`
	MeterType    string    `json:"meter_type"`
	Status       string    `json:"status"`
	PaidWith     string    `json:"paid_with"`
	AmountKobo   int64     `json:"amount_kobo"` // total paid, fee included
	FeeKobo      int64     `json:"fee_kobo"`
	EnergyKwh    float64   `json:"energy_kwh"`
	TokenLast4   string    `json:"token_last4"`
}

type Statement struct {
	From           string          `json:"from"` // YYYY-MM-DD, Lagos time
	To             string          `json:"to"`
	Name           string          `json:"name"`
	Email          string          `json:"email"`
	Purchases      []StatementLine `json:"purchases"`
	Wallet         []WalletEntry   `json:"wallet"`
	OpeningKobo    int64           `json:"wallet_opening_kobo"`
	ClosingKobo    int64           `json:"wallet_closing_kobo"`
	SpentKobo      int64           `json:"spent_kobo"` // successful purchases
	FeesKobo       int64           `json:"fees_kobo"`
	SuccessfulKwh  float64         `json:"energy_kwh"`
	SuccessfulRuns int             `json:"successful_purchases"`
}

var lagos = func() *time.Location {
	l, err := time.LoadLocation("Africa/Lagos")
	if err != nil {
		return time.FixedZone("WAT", 3600)
	}
	return l
}()

// AccountStatement lists purchases and wallet movements between two dates
// (inclusive, Lagos time). The range is at most a year.
func (s *Service) AccountStatement(ctx context.Context, p auth.Principal, t Tenant, from, to string) (Statement, error) {
	f, err1 := time.ParseInLocation("2006-01-02", from, lagos)
	e, err2 := time.ParseInLocation("2006-01-02", to, lagos)
	if err1 != nil || err2 != nil || e.Before(f) || e.Sub(f) > 366*24*time.Hour {
		return Statement{}, errf(http.StatusBadRequest, "invalid_range", "from and to must be dates (YYYY-MM-DD), at most a year apart")
	}
	end := e.AddDate(0, 0, 1) // to is inclusive
	st := Statement{From: from, To: to, Purchases: []StatementLine{}, Wallet: []WalletEntry{}}
	err := s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT name, email FROM users WHERE id=$1`, p.UserID).Scan(&st.Name, &st.Email); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT v.id, v.created_at, coalesce(tr.provider_name, m.provider_code), m.meter_number, m.meter_type,
				v.status, v.payment_method, v.amount_kobo, v.fee_kobo, v.energy_wh, coalesce(et.token_last4, '')
			FROM vend_transactions v JOIN meters m ON m.id = v.meter_id
			LEFT JOIN tariffs tr ON tr.tenant_id = v.tenant_id AND tr.provider_code = m.provider_code
			LEFT JOIN energy_tokens et ON et.vend_transaction_id = v.id
			WHERE v.tenant_id=$1 AND v.user_id=$2 AND v.created_at >= $3 AND v.created_at < $4
				AND NOT EXISTS (SELECT 1 FROM guest_orders g WHERE g.vend_transaction_id = v.id)
			ORDER BY v.created_at LIMIT 2000`, t.ID, p.UserID, f, end)
		if err != nil {
			return err
		}
		for rows.Next() {
			var l StatementLine
			var wh int64
			if err := rows.Scan(&l.ID, &l.Date, &l.ProviderName, &l.MeterNumber, &l.MeterType, &l.Status, &l.PaidWith,
				&l.AmountKobo, &l.FeeKobo, &wh, &l.TokenLast4); err != nil {
				rows.Close()
				return err
			}
			l.EnergyKwh = WhToKwh(wh)
			if l.Status == StVended || l.Status == StDelivered {
				st.SpentKobo += l.AmountKobo
				st.FeesKobo += l.FeeKobo
				st.SuccessfulKwh += l.EnergyKwh
				st.SuccessfulRuns++
			}
			st.Purchases = append(st.Purchases, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// Wallet: opening balance is the balance after the last entry before the range.
		if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT balance_after FROM ledger_entries WHERE account_type=$1 AND account_id=$2
			AND created_at < $3 ORDER BY id DESC LIMIT 1), 0)`, AcctWallet, p.UserID, f).Scan(&st.OpeningKobo); err != nil {
			return err
		}
		st.ClosingKobo = st.OpeningKobo
		rows, err = tx.Query(ctx, `SELECT id, kind, amount_kobo, balance_after, memo, reference, created_at FROM ledger_entries
			WHERE account_type=$1 AND account_id=$2 AND created_at >= $3 AND created_at < $4 ORDER BY id LIMIT 5000`, AcctWallet, p.UserID, f, end)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var w WalletEntry
			var ref string
			if err := rows.Scan(&w.ID, &w.Kind, &w.AmountKobo, &w.BalanceAfter, &w.Memo, &ref, &w.CreatedAt); err != nil {
				return err
			}
			if id, err := uuid.Parse(ref); err == nil && (w.Kind == "purchase" || w.Kind == "refund") {
				w.OrderID = &id
			}
			st.Wallet = append(st.Wallet, w)
			st.ClosingKobo = w.BalanceAfter
		}
		return rows.Err()
	})
	return st, err
}
