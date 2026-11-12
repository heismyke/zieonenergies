package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/heismyke/zieonenergies/internal/app"
	"github.com/heismyke/zieonenergies/internal/auth"
)

// ---------------------------------------------------------------------------
// Tenant administration (tenant_admin; read-mostly views also allow tenant_staff)
// ---------------------------------------------------------------------------

func (s *Server) adminTenant(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, false)
	if err != nil {
		return err
	}
	b, err := s.svc.Branding(r.Context(), c.Tenant)
	if err != nil {
		return err
	}
	pool, err := s.svc.RewardPoolBalance(r.Context(), c.Tenant.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"tenant": c.Tenant, "branding": b, "reward_pool_kobo": pool})
}

func (s *Server) adminBranding(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	var in app.BrandingInput
	if err := decode(r, &in); err != nil {
		return err
	}
	b, err := s.svc.UpdateBranding(r.Context(), c.Tenant, in)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, b)
}

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	var in app.TenantSettingsInput
	if err := decode(r, &in); err != nil {
		return err
	}
	t, err := s.svc.UpdateTenantSettings(r.Context(), c.Tenant, in)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, t)
}

func (s *Server) adminCustomers(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, false)
	if err != nil {
		return err
	}
	us, err := s.svc.ListUsers(r.Context(), c.Tenant.ID, []string{auth.RoleCustomer}, r.URL.Query().Get("q"), queryInt(r, "limit", 50, 500))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"customers": nonNil(us)})
}

func (s *Server) adminTeam(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	us, err := s.svc.ListUsers(r.Context(), c.Tenant.ID, []string{auth.RoleTenantAdmin, auth.RoleTenantStaff}, "", 200)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"team": nonNil(us)})
}

func (s *Server) adminAddTeam(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	var in app.TeamMemberInput
	if err := decode(r, &in); err != nil {
		return err
	}
	u, err := s.svc.AddTeamMember(r.Context(), c.Tenant.ID, in)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, u)
}

func (s *Server) adminUserStatus(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Status string `json:"status"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.svc.SetUserStatus(r.Context(), c.Tenant.ID, id, in.Status); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) adminMeters(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, false)
	if err != nil {
		return err
	}
	ms, err := s.svc.TenantMeters(r.Context(), c.Tenant.ID, r.URL.Query().Get("status"), queryInt(r, "limit", 100, 500))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"meters": ms})
}

func (s *Server) adminAssignMeter(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	var in app.AssignMeterInput
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.svc.AssignMeterToCustomer(r.Context(), c.Tenant.ID, id, in); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) adminMeterStatus(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Status string `json:"status"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.svc.SetMeterStatus(r.Context(), c.Tenant.ID, id, in.Status); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) adminReadings(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, false)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	out, err := s.svc.Readings(r.Context(), c.P, id, queryInt(r, "limit", 100, 1000))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"readings": out})
}

func (s *Server) adminMeterCommands(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, false)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	cs, err := s.svc.MeterCommands(r.Context(), c.Tenant.ID, id, queryInt(r, "limit", 50, 200))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"commands": cs})
}

func (s *Server) adminSendCommand(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	var in app.CommandInput
	if err := decode(r, &in); err != nil {
		return err
	}
	cmd, err := s.svc.SendMeterCommand(r.Context(), c.Tenant.ID, id, in)
	if err != nil {
		return err
	}
	return writeJSON(w, 202, cmd)
}

func (s *Server) adminTransfers(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, false)
	if err != nil {
		return err
	}
	ts, err := s.svc.ListTransfers(r.Context(), c.Tenant.ID, r.URL.Query().Get("status"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"transfers": ts})
}

func (s *Server) adminDecideTransfer(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Approve bool `json:"approve"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	t, err := s.svc.DecideTransfer(r.Context(), c.P, id, in.Approve)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, t)
}

func (s *Server) adminProperties(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, false)
	if err != nil {
		return err
	}
	owner, err := queryUUID(r, "owner_user_id")
	if err != nil {
		return err
	}
	ps, err := s.svc.Properties(r.Context(), c.P, owner)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"properties": ps})
}

func (s *Server) adminCreateProperty(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	var in struct {
		Name        string     `json:"name"`
		Address     string     `json:"address"`
		OwnerUserID *uuid.UUID `json:"owner_user_id"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	p, err := s.svc.CreateProperty(r.Context(), c.Tenant.ID, in.OwnerUserID, in.Name, in.Address)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, p)
}

func (s *Server) adminTransactions(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, false)
	if err != nil {
		return err
	}
	user, err := queryUUID(r, "user_id")
	if err != nil {
		return err
	}
	meter, err := queryUUID(r, "meter_id")
	if err != nil {
		return err
	}
	ps, err := s.svc.ListPurchases(r.Context(), c.Tenant.ID, app.PurchaseFilter{UserID: user, MeterID: meter,
		Status: r.URL.Query().Get("status"), Limit: queryInt(r, "limit", 50, 200)}, false)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"transactions": ps})
}

func (s *Server) adminTiers(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, false)
	if err != nil {
		return err
	}
	ts, err := s.svc.Tiers(r.Context(), c.Tenant.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"tiers": ts})
}

func (s *Server) adminUpsertTier(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	var in app.Tier
	if err := decode(r, &in); err != nil {
		return err
	}
	t, err := s.svc.UpsertTier(r.Context(), c.Tenant.ID, in)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, t)
}

func (s *Server) adminMembership(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		TierCode string `json:"tier_code"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.svc.SetMembership(r.Context(), c.Tenant.ID, id, in.TierCode); err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]string{"tier_code": in.TierCode})
}

func reportRange(r *http.Request) (time.Time, time.Time) {
	to := time.Now()
	from := to.AddDate(0, 0, -30)
	if v, err := time.Parse(time.DateOnly, r.URL.Query().Get("from")); err == nil {
		from = v
	}
	if v, err := time.Parse(time.DateOnly, r.URL.Query().Get("to")); err == nil {
		to = v.Add(24*time.Hour - time.Second)
	}
	return from, to
}

func (s *Server) adminReport(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, false)
	if err != nil {
		return err
	}
	from, to := reportRange(r)
	rep, err := s.svc.Report(r.Context(), c.Tenant.ID, from, to)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, rep)
}

func (s *Server) adminBroadcast(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	var in struct {
		Title    string `json:"title"`
		Message  string `json:"message"`
		Critical bool   `json:"critical"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.svc.Broadcast(r.Context(), c.Tenant.ID, in.Title, in.Message, in.Critical); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) adminAPIKeys(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	ks, err := s.svc.ListAPIKeys(r.Context(), c.Tenant.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"api_keys": nonNil(ks)})
}

func (s *Server) adminCreateAPIKey(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	var in struct {
		Name      string     `json:"name"`
		Scopes    []string   `json:"scopes"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	k, err := s.svc.CreateAPIKey(r.Context(), c.Tenant.ID, in.Name, in.Scopes, in.ExpiresAt)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, k)
}

func (s *Server) adminRevokeAPIKey(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	if err := s.svc.RevokeAPIKey(r.Context(), c.Tenant.ID, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) adminWebhooks(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	es, err := s.svc.WebhookEndpoints(r.Context(), c.Tenant.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"webhooks": es, "events": app.WebhookEvents})
}

func (s *Server) adminCreateWebhook(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	var in struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	e, err := s.svc.CreateWebhookEndpoint(r.Context(), c.Tenant.ID, in.URL, in.Events)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, e)
}

func (s *Server) adminDeleteWebhook(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	if err := s.svc.DeleteWebhookEndpoint(r.Context(), c.Tenant.ID, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) error {
	c, err := s.operator(r, true)
	if err != nil {
		return err
	}
	ls, err := s.svc.AuditLogs(r.Context(), &c.Tenant.ID, queryInt(r, "limit", 100, 500))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"audit_logs": ls})
}

// ---------------------------------------------------------------------------
// Platform administration. Every action inside a tenant is audited.
// ---------------------------------------------------------------------------

func (s *Server) platformTenants(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.platform(r); err != nil {
		return err
	}
	ts, err := s.svc.ListTenants(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"tenants": ts})
}

func (s *Server) platformProvision(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.platform(r); err != nil {
		return err
	}
	var in app.ProvisionInput
	if err := decode(r, &in); err != nil {
		return err
	}
	t, err := s.svc.ProvisionTenant(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, t)
}

func (s *Server) platformTenantUpdate(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.platform(r); err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Status      string           `json:"status"`
		VendingMode string           `json:"vending_mode"`
		Tariff      *app.TariffInput `json:"tariff"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Tariff != nil {
		if err := s.svc.UpsertTariff(r.Context(), id, *in.Tariff); err != nil {
			return err
		}
	}
	t, err := s.svc.SetTenantStatus(r.Context(), id, in.Status, in.VendingMode)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, t)
}

func (s *Server) platformPoolDeposit(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.platform(r); err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		AmountKobo int64  `json:"amount_kobo"`
		Reference  string `json:"reference"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	bal, err := s.svc.DepositRewardPool(r.Context(), id, in.AmountKobo, in.Reference)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, map[string]any{"reward_pool_kobo": bal})
}

func (s *Server) platformTenantReport(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.platform(r); err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	if err := s.svc.RecordPlatformAccess(r.Context(), id, "platform.tenant.report.view", nil); err != nil {
		return err
	}
	from, to := reportRange(r)
	rep, err := s.svc.Report(r.Context(), id, from, to)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, rep)
}

func (s *Server) platformMeters(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.platform(r); err != nil {
		return err
	}
	ms, err := s.svc.PlatformMeters(r.Context(), r.URL.Query().Get("status"), queryInt(r, "limit", 100, 1000))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"meters": ms})
}

func (s *Server) platformRegisterMeter(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.platform(r); err != nil {
		return err
	}
	var in app.RegisterMeterInput
	if err := decode(r, &in); err != nil {
		return err
	}
	m, err := s.svc.RegisterMeter(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, m)
}

func (s *Server) platformAssignMeter(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.platform(r); err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		TenantID uuid.UUID `json:"tenant_id"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.svc.AssignMeterToTenant(r.Context(), id, in.TenantID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) platformTransactions(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.platform(r); err != nil {
		return err
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	if err := s.svc.RecordPlatformAccess(r.Context(), id, "platform.tenant.transactions.view", map[string]any{"status": r.URL.Query().Get("status")}); err != nil {
		return err
	}
	ps, err := s.svc.ListPurchases(r.Context(), id, app.PurchaseFilter{Status: r.URL.Query().Get("status"), Limit: queryInt(r, "limit", 50, 200)}, false)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"transactions": ps})
}

func (s *Server) platformResolve(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.platform(r); err != nil {
		return err
	}
	tenant, err := pathUUID(r, "id")
	if err != nil {
		return err
	}
	txn, err := pathUUID(r, "txn")
	if err != nil {
		return err
	}
	var in struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.svc.ResolveReview(r.Context(), tenant, txn, in.Action, in.Note); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) platformAudit(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.platform(r); err != nil {
		return err
	}
	tenant, err := queryUUID(r, "tenant_id")
	if err != nil {
		return err
	}
	ls, err := s.svc.AuditLogs(r.Context(), tenant, queryInt(r, "limit", 100, 1000))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"audit_logs": ls})
}

func (s *Server) platformReconcile(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.platform(r); err != nil {
		return err
	}
	if err := s.svc.ReconcileVends(r.Context()); err != nil {
		return err
	}
	if err := s.svc.ReconcilePayments(r.Context()); err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]string{"status": "ok"})
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
