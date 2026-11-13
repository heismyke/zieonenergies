package app

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/db"
	"github.com/heismyke/zieonenergies/internal/secure"
)

type User struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	Email      string    `json:"email"`
	Phone      string    `json:"phone"`
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	Status     string    `json:"status"`
	ExternalID *string   `json:"external_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

const userCols = `id, tenant_id, email, phone, name, role, status, external_id, created_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.TenantID, &u.Email, &u.Phone, &u.Name, &u.Role, &u.Status, &u.ExternalID, &u.CreatedAt)
	return u, notFoundIfNoRows(err)
}

type Session struct {
	AccessToken  string    `json:"access_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	User         User      `json:"user"`
	Tenant       string    `json:"tenant"`
}

func validateEmail(e string) error {
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || !strings.Contains(e, "@") {
		return errf(http.StatusBadRequest, "invalid_email", "a valid email address is required")
	}
	return nil
}

func validatePassword(p string) error {
	if len(p) < 8 || len(p) > 128 {
		return errf(http.StatusBadRequest, "weak_password", "password must be 8-128 characters")
	}
	return nil
}

type RegisterInput struct {
	Email    string `json:"email"`
	Phone    string `json:"phone"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

func (s *Service) Register(ctx context.Context, t Tenant, in RegisterInput) (Session, error) {
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if t.Status != "active" {
		return Session{}, errf(http.StatusForbidden, "tenant_suspended", "this organisation is not accepting sign-ups")
	}
	if err := validateEmail(in.Email); err != nil {
		return Session{}, err
	}
	if err := validatePassword(in.Password); err != nil {
		return Session{}, err
	}
	if len(in.Phone) > 32 || len(in.Name) > 120 {
		return Session{}, errf(http.StatusBadRequest, "invalid_input", "phone or name too long")
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return Session{}, err
	}
	var u User
	err = s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		var err error
		// A passwordless customer (left by a guest order) is claimed by signing up.
		u, err = scanUser(tx.QueryRow(ctx, `INSERT INTO users (tenant_id, email, phone, name, password_hash, role)
			VALUES ($1,$2,$3,$4,$5,'customer')
			ON CONFLICT (tenant_id, lower(email)) DO UPDATE SET password_hash=EXCLUDED.password_hash,
				name=CASE WHEN EXCLUDED.name<>'' THEN EXCLUDED.name ELSE users.name END,
				phone=CASE WHEN EXCLUDED.phone<>'' THEN EXCLUDED.phone ELSE users.phone END, updated_at=now()
			WHERE users.password_hash IS NULL AND users.role='customer' AND users.status='active'
			RETURNING `+userCols, t.ID, in.Email, strings.TrimSpace(in.Phone), strings.TrimSpace(in.Name), hash))
		if db.IsUniqueViolation(err) || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ErrNotFound) {
			return errf(http.StatusConflict, "email_taken", "an account with this email already exists")
		}
		if err != nil {
			return err
		}
		return notify(ctx, tx, t.ID, &u.ID, "Welcome", "Your account is ready. Add a meter to start buying electricity.", "account", false)
	})
	if err != nil {
		return Session{}, err
	}
	return s.newSession(ctx, t, u, "password", true)
}

const maxFailedLogins = 5

var errBadLogin = errf(http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")

func (s *Service) Login(ctx context.Context, t Tenant, email, password string) (Session, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if t.Status != "active" {
		return Session{}, errf(http.StatusForbidden, "tenant_suspended", "this organisation is suspended")
	}
	var u User
	var hash *string
	var lockedUntil *time.Time
	err := s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT `+userCols+`, password_hash, locked_until FROM users WHERE tenant_id=$1 AND lower(email)=$2`, t.ID, email).
			Scan(&u.ID, &u.TenantID, &u.Email, &u.Phone, &u.Name, &u.Role, &u.Status, &u.ExternalID, &u.CreatedAt, &hash, &lockedUntil)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		auth.BurnPasswordCheck(password)
		return Session{}, errBadLogin
	}
	if err != nil {
		return Session{}, err
	}
	if lockedUntil != nil && lockedUntil.After(s.Now()) {
		return Session{}, errf(http.StatusTooManyRequests, "account_locked", "too many failed attempts; try again later")
	}
	if hash == nil || !auth.CheckPassword(*hash, password) || u.Status != "active" {
		_ = s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE users SET failed_logins = failed_logins + 1,
				locked_until = CASE WHEN failed_logins + 1 >= $2 THEN now() + interval '15 minutes' ELSE locked_until END
				WHERE id=$1`, u.ID, maxFailedLogins)
			if err == nil && u.Role != auth.RoleCustomer {
				err = s.audit(ctx, tx, AuditEntry{TenantID: &t.ID, Action: "auth.login.failed", TargetType: "user", TargetID: u.ID.String()})
			}
			return err
		})
		return Session{}, errBadLogin
	}
	_ = s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE users SET failed_logins=0, locked_until=NULL, last_login_at=now() WHERE id=$1`, u.ID)
		if err == nil && u.Role != auth.RoleCustomer {
			ctx := WithMeta(ctx, RequestMeta{IP: Meta(ctx).IP, RequestID: Meta(ctx).RequestID,
				Principal: auth.Principal{Kind: "user", UserID: u.ID, Role: u.Role, TenantID: t.ID}})
			err = s.audit(ctx, tx, AuditEntry{TenantID: &t.ID, Action: "auth.login", TargetType: "user", TargetID: u.ID.String()})
		}
		return err
	})
	return s.newSession(ctx, t, u, "password", true)
}

func (s *Service) newSession(ctx context.Context, t Tenant, u User, method string, withRefresh bool) (Session, error) {
	ttl := s.Cfg.AccessTTL
	if method == "partner_sso" {
		ttl = min(ttl, 15*time.Minute)
	}
	tok, exp, err := s.Signer.Issue(u.ID, t.ID, u.Role, method, ttl)
	if err != nil {
		return Session{}, err
	}
	sess := Session{AccessToken: tok, ExpiresAt: exp, User: u, Tenant: t.Slug}
	if withRefresh {
		rt := "zrt_" + secure.RandomToken(32)
		err := s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO refresh_tokens (tenant_id, user_id, token_hash, expires_at) VALUES ($1,$2,$3,$4)`,
				t.ID, u.ID, secure.HashSecret(rt), s.Now().Add(s.Cfg.RefreshTTL))
			return err
		})
		if err != nil {
			return Session{}, err
		}
		sess.RefreshToken = rt
	}
	return sess, nil
}

// Refresh rotates a refresh token. Presenting an already-rotated token is
// treated as theft: the whole token family for that user is revoked.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (Session, error) {
	invalid := errf(http.StatusUnauthorized, "invalid_refresh_token", "refresh token is invalid or expired")
	if !strings.HasPrefix(refreshToken, "zrt_") {
		return Session{}, invalid
	}
	var tenantID, userID, tokenID uuid.UUID
	var revoked *time.Time
	var expires time.Time
	// The refresh token is the credential, so the lookup precedes tenant context.
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id, tenant_id, user_id, revoked_at, expires_at FROM refresh_tokens WHERE token_hash=$1`,
			secure.HashSecret(refreshToken)).Scan(&tokenID, &tenantID, &userID, &revoked, &expires)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, invalid
	}
	if err != nil {
		return Session{}, err
	}
	if revoked != nil {
		_ = s.DB.Tenant(ctx, tenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, userID)
			return err
		})
		return Session{}, invalid
	}
	if expires.Before(s.Now()) {
		return Session{}, invalid
	}
	t, err := s.TenantByID(ctx, tenantID)
	if err != nil {
		return Session{}, err
	}
	if t.Status != "active" {
		return Session{}, errf(http.StatusForbidden, "tenant_suspended", "this organisation is suspended")
	}
	var u User
	err = s.DB.Tenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, tokenID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return invalid // lost a race with a concurrent refresh
		}
		u, err = scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1 AND status='active'`, userID))
		return err
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Session{}, invalid
		}
		return Session{}, err
	}
	return s.newSession(ctx, t, u, "password", true)
}

func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	return s.DB.System(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE token_hash=$1 AND revoked_at IS NULL`, secure.HashSecret(refreshToken))
		return err
	})
}

// ActiveUser checks a verified token still maps to an active user in an
// active tenant; tokens are short-lived but disabling must take effect.
func (s *Service) ActiveUser(ctx context.Context, p auth.Principal) (User, Tenant, error) {
	t, err := s.TenantByID(ctx, p.TenantID)
	if err != nil {
		return User{}, t, err
	}
	if t.Status != "active" {
		return User{}, t, errf(http.StatusForbidden, "tenant_suspended", "this organisation is suspended")
	}
	var u User
	err = s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1 AND tenant_id=$2`, p.UserID, t.ID))
		return err
	})
	if err != nil {
		return u, t, err
	}
	if u.Status != "active" || u.Role != p.Role {
		return u, t, errf(http.StatusUnauthorized, "session_revoked", "please sign in again")
	}
	return u, t, nil
}

type ProfileInput struct {
	Name  *string `json:"name"`
	Phone *string `json:"phone"`
}

func (s *Service) UpdateProfile(ctx context.Context, p auth.Principal, in ProfileInput) (User, error) {
	if (in.Name != nil && len(*in.Name) > 120) || (in.Phone != nil && len(*in.Phone) > 32) {
		return User{}, errf(http.StatusBadRequest, "invalid_input", "name or phone too long")
	}
	var u User
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRow(ctx, `UPDATE users SET name=coalesce($2,name), phone=coalesce($3,phone), updated_at=now()
			WHERE id=$1 RETURNING `+userCols, p.UserID, in.Name, in.Phone))
		return err
	})
	return u, err
}

func (s *Service) ChangePassword(ctx context.Context, p auth.Principal, current, next string) error {
	if err := validatePassword(next); err != nil {
		return err
	}
	return s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		var hash *string
		if err := tx.QueryRow(ctx, `SELECT password_hash FROM users WHERE id=$1`, p.UserID).Scan(&hash); err != nil {
			return err
		}
		if hash == nil || !auth.CheckPassword(*hash, current) {
			return errf(http.StatusBadRequest, "invalid_credentials", "current password is incorrect")
		}
		nh, err := auth.HashPassword(next)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2, updated_at=now() WHERE id=$1`, p.UserID, nh); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, p.UserID)
		return err
	})
}

// ---------------------------------------------------------------------------
// Team members (tenant administrators)
// ---------------------------------------------------------------------------

type TeamMemberInput struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Password string `json:"password"`
}

func (s *Service) AddTeamMember(ctx context.Context, tenant uuid.UUID, in TeamMemberInput) (User, error) {
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if in.Role != auth.RoleTenantAdmin && in.Role != auth.RoleTenantStaff {
		return User{}, errf(http.StatusBadRequest, "invalid_role", "role must be tenant_admin or tenant_staff")
	}
	if err := validateEmail(in.Email); err != nil {
		return User{}, err
	}
	if err := validatePassword(in.Password); err != nil {
		return User{}, err
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return User{}, err
	}
	var u User
	err = s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRow(ctx, `INSERT INTO users (tenant_id, email, name, password_hash, role) VALUES ($1,$2,$3,$4,$5) RETURNING `+userCols,
			tenant, in.Email, in.Name, hash, in.Role))
		if db.IsUniqueViolation(err) {
			return errf(http.StatusConflict, "email_taken", "a user with this email already exists")
		}
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "team.add", TargetType: "user", TargetID: u.ID.String(),
			Metadata: map[string]any{"role": in.Role, "email": in.Email}})
	})
	return u, err
}

func (s *Service) SetUserStatus(ctx context.Context, tenant, userID uuid.UUID, status string) error {
	if status != "active" && status != "disabled" {
		return errf(http.StatusBadRequest, "invalid_status", "status must be active or disabled")
	}
	if Meta(ctx).Principal.UserID == userID {
		return errf(http.StatusBadRequest, "invalid_target", "you cannot change your own status")
	}
	return s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET status=$3, updated_at=now() WHERE id=$2 AND tenant_id=$1 AND role <> 'platform_admin'`, tenant, userID, status)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if status == "disabled" {
			if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, userID); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "user.status", TargetType: "user", TargetID: userID.String(),
			Metadata: map[string]any{"status": status}})
	})
}

func (s *Service) ListUsers(ctx context.Context, tenant uuid.UUID, roles []string, q string, limit int) ([]User, error) {
	var out []User
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+userCols+` FROM users WHERE tenant_id=$1 AND role = ANY($2)
			AND ($3 = '' OR email ILIKE '%'||$3||'%' OR name ILIKE '%'||$3||'%') ORDER BY created_at DESC LIMIT $4`, tenant, roles, q, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanUser(rows)
			if err != nil {
				return err
			}
			out = append(out, u)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Partner API keys
// ---------------------------------------------------------------------------

type APIKey struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	CreatedAt  time.Time  `json:"created_at"`
	Secret     string     `json:"secret,omitempty"` // only on creation
}

func (s *Service) CreateAPIKey(ctx context.Context, tenant uuid.UUID, name string, scopes []string, expiresAt *time.Time) (APIKey, error) {
	if strings.TrimSpace(name) == "" || len(scopes) == 0 {
		return APIKey{}, errf(http.StatusBadRequest, "invalid_input", "name and at least one scope are required")
	}
	for _, sc := range scopes {
		ok := false
		for _, a := range auth.AllScopes {
			ok = ok || a == sc
		}
		if !ok {
			return APIKey{}, errf(http.StatusBadRequest, "invalid_scope", "unknown scope %q", sc)
		}
	}
	prefix := "zk_" + secure.RandomToken(6)
	secret := prefix + "." + secure.RandomToken(32)
	k := APIKey{Name: name, Prefix: prefix, Scopes: scopes, ExpiresAt: expiresAt, Secret: secret}
	creator := Meta(ctx).Principal.UserID
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO api_keys (tenant_id, name, prefix, key_hash, scopes, created_by, expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7)
			RETURNING id, created_at`, tenant, name, prefix, secure.HashSecret(secret), scopes, creator, expiresAt).Scan(&k.ID, &k.CreatedAt)
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "api_key.create", TargetType: "api_key", TargetID: k.ID.String(),
			Metadata: map[string]any{"scopes": scopes, "prefix": prefix}})
	})
	return k, err
}

func (s *Service) ListAPIKeys(ctx context.Context, tenant uuid.UUID) ([]APIKey, error) {
	var out []APIKey
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, name, prefix, scopes, last_used_at, expires_at, revoked_at, created_at FROM api_keys
			WHERE tenant_id=$1 ORDER BY created_at DESC`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k APIKey
			if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Scopes, &k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt, &k.CreatedAt); err != nil {
				return err
			}
			out = append(out, k)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Service) RevokeAPIKey(ctx context.Context, tenant, id uuid.UUID) error {
	return s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at=now() WHERE id=$1 AND tenant_id=$2 AND revoked_at IS NULL`, id, tenant)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "api_key.revoke", TargetType: "api_key", TargetID: id.String()})
	})
}

// AuthenticateAPIKey resolves "zk_xxx.secret" to a tenant-scoped principal.
func (s *Service) AuthenticateAPIKey(ctx context.Context, secret string) (auth.Principal, error) {
	invalid := errf(http.StatusUnauthorized, "invalid_api_key", "API key is invalid, expired or revoked")
	prefix, _, ok := strings.Cut(secret, ".")
	if !ok || !strings.HasPrefix(prefix, "zk_") {
		return auth.Principal{}, invalid
	}
	var p auth.Principal
	var hash []byte
	var expires, revoked *time.Time
	var tenantStatus string
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT k.id, k.tenant_id, k.key_hash, k.scopes, k.expires_at, k.revoked_at, t.status
			FROM api_keys k JOIN tenants t ON t.id=k.tenant_id WHERE k.prefix=$1`, prefix).
			Scan(&p.KeyID, &p.TenantID, &hash, &p.Scopes, &expires, &revoked, &tenantStatus)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE api_keys SET last_used_at=now() WHERE id=$1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, p.KeyID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Principal{}, invalid
	}
	if err != nil {
		return auth.Principal{}, err
	}
	if string(hash) != string(secure.HashSecret(secret)) || revoked != nil || (expires != nil && expires.Before(s.Now())) || tenantStatus != "active" {
		return auth.Principal{}, invalid
	}
	p.Kind = "api_key"
	return p, nil
}

// ---------------------------------------------------------------------------
// Partner single sign-on for the hosted electricity module
// ---------------------------------------------------------------------------

type PartnerSessionInput struct {
	ExternalUserID string `json:"external_user_id"`
	Email          string `json:"email"`
	Name           string `json:"name"`
	Phone          string `json:"phone"`
	ReturnOrigin   string `json:"return_origin"`
}

type PartnerSession struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	EmbedURL  string    `json:"embed_url"`
	UserID    uuid.UUID `json:"user_id"`
}

// CreatePartnerSession is called server-to-server by the partner after it has
// authenticated its own user. It upserts the customer (keyed by the partner's
// stable external id) and returns a single-use code valid for ~2 minutes.
func (s *Service) CreatePartnerSession(ctx context.Context, p auth.Principal, in PartnerSessionInput) (PartnerSession, error) {
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if in.ExternalUserID == "" || len(in.ExternalUserID) > 128 {
		return PartnerSession{}, errf(http.StatusBadRequest, "invalid_input", "external_user_id is required (max 128 chars)")
	}
	if err := validateEmail(in.Email); err != nil {
		return PartnerSession{}, err
	}
	t, err := s.TenantByID(ctx, p.TenantID)
	if err != nil {
		return PartnerSession{}, err
	}
	if in.ReturnOrigin != "" {
		allowed := false
		for _, o := range t.AllowedOrigins {
			allowed = allowed || o == in.ReturnOrigin
		}
		if !allowed {
			return PartnerSession{}, errf(http.StatusBadRequest, "origin_not_allowed", "return_origin is not an approved origin for this tenant")
		}
	}
	code := "zec_" + secure.RandomToken(24)
	out := PartnerSession{Code: code, ExpiresAt: s.Now().Add(s.Cfg.EmbedTTL)}
	err = s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO users (tenant_id, email, phone, name, external_id, role) VALUES ($1,$2,$3,$4,$5,'customer')
			ON CONFLICT (tenant_id, external_id) WHERE external_id IS NOT NULL
			DO UPDATE SET email=EXCLUDED.email, name=coalesce(nullif(EXCLUDED.name,''), users.name),
			phone=coalesce(nullif(EXCLUDED.phone,''), users.phone), updated_at=now()
			RETURNING id`, t.ID, in.Email, in.Phone, in.Name, in.ExternalUserID).Scan(&out.UserID)
		if db.IsUniqueViolation(err, "users_tenant_email") {
			return errf(http.StatusConflict, "email_taken", "another account in this tenant already uses that email")
		}
		if err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1`, out.UserID).Scan(&status); err != nil {
			return err
		}
		if status != "active" {
			return errf(http.StatusForbidden, "user_disabled", "this customer is disabled")
		}
		_, err = tx.Exec(ctx, `INSERT INTO embed_codes (tenant_id, user_id, code_hash, expires_at) VALUES ($1,$2,$3,$4)`,
			t.ID, out.UserID, secure.HashSecret(code), out.ExpiresAt)
		return err
	})
	if err != nil {
		return PartnerSession{}, err
	}
	out.EmbedURL = s.Cfg.EmbedBaseURL + "/?embed=1&tenant=" + t.Slug + "#code=" + code
	return out, nil
}

// ExchangeEmbedCode redeems a single-use code for a short-lived access token.
// No refresh token is issued: the partner mints a new code when needed.
func (s *Service) ExchangeEmbedCode(ctx context.Context, code string) (Session, error) {
	invalid := errf(http.StatusUnauthorized, "invalid_code", "embed code is invalid, used or expired")
	var tenantID, userID uuid.UUID
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `UPDATE embed_codes SET used_at=now() WHERE code_hash=$1 AND used_at IS NULL AND expires_at > now()
			RETURNING tenant_id, user_id`, secure.HashSecret(code)).Scan(&tenantID, &userID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, invalid
	}
	if err != nil {
		return Session{}, err
	}
	t, err := s.TenantByID(ctx, tenantID)
	if err != nil {
		return Session{}, err
	}
	var u User
	err = s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1 AND status='active'`, userID))
		return err
	})
	if err != nil {
		return Session{}, invalid
	}
	return s.newSession(ctx, t, u, "partner_sso", false)
}

func (s *Service) PartnerCustomer(ctx context.Context, tenant uuid.UUID, externalID string) (User, error) {
	var u User
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE tenant_id=$1 AND external_id=$2`, tenant, externalID))
		return err
	})
	return u, err
}
