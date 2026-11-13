package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/messaging"
	"github.com/heismyke/zieonenergies/internal/secure"
)

const (
	passwordResetTTL      = 30 * time.Minute
	passwordResetsPerHour = 3
)

// ForgotPassword emails a reset link to a customer account with that email.
// It reports success whatever the email is, and sends in the background, so
// the response does not reveal whether an account exists.
func (s *Service) ForgotPassword(ctx context.Context, t Tenant, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if err := validateEmail(email); err != nil {
		return err
	}
	sender := s.Messengers[messaging.Email]
	if sender == nil {
		return errf(http.StatusServiceUnavailable, "email_unavailable", "password reset by email is not available right now")
	}
	if t.Status != "active" {
		return nil
	}
	var msg messaging.Message
	err := s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		var user uuid.UUID
		var name string
		err := tx.QueryRow(ctx, `SELECT id, name FROM users WHERE tenant_id=$1 AND lower(email)=$2 AND role=$3 AND status='active'`,
			t.ID, email, auth.RoleCustomer).Scan(&user, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var recent int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM password_resets WHERE user_id=$1 AND created_at > now() - interval '1 hour'`,
			user).Scan(&recent); err != nil {
			return err
		}
		if recent >= passwordResetsPerHour {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE password_resets SET expires_at=now() WHERE user_id=$1 AND used_at IS NULL AND expires_at > now()`, user); err != nil {
			return err
		}
		code := secure.RandomToken(24)
		if _, err := tx.Exec(ctx, `INSERT INTO password_resets (tenant_id, user_id, code_hash, expires_at) VALUES ($1,$2,$3,$4)`,
			t.ID, user, secure.HashSecret(code), s.Now().Add(passwordResetTTL)); err != nil {
			return err
		}
		var b brandInfo
		if err := tx.QueryRow(ctx, `SELECT coalesce(b.display_name,''), coalesce(b.logo_url,''), coalesce(b.theme->>'primary',''),
				coalesce(b.support_email,''), coalesce(b.custom_domain,'')
			FROM tenants t LEFT JOIN tenant_branding b ON b.tenant_id = t.id WHERE t.id=$1`, t.ID).
			Scan(&b.name, &b.logo, &b.accent, &b.support, &b.domain); err != nil {
			return err
		}
		msg = renderResetMessage(b, s.siteFor(b.domain), email, name, code)
		return nil
	})
	if err != nil || msg.To == "" {
		return err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := sender.Send(ctx, msg); err != nil {
			s.Log.Warn("password reset email failed", "tenant", t.Slug, "err", err)
		}
	}()
	return nil
}

var errResetLink = errf(http.StatusBadRequest, "invalid_reset_link", "this reset link is invalid or has expired; ask for a new one")

// ResetPassword sets a new password from a reset link. The link is used up,
// and every signed-in device is signed out.
func (s *Service) ResetPassword(ctx context.Context, t Tenant, code, password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	if code == "" || len(code) > 200 {
		return errResetLink
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	return s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		var user uuid.UUID
		err := tx.QueryRow(ctx, `UPDATE password_resets SET used_at=now()
			WHERE tenant_id=$1 AND code_hash=$2 AND used_at IS NULL AND expires_at > now() RETURNING user_id`,
			t.ID, secure.HashSecret(code)).Scan(&user)
		if errors.Is(err, pgx.ErrNoRows) {
			return errResetLink
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2, failed_logins=0, locked_until=NULL, updated_at=now()
			WHERE id=$1 AND status='active'`, user, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errResetLink
		}
		_, err = tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, user)
		return err
	})
}

type brandInfo struct{ name, logo, accent, support, domain string }

// siteFor is a tenant's buy site: its own domain, or the platform site.
func (s *Service) siteFor(domain string) string {
	if domain != "" {
		return "https://" + domain
	}
	return s.Cfg.SiteURL
}

func renderResetMessage(b brandInfo, site, email, name, code string) messaging.Message {
	brand := b.name
	if brand == "" {
		brand = "Zenerlink"
	}
	logo := b.logo
	if logo == "" && brand == "Zenerlink" {
		logo = site + "/icon-192.png"
	}
	link := site + "/reset-password?code=" + url.QueryEscape(code)
	hello := "Hi,"
	if first := strings.Fields(name); len(first) > 0 {
		hello = "Hi " + first[0] + ","
	}
	text := fmt.Sprintf("%s\n\nWe got a request to reset the password for your %s account (%s).\n\nSet a new password here (the link works once and expires in 30 minutes):\n%s\n\nIf you didn't ask for this, ignore this email. Your password stays the same.\n\n%s",
		hello, brand, email, link, brand)
	html := renderResetEmail(resetEmail{Brand: strings.ToUpper(brand), BrandName: brand, Logo: logo, Accent: b.accent,
		Hello: hello, Email: email, Link: link, Support: b.support, SiteURL: site})
	return messaging.Message{To: email, Subject: "Reset your " + brand + " password", Text: text, HTML: html}
}
