package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/secure"
)

// SeedResult lists credentials created by SeedPilot. Passwords are random
// and printed once; they are never stored in plaintext.
type SeedResult struct {
	Lines []string
}

func (r *SeedResult) add(f string, a ...any) { r.Lines = append(r.Lines, fmt.Sprintf(f, a...)) }

// SeedPilot provisions the non-production pilot: the Zieon platform tenant
// (which also serves the first-party Zenerlink app), PCE Energy and Red Real
// Estate, all in sandbox vending mode with sandbox meters. It is idempotent:
// existing tenants are left untouched.
func (s *Service) SeedPilot(ctx context.Context) (SeedResult, error) {
	var res SeedResult
	if !s.Cfg.SandboxAllowed() {
		return res, fmt.Errorf("refusing to seed pilot data in production")
	}
	type spec struct {
		in        ProvisionInput
		customers []string
		meters    []string // meter numbers for sandbox Zieon meters
	}
	str := func(v string) *string { return &v }
	specs := []spec{
		{in: ProvisionInput{Slug: "zenerlink", Name: "Zieon Energies", IsPlatform: true,
			Branding: BrandingInput{DisplayName: str("Zenerlink"), SupportEmail: str("support@zenerlink.com"), Subdomain: str("app")},
			Features: map[string]bool{"benefits": false}, AllowedOrigins: []string{"http://localhost:5173", "https://zenerlink.vercel.app"}},
			customers: []string{"demo@zenerlink.app"}, meters: []string{"11111111111", "33333333333", "44444444444"}},
		{in: ProvisionInput{Slug: "pce-energy", Name: "PCE Energy",
			Branding: BrandingInput{DisplayName: str("PCE Power"), Theme: map[string]string{"primary": "#1F7AE0", "secondary": "#0B2545", "panel": "#0E1B2E", "success": "#2BD47D"},
				SupportEmail: str("support@pce.example"), SupportPhone: str("+234 800 000 0001"), Subdomain: str("pce")},
			Features: map[string]bool{"spark_share": false, "benefits": false}, AllowedOrigins: []string{"https://app.pce.example"}},
			customers: []string{"ada@pce.example", "bayo@pce.example"}, meters: []string{"21000000001", "21000000002"}},
		{in: ProvisionInput{Slug: "red-real-estate", Name: "Red Real Estate Development Company",
			Branding: BrandingInput{DisplayName: str("Red Energy"), Theme: map[string]string{"primary": "#D62828", "secondary": "#2B2B2B", "panel": "#1E0E0E"},
				SupportEmail: str("investors@red.example"), Subdomain: str("red")},
			Features: map[string]bool{"benefits": true, "properties": true, "external_meters": false}, AllowedOrigins: []string{"https://investors.red.example"}},
			customers: []string{"investor.gold@red.example", "investor.platinum@red.example"}, meters: []string{"31000000001", "31000000002", "31000000003"}},
	}
	for i, sp := range specs {
		existing, err := s.TenantBySlug(ctx, sp.in.Slug)
		if err == nil {
			res.add("tenant %-16s exists (%s), skipped", existing.Slug, existing.ID)
			continue
		}
		pw := secure.RandomToken(12)
		sp.in.Admin.Password = pw
		if sp.in.IsPlatform {
			sp.in.Admin.Email, sp.in.Admin.Name = "platform-admin@zieonenergies.com", "Zieon Platform Admin"
		} else {
			sp.in.Admin.Email, sp.in.Admin.Name = "admin@"+strings.Split(sp.in.Slug, "-")[0]+".example", sp.in.Name+" Admin"
		}
		t, err := s.ProvisionTenant(ctx, sp.in)
		if err != nil {
			return res, fmt.Errorf("provision %s: %w", sp.in.Slug, err)
		}
		res.add("tenant %-16s %s  admin %s / %s", t.Slug, t.ID, sp.in.Admin.Email, pw)

		var users []uuid.UUID
		for _, email := range sp.customers {
			cpw := secure.RandomToken(9)
			sess, err := s.Register(ctx, t, RegisterInput{Email: email, Name: strings.Split(email, "@")[0], Phone: "+2348000000000", Password: cpw})
			if err != nil {
				return res, err
			}
			users = append(users, sess.User.ID)
			res.add("  customer %s / %s", email, cpw)
		}
		for j, num := range sp.meters {
			serial := fmt.Sprintf("SBX-%d%02d-%s", i, j, num[len(num)-4:])
			m, err := s.RegisterMeter(ctx, RegisterMeterInput{SerialNumber: serial, MeterNumber: num, Model: "ZE-SANDBOX", CommsMode: "gsm",
				Capabilities: Capabilities{Telemetry: true, RemoteCredit: true, RemoteDebit: true, Tamper: true}, FirmwareVersion: "sandbox-1.0"})
			if err != nil {
				return res, err
			}
			if err := s.AssignMeterToTenant(ctx, m.ID, t.ID); err != nil {
				return res, err
			}
			owner := users[min(j, len(users)-1)]
			labels := []string{"Home", "Office", "School"}
			if err := s.AssignMeterToCustomer(ctx, t.ID, m.ID, AssignMeterInput{UserID: owner, Label: labels[j%3],
				CustomerName: "Sandbox Customer", Address: "Sandbox address " + num}); err != nil {
				return res, err
			}
			res.add("  meter %s serial %s device_secret %s", num, serial, m.DeviceSecret)
		}
		if sp.in.Slug == "red-real-estate" {
			if err := s.seedRed(ctx, t, users); err != nil {
				return res, err
			}
			res.add("  tiers silver(2%%) gold(5%%) platinum(10%%); reward pool ₦500,000 (sandbox)")
		}
	}
	return res, nil
}

func (s *Service) seedRed(ctx context.Context, t Tenant, users []uuid.UUID) error {
	tiers := []Tier{
		{Code: "silver", Name: "Silver", Rank: 1, CashbackBps: 200, MaxCashbackKobo: 100000, MonthlyCashbackCapKobo: 300000, RewardValidityDays: 90, Active: true},
		{Code: "gold", Name: "Gold", Rank: 2, CashbackBps: 500, MaxCashbackKobo: 250000, MonthlyCashbackCapKobo: 750000, MonthlyCreditKobo: 200000, RewardValidityDays: 90, Active: true},
		{Code: "platinum", Name: "Platinum", Rank: 3, CashbackBps: 1000, MaxCashbackKobo: 500000, MonthlyCashbackCapKobo: 1500000, MonthlyCreditKobo: 500000,
			FixedRewardKobo: 50000, FixedRewardMinPurchaseKobo: 2000000, RewardValidityDays: 90, Active: true},
	}
	for _, tr := range tiers {
		if _, err := s.UpsertTier(ctx, t.ID, tr); err != nil {
			return err
		}
	}
	if err := s.SetMembership(ctx, t.ID, users[0], "gold"); err != nil {
		return err
	}
	if err := s.SetMembership(ctx, t.ID, users[1], "platinum"); err != nil {
		return err
	}
	if _, err := s.DepositRewardPool(ctx, t.ID, 50000000, "sandbox-seed-pool"); err != nil {
		return err
	}
	// Two properties for the platinum investor, each holding a meter.
	return s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		for i, name := range []string{"Lekki Phase 1 Duplex", "Wuse II Apartments"} {
			var pid uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO properties (tenant_id, owner_user_id, name, address) VALUES ($1,$2,$3,$4) RETURNING id`,
				t.ID, users[1], name, "Sandbox address").Scan(&pid); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE meters SET property_id=$1 WHERE id = (
				SELECT meter_id FROM meter_links WHERE user_id=$2 AND is_owner ORDER BY created_at OFFSET $3 LIMIT 1)`, pid, users[1], i); err != nil {
				return err
			}
		}
		return nil
	})
}
