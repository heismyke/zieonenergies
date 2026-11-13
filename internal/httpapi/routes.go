package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/heismyke/zieonenergies/internal/app"
	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/secure"
)

func (s *Server) routes() {
	s.handle("GET /healthz", func(w http.ResponseWriter, r *http.Request) error {
		return writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	s.handle("GET /readyz", func(w http.ResponseWriter, r *http.Request) error {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.svc.DB.Pool.Ping(ctx); err != nil {
			return &app.Error{Status: 503, Code: "db_unavailable", Message: "database unavailable"}
		}
		st := s.svc.DB.Pool.Stat()
		return writeJSON(w, 200, map[string]any{"status": "ready", "db_conns": st.TotalConns(), "db_idle": st.IdleConns()})
	})

	// Public: tenant branding and catalogue.
	s.handle("GET /v1/branding", s.getBranding)

	// Public guest checkout ("buy electricity" without an account).
	s.handle("GET /v1/public/discos", s.publicDiscos)
	s.handle("POST /v1/public/meters/verify", s.publicVerifyMeter)
	s.handle("POST /v1/public/orders", s.publicCreateOrder)
	s.handle("GET /v1/public/orders/{id}", s.publicGetOrder)

	// Customer authentication.
	s.handle("POST /v1/auth/register", s.register)
	s.handle("POST /v1/auth/login", s.login)
	s.handle("POST /v1/auth/refresh", s.refresh)
	s.handle("POST /v1/auth/logout", s.logout)
	s.handle("POST /v1/embed/exchange", s.embedExchange)

	// Signed-in user (any role).
	s.handle("GET /v1/me", s.me)
	s.handle("PATCH /v1/me", s.updateMe)
	s.handle("POST /v1/me/password", s.changePassword)

	// Customer electricity experience.
	s.handle("GET /v1/tariffs", s.tariffs)
	s.handle("GET /v1/plans", s.plans)
	s.handle("POST /v1/quotes", s.quote)
	s.handle("GET /v1/wallet", s.wallet)
	s.handle("POST /v1/wallet/fund", s.fundWallet)
	s.handle("GET /v1/wallet/activity", s.walletActivity)
	s.handle("GET /v1/meters", s.listMeters)
	s.handle("POST /v1/meters", s.linkMeter)
	s.handle("POST /v1/meters/validate", s.validateMeter)
	s.handle("GET /v1/meters/{id}", s.getMeter)
	s.handle("PATCH /v1/meters/{id}", s.updateMeter)
	s.handle("DELETE /v1/meters/{id}", s.unlinkMeter)
	s.handle("GET /v1/meters/{id}/consumption", s.consumption)
	s.handle("GET /v1/meters/{id}/readings", s.readings)
	s.handle("POST /v1/meters/{id}/transfer", s.requestTransfer)
	s.handle("POST /v1/orders", s.createOrder)
	s.handle("GET /v1/orders", s.listOrders)
	s.handle("GET /v1/orders/{id}", s.getOrder)
	s.handle("POST /v1/purchases", s.purchase)
	s.handle("GET /v1/purchases", s.listPurchases)
	s.handle("GET /v1/purchases/{id}", s.getPurchase)
	s.handle("GET /v1/tokens", s.pendingTokens)
	s.handle("POST /v1/tokens/redeem", s.redeemToken)
	s.handle("POST /v1/shares", s.share)
	s.handle("GET /v1/recipients", s.recipients)
	s.handle("POST /v1/recipients", s.addRecipient)
	s.handle("DELETE /v1/recipients/{meter_number}", s.removeRecipient)
	s.handle("GET /v1/activity", s.activity)
	s.handle("GET /v1/notifications", s.notifications)
	s.handle("POST /v1/notifications/{id}/read", s.readNotification)
	s.handle("GET /v1/properties", s.myProperties)
	s.handle("POST /v1/properties", s.createMyProperty)
	s.handle("GET /v1/benefits", s.benefits)
	s.handle("POST /v1/benefits/monthly-credit", s.claimMonthly)
	s.handle("GET /v1/support/tickets", s.tickets)
	s.handle("POST /v1/support/tickets", s.createTicket)
	s.handle("GET /v1/support/tickets/{id}", s.getTicket)
	s.handle("POST /v1/support/tickets/{id}/messages", s.ticketMessage)

	// Payment provider webhooks (inbound).
	s.handle("POST /v1/webhooks/payments/paystack", s.paystackWebhook)
	s.handle("POST /v1/webhooks/payments/sandbox", s.sandboxWebhook)

	// Meter devices.
	s.handle("POST /v1/devices/telemetry", s.telemetry)

	// Partner server-to-server API (API key with scopes).
	s.handle("POST /v1/partner/sessions", s.partnerSession)
	s.handle("GET /v1/partner/customers/{external_id}", s.partnerCustomer)
	s.handle("PUT /v1/partner/customers/{external_id}/membership", s.partnerMembership)
	s.handle("GET /v1/partner/transactions", s.partnerTransactions)
	s.handle("GET /v1/partner/meters", s.partnerMeters)

	// Tenant administration.
	s.handle("GET /v1/admin/tenant", s.adminTenant)
	s.handle("PATCH /v1/admin/branding", s.adminBranding)
	s.handle("PATCH /v1/admin/settings", s.adminSettings)
	s.handle("GET /v1/admin/customers", s.adminCustomers)
	s.handle("GET /v1/admin/team", s.adminTeam)
	s.handle("POST /v1/admin/team", s.adminAddTeam)
	s.handle("PATCH /v1/admin/users/{id}", s.adminUserStatus)
	s.handle("GET /v1/admin/meters", s.adminMeters)
	s.handle("POST /v1/admin/meters/{id}/assign", s.adminAssignMeter)
	s.handle("PATCH /v1/admin/meters/{id}", s.adminMeterStatus)
	s.handle("GET /v1/admin/meters/{id}/readings", s.adminReadings)
	s.handle("GET /v1/admin/meters/{id}/commands", s.adminMeterCommands)
	s.handle("POST /v1/admin/meters/{id}/commands", s.adminSendCommand)
	s.handle("GET /v1/admin/transfers", s.adminTransfers)
	s.handle("POST /v1/admin/transfers/{id}/decision", s.adminDecideTransfer)
	s.handle("GET /v1/admin/properties", s.adminProperties)
	s.handle("POST /v1/admin/properties", s.adminCreateProperty)
	s.handle("GET /v1/admin/transactions", s.adminTransactions)
	s.handle("GET /v1/admin/tiers", s.adminTiers)
	s.handle("PUT /v1/admin/tiers", s.adminUpsertTier)
	s.handle("PUT /v1/admin/customers/{id}/membership", s.adminMembership)
	s.handle("GET /v1/admin/reports/summary", s.adminReport)
	s.handle("GET /v1/admin/support/tickets", s.tickets)
	s.handle("POST /v1/admin/support/tickets/{id}/messages", s.ticketMessage)
	s.handle("POST /v1/admin/notifications/broadcast", s.adminBroadcast)
	s.handle("GET /v1/admin/api-keys", s.adminAPIKeys)
	s.handle("POST /v1/admin/api-keys", s.adminCreateAPIKey)
	s.handle("DELETE /v1/admin/api-keys/{id}", s.adminRevokeAPIKey)
	s.handle("GET /v1/admin/webhooks", s.adminWebhooks)
	s.handle("POST /v1/admin/webhooks", s.adminCreateWebhook)
	s.handle("DELETE /v1/admin/webhooks/{id}", s.adminDeleteWebhook)
	s.handle("GET /v1/admin/audit-logs", s.adminAudit)

	// Zieon platform administration.
	s.handle("GET /v1/platform/tenants", s.platformTenants)
	s.handle("POST /v1/platform/tenants", s.platformProvision)
	s.handle("PATCH /v1/platform/tenants/{id}", s.platformTenantUpdate)
	s.handle("POST /v1/platform/tenants/{id}/reward-pool/deposits", s.platformPoolDeposit)
	s.handle("GET /v1/platform/tenants/{id}/report", s.platformTenantReport)
	s.handle("GET /v1/platform/meters", s.platformMeters)
	s.handle("POST /v1/platform/meters", s.platformRegisterMeter)
	s.handle("POST /v1/platform/meters/{id}/assign", s.platformAssignMeter)
	s.handle("GET /v1/platform/tenants/{id}/transactions", s.platformTransactions)
	s.handle("POST /v1/platform/tenants/{id}/transactions/{txn}/resolve", s.platformResolve)
	s.handle("GET /v1/platform/audit-logs", s.platformAudit)
	s.handle("POST /v1/platform/reconcile", s.platformReconcile)
}

// ---------------------------------------------------------------------------
// Public and auth
// ---------------------------------------------------------------------------

func (s *Server) getBranding(w http.ResponseWriter, r *http.Request) error {
	if !s.publicLimiter.Allow(app.Meta(r.Context()).IP) {
		return tooMany()
	}
	t, err := s.publicTenant(r)
	if err != nil {
		return err
	}
	b, err := s.svc.Branding(r.Context(), t)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	return writeJSON(w, 200, b)
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) error {
	if !s.authLimiter.Allow("register:" + app.Meta(r.Context()).IP) {
		return tooMany()
	}
	var in app.RegisterInput
	if err := decode(r, &in); err != nil {
		return err
	}
	t, err := s.publicTenant(r)
	if err != nil {
		return err
	}
	sess, err := s.svc.Register(r.Context(), t, in)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, sess)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	if !s.authLimiter.Allow("login:" + app.Meta(r.Context()).IP) {
		return tooMany()
	}
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	t, err := s.publicTenant(r)
	if err != nil {
		return err
	}
	sess, err := s.svc.Login(r.Context(), t, in.Email, in.Password)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, sess)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) error {
	if !s.authLimiter.Allow("refresh:" + app.Meta(r.Context()).IP) {
		return tooMany()
	}
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	sess, err := s.svc.Refresh(r.Context(), in.RefreshToken)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, sess)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.svc.Logout(r.Context(), in.RefreshToken); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) embedExchange(w http.ResponseWriter, r *http.Request) error {
	if !s.authLimiter.Allow("embed:" + app.Meta(r.Context()).IP) {
		return tooMany()
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	sess, err := s.svc.ExchangeEmbedCode(r.Context(), in.Code)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, sess)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) error {
	c, err := s.user(r)
	if err != nil {
		return err
	}
	b, err := s.svc.Branding(r.Context(), c.Tenant)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"user": c.User, "tenant": c.Tenant.Slug, "branding": b, "auth_method": c.P.Method})
}

func (s *Server) updateMe(w http.ResponseWriter, r *http.Request) error {
	c, err := s.user(r)
	if err != nil {
		return err
	}
	var in app.ProfileInput
	if err := decode(r, &in); err != nil {
		return err
	}
	u, err := s.svc.UpdateProfile(r.Context(), c.P, in)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, u)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) error {
	if !s.authLimiter.Allow("password:" + app.Meta(r.Context()).IP) {
		return tooMany()
	}
	c, err := s.user(r)
	if err != nil {
		return err
	}
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.svc.ChangePassword(r.Context(), c.P, in.Current, in.New); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---------------------------------------------------------------------------
// Customer
// ---------------------------------------------------------------------------

func (s *Server) tariffs(w http.ResponseWriter, r *http.Request) error {
	c, err := s.user(r)
	if err != nil {
		return err
	}
	ts, err := s.svc.Tariffs(r.Context(), c.P.TenantID)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"tariffs": ts})
}

func (s *Server) plans(w http.ResponseWriter, r *http.Request) error {
	c, err := s.user(r)
	if err != nil {
		return err
	}
	if !c.Tenant.Enabled("subscriptions") {
		return app.ErrFeatureOff
	}
	ps, err := s.svc.Plans(r.Context(), c.P.TenantID)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"plans": ps, "checkout_available": false})
}

func (s *Server) quote(w http.ResponseWriter, r *http.Request) error {
	c, err := s.user(r)
	if err != nil {
		return err
	}
	var in struct {
		ProviderCode string     `json:"provider_code"`
		MeterID      *uuid.UUID `json:"meter_id"`
		AmountKobo   int64      `json:"amount_kobo"`
		Kwh          float64    `json:"kwh"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.MeterID != nil {
		m, err := s.svc.MyMeter(r.Context(), c.P, *in.MeterID)
		if err != nil {
			return err
		}
		in.ProviderCode = m.ProviderCode
	}
	q, err := s.svc.Quote(r.Context(), c.P, in.ProviderCode, in.AmountKobo, in.Kwh)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, q)
}

func (s *Server) wallet(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	wl, err := s.svc.Wallet(r.Context(), c.P)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, wl)
}

func (s *Server) walletActivity(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	a, err := s.svc.WalletActivity(r.Context(), c.P, queryInt(r, "limit", 30, 100))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, a)
}

func (s *Server) fundWallet(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	var in struct {
		AmountKobo  int64  `json:"amount_kobo"`
		Method      string `json:"method"`
		CallbackURL string `json:"callback_url"`
	}
	if err := decodeBytes(body, &in); err != nil {
		return err
	}
	// Paystack may only send the buyer back to an approved site.
	if u, err := url.Parse(in.CallbackURL); in.CallbackURL != "" && (err != nil || !s.originAllowed(r.Context(), u.Scheme+"://"+u.Host)) {
		in.CallbackURL = ""
	}
	if o := strings.TrimRight(r.Header.Get("Origin"), "/"); in.CallbackURL == "" && o != "" && s.originAllowed(r.Context(), o) {
		in.CallbackURL = o + "/wallet?funded=1"
	}
	return s.idempotent(w, r, c, body, func() (int, any, error) {
		res, err := s.svc.FundWallet(r.Context(), c.P, c.Tenant, in.AmountKobo, in.Method, in.CallbackURL)
		return 201, res, err
	})
}

func (s *Server) listMeters(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	ms, err := s.svc.MyMeters(r.Context(), c.P)
	if err != nil {
		return err
	}
	if ms == nil {
		ms = []app.Meter{}
	}
	return writeJSON(w, 200, map[string]any{"meters": ms})
}

func (s *Server) linkMeter(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	var in app.LinkMeterInput
	if err := decode(r, &in); err != nil {
		return err
	}
	m, err := s.svc.LinkMeter(r.Context(), c.P, c.Tenant, in)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, m)
}

func (s *Server) validateMeter(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	var in struct {
		ProviderCode string `json:"provider_code"`
		MeterNumber  string `json:"meter_number"`
		MeterType    string `json:"meter_type"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	v, err := s.svc.ValidateMeter(r.Context(), c.P, in.ProviderCode, in.MeterNumber, in.MeterType)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, v)
}

func (s *Server) getMeter(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	m, err := s.svc.MyMeter(r.Context(), c.P, id)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, m)
}

func (s *Server) updateMeter(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	var in app.MeterSettingsInput
	if err := decode(r, &in); err != nil {
		return err
	}
	m, err := s.svc.UpdateMeterSettings(r.Context(), c.P, c.Tenant, id, in)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, m)
}

func (s *Server) unlinkMeter(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	if err := s.svc.UnlinkMeter(r.Context(), c.P, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) consumption(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	out, err := s.svc.Consumption(r.Context(), c.P, id, queryInt(r, "days", 7, 90))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, out)
}

func (s *Server) readings(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	out, err := s.svc.Readings(r.Context(), c.P, id, queryInt(r, "limit", 50, 500))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"readings": out})
}

func (s *Server) requestTransfer(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		ToEmail string `json:"to_email"`
		Reason  string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	t, err := s.svc.RequestTransfer(r.Context(), c.P, id, in.ToEmail, in.Reason)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, t)
}

func (s *Server) purchase(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	var in app.PurchaseInput
	if err := decodeBytes(body, &in); err != nil {
		return err
	}
	return s.idempotent(w, r, c, body, func() (int, any, error) {
		p, err := s.svc.Purchase(r.Context(), c.P, c.Tenant, in)
		return 201, p, err
	})
}

// Orders: the buy page for signed-in customers (DisCo meters, card checkout).
func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	if !s.orderLimiter.Allow(c.P.UserID.String()) {
		return tooMany()
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	var in app.OrderInput
	if err := decodeBytes(body, &in); err != nil {
		return err
	}
	// Paystack returns the buyer to the site they started on, if it is approved.
	if o := strings.TrimRight(r.Header.Get("Origin"), "/"); o != "" && s.originAllowed(r.Context(), o) {
		in.ReturnURL = o
	}
	return s.idempotent(w, r, c, body, func() (int, any, error) {
		o, err := s.svc.CreateOrder(r.Context(), c.P, c.Tenant, in)
		return 201, o, err
	})
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	os, err := s.svc.Orders(r.Context(), c.P, c.Tenant, queryInt(r, "limit", 20, 100))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"orders": os})
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	o, err := s.svc.Order(r.Context(), c.P, c.Tenant, id)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, o)
}

func (s *Server) listPurchases(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	meter, err := queryUUID(r, "meter_id")
	if err != nil {
		return err
	}
	ps, err := s.svc.ListPurchases(r.Context(), c.P.TenantID, app.PurchaseFilter{UserID: &c.P.UserID, MeterID: meter,
		Status: r.URL.Query().Get("status"), Limit: queryInt(r, "limit", 50, 200)}, true)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"purchases": ps})
}

func (s *Server) getPurchase(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	p, err := s.svc.GetPurchase(r.Context(), c.P, id)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, p)
}

func (s *Server) pendingTokens(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	meter, err := queryUUID(r, "meter_id")
	if err != nil {
		return err
	}
	ts, err := s.svc.PendingTokens(r.Context(), c.P, meter)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"tokens": ts})
}

func (s *Server) redeemToken(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	var in struct {
		MeterID uuid.UUID `json:"meter_id"`
		Token   string    `json:"token"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	res, err := s.svc.RedeemToken(r.Context(), c.P, in.MeterID, in.Token)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, res)
}

func (s *Server) share(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	var in app.ShareInput
	if err := decodeBytes(body, &in); err != nil {
		return err
	}
	return s.idempotent(w, r, c, body, func() (int, any, error) {
		res, err := s.svc.Share(r.Context(), c.P, c.Tenant, in)
		return 201, res, err
	})
}

func (s *Server) recipients(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	rs, err := s.svc.Recipients(r.Context(), c.P)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"recipients": rs})
}

func (s *Server) addRecipient(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	var in struct {
		Name        string `json:"name"`
		MeterNumber string `json:"meter_number"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	rec, err := s.svc.AddRecipient(r.Context(), c.P, in.Name, in.MeterNumber)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, rec)
}

func (s *Server) removeRecipient(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	if err := s.svc.RemoveRecipient(r.Context(), c.P, r.PathValue("meter_number")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	a, err := s.svc.Activity(r.Context(), c.P, queryInt(r, "limit", 30, 100))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"activity": a})
}

func (s *Server) notifications(w http.ResponseWriter, r *http.Request) error {
	c, err := s.user(r)
	if err != nil {
		return err
	}
	ns, err := s.svc.Notifications(r.Context(), c.P, queryInt(r, "limit", 30, 100))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"notifications": ns})
}

func (s *Server) readNotification(w http.ResponseWriter, r *http.Request) error {
	c, err := s.user(r)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	if err := s.svc.MarkNotificationRead(r.Context(), c.P, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) myProperties(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	ps, err := s.svc.Properties(r.Context(), c.P, &c.P.UserID)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"properties": ps})
}

func (s *Server) createMyProperty(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	if !c.Tenant.Enabled("properties") {
		return app.ErrFeatureOff
	}
	var in struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	p, err := s.svc.CreateProperty(r.Context(), c.P.TenantID, &c.P.UserID, in.Name, in.Address)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, p)
}

func (s *Server) benefits(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	b, err := s.svc.Benefits(r.Context(), c.P, c.Tenant)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, b)
}

func (s *Server) claimMonthly(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	amt, err := s.svc.ClaimMonthlyCredit(r.Context(), c.P, c.Tenant)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, map[string]any{"granted_kobo": amt})
}

func (s *Server) tickets(w http.ResponseWriter, r *http.Request) error {
	c, err := s.user(r)
	if err != nil {
		return err
	}
	ts, err := s.svc.Tickets(r.Context(), c.P, r.URL.Query().Get("status"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"tickets": ts})
}

func (s *Server) createTicket(w http.ResponseWriter, r *http.Request) error {
	c, err := s.customer(r)
	if err != nil {
		return err
	}
	var in struct {
		Subject string `json:"subject"`
		Message string `json:"message"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	t, err := s.svc.CreateTicket(r.Context(), c.P, in.Subject, in.Message)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, t)
}

func (s *Server) getTicket(w http.ResponseWriter, r *http.Request) error {
	c, err := s.user(r)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	t, err := s.svc.Ticket(r.Context(), c.P, id)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, t)
}

func (s *Server) ticketMessage(w http.ResponseWriter, r *http.Request) error {
	c, err := s.user(r, auth.RoleCustomer, auth.RoleTenantAdmin, auth.RoleTenantStaff)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	t, err := s.svc.AddTicketMessage(r.Context(), c.P, id, in.Message, in.Status)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, t)
}

// ---------------------------------------------------------------------------
// Inbound payment webhooks
// ---------------------------------------------------------------------------

func (s *Server) paystackWebhook(w http.ResponseWriter, r *http.Request) error {
	secret := s.svc.Cfg.PaystackSecretKey
	if secret == "" {
		return app.ErrNotFound
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	if !secure.EqualHex(r.Header.Get("X-Paystack-Signature"), secure.HMACSHA512Hex([]byte(secret), body)) {
		return &app.Error{Status: 401, Code: "invalid_signature", Message: "signature verification failed"}
	}
	var ev struct {
		Event string `json:"event"`
		Data  struct {
			ID        int64  `json:"id"`
			Reference string `json:"reference"`
			Amount    int64  `json:"amount"`
			Status    string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return badRequest("invalid_json", "invalid webhook body")
	}
	fresh, err := s.svc.RecordWebhookEvent(r.Context(), "paystack", ev.Event+":"+ev.Data.Reference, body)
	if err != nil {
		return err
	}
	if !fresh {
		return writeJSON(w, 200, map[string]string{"status": "duplicate"})
	}
	switch ev.Event {
	case "charge.success":
		// Re-verify with the provider before crediting; the webhook alone is not trusted for amount.
		v, err := s.svc.Payments["paystack"].Verify(r.Context(), ev.Data.Reference)
		if err != nil {
			return err
		}
		if v.Status == "succeeded" {
			err = s.svc.ConfirmPayment(r.Context(), "paystack", ev.Data.Reference, v.AmountKobo)
		}
		if err != nil && err != app.ErrNotFound {
			return err
		}
	case "charge.failed":
		_ = s.svc.FailPayment(r.Context(), ev.Data.Reference, "payment failed")
	}
	return writeJSON(w, 200, map[string]string{"status": "ok"})
}

// sandboxWebhook exercises the same verification and de-duplication path as a
// real provider (Zieon-Signature HMAC). Disabled in production.
func (s *Server) sandboxWebhook(w http.ResponseWriter, r *http.Request) error {
	secret := s.svc.Cfg.SandboxWebhookSecret
	if !s.svc.Cfg.SandboxAllowed() || secret == "" {
		return app.ErrNotFound
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	if !secure.VerifyWebhook([]byte(secret), r.Header.Get("Zieon-Signature"), body, time.Now(), 5*time.Minute) {
		return &app.Error{Status: 401, Code: "invalid_signature", Message: "signature verification failed"}
	}
	var ev struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Reference  string `json:"reference"`
		AmountKobo int64  `json:"amount_kobo"`
	}
	if err := decodeBytes(body, &ev); err != nil {
		return err
	}
	fresh, err := s.svc.RecordWebhookEvent(r.Context(), "sandbox", ev.ID, body)
	if err != nil {
		return err
	}
	if !fresh {
		return writeJSON(w, 200, map[string]string{"status": "duplicate"})
	}
	switch ev.Type {
	case "payment.succeeded":
		err = s.svc.ConfirmPayment(r.Context(), "sandbox", ev.Reference, ev.AmountKobo)
	case "payment.failed":
		err = s.svc.FailPayment(r.Context(), ev.Reference, "payment failed")
	default:
		return badRequest("invalid_event", "unknown event type")
	}
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Devices
// ---------------------------------------------------------------------------

func (s *Server) telemetry(w http.ResponseWriter, r *http.Request) error {
	body, err := readBody(r)
	if err != nil {
		return err
	}
	serial := r.Header.Get("X-Device-Serial")
	if !s.deviceLimiter.Allow("device:" + serial) {
		return tooMany()
	}
	p, err := s.svc.AuthenticateDevice(r.Context(), serial, r.Header.Get("X-Device-Timestamp"), r.Header.Get("X-Device-Nonce"),
		r.Header.Get("X-Device-Signature"), body)
	if err != nil {
		return err
	}
	var in app.Telemetry
	if err := decodeBytes(body, &in); err != nil {
		return err
	}
	res, err := s.svc.IngestTelemetry(r.Context(), p, in)
	if err != nil {
		return err
	}
	return writeJSON(w, 202, res)
}

// ---------------------------------------------------------------------------
// Partner API
// ---------------------------------------------------------------------------

func (s *Server) partnerSession(w http.ResponseWriter, r *http.Request) error {
	c, err := s.apiKey(r, auth.ScopeSessionsWrite)
	if err != nil {
		return err
	}
	var in app.PartnerSessionInput
	if err := decode(r, &in); err != nil {
		return err
	}
	out, err := s.svc.CreatePartnerSession(r.Context(), c.P, in)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, out)
}

func (s *Server) partnerCustomer(w http.ResponseWriter, r *http.Request) error {
	c, err := s.apiKey(r, auth.ScopeCustomersRead)
	if err != nil {
		return err
	}
	u, err := s.svc.PartnerCustomer(r.Context(), c.P.TenantID, r.PathValue("external_id"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, u)
}

func (s *Server) partnerMembership(w http.ResponseWriter, r *http.Request) error {
	c, err := s.apiKey(r, auth.ScopeMembershipsWrite)
	if err != nil {
		return err
	}
	var in struct {
		TierCode string `json:"tier_code"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	u, err := s.svc.PartnerCustomer(r.Context(), c.P.TenantID, r.PathValue("external_id"))
	if err != nil {
		return err
	}
	if err := s.svc.SetMembership(r.Context(), c.P.TenantID, u.ID, in.TierCode); err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]string{"tier_code": in.TierCode})
}

func (s *Server) partnerTransactions(w http.ResponseWriter, r *http.Request) error {
	c, err := s.apiKey(r, auth.ScopeTransactionsRead)
	if err != nil {
		return err
	}
	var userID *uuid.UUID
	if ext := r.URL.Query().Get("external_user_id"); ext != "" {
		u, err := s.svc.PartnerCustomer(r.Context(), c.P.TenantID, ext)
		if err != nil {
			return err
		}
		userID = &u.ID
	}
	ps, err := s.svc.ListPurchases(r.Context(), c.P.TenantID, app.PurchaseFilter{UserID: userID, Status: r.URL.Query().Get("status"),
		Limit: queryInt(r, "limit", 50, 200)}, false)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"transactions": ps})
}

func (s *Server) partnerMeters(w http.ResponseWriter, r *http.Request) error {
	c, err := s.apiKey(r, auth.ScopeMetersRead)
	if err != nil {
		return err
	}
	ms, err := s.svc.TenantMeters(r.Context(), c.P.TenantID, r.URL.Query().Get("status"), queryInt(r, "limit", 100, 500))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"meters": ms})
}

// ---------------------------------------------------------------------------
// Guest checkout
// ---------------------------------------------------------------------------

func (s *Server) publicDiscos(w http.ResponseWriter, r *http.Request) error {
	if !s.publicLimiter.Allow(app.Meta(r.Context()).IP) {
		return tooMany()
	}
	t, err := s.publicTenant(r)
	if err != nil {
		return err
	}
	ds, err := s.svc.PublicDiscos(r.Context(), t)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	return writeJSON(w, 200, map[string]any{"discos": ds})
}

func (s *Server) publicVerifyMeter(w http.ResponseWriter, r *http.Request) error {
	if !s.lookupLimiter.Allow(app.Meta(r.Context()).IP) {
		return tooMany()
	}
	var in struct {
		ProviderCode string `json:"provider_code"`
		MeterNumber  string `json:"meter_number"`
		MeterType    string `json:"meter_type"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	t, err := s.publicTenant(r)
	if err != nil {
		return err
	}
	m, err := s.svc.VerifyPublicMeter(r.Context(), t, in.ProviderCode, in.MeterNumber, in.MeterType)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, m)
}

func (s *Server) publicCreateOrder(w http.ResponseWriter, r *http.Request) error {
	if !s.orderLimiter.Allow(app.Meta(r.Context()).IP) {
		return tooMany()
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	var in app.GuestOrderInput
	if err := decodeBytes(body, &in); err != nil {
		return err
	}
	t, err := s.publicTenant(r)
	if err != nil {
		return err
	}
	// Paystack returns the buyer to the site they started on, if it is approved.
	if o := strings.TrimRight(r.Header.Get("Origin"), "/"); o != "" && s.originAllowed(r.Context(), o) {
		in.ReturnURL = o
	}
	status, resp, err := s.svc.Idempotent(r.Context(), t.ID, "guest", r.Header.Get("Idempotency-Key"), body, func() (int, any, error) {
		o, err := s.svc.CreateGuestOrder(r.Context(), t, in)
		return 201, o, err
	})
	if err != nil {
		return err
	}
	return writeRaw(w, status, resp)
}

func (s *Server) publicGetOrder(w http.ResponseWriter, r *http.Request) error {
	if !s.publicLimiter.Allow(app.Meta(r.Context()).IP) {
		return tooMany()
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	t, err := s.publicTenant(r)
	if err != nil {
		return err
	}
	o, err := s.svc.GuestOrder(r.Context(), t, id, r.URL.Query().Get("key"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, o)
}
