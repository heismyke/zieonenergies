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
	p := e.provision("zen", nil, nil)
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

	v := e.must(e.do("POST", "/v1/public/meters/verify", "", map[string]string{"provider_code": "IKEDC", "meter_number": "1111111111111"}, h), 200)
	if v.str("customer_name") != "TESTMETER1" {
		t.Fatalf("verify: %s", v.Raw)
	}
	e.must(e.do("POST", "/v1/public/meters/verify", "", map[string]string{"provider_code": "IKEDC", "meter_number": "9999999999"}, h), 404)

	order := func(meter string, amount int64, opts ...reqOpt) resp {
		return e.do("POST", "/v1/public/orders", "", map[string]any{"provider_code": "IKEDC", "meter_number": meter,
			"amount_kobo": amount, "email": "buyer@example.com", "phone": "08031234567"}, append([]reqOpt{h, idem(uuid.NewString())}, opts...)...)
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
	p := e.provision("zen", nil, nil)
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
