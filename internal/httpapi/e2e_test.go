package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/payments"
	"github.com/heismyke/zieonenergies/internal/secure"
)

// TestTwoTenantIsolation is the end-to-end pilot scenario: PCE Energy and Red
// Real Estate are provisioned side by side and must not see each other's
// users, meters, transactions, branding or benefits.
func TestTwoTenantIsolation(t *testing.T) {
	e := newEnv(t, false)
	pce := e.provision("pce-energy", map[string]bool{"benefits": false}, map[string]string{"primary": "#1F7AE0"})
	red := e.provision("red-real-estate", map[string]bool{"benefits": true, "properties": true}, map[string]string{"primary": "#D62828"})

	ada := e.customer(pce.Slug, "ada@pce.test")
	inv := e.customer(red.Slug, "investor@red.test")
	// Same email may exist independently in two tenants.
	twin := e.customer(red.Slug, "ada@pce.test")

	pm := e.zieonMeter(pce, ada, "21000000001", nil)
	rm := e.zieonMeter(red, inv, "31000000001", nil)

	// Red benefits: Gold 5% cashback, funded pool.
	e.must(e.do("PUT", "/v1/admin/tiers", red.Admin, map[string]any{"code": "gold", "name": "Gold", "cashback_bps": 500,
		"max_cashback_kobo": 250000, "active": true}), 200)
	e.must(e.do("PUT", "/v1/admin/customers/"+inv.ID+"/membership", red.Admin, map[string]string{"tier_code": "gold"}), 200)
	e.must(e.do("POST", "/v1/platform/tenants/"+red.ID+"/reward-pool/deposits", e.platform, map[string]any{"amount_kobo": 1000000, "reference": "dep-1"}), 201)

	e.fund(ada, 1000000)
	e.fund(inv, 1000000)
	pp := e.must(e.buy(ada, pm, 500000), 201)
	rp := e.must(e.buy(inv, rm, 500000), 201)
	if pp.str("status") != "vended" || rp.str("status") != "vended" {
		t.Fatalf("purchases should vend: %s / %s", pp.Raw, rp.Raw)
	}
	if rp.num("reward_earned_kobo") != 25000 || pp.num("reward_earned_kobo") != 0 {
		t.Fatalf("cashback must apply to Red only: red=%v pce=%v", rp.num("reward_earned_kobo"), pp.num("reward_earned_kobo"))
	}

	// Branding is tenant-specific and public.
	pb := e.must(e.do("GET", "/v1/branding", "", nil, tenantHdr(pce.Slug)), 200)
	rb := e.must(e.do("GET", "/v1/branding", "", nil, tenantHdr(red.Slug)), 200)
	if pb.str("theme", "primary") != "#1F7AE0" || rb.str("theme", "primary") != "#D62828" || pb.str("display_name") == rb.str("display_name") {
		t.Fatalf("branding leaked or wrong: %s | %s", pb.Raw, rb.Raw)
	}

	// Cross-tenant reads by id are 404, never 403 (no existence oracle).
	e.must(e.do("GET", "/v1/meters/"+rm.ID, ada.Token, nil), 404)
	e.must(e.do("GET", "/v1/purchases/"+rp.str("id"), ada.Token, nil), 404)
	e.must(e.do("POST", "/v1/purchases", ada.Token, map[string]any{"meter_id": rm.ID, "amount_kobo": 100000, "payment_method": "wallet"}, idem("x")), 404)
	e.must(e.do("POST", "/v1/tokens/redeem", ada.Token, map[string]any{"meter_id": rm.ID, "token": rp.str("token")}), 404)
	// A Red token cannot be redeemed on a PCE meter.
	e.must(e.do("POST", "/v1/tokens/redeem", ada.Token, map[string]any{"meter_id": pm.ID, "token": rp.str("token")}), 400)

	// Lists only contain own-tenant rows.
	if l := e.must(e.do("GET", "/v1/admin/customers", pce.Admin, nil), 200).list("customers"); len(l) != 1 {
		t.Fatalf("PCE admin should see 1 customer, got %d", len(l))
	}
	if l := e.must(e.do("GET", "/v1/admin/transactions", pce.Admin, nil), 200).list("transactions"); len(l) != 1 {
		t.Fatalf("PCE admin should see 1 transaction, got %d", len(l))
	}
	if l := e.must(e.do("GET", "/v1/admin/meters", red.Admin, nil), 200).list("meters"); len(l) != 1 {
		t.Fatalf("Red admin should see 1 meter, got %d", len(l))
	}
	e.must(e.do("PUT", "/v1/admin/customers/"+ada.ID+"/membership", red.Admin, map[string]string{"tier_code": "gold"}), 404)
	e.must(e.do("POST", "/v1/admin/meters/"+pm.ID+"/assign", red.Admin, map[string]any{"user_id": inv.ID}), 404)

	// Tenant context comes from the token; a conflicting header is refused.
	e.must(e.do("GET", "/v1/meters", ada.Token, nil, tenantHdr(red.Slug)), 403)
	// Credentials are tenant-scoped: Red's twin password works only in Red.
	e.must(e.do("POST", "/v1/auth/login", "", map[string]string{"email": twin.Email, "password": "customer-pass-1"}, tenantHdr(red.Slug)), 200)
	if e.walletKobo(twin) != 0 {
		t.Fatal("same-email user in another tenant must have an independent wallet")
	}
	// Tenant admins cannot reach platform endpoints.
	e.must(e.do("GET", "/v1/platform/tenants", red.Admin, nil), 403)
	// Benefits are off for PCE.
	e.must(e.do("POST", "/v1/benefits/monthly-credit", ada.Token, nil), 403)

	// Database-level guarantee: inside a PCE-bound transaction, RLS hides Red
	// rows even from a query with no tenant predicate at all.
	pceID := uuid.MustParse(pce.ID)
	err := e.svc.DB.Tenant(context.Background(), pceID, func(tx pgx.Tx) error {
		var users, meters, vends, tiers int
		if err := tx.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM meters),
			(SELECT count(*) FROM vend_transactions), (SELECT count(*) FROM membership_tiers)`).Scan(&users, &meters, &vends, &tiers); err != nil {
			return err
		}
		if users != 2 || meters != 1 || vends != 1 || tiers != 0 { // admin + ada
			t.Errorf("RLS leak: users=%d meters=%d vends=%d tiers=%d", users, meters, vends, tiers)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Writing a row for another tenant is rejected by the policy.
	err = e.svc.DB.Tenant(context.Background(), pceID, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO notifications (tenant_id, title, message) VALUES ($1,'x','y')`, red.ID)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("RLS must refuse a cross-tenant insert, got %v", err)
	}
}

func TestPurchaseDeliveryAndHistory(t *testing.T) {
	e := newEnv(t, false)
	p := e.provision("zen", nil, nil)
	c := e.customer(p.Slug, "c@zen.test")
	m := e.zieonMeter(p, c, "11111111111", nil)

	q := e.must(e.do("POST", "/v1/quotes", c.Token, map[string]any{"meter_id": m.ID, "amount_kobo": 500000}), 200)
	if q.num("energy_kwh") != 25 {
		t.Fatalf("₦5,000 at ₦200/kWh should be 25 kWh, got %v", q.num("energy_kwh"))
	}
	e.must(e.buy(c, m, 500000), 402) // empty wallet
	e.fund(c, 1000000)
	r := e.must(e.buy(c, m, 500000), 201)
	tok := r.str("token")
	if r.str("status") != "vended" || len(tok) != 20 || r.get("sandbox") != true {
		t.Fatalf("bad purchase: %s", r.Raw)
	}
	if e.walletKobo(c) != 500000 {
		t.Fatal("wallet not debited")
	}
	pend := e.must(e.do("GET", "/v1/tokens?meter_id="+m.ID, c.Token, nil), 200).list("tokens")
	if len(pend) != 1 {
		t.Fatalf("expected 1 pending token, got %d", len(pend))
	}
	d := e.must(e.do("POST", "/v1/tokens/redeem", c.Token, map[string]any{"meter_id": m.ID, "token": tok}), 200)
	if d.str("status") != "delivered" || d.get("simulated") != true || d.num("meter", "kwh_reserve") != 25 {
		t.Fatalf("bad redeem: %s", d.Raw)
	}
	e.must(e.do("POST", "/v1/tokens/redeem", c.Token, map[string]any{"meter_id": m.ID, "token": tok}), 409) // reuse
	if g := e.must(e.do("GET", "/v1/purchases/"+r.str("id"), c.Token, nil), 200); g.str("status") != "delivered" {
		t.Fatalf("purchase should be delivered: %s", g.Raw)
	}
	acts := e.must(e.do("GET", "/v1/activity", c.Token, nil), 200).list("activity")
	if len(acts) < 4 { // fund, purchase, delivered, meter added
		t.Fatalf("activity too short: %v", acts)
	}
}

func TestIdempotencyAndConcurrency(t *testing.T) {
	e := newEnv(t, false)
	p := e.provision("zen", nil, nil)
	c := e.customer(p.Slug, "c@zen.test")
	m := e.zieonMeter(p, c, "11111111112", nil)
	e.fund(c, 300000)

	body := map[string]any{"meter_id": m.ID, "amount_kobo": 100000, "payment_method": "wallet"}
	a := e.must(e.do("POST", "/v1/purchases", c.Token, body, idem("k-1")), 201)
	b := e.must(e.do("POST", "/v1/purchases", c.Token, body, idem("k-1")), 201)
	if a.str("id") != b.str("id") || a.str("token") != b.str("token") {
		t.Fatal("replay must return the original purchase")
	}
	e.must(e.do("POST", "/v1/purchases", c.Token, map[string]any{"meter_id": m.ID, "amount_kobo": 200000, "payment_method": "wallet"}, idem("k-1")), 422)
	e.must(e.do("POST", "/v1/purchases", c.Token, body), 400) // key required
	if e.walletKobo(c) != 200000 {
		t.Fatalf("replay must not debit twice, wallet=%v", e.walletKobo(c))
	}

	// 10 concurrent ₦1,000 purchases against a ₦2,000 balance: exactly 2 succeed.
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.do("POST", "/v1/purchases", c.Token, body, idem(uuid.NewString()))
			mu.Lock()
			codes[r.Status]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[201] != 2 || codes[402] != 8 {
		t.Fatalf("concurrency: %v", codes)
	}
	if e.walletKobo(c) != 0 {
		t.Fatal("wallet should be exactly zero")
	}
}

func TestVendFailureRefundAndReconciliation(t *testing.T) {
	e := newEnv(t, false)
	p := e.provision("zen", nil, nil)
	c := e.customer(p.Slug, "c@zen.test")
	rejecting := e.zieonMeter(p, c, "12345670000", nil) // sandbox rejects numbers ending 0000
	ok := e.zieonMeter(p, c, "12345671111", nil)
	e.fund(c, 500000)

	r := e.must(e.buy(c, rejecting, 200000), 201)
	if r.str("status") != "refunded" || r.get("token") != nil {
		t.Fatalf("rejected vend must refund without a token: %s", r.Raw)
	}
	if e.walletKobo(c) != 500000 {
		t.Fatal("refund did not restore the wallet")
	}

	// Network failure: outcome unknown, so the purchase stays "vending" (no token, no refund).
	e.vend.FailNext = errors.New("connection reset")
	r = e.must(e.buy(c, ok, 200000), 201)
	if r.str("status") != "vending" {
		t.Fatalf("unknown outcome must stay vending: %s", r.Raw)
	}
	// Reconciliation: provider has no record -> retried with the same reference -> vended.
	_, err := e.svc.DB.Pool.Exec(context.Background(), `UPDATE vend_transactions SET updated_at = now() - interval '5 minutes' WHERE id=$1`, r.str("id"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ReconcileVends(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := e.must(e.do("GET", "/v1/purchases/"+r.str("id"), c.Token, nil), 200)
	if g.str("status") != "vended" || len(g.str("token")) != 20 {
		t.Fatalf("reconciliation should complete the vend: %s", g.Raw)
	}
	if e.walletKobo(c) != 300000 {
		t.Fatalf("exactly one debit expected, wallet=%v", e.walletKobo(c))
	}
}

func TestCardPaymentWebhooks(t *testing.T) {
	e := newEnv(t, true)
	p := e.provision("zen", nil, nil)
	c := e.customer(p.Slug, "c@zen.test")
	m := e.zieonMeter(p, c, "11111111113", nil)

	r := e.must(e.do("POST", "/v1/purchases", c.Token, map[string]any{"meter_id": m.ID, "amount_kobo": 300000, "payment_method": "card"}, idem("card-1")), 201)
	ref := r.str("payment", "reference")
	if r.str("status") != "payment_pending" || r.str("payment", "checkout_url") == "" {
		t.Fatalf("card purchase should await payment: %s", r.Raw)
	}
	// Bad signature is rejected.
	e.must(e.do("POST", "/v1/webhooks/payments/sandbox", "", []byte(`{"id":"e1","type":"payment.succeeded","reference":"`+ref+`","amount_kobo":300000}`),
		withHeader("Zieon-Signature", "t=1,v1=00")), 401)
	ev := map[string]any{"id": "evt-1", "type": "payment.succeeded", "reference": ref, "amount_kobo": 300000}
	e.must(e.signedWebhook(ev), 200)
	if dup := e.must(e.signedWebhook(ev), 200); dup.str("status") != "duplicate" {
		t.Fatalf("duplicate webhook should be ignored: %s", dup.Raw)
	}
	// A second, different event for the same payment must not vend again.
	e.must(e.signedWebhook(map[string]any{"id": "evt-2", "type": "payment.succeeded", "reference": ref, "amount_kobo": 300000}), 200)
	g := e.must(e.do("GET", "/v1/purchases/"+r.str("id"), c.Token, nil), 200)
	if g.str("status") != "vended" {
		t.Fatalf("payment confirmation should vend: %s", g.Raw)
	}
	var tokens int
	_ = e.svc.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM energy_tokens WHERE vend_transaction_id=$1`, r.str("id")).Scan(&tokens)
	if tokens != 1 {
		t.Fatalf("duplicate-credit prevention failed: %d tokens", tokens)
	}

	// Amount mismatch is not credited.
	f := e.must(e.do("POST", "/v1/wallet/fund", c.Token, map[string]any{"amount_kobo": 100000, "method": "card"}, idem("fund-1")), 201)
	e.must(e.signedWebhook(map[string]any{"id": "evt-3", "type": "payment.succeeded", "reference": f.str("payment", "reference"), "amount_kobo": 1}), 200)
	if e.walletKobo(c) != 0 {
		t.Fatal("underpaid webhook must not credit the wallet")
	}
	// Delayed notification: no webhook, reconciliation verifies with the provider.
	f2 := e.must(e.do("POST", "/v1/wallet/fund", c.Token, map[string]any{"amount_kobo": 100000, "method": "card"}, idem("fund-2")), 201)
	ref2 := f2.str("payment", "reference")
	e.pay.mu.Lock()
	e.pay.verified[ref2] = payments.Verification{Status: "succeeded", AmountKobo: 100000}
	e.pay.mu.Unlock()
	_, _ = e.svc.DB.Pool.Exec(context.Background(), `UPDATE payments SET created_at = now() - interval '20 minutes' WHERE reference=$1`, ref2)
	if err := e.svc.ReconcilePayments(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.walletKobo(c) != 100000 {
		t.Fatalf("reconciliation should credit the delayed payment, wallet=%v", e.walletKobo(c))
	}
}

func TestBenefitsEngine(t *testing.T) {
	e := newEnv(t, false)
	red := e.provision("red", map[string]bool{"benefits": true}, nil)
	gold := e.customer(red.Slug, "gold@red.test")
	plat := e.customer(red.Slug, "plat@red.test")
	gm := e.zieonMeter(red, gold, "31000000011", nil)
	pm := e.zieonMeter(red, plat, "31000000012", nil)

	e.must(e.do("PUT", "/v1/admin/tiers", red.Admin, map[string]any{"code": "gold", "name": "Gold", "cashback_bps": 500, "max_cashback_kobo": 250000,
		"monthly_credit_kobo": 200000, "active": true}), 200)
	e.must(e.do("PUT", "/v1/admin/tiers", red.Admin, map[string]any{"code": "platinum", "name": "Platinum", "cashback_bps": 1000, "max_cashback_kobo": 500000,
		"monthly_cashback_cap_kobo": 600000, "active": true}), 200)
	e.must(e.do("PUT", "/v1/admin/tiers", red.Admin, map[string]any{"code": "bad", "name": "Bad", "cashback_bps": 500}), 400) // cap required
	e.must(e.do("PUT", "/v1/admin/customers/"+gold.ID+"/membership", red.Admin, map[string]string{"tier_code": "gold"}), 200)
	e.must(e.do("PUT", "/v1/admin/customers/"+plat.ID+"/membership", red.Admin, map[string]string{"tier_code": "platinum"}), 200)

	// Empty pool: purchases succeed but earn nothing, and claims are refused.
	e.fund(gold, 10000000)
	e.fund(plat, 20000000)
	if r := e.must(e.buy(gold, gm, 100000), 201); r.num("reward_earned_kobo") != 0 {
		t.Fatal("no rewards without pool funding")
	}
	e.must(e.do("POST", "/v1/benefits/monthly-credit", gold.Token, nil), 503)

	e.must(e.do("POST", "/v1/platform/tenants/"+red.ID+"/reward-pool/deposits", e.platform, map[string]any{"amount_kobo": 2000000, "reference": "dep"}), 201)
	e.must(e.do("POST", "/v1/platform/tenants/"+red.ID+"/reward-pool/deposits", e.platform, map[string]any{"amount_kobo": 2000000, "reference": "dep"}), 409)

	// Gold: 5% of ₦10,000 = ₦500.
	r := e.must(e.buy(gold, gm, 1000000), 201)
	if r.num("reward_earned_kobo") != 50000 {
		t.Fatalf("gold cashback want 50000, got %v", r.num("reward_earned_kobo"))
	}
	// Platinum: 10% of ₦100,000 = ₦10,000, capped at ₦5,000 per purchase.
	if r := e.must(e.buy(plat, pm, 10000000), 201); r.num("reward_earned_kobo") != 500000 {
		t.Fatalf("platinum per-purchase cap, got %v", r.num("reward_earned_kobo"))
	}
	// Monthly cap ₦6,000: next purchase earns only ₦1,000.
	if r := e.must(e.buy(plat, pm, 5000000), 201); r.num("reward_earned_kobo") != 100000 {
		t.Fatalf("platinum monthly cap, got %v", r.num("reward_earned_kobo"))
	}

	// Monthly credit: once per month.
	e.must(e.do("POST", "/v1/benefits/monthly-credit", gold.Token, nil), 201)
	e.must(e.do("POST", "/v1/benefits/monthly-credit", gold.Token, nil), 409)
	e.must(e.do("POST", "/v1/benefits/monthly-credit", plat.Token, nil), 409) // tier has no monthly credit

	// Spending rewards is a funding leg: energy and tariff are unchanged.
	b := e.must(e.do("GET", "/v1/benefits", gold.Token, nil), 200)
	rewardBal := b.num("reward_balance_kobo") // 50000 + 200000
	if rewardBal != 250000 {
		t.Fatalf("reward balance want 250000, got %v", rewardBal)
	}
	walletBefore := e.walletKobo(gold)
	r = e.must(e.buy(gold, gm, 1000000, map[string]any{"use_rewards": true}), 201)
	if r.num("reward_kobo") != 250000 || r.num("wallet_kobo") != 750000 || r.num("energy_kwh") != 50 || r.num("rate_kobo_per_kwh") != 20000 {
		t.Fatalf("reward leg wrong: %s", r.Raw)
	}
	if e.walletKobo(gold) != walletBefore-750000 {
		t.Fatal("wallet leg wrong")
	}
	// Cashback is earned on the paid portion only: 5% of ₦7,500.
	if r.num("reward_earned_kobo") != 37500 {
		t.Fatalf("cashback must exclude the reward leg, got %v", r.num("reward_earned_kobo"))
	}
}

func TestDeviceTelemetry(t *testing.T) {
	e := newEnv(t, false)
	p := e.provision("zen", map[string]bool{"auto_topup": true}, nil)
	c := e.customer(p.Slug, "c@zen.test")
	m := e.zieonMeter(p, c, "11111111114", nil)
	dumb := e.zieonMeter(p, e.customer(p.Slug, "d@zen.test"), "11111111115", map[string]bool{})

	e.must(e.deviceReport(m, map[string]any{"credit_kwh": 50.0, "cumulative_kwh": 1000.0, "power_status": "lightOn"}, "nonce-0001"), 202)
	e.must(e.deviceReport(m, map[string]any{"credit_kwh": 50.0}, "nonce-0001"), 401) // replay
	bad := m
	bad.Secret = "zds_wrong"
	e.must(e.deviceReport(bad, map[string]any{"credit_kwh": 1.0}, "nonce-0002"), 401)
	e.must(e.deviceReport(dumb, map[string]any{"credit_kwh": 1.0}, "nonce-0003"), 401) // no comms secret

	got := e.must(e.do("GET", "/v1/meters/"+m.ID, c.Token, nil), 200)
	if got.str("balance_source") != "device" || got.num("kwh_reserve") != 50 || got.str("connectivity") != "online" {
		t.Fatalf("device balance not reflected: %s", got.Raw)
	}

	// Enable auto top-up, then drop below thresholds.
	e.fund(c, 500000)
	e.must(e.do("PATCH", "/v1/meters/"+m.ID, c.Token, map[string]any{"low_balance_threshold_kwh": 10,
		"auto_topup": map[string]any{"enabled": true, "threshold_kwh": 5, "amount_kobo": 200000}}), 200)
	e.must(e.deviceReport(m, map[string]any{"credit_kwh": 3.0, "cumulative_kwh": 1047.0, "tamper": true}, "nonce-0004"), 202)
	ns := e.must(e.do("GET", "/v1/notifications", c.Token, nil), 200).list("notifications")
	foundLow := false
	for _, n := range ns {
		foundLow = foundLow || n.(map[string]any)["kind"] == "low_balance"
	}
	if !foundLow {
		t.Fatal("low balance notification missing")
	}
	if err := e.svc.RunAutoTopups(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.walletKobo(c) != 300000 {
		t.Fatalf("auto top-up should spend ₦2,000, wallet=%v", e.walletKobo(c))
	}
	var alerts int
	_ = e.svc.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM meter_alerts WHERE meter_id=$1 AND kind='tamper'`, m.ID).Scan(&alerts)
	if alerts != 1 {
		t.Fatalf("tamper alert expected, got %d", alerts)
	}
	cons := e.must(e.do("GET", "/v1/meters/"+m.ID+"/consumption", c.Token, nil), 200)
	if cons.str("data_source") != "device" || cons.num("total_kwh") != 47 {
		t.Fatalf("consumption from register readings: %s", cons.Raw)
	}
	// A meter that never reports must not invent consumption.
	dc := e.customer(p.Slug, "e@zen.test")
	em := e.zieonMeter(p, dc, "11111111116", map[string]bool{})
	if r := e.must(e.do("GET", "/v1/meters/"+em.ID+"/consumption", dc.Token, nil), 200); r.str("data_source") != "none" {
		t.Fatalf("expected no data: %s", r.Raw)
	}
	// Offline detection.
	_, _ = e.svc.DB.Pool.Exec(context.Background(), `UPDATE meters SET last_seen_at = now() - interval '2 hours' WHERE id=$1`, m.ID)
	_ = e.svc.DetectOfflineMeters(context.Background())
	if r := e.must(e.do("GET", "/v1/meters/"+m.ID, c.Token, nil), 200); r.str("connectivity") != "offline" {
		t.Fatal("meter should be offline")
	}
}

func TestSparkShare(t *testing.T) {
	e := newEnv(t, false)
	p := e.provision("zen", nil, nil)
	a := e.customer(p.Slug, "a@zen.test")
	b := e.customer(p.Slug, "b@zen.test")
	am := e.zieonMeter(p, a, "11111111117", map[string]bool{"remote_debit": true, "remote_credit": true})
	bm := e.zieonMeter(p, b, "11111111118", map[string]bool{"remote_credit": true})
	e.fund(a, 1000000)
	r := e.must(e.buy(a, am, 1000000), 201)
	e.must(e.do("POST", "/v1/tokens/redeem", a.Token, map[string]any{"meter_id": am.ID, "token": r.str("token")}), 200) // 50 kWh

	e.must(e.do("POST", "/v1/shares", a.Token, map[string]any{"from_meter_id": am.ID, "to_meter_number": bm.Number, "kwh": 60}, idem("s0")), 409)
	e.must(e.do("POST", "/v1/shares", a.Token, map[string]any{"from_meter_id": am.ID, "to_meter_number": bm.Number, "kwh": 20, "note": "Mom"}, idem("s1")), 201)
	tok := e.must(e.do("POST", "/v1/shares", b.Token, map[string]any{"from_meter_id": bm.ID, "to_meter_number": am.Number, "kwh": 1}, idem("s2")), 409)
	_ = tok // b's meter cannot release units remotely
	tr := e.must(e.do("POST", "/v1/shares", a.Token, map[string]any{"from_meter_id": am.ID, "to_meter_number": bm.Number, "kwh": 5, "mode": "token"}, idem("s3")), 201)
	e.must(e.do("POST", "/v1/tokens/redeem", b.Token, map[string]any{"meter_id": bm.ID, "token": tr.str("token")}), 200)

	if r := e.must(e.do("GET", "/v1/meters/"+am.ID, a.Token, nil), 200); r.num("platform_kwh") != 25 {
		t.Fatalf("sender should have 25 kWh, got %v", r.num("platform_kwh"))
	}
	if r := e.must(e.do("GET", "/v1/meters/"+bm.ID, b.Token, nil), 200); r.num("platform_kwh") != 25 {
		t.Fatalf("recipient should have 25 kWh, got %v", r.num("platform_kwh"))
	}
	if l := e.must(e.do("GET", "/v1/recipients", a.Token, nil), 200).list("recipients"); len(l) != 1 {
		t.Fatalf("recipient list: %v", l)
	}
	var sum int64
	_ = e.svc.DB.Pool.QueryRow(context.Background(), `SELECT sum(wh) FROM energy_entries`).Scan(&sum)
	if sum != 50000 {
		t.Fatalf("energy must be conserved across shares, total=%d", sum)
	}
}

func TestRBACAndAuth(t *testing.T) {
	e := newEnv(t, false)
	p := e.provision("zen", nil, nil)
	c := e.customer(p.Slug, "c@zen.test")
	staff := e.must(e.do("POST", "/v1/admin/team", p.Admin, map[string]string{"email": "s@zen.test", "name": "S", "role": "tenant_staff", "password": "staff-pass-1"}), 201)
	st := e.login(p.Slug, "s@zen.test", "staff-pass-1")

	e.must(e.do("GET", "/v1/admin/customers", c.Token, nil), 403)
	e.must(e.do("GET", "/v1/admin/customers", st, nil), 200)
	e.must(e.do("POST", "/v1/admin/api-keys", st, map[string]any{"name": "k", "scopes": []string{"sessions:write"}}), 403)
	e.must(e.do("POST", "/v1/admin/team", p.Admin, map[string]string{"email": "x@zen.test", "role": "platform_admin", "password": "xxxxxxxx1"}), 400)
	e.must(e.do("GET", "/v1/platform/tenants", p.Admin, nil), 403)
	e.must(e.do("GET", "/v1/wallet", p.Admin, nil), 403)
	e.must(e.do("GET", "/v1/meters", "", nil), 401)
	e.must(e.do("GET", "/v1/meters", "not-a-jwt", nil), 401)
	e.must(e.do("POST", "/v1/auth/register", "", map[string]string{"email": "bad", "password": "x"}, tenantHdr(p.Slug)), 400)
	e.must(e.do("POST", "/v1/auth/register", "", map[string]any{"email": "z@zen.test", "password": "longenough", "role": "tenant_admin"}, tenantHdr(p.Slug)), 400) // unknown field

	// Disabling a user revokes access immediately, even with a live token.
	e.must(e.do("PATCH", "/v1/admin/users/"+staff.str("id"), p.Admin, map[string]string{"status": "disabled"}), 204)
	e.must(e.do("GET", "/v1/admin/customers", st, nil), 401)

	// Lockout after repeated failures.
	for i := 0; i < 5; i++ {
		e.must(e.do("POST", "/v1/auth/login", "", map[string]string{"email": c.Email, "password": "wrong"}, tenantHdr(p.Slug)), 401)
	}
	e.must(e.do("POST", "/v1/auth/login", "", map[string]string{"email": c.Email, "password": "customer-pass-1"}, tenantHdr(p.Slug)), 429)

	// Refresh rotation and reuse detection.
	c2 := e.must(e.do("POST", "/v1/auth/register", "", map[string]string{"email": "r@zen.test", "password": "customer-pass-1"}, tenantHdr(p.Slug)), 201)
	rt := c2.str("refresh_token")
	n := e.must(e.do("POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": rt}), 200)
	e.must(e.do("POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": rt}), 401)                     // reuse
	e.must(e.do("POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": n.str("refresh_token")}), 401) // family revoked

	// Suspended tenant: no logins, existing tokens stop working.
	e.must(e.do("PATCH", "/v1/platform/tenants/"+p.ID, e.platform, map[string]string{"status": "suspended"}), 200)
	e.must(e.do("GET", "/v1/admin/customers", p.Admin, nil), 403)
	e.must(e.do("POST", "/v1/auth/login", "", map[string]string{"email": "admin@zen.test", "password": "admin-pass-1"}, tenantHdr(p.Slug)), 403)
	// Live vending cannot be enabled without an authorised route.
	e.must(e.do("PATCH", "/v1/platform/tenants/"+p.ID, e.platform, map[string]string{"vending_mode": "live"}), 409)

	var n2 int
	_ = e.svc.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action IN ('platform.tenant.update','user.status','team.add')`).Scan(&n2)
	if n2 < 3 {
		t.Fatalf("privileged actions must be audited, got %d", n2)
	}
}

func TestPartnerSSOAndWebhooks(t *testing.T) {
	e := newEnv(t, false)
	pce := e.provision("pce", nil, nil)
	red := e.provision("red", nil, nil)
	e.must(e.do("PATCH", "/v1/admin/settings", pce.Admin, map[string]any{"allowed_origins": []string{"https://app.pce.test"}}), 200)

	k := e.must(e.do("POST", "/v1/admin/api-keys", pce.Admin, map[string]any{"name": "backend", "scopes": []string{"sessions:write", "transactions:read"}}), 201)
	key := k.str("secret")
	if !strings.HasPrefix(key, "zk_") {
		t.Fatal("secret must be returned once on creation")
	}
	if l := e.must(e.do("GET", "/v1/admin/api-keys", pce.Admin, nil), 200).list("api_keys"); l[0].(map[string]any)["secret"] != nil {
		t.Fatal("secret must not be listed")
	}

	// Partner mints an SSO code for its signed-in user.
	s := e.must(e.do("POST", "/v1/partner/sessions", key, map[string]string{"external_user_id": "pce-user-42", "email": "u42@pce.test",
		"name": "Uche", "return_origin": "https://app.pce.test"}), 201)
	e.must(e.do("POST", "/v1/partner/sessions", key, map[string]string{"external_user_id": "x", "email": "x@pce.test", "return_origin": "https://evil.test"}), 400)
	code := s.str("code")
	ex := e.must(e.do("POST", "/v1/embed/exchange", "", map[string]string{"code": code}), 200)
	if ex.str("refresh_token") != "" || ex.str("tenant") != "pce" {
		t.Fatalf("embed session must be short-lived and tenant-bound: %s", ex.Raw)
	}
	e.must(e.do("POST", "/v1/embed/exchange", "", map[string]string{"code": code}), 401) // single use
	me := e.must(e.do("GET", "/v1/me", ex.str("access_token"), nil), 200)
	if me.str("auth_method") != "partner_sso" || me.str("user", "external_id") != "pce-user-42" {
		t.Fatalf("me: %s", me.Raw)
	}
	// Same external id maps to the same customer.
	s2 := e.must(e.do("POST", "/v1/partner/sessions", key, map[string]string{"external_user_id": "pce-user-42", "email": "u42@pce.test"}), 201)
	if s2.str("user_id") != s.str("user_id") {
		t.Fatal("external id must be stable")
	}
	// Scope enforcement and tenant binding.
	e.must(e.do("GET", "/v1/partner/meters", key, nil), 403)
	e.must(e.do("GET", "/v1/partner/customers/pce-user-42", key, nil), 403)
	rk := e.must(e.do("POST", "/v1/admin/api-keys", red.Admin, map[string]any{"name": "r", "scopes": []string{"customers:read"}}), 201).str("secret")
	e.must(e.do("GET", "/v1/partner/customers/pce-user-42", rk, nil), 404)
	// An API key is not a user token and vice versa.
	e.must(e.do("GET", "/v1/meters", key, nil), 401)
	e.must(e.do("GET", "/v1/partner/transactions", pce.Admin, nil), 401)
	// Revocation.
	e.must(e.do("DELETE", "/v1/admin/api-keys/"+k.str("id"), pce.Admin, nil), 204)
	e.must(e.do("POST", "/v1/partner/sessions", key, map[string]string{"external_user_id": "y", "email": "y@pce.test"}), 401)

	// Outbound webhooks: private targets refused; deliveries signed and retried.
	e.must(e.do("POST", "/v1/admin/webhooks", pce.Admin, map[string]any{"url": "https://127.0.0.1/hook", "events": []string{"*"}}), 400)
	e.must(e.do("POST", "/v1/admin/webhooks", pce.Admin, map[string]any{"url": "http://partner.test/hook", "events": []string{"*"}}), 400)
	wh := e.must(e.do("POST", "/v1/admin/webhooks", pce.Admin, map[string]any{"url": "https://hooks.pce.test/zieon", "events": []string{"wallet.funded"}}), 201)
	secret := wh.str("secret")

	var mu sync.Mutex
	var got []string
	fail := true
	partnerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		defer mu.Unlock()
		if !secure.VerifyWebhook([]byte(secret), r.Header.Get("Zieon-Signature"), buf[:n], time.Now(), 5*time.Minute) {
			w.WriteHeader(400)
			return
		}
		if fail {
			fail = false
			w.WriteHeader(500)
			return
		}
		got = append(got, r.Header.Get("Zieon-Event"))
	}))
	defer partnerSrv.Close()
	target, _ := url.Parse(partnerSrv.URL)
	e.svc.HTTP = &http.Client{Transport: rewrite{target}}

	c := e.customer(pce.Slug, "w@pce.test")
	e.fund(c, 100000)
	_ = e.svc.DeliverWebhooks(context.Background()) // first attempt fails (500)
	_, _ = e.svc.DB.Pool.Exec(context.Background(), `UPDATE webhook_deliveries SET next_attempt_at = now()`)
	_ = e.svc.DeliverWebhooks(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "wallet.funded" {
		t.Fatalf("expected one signed wallet.funded delivery after retry, got %v", got)
	}
}

type rewrite struct{ to *url.URL }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme, req.URL.Host = r.to.Scheme, r.to.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestLedgerIsAppendOnly(t *testing.T) {
	e := newEnv(t, false)
	p := e.provision("zen", nil, nil)
	c := e.customer(p.Slug, "c@zen.test")
	e.fund(c, 100000)
	ctx := context.Background()
	for _, q := range []string{`UPDATE ledger_entries SET amount_kobo = 1`, `DELETE FROM ledger_entries`, `DELETE FROM audit_logs`} {
		err := e.svc.DB.System(ctx, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		if err == nil {
			t.Fatalf("%q must be refused", q)
		}
	}
	// Even the table owner (migration role) is stopped by the trigger.
	if _, err := e.svc.DB.Pool.Exec(ctx, `UPDATE ledger_entries SET amount_kobo = 1`); err == nil {
		t.Fatal("trigger must block owner updates")
	}
	// Ledger balances reconcile with the balance table.
	var mismatches int
	_ = e.svc.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM balances b WHERE b.balance_kobo <>
		(SELECT coalesce(sum(amount_kobo),0) FROM ledger_entries l WHERE l.account_type=b.account_type AND l.account_id=b.account_id)`).Scan(&mismatches)
	if mismatches != 0 {
		t.Fatalf("%d balances disagree with the ledger", mismatches)
	}
}
