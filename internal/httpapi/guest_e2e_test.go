package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/heismyke/zieonenergies/internal/vending"
)

func TestGuestCheckout(t *testing.T) {
	e := newEnv(t, false)
	agg := &fakeAggregator{settled: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(agg.handler))
	defer srv.Close()
	e.svc.Vending["vtpass"] = vending.NewVTpass(srv.URL+"/sandbox", "AK", "PK", "SK", "08011111111")
	p := e.provision("zen", map[string]bool{"guest_orders": true}, nil)
	h := tenantHdr(p.Slug)

	ds := e.must(e.do("GET", "/v1/public/discos", "", nil, h), 200).list("discos")
	for _, d := range ds {
		if d.(map[string]any)["provider_code"] == "ZIEON" {
			t.Fatal("Zieon hardware must not appear in the public DisCo list")
		}
	}
	if len(ds) < 10 {
		t.Fatalf("expected the DisCo catalogue, got %d", len(ds))
	}
	for _, d := range ds {
		if m := d.(map[string]any); m["provider_code"] == "IKEDC" {
			// The DisCo's own minimum (₦500) and logo come from the aggregator catalogue.
			if m["min_purchase_kobo"] != 50000.0 || m["max_purchase_kobo"] != 40000000.0 || m["logo_url"] != "https://img/ikedc.jpg" ||
				m["rate_kobo_per_kwh"].(float64) <= 0 {
				t.Fatalf("IKEDC catalogue not applied: %v", m)
			}
		}
	}

	v := e.must(e.do("POST", "/v1/public/meters/verify", "", map[string]string{"provider_code": "IKEDC", "meter_number": "1111111111111"}, h), 200)
	if v.str("customer_name") != "TESTMETER1" {
		t.Fatalf("verify: %s", v.Raw)
	}
	e.must(e.do("POST", "/v1/public/meters/verify", "", map[string]string{"provider_code": "IKEDC", "meter_number": "9999999999"}, h), 404)

	order := func(meter string, amount int64, opts ...reqOpt) resp {
		return e.do("POST", "/v1/public/orders", "", map[string]any{"provider_code": "IKEDC", "meter_number": meter,
			"amount_kobo": amount, "email": "buyer@example.com", "phone": "08031234567"}, append([]reqOpt{h, idem(uuid.NewString())}, opts...)...)
	}
	// Below the DisCo's own minimum: refused before verification or payment.
	if r := e.must(order("1111111111111", 40000), 400); !strings.Contains(r.str("error", "message"), "between") {
		t.Fatalf("below-minimum message: %s", r.Raw)
	}
	// Wrong meter: refused before any payment is taken.
	e.must(order("9999999999", 200000), 404)
	e.must(e.do("POST", "/v1/public/orders", "", map[string]any{"provider_code": "IKEDC", "meter_number": "1111111111111",
		"amount_kobo": 200000, "email": "buyer@example.com", "phone": "08031234567"}, h), 400) // Idempotency-Key required

	// Sandbox payments confirm instantly: the token comes straight back.
	o := e.must(order("1111111111111", 200000), 201)
	key := o.str("key")
	if o.str("status") != "vended" || len(o.str("token")) != 20 || o.num("energy_kwh") != 9.5 || !strings.HasPrefix(key, "zgo_") {
		t.Fatalf("guest order: %s", o.Raw)
	}
	g := e.must(e.do("GET", "/v1/public/orders/"+o.str("id")+"?key="+key, "", nil, h), 200)
	if g.str("token") != o.str("token") || g.str("key") != "" || g.str("customer_name") != "TESTMETER1" {
		t.Fatalf("order lookup: %s", g.Raw)
	}
	e.must(e.do("GET", "/v1/public/orders/"+o.str("id")+"?key=zgo_wrong", "", nil, h), 404)
	e.must(e.do("GET", "/v1/public/orders/"+o.str("id"), "", nil, tenantHdr("zieon")), 404) // other tenant

	// DisCo fails after payment: refund goes to the card, not to a wallet.
	f := e.must(order("3000000000000", 100000), 201)
	if f.str("status") != "refunded" || f.get("token") != nil || !strings.Contains(f.str("message"), "refunded") {
		t.Fatalf("failed guest order: %s", f.Raw)
	}
	var payStatus string
	var walletRows int
	_ = e.svc.DB.Pool.QueryRow(context.Background(), `SELECT status FROM payments WHERE vend_transaction_id=$1`, f.str("id")).Scan(&payStatus)
	_ = e.svc.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM ledger_entries WHERE account_type='wallet' AND kind='refund'`).Scan(&walletRows)
	if payStatus != "refund_pending" || walletRows != 0 {
		t.Fatalf("guest refund must be queued to the card: payment=%s wallet refunds=%d", payStatus, walletRows)
	}
	if err := e.svc.RunCardRefunds(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = e.svc.DB.Pool.QueryRow(context.Background(), `SELECT status FROM payments WHERE vend_transaction_id=$1`, f.str("id")).Scan(&payStatus)
	if payStatus != "refunded" {
		t.Fatalf("card refund not processed: %s", payStatus)
	}

	// Feature off: no public buying.
	e.must(e.do("PATCH", "/v1/admin/settings", p.Admin, map[string]any{"features": map[string]bool{"guest_checkout": false}}), 200)
	e.must(e.do("GET", "/v1/public/discos", "", nil, h), 403)
}

func TestGuestCheckoutRedirect(t *testing.T) {
	e := newEnv(t, true) // payments wait for a webhook, like Paystack
	agg := &fakeAggregator{settled: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(agg.handler))
	defer srv.Close()
	e.svc.Vending["vtpass"] = vending.NewVTpass(srv.URL+"/sandbox", "AK", "PK", "SK", "08011111111")
	p := e.provision("zen", map[string]bool{"guest_orders": true}, nil)
	e.must(e.do("PATCH", "/v1/admin/settings", p.Admin, map[string]any{"allowed_origins": []string{"https://shop.zen.test"}}), 200)

	o := e.must(e.do("POST", "/v1/public/orders", "", map[string]any{"provider_code": "IKEDC", "meter_number": "1111111111111",
		"amount_kobo": 500000, "email": "b@example.com", "phone": "+2348031234567"},
		tenantHdr(p.Slug), idem(uuid.NewString()), withHeader("Origin", "https://shop.zen.test")), 201)
	if o.str("status") != "payment_pending" || o.get("token") != nil || o.str("checkout_url") == "" {
		t.Fatalf("order must await payment: %s", o.Raw)
	}
	want := "https://shop.zen.test/order/" + o.str("id") + "?key=" + o.str("key")
	if e.pay.callback != want {
		t.Fatalf("Paystack must return the buyer to their order page:\n got %s\nwant %s", e.pay.callback, want)
	}
	var cb string
	_ = e.svc.DB.Pool.QueryRow(context.Background(), `SELECT reference FROM payments WHERE vend_transaction_id=$1`, o.str("id")).Scan(&cb)
	e.must(e.signedWebhook(map[string]any{"id": "evt-guest", "type": "payment.succeeded", "reference": cb, "amount_kobo": 500000}), 200)
	g := e.must(e.do("GET", "/v1/public/orders/"+o.str("id")+"?key="+o.str("key"), "", nil, tenantHdr(p.Slug)), 200)
	if g.str("status") != "vended" || len(g.str("token")) != 20 {
		t.Fatalf("after payment the order shows the token: %s", g.Raw)
	}
}

// The public buy page with an account: sign up, buy, see the order history.
func TestAccountOrders(t *testing.T) {
	e := newEnv(t, false)
	agg := &fakeAggregator{settled: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(agg.handler))
	defer srv.Close()
	e.svc.Vending["vtpass"] = vending.NewVTpass(srv.URL+"/sandbox", "AK", "PK", "SK", "08011111111")
	p := e.provision("zen", map[string]bool{"wallet": false}, nil) // buy-only, like production
	h := tenantHdr(p.Slug)

	// Buying without an account is off by default.
	e.must(e.do("POST", "/v1/public/orders", "", map[string]any{"provider_code": "IKEDC", "meter_number": "1111111111111",
		"amount_kobo": 200000, "email": "buyer@example.com", "phone": "08031234567"}, h, idem(uuid.NewString())), 403)

	order := func(token, meter string, amount int64) resp {
		return e.do("POST", "/v1/orders", token, map[string]any{"provider_code": "IKEDC", "meter_number": meter, "amount_kobo": amount},
			idem(uuid.NewString()))
	}
	e.must(e.do("POST", "/v1/orders", "", map[string]any{"provider_code": "IKEDC", "meter_number": "1111111111111", "amount_kobo": 200000}, h, idem(uuid.NewString())), 401)

	c := e.customer(p.Slug, "buyer@example.com")
	e.must(order(c.Token, "1111111111111", 40000), 400) // below the DisCo minimum
	e.must(order(c.Token, "9999999999", 200000), 404)   // unknown meter, nothing charged
	o := e.must(order(c.Token, "1111111111111", 200000), 201)
	if o.str("status") != "vended" || len(o.str("token")) != 20 || o.str("key") != "" || o.str("customer_name") != "TESTMETER1" {
		t.Fatalf("account order: %s", o.Raw)
	}
	if got := e.must(e.do("GET", "/v1/orders/"+o.str("id"), c.Token, nil), 200); got.str("token") != o.str("token") {
		t.Fatalf("get order: %s", got.Raw)
	}
	if ms := e.must(e.do("GET", "/v1/meters", c.Token, nil), 200).list("meters"); len(ms) != 1 {
		t.Fatalf("the meter is saved to the account: %v", ms)
	}
	other := e.customer(p.Slug, "other@example.com")
	e.must(e.do("GET", "/v1/orders/"+o.str("id"), other.Token, nil), 404)

	// DisCo fails after payment: with wallets off the money goes back to the card.
	f := e.must(order(c.Token, "3000000000000", 100000), 201)
	if f.str("status") != "refunded" || f.get("token") != nil {
		t.Fatalf("failed account order: %s", f.Raw)
	}
	var payStatus string
	_ = e.svc.DB.Pool.QueryRow(context.Background(), `SELECT status FROM payments WHERE vend_transaction_id=$1`, f.str("id")).Scan(&payStatus)
	if payStatus != "refund_pending" {
		t.Fatalf("refund must go to the card: payment=%s", payStatus)
	}
	if os := e.must(e.do("GET", "/v1/orders", c.Token, nil), 200).list("orders"); len(os) != 2 || os[0].(map[string]any)["id"] != f.str("id") {
		t.Fatalf("order history, newest first: %v", os)
	}
}

// Signing up claims the passwordless record a guest order left behind, but
// does not reveal those guest orders: nothing proved the email.
func TestSignUpClaimsGuestRecord(t *testing.T) {
	e := newEnv(t, false)
	agg := &fakeAggregator{settled: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(agg.handler))
	defer srv.Close()
	e.svc.Vending["vtpass"] = vending.NewVTpass(srv.URL+"/sandbox", "AK", "PK", "SK", "08011111111")
	p := e.provision("zen", map[string]bool{"guest_orders": true}, nil)
	h := tenantHdr(p.Slug)
	e.must(e.do("POST", "/v1/public/orders", "", map[string]any{"provider_code": "IKEDC", "meter_number": "1111111111111",
		"amount_kobo": 200000, "email": "Guest@Example.com", "phone": "08031234567"}, h, idem(uuid.NewString())), 201)

	c := e.customer(p.Slug, "guest@example.com")
	if os := e.must(e.do("GET", "/v1/orders", c.Token, nil), 200).list("orders"); len(os) != 0 {
		t.Fatalf("guest orders must not appear in a newly claimed account: %v", os)
	}
	e.login(p.Slug, "guest@example.com", "customer-pass-1")
	// Once claimed, the email is taken.
	e.must(e.do("POST", "/v1/auth/register", "", map[string]string{"email": "guest@example.com", "password": "another-pass-1"}, h), 409)
}
