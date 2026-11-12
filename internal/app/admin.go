package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/secure"
)

// ---------------------------------------------------------------------------
// Notifications
// ---------------------------------------------------------------------------

type Notification struct {
	ID        uuid.UUID  `json:"id"`
	Title     string     `json:"title"`
	Message   string     `json:"message"`
	Critical  bool       `json:"critical"`
	Kind      string     `json:"kind"`
	ReadAt    *time.Time `json:"read_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// Notifications returns the user's notifications plus tenant-wide broadcasts (user_id NULL).
func (s *Service) Notifications(ctx context.Context, p auth.Principal, limit int) ([]Notification, error) {
	out := []Notification{}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, title, message, critical, kind, read_at, created_at FROM notifications
			WHERE tenant_id=$1 AND (user_id=$2 OR user_id IS NULL) ORDER BY created_at DESC LIMIT $3`, p.TenantID, p.UserID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n Notification
			if err := rows.Scan(&n.ID, &n.Title, &n.Message, &n.Critical, &n.Kind, &n.ReadAt, &n.CreatedAt); err != nil {
				return err
			}
			out = append(out, n)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Service) MarkNotificationRead(ctx context.Context, p auth.Principal, id uuid.UUID) error {
	return s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE notifications SET read_at=now() WHERE id=$1 AND tenant_id=$2 AND user_id=$3 AND read_at IS NULL`, id, p.TenantID, p.UserID)
		return err
	})
}

// Broadcast sends a tenant-wide notice (e.g. the "weather outage alert").
func (s *Service) Broadcast(ctx context.Context, tenant uuid.UUID, title, message string, critical bool) error {
	if strings.TrimSpace(title) == "" || strings.TrimSpace(message) == "" || len(message) > 500 {
		return errf(http.StatusBadRequest, "invalid_input", "title and message (max 500) are required")
	}
	return s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := notify(ctx, tx, tenant, nil, title, message, "broadcast", critical); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "notification.broadcast", Metadata: map[string]any{"title": title}})
	})
}

// ---------------------------------------------------------------------------
// Support
// ---------------------------------------------------------------------------

type Ticket struct {
	ID        uuid.UUID        `json:"id"`
	UserID    uuid.UUID        `json:"user_id"`
	Subject   string           `json:"subject"`
	Status    string           `json:"status"`
	Messages  []SupportMessage `json:"messages"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

type SupportMessage struct {
	Body      string    `json:"body"`
	FromStaff bool      `json:"from_staff"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) CreateTicket(ctx context.Context, p auth.Principal, subject, body string) (Ticket, error) {
	subject, body = strings.TrimSpace(subject), strings.TrimSpace(body)
	if subject == "" || body == "" || len(subject) > 120 || len(body) > 4000 {
		return Ticket{}, errf(http.StatusBadRequest, "invalid_input", "subject (max 120) and message (max 4000) are required")
	}
	var id uuid.UUID
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO support_tickets (tenant_id, user_id, subject) VALUES ($1,$2,$3) RETURNING id`, p.TenantID, p.UserID, subject).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO support_messages (tenant_id, ticket_id, author_id, body) VALUES ($1,$2,$3,$4)`, p.TenantID, id, p.UserID, body)
		return err
	})
	if err != nil {
		return Ticket{}, err
	}
	return s.Ticket(ctx, p, id)
}

func (s *Service) AddTicketMessage(ctx context.Context, p auth.Principal, id uuid.UUID, body string, status string) (Ticket, error) {
	body = strings.TrimSpace(body)
	if len(body) > 4000 {
		return Ticket{}, errf(http.StatusBadRequest, "invalid_input", "message max 4000 characters")
	}
	staff := p.IsTenantOperator()
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		var owner uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT user_id FROM support_tickets WHERE id=$1 AND tenant_id=$2 FOR UPDATE`, id, p.TenantID).Scan(&owner); err != nil {
			return notFoundIfNoRows(err)
		}
		if !staff && owner != p.UserID {
			return ErrNotFound
		}
		if body != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO support_messages (tenant_id, ticket_id, author_id, from_staff, body) VALUES ($1,$2,$3,$4,$5)`,
				p.TenantID, id, p.UserID, staff, body); err != nil {
				return err
			}
		}
		if status != "" {
			if !staff && status != "closed" {
				return ErrForbidden
			}
			switch status {
			case "open", "in_progress", "resolved", "closed":
			default:
				return errf(http.StatusBadRequest, "invalid_status", "invalid ticket status")
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE support_tickets SET status=coalesce(nullif($2,''),status), updated_at=now() WHERE id=$1`, id, status); err != nil {
			return err
		}
		if staff && body != "" {
			return notify(ctx, tx, p.TenantID, &owner, "Support replied", body, "support", false)
		}
		return nil
	})
	if err != nil {
		return Ticket{}, err
	}
	return s.Ticket(ctx, p, id)
}

func (s *Service) Ticket(ctx context.Context, p auth.Principal, id uuid.UUID) (Ticket, error) {
	var t Ticket
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id, user_id, subject, status, created_at, updated_at FROM support_tickets
			WHERE id=$1 AND tenant_id=$2 AND ($3 OR user_id=$4)`, id, p.TenantID, p.IsTenantOperator(), p.UserID).
			Scan(&t.ID, &t.UserID, &t.Subject, &t.Status, &t.CreatedAt, &t.UpdatedAt)
		if err != nil {
			return notFoundIfNoRows(err)
		}
		rows, err := tx.Query(ctx, `SELECT body, from_staff, created_at FROM support_messages WHERE ticket_id=$1 ORDER BY created_at`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		t.Messages = []SupportMessage{}
		for rows.Next() {
			var m SupportMessage
			if err := rows.Scan(&m.Body, &m.FromStaff, &m.CreatedAt); err != nil {
				return err
			}
			t.Messages = append(t.Messages, m)
		}
		return rows.Err()
	})
	return t, err
}

func (s *Service) Tickets(ctx context.Context, p auth.Principal, status string) ([]Ticket, error) {
	out := []Ticket{}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, user_id, subject, status, created_at, updated_at FROM support_tickets
			WHERE tenant_id=$1 AND ($2 OR user_id=$3) AND ($4='' OR status=$4) ORDER BY updated_at DESC LIMIT 100`,
			p.TenantID, p.IsTenantOperator(), p.UserID, status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Ticket
			if err := rows.Scan(&t.ID, &t.UserID, &t.Subject, &t.Status, &t.CreatedAt, &t.UpdatedAt); err != nil {
				return err
			}
			t.Messages = []SupportMessage{}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Reports
// ---------------------------------------------------------------------------

type Report struct {
	From               time.Time        `json:"from"`
	To                 time.Time        `json:"to"`
	Customers          int64            `json:"customers"`
	ActiveMeters       int64            `json:"active_meters"`
	OfflineMeters      int64            `json:"offline_meters"`
	PurchasesByStatus  map[string]int64 `json:"purchases_by_status"`
	VendedAmountKobo   int64            `json:"vended_amount_kobo"`
	VendedEnergyKwh    float64          `json:"vended_energy_kwh"`
	RewardsGrantedKobo int64            `json:"rewards_granted_kobo"`
	RewardPoolKobo     int64            `json:"reward_pool_kobo"`
	OpenTickets        int64            `json:"open_tickets"`
	PendingTransfers   int64            `json:"pending_transfers"`
}

func (s *Service) Report(ctx context.Context, tenant uuid.UUID, from, to time.Time) (Report, error) {
	r := Report{From: from, To: to, PurchasesByStatus: map[string]int64{}}
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		var energyWh int64
		err := tx.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM users WHERE tenant_id=$1 AND role='customer'),
			(SELECT count(*) FROM meters WHERE tenant_id=$1 AND status='active'),
			(SELECT count(*) FROM meters WHERE tenant_id=$1 AND status='active' AND connectivity='offline'),
			(SELECT coalesce(sum(amount_kobo),0) FROM vend_transactions WHERE tenant_id=$1 AND status IN ('vended','delivered') AND created_at BETWEEN $2 AND $3),
			(SELECT coalesce(sum(energy_wh),0) FROM vend_transactions WHERE tenant_id=$1 AND status IN ('vended','delivered') AND created_at BETWEEN $2 AND $3),
			(SELECT coalesce(sum(amount_kobo),0) FROM reward_grants WHERE tenant_id=$1 AND kind <> 'refund' AND created_at BETWEEN $2 AND $3),
			(SELECT count(*) FROM support_tickets WHERE tenant_id=$1 AND status IN ('open','in_progress')),
			(SELECT count(*) FROM meter_transfers WHERE tenant_id=$1 AND status='pending')`, tenant, from, to).
			Scan(&r.Customers, &r.ActiveMeters, &r.OfflineMeters, &r.VendedAmountKobo, &energyWh, &r.RewardsGrantedKobo, &r.OpenTickets, &r.PendingTransfers)
		if err != nil {
			return err
		}
		r.VendedEnergyKwh = WhToKwh(energyWh)
		if r.RewardPoolKobo, err = balance(ctx, tx, AcctRewardPool, tenant); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT status, count(*) FROM vend_transactions WHERE tenant_id=$1 AND created_at BETWEEN $2 AND $3 GROUP BY status`, tenant, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var st string
			var n int64
			if err := rows.Scan(&st, &n); err != nil {
				return err
			}
			r.PurchasesByStatus[st] = n
		}
		return rows.Err()
	})
	return r, err
}

// ---------------------------------------------------------------------------
// Partner webhook endpoints
// ---------------------------------------------------------------------------

var WebhookEvents = []string{"purchase.vended", "purchase.refunded", "purchase.payment_failed", "purchase.needs_review",
	"wallet.funded", "token.delivered", "meter.low_balance", "meter.offline", "meter.tamper", "reward.granted", "*"}

type WebhookEndpoint struct {
	ID        uuid.UUID `json:"id"`
	URL       string    `json:"url"`
	Events    []string  `json:"events"`
	Active    bool      `json:"active"`
	Secret    string    `json:"secret,omitempty"` // only on creation
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) CreateWebhookEndpoint(ctx context.Context, tenant uuid.UUID, rawURL string, events []string) (WebhookEndpoint, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return WebhookEndpoint{}, errf(http.StatusBadRequest, "invalid_url", "webhook url must be https")
	}
	if isPrivateHost(u.Hostname()) {
		return WebhookEndpoint{}, errf(http.StatusBadRequest, "invalid_url", "webhook url must be a public host")
	}
	if len(events) == 0 {
		return WebhookEndpoint{}, errf(http.StatusBadRequest, "invalid_events", "at least one event is required")
	}
	for _, e := range events {
		ok := false
		for _, k := range WebhookEvents {
			ok = ok || k == e
		}
		if !ok {
			return WebhookEndpoint{}, errf(http.StatusBadRequest, "invalid_events", "unknown event %q", e)
		}
	}
	secret := "whsec_" + secure.RandomToken(32)
	enc, err := s.Keys.Encrypt([]byte(secret))
	if err != nil {
		return WebhookEndpoint{}, err
	}
	ep := WebhookEndpoint{URL: rawURL, Events: events, Active: true, Secret: secret}
	err = s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO webhook_endpoints (tenant_id, url, secret_enc, events) VALUES ($1,$2,$3,$4) RETURNING id, created_at`,
			tenant, rawURL, enc, events).Scan(&ep.ID, &ep.CreatedAt); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "webhook.create", TargetType: "webhook_endpoint", TargetID: ep.ID.String(),
			Metadata: map[string]any{"url": rawURL, "events": events}})
	})
	return ep, err
}

func (s *Service) WebhookEndpoints(ctx context.Context, tenant uuid.UUID) ([]WebhookEndpoint, error) {
	out := []WebhookEndpoint{}
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, url, events, active, created_at FROM webhook_endpoints WHERE tenant_id=$1 ORDER BY created_at`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e WebhookEndpoint
			if err := rows.Scan(&e.ID, &e.URL, &e.Events, &e.Active, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Service) DeleteWebhookEndpoint(ctx context.Context, tenant, id uuid.UUID) error {
	return s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET active=false WHERE id=$1 AND tenant_id=$2`, id, tenant)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "webhook.disable", TargetType: "webhook_endpoint", TargetID: id.String()})
	})
}

func isPrivateHost(h string) bool {
	h = strings.ToLower(h)
	if h == "localhost" || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") {
		return true
	}
	for _, p := range []string{"10.", "127.", "192.168.", "169.254.", "0."} {
		if strings.HasPrefix(h, p) {
			return true
		}
	}
	if strings.HasPrefix(h, "172.") {
		var second int
		for _, c := range strings.Split(h, ".")[1] {
			second = second*10 + int(c-'0')
		}
		return second >= 16 && second <= 31
	}
	return h == "::1" || strings.HasPrefix(h, "fc") || strings.HasPrefix(h, "fd")
}

// ---------------------------------------------------------------------------
// Audit log access
// ---------------------------------------------------------------------------

type AuditLog struct {
	ID         int64           `json:"id"`
	TenantID   *uuid.UUID      `json:"tenant_id"`
	ActorID    *uuid.UUID      `json:"actor_id"`
	ActorType  string          `json:"actor_type"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	Metadata   json.RawMessage `json:"metadata"`
	IP         string          `json:"ip"`
	RequestID  string          `json:"request_id"`
	CreatedAt  time.Time       `json:"created_at"`
}

// AuditLogs lists entries for one tenant, or all tenants when tenant is nil (platform).
func (s *Service) AuditLogs(ctx context.Context, tenant *uuid.UUID, limit int) ([]AuditLog, error) {
	out := []AuditLog{}
	run := func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, tenant_id, actor_user_id, actor_type, action, target_type, target_id, metadata, ip, request_id, created_at
			FROM audit_logs WHERE ($1::uuid IS NULL OR tenant_id=$1) ORDER BY id DESC LIMIT $2`, tenant, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a AuditLog
			if err := rows.Scan(&a.ID, &a.TenantID, &a.ActorID, &a.ActorType, &a.Action, &a.TargetType, &a.TargetID, &a.Metadata, &a.IP, &a.RequestID, &a.CreatedAt); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	}
	if tenant != nil {
		return out, s.DB.Tenant(ctx, *tenant, run)
	}
	return out, s.DB.System(ctx, run)
}

// RecordPlatformAccess audits a platform administrator acting inside a tenant.
func (s *Service) RecordPlatformAccess(ctx context.Context, tenant uuid.UUID, action string, md map[string]any) error {
	return s.DB.System(ctx, func(tx pgx.Tx) error {
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: action, TargetType: "tenant", TargetID: tenant.String(), Metadata: md})
	})
}
