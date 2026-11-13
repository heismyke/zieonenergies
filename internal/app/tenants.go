package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/db"
)

// Feature flags a tenant can toggle. Unknown keys are rejected.
var KnownFeatures = map[string]bool{
	"wallet": true, "spark_share": true, "external_meters": true, "auto_topup": true,
	"card_payments": true, "bank_transfer": true, "crypto_payments": false, "benefits": false,
	"energy_exchange": false, "subscriptions": true, "support_chat": true, "properties": false,
	"guest_checkout": true, // public buy page: DisCo list, meter lookup, account orders
	"guest_orders":   false, // buying without an account (order key instead of a login)
}

func DefaultFeatures() map[string]bool {
	out := map[string]bool{}
	for k, v := range KnownFeatures {
		out[k] = v
	}
	return out
}

type Tenant struct {
	ID             uuid.UUID       `json:"id"`
	Slug           string          `json:"slug"`
	Name           string          `json:"name"`
	Status         string          `json:"status"`
	IsPlatform     bool            `json:"is_platform"`
	Features       map[string]bool `json:"features"`
	AllowedOrigins []string        `json:"allowed_origins"`
	VendingMode    string          `json:"vending_mode"`
	CreatedAt      time.Time       `json:"created_at"`
}

func (t Tenant) Enabled(feature string) bool { return t.Features[feature] }

// Theme tokens mirror the frontends' design tokens (zener.gold, surface, ...),
// so a tenant re-skins the apps without code changes.
var themeKeys = []string{"primary", "secondary", "background", "surface", "panel", "border", "success", "danger", "text", "text_muted"}

var hexColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

func DefaultTheme() map[string]string {
	return map[string]string{
		"primary": "#C8823A", "secondary": "#222222", "background": "#050505", "surface": "#111111",
		"panel": "#17110A", "border": "#222222", "success": "#0DF020", "danger": "#FF5A5A",
		"text": "#FFFFFF", "text_muted": "#8C8C8C",
	}
}

type Branding struct {
	TenantSlug    string            `json:"tenant"`
	DisplayName   string            `json:"display_name"`
	LogoURL       string            `json:"logo_url"`
	LogoDarkURL   string            `json:"logo_dark_url"`
	Theme         map[string]string `json:"theme"`
	FontFamily    string            `json:"font_family"`
	Subdomain     *string           `json:"subdomain"`
	CustomDomain  *string           `json:"custom_domain"`
	SupportEmail  string            `json:"support_email"`
	SupportPhone  string            `json:"support_phone"`
	SupportURL    string            `json:"support_url"`
	Currency      string            `json:"currency"`
	Locale        string            `json:"locale"`
	EmailFromName string            `json:"email_from_name"`
	EmailFooter   string            `json:"email_footer"`
	Features      map[string]bool   `json:"features"`
	Sandbox       bool              `json:"sandbox"`
}

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	var feats []byte
	err := row.Scan(&t.ID, &t.Slug, &t.Name, &t.Status, &t.IsPlatform, &feats, &t.AllowedOrigins, &t.VendingMode, &t.CreatedAt)
	if err != nil {
		return t, notFoundIfNoRows(err)
	}
	t.Features = DefaultFeatures()
	var stored map[string]bool
	_ = json.Unmarshal(feats, &stored)
	for k, v := range stored {
		t.Features[k] = v
	}
	return t, nil
}

const tenantCols = `id, slug, name, status, is_platform, features, allowed_origins, vending_mode, created_at`

// TenantBySlug is a public lookup (tenants carry no secrets).
func (s *Service) TenantBySlug(ctx context.Context, slug string) (Tenant, error) {
	var t Tenant
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		var err error
		t, err = scanTenant(tx.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE slug=$1`, strings.ToLower(slug)))
		return err
	})
	return t, err
}

// TenantByHost resolves a tenant from a custom domain or <subdomain>.<base>.
func (s *Service) TenantByHost(ctx context.Context, host string) (Tenant, error) {
	host = strings.ToLower(strings.Split(host, ":")[0])
	var t Tenant
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		var err error
		t, err = scanTenant(tx.QueryRow(ctx, `SELECT `+tenantColsT+` FROM tenants t JOIN tenant_branding b ON b.tenant_id=t.id
			WHERE b.custom_domain=$1 OR b.subdomain=split_part($1,'.',1) LIMIT 1`, host))
		return err
	})
	return t, err
}

const tenantColsT = `t.id, t.slug, t.name, t.status, t.is_platform, t.features, t.allowed_origins, t.vending_mode, t.created_at`

func (s *Service) TenantByID(ctx context.Context, id uuid.UUID) (Tenant, error) {
	var t Tenant
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		var err error
		t, err = scanTenant(tx.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id=$1`, id))
		return err
	})
	return t, err
}

// OriginAllowed is used by CORS: any active tenant may list approved origins.
func (s *Service) OriginAllowed(ctx context.Context, origin string) bool {
	var ok bool
	_ = s.DB.System(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE status='active' AND $1 = ANY(allowed_origins))`, origin).Scan(&ok)
	})
	return ok
}

func (s *Service) Branding(ctx context.Context, t Tenant) (Branding, error) {
	b := Branding{TenantSlug: t.Slug, Features: t.Features, Sandbox: t.VendingMode == "sandbox"}
	err := s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		var theme []byte
		err := tx.QueryRow(ctx, `SELECT display_name, logo_url, logo_dark_url, theme, font_family, subdomain, custom_domain,
			support_email, support_phone, support_url, currency, locale, email_from_name, email_footer
			FROM tenant_branding WHERE tenant_id=$1`, t.ID).Scan(&b.DisplayName, &b.LogoURL, &b.LogoDarkURL, &theme, &b.FontFamily,
			&b.Subdomain, &b.CustomDomain, &b.SupportEmail, &b.SupportPhone, &b.SupportURL, &b.Currency, &b.Locale,
			&b.EmailFromName, &b.EmailFooter)
		if err != nil {
			return notFoundIfNoRows(err)
		}
		b.Theme = DefaultTheme()
		var stored map[string]string
		_ = json.Unmarshal(theme, &stored)
		for k, v := range stored {
			b.Theme[k] = v
		}
		return nil
	})
	return b, err
}

type BrandingInput struct {
	DisplayName   *string           `json:"display_name"`
	LogoURL       *string           `json:"logo_url"`
	LogoDarkURL   *string           `json:"logo_dark_url"`
	Theme         map[string]string `json:"theme"`
	FontFamily    *string           `json:"font_family"`
	Subdomain     *string           `json:"subdomain"`
	CustomDomain  *string           `json:"custom_domain"`
	SupportEmail  *string           `json:"support_email"`
	SupportPhone  *string           `json:"support_phone"`
	SupportURL    *string           `json:"support_url"`
	Currency      *string           `json:"currency"`
	Locale        *string           `json:"locale"`
	EmailFromName *string           `json:"email_from_name"`
	EmailFooter   *string           `json:"email_footer"`
}

var hostname = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
var label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,40}[a-z0-9])?$`)

func validateBranding(in BrandingInput) error {
	for k, v := range in.Theme {
		known := false
		for _, tk := range themeKeys {
			known = known || tk == k
		}
		if !known {
			return errf(http.StatusBadRequest, "invalid_theme", "unknown theme token %q", k)
		}
		if !hexColor.MatchString(v) {
			return errf(http.StatusBadRequest, "invalid_theme", "theme token %q must be a #RRGGBB colour", k)
		}
	}
	for name, u := range map[string]*string{"logo_url": in.LogoURL, "logo_dark_url": in.LogoDarkURL, "support_url": in.SupportURL} {
		if u != nil && *u != "" {
			p, err := url.Parse(*u)
			if err != nil || p.Scheme != "https" || p.Host == "" {
				return errf(http.StatusBadRequest, "invalid_url", "%s must be an https URL", name)
			}
		}
	}
	if in.CustomDomain != nil && *in.CustomDomain != "" && !hostname.MatchString(*in.CustomDomain) {
		return errf(http.StatusBadRequest, "invalid_domain", "custom_domain must be a hostname")
	}
	if in.Subdomain != nil && *in.Subdomain != "" && !label.MatchString(*in.Subdomain) {
		return errf(http.StatusBadRequest, "invalid_domain", "subdomain must be a DNS label")
	}
	if in.Currency != nil && !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(*in.Currency) {
		return errf(http.StatusBadRequest, "invalid_currency", "currency must be an ISO 4217 code")
	}
	if in.DisplayName != nil && strings.TrimSpace(*in.DisplayName) == "" {
		return errf(http.StatusBadRequest, "invalid_name", "display_name is required")
	}
	return nil
}

func (s *Service) UpdateBranding(ctx context.Context, t Tenant, in BrandingInput) (Branding, error) {
	if err := validateBranding(in); err != nil {
		return Branding{}, err
	}
	err := s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		var theme []byte
		if in.Theme != nil {
			theme, _ = json.Marshal(in.Theme)
		}
		nullable := func(p *string) any {
			if p == nil {
				return nil
			}
			return *p
		}
		_, err := tx.Exec(ctx, `UPDATE tenant_branding SET
			display_name=coalesce($2,display_name), logo_url=coalesce($3,logo_url), logo_dark_url=coalesce($4,logo_dark_url),
			theme=CASE WHEN $5::jsonb IS NULL THEN theme ELSE theme || $5::jsonb END,
			font_family=coalesce($6,font_family),
			subdomain=CASE WHEN $7::text IS NULL THEN subdomain ELSE nullif($7,'') END,
			custom_domain=CASE WHEN $8::text IS NULL THEN custom_domain ELSE nullif($8,'') END,
			support_email=coalesce($9,support_email), support_phone=coalesce($10,support_phone), support_url=coalesce($11,support_url),
			currency=coalesce($12,currency), locale=coalesce($13,locale), email_from_name=coalesce($14,email_from_name),
			email_footer=coalesce($15,email_footer), updated_at=now()
			WHERE tenant_id=$1`, t.ID, nullable(in.DisplayName), nullable(in.LogoURL), nullable(in.LogoDarkURL), theme,
			nullable(in.FontFamily), nullable(in.Subdomain), nullable(in.CustomDomain), nullable(in.SupportEmail),
			nullable(in.SupportPhone), nullable(in.SupportURL), nullable(in.Currency), nullable(in.Locale),
			nullable(in.EmailFromName), nullable(in.EmailFooter))
		if db.IsUniqueViolation(err) {
			return errf(http.StatusConflict, "domain_taken", "that subdomain or custom domain is already in use")
		}
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &t.ID, Action: "tenant.branding.update", TargetType: "tenant", TargetID: t.ID.String()})
	})
	if err != nil {
		return Branding{}, err
	}
	return s.Branding(ctx, t)
}

type TenantSettingsInput struct {
	Features       map[string]bool `json:"features"`
	AllowedOrigins *[]string       `json:"allowed_origins"`
}

func (s *Service) UpdateTenantSettings(ctx context.Context, t Tenant, in TenantSettingsInput) (Tenant, error) {
	for k := range in.Features {
		if _, ok := KnownFeatures[k]; !ok {
			return t, errf(http.StatusBadRequest, "invalid_feature", "unknown feature %q", k)
		}
	}
	if in.AllowedOrigins != nil {
		for _, o := range *in.AllowedOrigins {
			u, err := url.Parse(o)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
				if !(s.Cfg.Env != "production" && strings.HasPrefix(o, "http://localhost")) {
					return t, errf(http.StatusBadRequest, "invalid_origin", "origin %q must look like https://host", o)
				}
			}
		}
	}
	err := s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		feats, _ := json.Marshal(in.Features)
		if in.Features == nil {
			feats = []byte("{}")
		}
		var origins any
		if in.AllowedOrigins != nil {
			origins = *in.AllowedOrigins
		}
		// tenants has no RLS; the explicit id predicate is the guard.
		if _, err := tx.Exec(ctx, `UPDATE tenants SET features = features || $2::jsonb,
			allowed_origins = coalesce($3::text[], allowed_origins), updated_at=now() WHERE id=$1`, t.ID, feats, origins); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &t.ID, Action: "tenant.settings.update", TargetType: "tenant", TargetID: t.ID.String(),
			Metadata: map[string]any{"features": in.Features, "allowed_origins": in.AllowedOrigins}})
	})
	if err != nil {
		return t, err
	}
	return s.TenantByID(ctx, t.ID)
}

// ---------------------------------------------------------------------------
// Provisioning (platform administrators only)
// ---------------------------------------------------------------------------

type TariffInput struct {
	ProviderCode    string `json:"provider_code"`
	ProviderName    string `json:"provider_name"`
	RateKoboPerKwh  int64  `json:"rate_kobo_per_kwh"`
	ServiceFeeKobo  int64  `json:"service_fee_kobo"`
	VatBps          int    `json:"vat_bps"`
	MinPurchaseKobo int64  `json:"min_purchase_kobo"`
	MaxPurchaseKobo int64  `json:"max_purchase_kobo"`
}

type ProvisionInput struct {
	Slug           string          `json:"slug"`
	Name           string          `json:"name"`
	Branding       BrandingInput   `json:"branding"`
	Features       map[string]bool `json:"features"`
	AllowedOrigins []string        `json:"allowed_origins"`
	Admin          struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
	} `json:"admin"`
	Tariffs    []TariffInput `json:"tariffs"`
	IsPlatform bool          `json:"-"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,40}$`)

// NigerianDiscos is the default provider catalogue shown in both apps.
var NigerianDiscos = [][2]string{
	{"AEDC", "Abuja Electricity"}, {"EKEDC", "Eko Electricity"}, {"KEDC", "Kaduna Electricity"},
	{"IKEDC", "Ikeja Electricity"}, {"PHED", "Port Harcourt Electricity"}, {"BEDC", "Benin Electricity"},
	{"EEDC", "Enugu Electricity"}, {"IBEDC", "Ibadan Electricity"}, {"JED", "Jos Electricity"},
	{"KEDCO", "Kano Electricity"}, {"YEDC", "Yola Electricity"}, {"ABA", "Aba Electricity"}, {"ZIEON", "Zieon Smart Meter"},
}

func (s *Service) ProvisionTenant(ctx context.Context, in ProvisionInput) (Tenant, error) {
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	if !slugRe.MatchString(in.Slug) {
		return Tenant{}, errf(http.StatusBadRequest, "invalid_slug", "slug must be 2-41 lowercase letters, digits or hyphens")
	}
	if strings.TrimSpace(in.Name) == "" {
		return Tenant{}, errf(http.StatusBadRequest, "invalid_name", "name is required")
	}
	if err := validateEmail(in.Admin.Email); err != nil {
		return Tenant{}, err
	}
	if err := validatePassword(in.Admin.Password); err != nil {
		return Tenant{}, err
	}
	if in.Branding.DisplayName == nil {
		in.Branding.DisplayName = &in.Name
	}
	if err := validateBranding(in.Branding); err != nil {
		return Tenant{}, err
	}
	for k := range in.Features {
		if _, ok := KnownFeatures[k]; !ok {
			return Tenant{}, errf(http.StatusBadRequest, "invalid_feature", "unknown feature %q", k)
		}
	}
	if len(in.Tariffs) == 0 {
		for _, d := range NigerianDiscos {
			in.Tariffs = append(in.Tariffs, TariffInput{ProviderCode: d[0], ProviderName: d[1], RateKoboPerKwh: 20000})
		}
	}
	pwHash, err := auth.HashPassword(in.Admin.Password)
	if err != nil {
		return Tenant{}, err
	}
	adminRole := auth.RoleTenantAdmin
	if in.IsPlatform {
		adminRole = auth.RolePlatformAdmin
	}
	var id uuid.UUID
	err = s.DB.System(ctx, func(tx pgx.Tx) error {
		feats, _ := json.Marshal(in.Features)
		if in.Features == nil {
			feats = []byte("{}")
		}
		if in.AllowedOrigins == nil {
			in.AllowedOrigins = []string{}
		}
		err := tx.QueryRow(ctx, `INSERT INTO tenants (slug, name, is_platform, features, allowed_origins) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			in.Slug, in.Name, in.IsPlatform, feats, in.AllowedOrigins).Scan(&id)
		if db.IsUniqueViolation(err) {
			return errf(http.StatusConflict, "slug_taken", "a tenant with that slug already exists")
		}
		if err != nil {
			return err
		}
		b := in.Branding
		theme := DefaultTheme()
		for k, v := range b.Theme {
			theme[k] = v
		}
		themeJSON, _ := json.Marshal(theme)
		str := func(p *string, def string) string {
			if p == nil {
				return def
			}
			return *p
		}
		nullable := func(p *string) any {
			if p == nil || *p == "" {
				return nil
			}
			return *p
		}
		_, err = tx.Exec(ctx, `INSERT INTO tenant_branding (tenant_id, display_name, logo_url, logo_dark_url, theme, font_family, subdomain,
			custom_domain, support_email, support_phone, support_url, currency, locale, email_from_name, email_footer)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			id, *b.DisplayName, str(b.LogoURL, ""), str(b.LogoDarkURL, ""), themeJSON, str(b.FontFamily, "Satoshi"),
			nullable(b.Subdomain), nullable(b.CustomDomain), str(b.SupportEmail, ""), str(b.SupportPhone, ""), str(b.SupportURL, ""),
			str(b.Currency, "NGN"), str(b.Locale, "en-NG"), str(b.EmailFromName, *b.DisplayName), str(b.EmailFooter, ""))
		if db.IsUniqueViolation(err) {
			return errf(http.StatusConflict, "domain_taken", "that subdomain or custom domain is already in use")
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO users (tenant_id, email, name, password_hash, role) VALUES ($1,$2,$3,$4,$5)`,
			id, strings.ToLower(in.Admin.Email), in.Admin.Name, pwHash, adminRole); err != nil {
			return err
		}
		for _, tr := range in.Tariffs {
			if err := upsertTariff(ctx, tx, id, tr); err != nil {
				return err
			}
		}
		for i, p := range defaultPlans {
			if _, err := tx.Exec(ctx, `INSERT INTO subscription_plans (tenant_id, code, name, price_kobo, features, rank) VALUES ($1,$2,$3,$4,$5,$6)`,
				id, p.Code, p.Name, p.PriceKobo, p.Features, i); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &id, Action: "platform.tenant.provision", TargetType: "tenant", TargetID: id.String(),
			Metadata: map[string]any{"slug": in.Slug, "admin_email": in.Admin.Email}})
	})
	if err != nil {
		return Tenant{}, err
	}
	return s.TenantByID(ctx, id)
}

func upsertTariff(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, tr TariffInput) error {
	tr.ProviderCode = strings.ToUpper(strings.TrimSpace(tr.ProviderCode))
	if tr.ProviderCode == "" || tr.RateKoboPerKwh <= 0 || tr.ServiceFeeKobo < 0 || tr.VatBps < 0 || tr.VatBps > 5000 {
		return errf(http.StatusBadRequest, "invalid_tariff", "tariff needs provider_code, positive rate_kobo_per_kwh, vat_bps 0-5000")
	}
	if tr.MinPurchaseKobo == 0 {
		tr.MinPurchaseKobo = 10000
	}
	if tr.MaxPurchaseKobo == 0 {
		tr.MaxPurchaseKobo = 50000000
	}
	if tr.ProviderName == "" {
		tr.ProviderName = tr.ProviderCode
	}
	_, err := tx.Exec(ctx, `INSERT INTO tariffs (tenant_id, provider_code, provider_name, rate_kobo_per_kwh, service_fee_kobo, vat_bps, min_purchase_kobo, max_purchase_kobo)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (tenant_id, provider_code) DO UPDATE SET provider_name=EXCLUDED.provider_name, rate_kobo_per_kwh=EXCLUDED.rate_kobo_per_kwh,
		service_fee_kobo=EXCLUDED.service_fee_kobo, vat_bps=EXCLUDED.vat_bps, min_purchase_kobo=EXCLUDED.min_purchase_kobo,
		max_purchase_kobo=EXCLUDED.max_purchase_kobo, updated_at=now()`,
		tenant, tr.ProviderCode, tr.ProviderName, tr.RateKoboPerKwh, tr.ServiceFeeKobo, tr.VatBps, tr.MinPurchaseKobo, tr.MaxPurchaseKobo)
	return err
}

func (s *Service) UpsertTariff(ctx context.Context, tenant uuid.UUID, tr TariffInput) error {
	return s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := upsertTariff(ctx, tx, tenant, tr); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "tariff.upsert", TargetType: "tariff", TargetID: tr.ProviderCode,
			Metadata: map[string]any{"rate_kobo_per_kwh": tr.RateKoboPerKwh, "fee": tr.ServiceFeeKobo, "vat_bps": tr.VatBps}})
	})
}

func (s *Service) ListTenants(ctx context.Context) ([]Tenant, error) {
	var out []Tenant
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+tenantCols+` FROM tenants ORDER BY created_at`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTenant(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Service) SetTenantStatus(ctx context.Context, id uuid.UUID, status, vendingMode string) (Tenant, error) {
	if status != "" && status != "active" && status != "suspended" {
		return Tenant{}, errf(http.StatusBadRequest, "invalid_status", "status must be active or suspended")
	}
	if vendingMode != "" && vendingMode != "sandbox" && vendingMode != "live" {
		return Tenant{}, errf(http.StatusBadRequest, "invalid_vending_mode", "vending_mode must be sandbox or live")
	}
	if vendingMode == "live" && !s.LiveRouteAvailable() {
		return Tenant{}, errf(http.StatusConflict, "live_vending_unavailable",
			"no authorised live vending route is configured on this platform; tenant must stay in sandbox")
	}
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tenants SET status=coalesce(nullif($2,''),status), vending_mode=coalesce(nullif($3,''),vending_mode),
			updated_at=now() WHERE id=$1 AND (NOT is_platform OR $2 = '')`, id, status, vendingMode) // the platform tenant can switch vending mode but never be suspended
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, AuditEntry{TenantID: &id, Action: "platform.tenant.update", TargetType: "tenant", TargetID: id.String(),
			Metadata: map[string]any{"status": status, "vending_mode": vendingMode}})
	})
	if err != nil {
		return Tenant{}, err
	}
	return s.TenantByID(ctx, id)
}

// ---------------------------------------------------------------------------
// Tariffs and subscription plans (customer-facing catalogue)
// ---------------------------------------------------------------------------

type Tariff struct {
	ProviderCode    string  `json:"provider_code"`
	ProviderName    string  `json:"provider_name"`
	RateKoboPerKwh  int64   `json:"rate_kobo_per_kwh"`
	RatePerKwh      float64 `json:"rate_per_kwh"`
	ServiceFeeKobo  int64   `json:"service_fee_kobo"`
	VatBps          int     `json:"vat_bps"`
	MinPurchaseKobo int64   `json:"min_purchase_kobo"`
	MaxPurchaseKobo int64   `json:"max_purchase_kobo"`
}

func scanTariff(row pgx.Row) (Tariff, error) {
	var t Tariff
	err := row.Scan(&t.ProviderCode, &t.ProviderName, &t.RateKoboPerKwh, &t.ServiceFeeKobo, &t.VatBps, &t.MinPurchaseKobo, &t.MaxPurchaseKobo)
	t.RatePerKwh = float64(t.RateKoboPerKwh) / 100
	return t, err
}

const tariffCols = `provider_code, provider_name, rate_kobo_per_kwh, service_fee_kobo, vat_bps, min_purchase_kobo, max_purchase_kobo`

func (s *Service) Tariffs(ctx context.Context, tenant uuid.UUID) ([]Tariff, error) {
	var out []Tariff
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+tariffCols+` FROM tariffs WHERE tenant_id=$1 AND active ORDER BY provider_code`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTariff(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

type Plan struct {
	Code      string   `json:"code"`
	Name      string   `json:"name"`
	PriceKobo int64    `json:"price_kobo"`
	Period    string   `json:"period"`
	Features  []string `json:"features"`
}

// Seeded from the Ohm plans shown in the mobile app.
var defaultPlans = []Plan{
	{Code: "ohm_free", Name: "Ohm Free", PriceKobo: 0, Features: []string{"Daily kWh summary", "Weekly 7-day report", "Basic days-remaining forecast", "Personal average comparisons"}},
	{Code: "ohm_premium", Name: "Ohm Premium", PriceKobo: 50000, Features: []string{"Hourly consumption breakdown", "Appliance-level estimation", "30-day Naira forecast", "Anomaly alerts via SMS"}},
	{Code: "landlord_pro", Name: "Landlord Pro", PriceKobo: 150000, Features: []string{"Everything in Premium", "Monthly PDF reports", "Multi-meter monitoring", "SparkShare low-balance actions"}},
}

func (s *Service) Plans(ctx context.Context, tenant uuid.UUID) ([]Plan, error) {
	var out []Plan
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT code, name, price_kobo, period, features FROM subscription_plans WHERE tenant_id=$1 AND active ORDER BY rank`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p Plan
			if err := rows.Scan(&p.Code, &p.Name, &p.PriceKobo, &p.Period, &p.Features); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}
