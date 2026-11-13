package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/messaging"
	"github.com/heismyke/zieonenergies/internal/payments"
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

// BuyPower-style wallet: fund it, then buy from the balance.
func TestAccountOrderFromWallet(t *testing.T) {
	e := newEnv(t, false)
	agg := &fakeAggregator{settled: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(agg.handler))
	defer srv.Close()
	e.svc.Vending["vtpass"] = vending.NewVTpass(srv.URL+"/sandbox", "AK", "PK", "SK", "08011111111")
	p := e.provision("zen", nil, nil)
	c := e.customer(p.Slug, "wallet@example.com")
	order := func(amount int64, method string) resp {
		return e.do("POST", "/v1/orders", c.Token, map[string]any{"provider_code": "IKEDC", "meter_number": "1111111111111",
			"amount_kobo": amount, "payment_method": method}, idem(uuid.NewString()))
	}
	if r := order(200000, "wallet"); r.Status < 400 {
		t.Fatalf("an empty wallet cannot pay: %s", r.Raw)
	}
	e.fund(c, 300000)
	if o := e.must(order(200000, "wallet"), 201); o.str("status") != "vended" || len(o.str("token")) != 20 {
		t.Fatalf("wallet order: %s", o.Raw)
	}
	if got := e.walletKobo(c); got != 100000 {
		t.Fatalf("wallet after purchase: %v", got)
	}
	e.must(order(200000, "crypto"), 400)

	// The wallet page's history: newest first, the purchase linked to its order.
	a := e.must(e.do("GET", "/v1/wallet/activity", c.Token, nil), 200)
	entries := a.list("entries")
	if len(entries) != 2 {
		t.Fatalf("wallet activity: %s", a.Raw)
	}
	buy, top := entries[0].(map[string]any), entries[1].(map[string]any)
	if buy["kind"] != "purchase" || buy["amount_kobo"] != float64(-200000) || buy["balance_after_kobo"] != float64(100000) || buy["order_id"] == nil {
		t.Fatalf("purchase entry: %v", buy)
	}
	if top["kind"] != "wallet_fund" || top["amount_kobo"] != float64(300000) || top["order_id"] != nil {
		t.Fatalf("top-up entry: %v", top)
	}
	months := a.list("months")
	last := months[len(months)-1].(map[string]any)
	if len(months) != 6 || last["in_kobo"] != float64(300000) || last["out_kobo"] != float64(200000) {
		t.Fatalf("monthly totals: %v", months)
	}
}

// A buyer back from the payment page sees the token without the webhook:
// reading the order asks the provider, and only a confirmed success counts.
func TestOrderConfirmsOnReturn(t *testing.T) {
	e := newEnv(t, true) // payments wait for confirmation, like Paystack
	agg := &fakeAggregator{settled: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(agg.handler))
	defer srv.Close()
	e.svc.Vending["vtpass"] = vending.NewVTpass(srv.URL+"/sandbox", "AK", "PK", "SK", "08011111111")
	ps := &namedPay{e.pay, "paystack"}
	e.svc.Payments["paystack"] = ps
	p := e.provision("zen", nil, nil)
	c := e.customer(p.Slug, "return@example.com")
	o := e.must(e.do("POST", "/v1/orders", c.Token, map[string]any{"provider_code": "IKEDC", "meter_number": "1111111111111", "amount_kobo": 200000},
		idem(uuid.NewString())), 201)
	if o.str("status") != "payment_pending" {
		t.Fatalf("order must await payment: %s", o.Raw)
	}
	// Not paid yet: reading the order leaves it pending.
	if g := e.must(e.do("GET", "/v1/orders/"+o.str("id"), c.Token, nil), 200); g.str("status") != "payment_pending" {
		t.Fatalf("unpaid order changed: %s", g.Raw)
	}
	// The buyer completes the checkout; no webhook arrives.
	var ref string
	_ = e.svc.DB.Pool.QueryRow(context.Background(), `SELECT reference FROM payments WHERE vend_transaction_id=$1`, o.str("id")).Scan(&ref)
	e.pay.mu.Lock()
	e.pay.verified[ref] = payments.Verification{Status: "succeeded", AmountKobo: 200000}
	e.pay.mu.Unlock()
	g := e.must(e.do("GET", "/v1/orders/"+o.str("id"), c.Token, nil), 200)
	if g.str("status") != "vended" || len(g.str("token")) != 20 {
		t.Fatalf("order must be confirmed on return: %s", g.Raw)
	}
}

// namedPay is the deferred test provider under another name (e.g. "paystack").
type namedPay struct {
	*deferredPay
	name string
}

func (n *namedPay) Name() string { return n.name }

// Convenience fees by payment method: added on top of the amount, shown in the
// public catalogue, and never taken from the electricity the DisCo vends.
func TestPaymentMethodFees(t *testing.T) {
	e := newEnv(t, false)
	agg := &fakeAggregator{settled: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(agg.handler))
	defer srv.Close()
	e.svc.Vending["vtpass"] = vending.NewVTpass(srv.URL+"/sandbox", "AK", "PK", "SK", "08011111111")
	p := e.provision("zen", nil, nil)
	e.must(e.do("PATCH", "/v1/admin/settings", p.Admin, map[string]any{"fees": map[string]int64{"checkout_kobo": 15000, "wallet_kobo": 5000}}), 200)
	e.must(e.do("PATCH", "/v1/admin/settings", p.Admin, map[string]any{"fees": map[string]int64{"checkout_kobo": 200000}}), 400)

	d := e.must(e.do("GET", "/v1/public/discos", "", nil, tenantHdr("zen")), 200).list("discos")[0].(map[string]any)
	if d["checkout_fee_kobo"] != float64(15000) || d["wallet_fee_kobo"] != float64(5000) {
		t.Fatalf("catalogue fees: %v", d)
	}

	c := e.customer(p.Slug, "fees@example.com")
	e.fund(c, 1000000)
	o := e.must(e.do("POST", "/v1/orders", c.Token, map[string]any{"provider_code": "IKEDC", "meter_number": "1111111111111",
		"amount_kobo": 500000, "payment_method": "wallet"}, idem(uuid.NewString())), 201)
	if o.str("status") != "vended" || o.num("amount_kobo") != 505000 || o.num("fee_kobo") != 5000 {
		t.Fatalf("wallet order with fee: %s", o.Raw)
	}
	if got := e.walletKobo(c); got != 1000000-505000 {
		t.Fatalf("wallet pays amount + fee: %v", got)
	}
	if len(agg.amounts) != 1 || agg.amounts[0] != 5000 {
		t.Fatalf("the DisCo gets the full ₦5,000: %v", agg.amounts)
	}

	co := e.must(e.do("POST", "/v1/orders", c.Token, map[string]any{"provider_code": "IKEDC", "meter_number": "1111111111111",
		"amount_kobo": 500000, "payment_method": "checkout"}, idem(uuid.NewString())), 201)
	if co.num("amount_kobo") != 515000 || co.num("fee_kobo") != 15000 {
		t.Fatalf("checkout order with fee: %s", co.Raw)
	}
}

func TestSubscribe(t *testing.T) {
	e := newEnv(t, false)
	e.provision("zen", nil, nil)
	sub := func(email string) resp {
		return e.do("POST", "/v1/public/subscribe", "", map[string]string{"email": email}, tenantHdr("zen"))
	}
	e.must(sub("Reader@Example.com"), 204)
	e.must(sub("reader@example.com"), 204) // twice is fine
	e.must(sub("not-an-email"), 400)
	var n int
	if err := e.svc.DB.System(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM subscribers WHERE email='reader@example.com'`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("one subscriber row: %d %v", n, err)
	}
}

type fakeMessenger struct {
	mu   sync.Mutex
	sent []messaging.Message
	fail error
}

func (f *fakeMessenger) Send(_ context.Context, m messaging.Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	f.sent = append(f.sent, m)
	return fmt.Sprintf("msg-%d", len(f.sent)), nil
}

// Tokens go out by email and SMS once issued, never before, and only on the
// channels that are configured.
func TestTokenByEmailAndSMS(t *testing.T) {
	e := newEnv(t, false)
	agg := &fakeAggregator{settled: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(agg.handler))
	defer srv.Close()
	e.svc.Vending["vtpass"] = vending.NewVTpass(srv.URL+"/sandbox", "AK", "PK", "SK", "08011111111")
	mail, sms := &fakeMessenger{}, &fakeMessenger{}
	e.svc.Messengers = map[string]messaging.Sender{messaging.Email: mail, messaging.SMS: sms}
	p := e.provision("zen", nil, nil)

	ch := e.must(e.do("GET", "/v1/public/discos", "", nil, tenantHdr("zen")), 200).list("delivery_channels")
	if len(ch) != 2 || ch[0] != "email" || ch[1] != "sms" {
		t.Fatalf("delivery channels: %v", ch)
	}

	c := e.customer(p.Slug, "receipts@example.com")
	e.fund(c, 1000000)
	order := func(deliver map[string]any) resp {
		return e.do("POST", "/v1/orders", c.Token, map[string]any{"provider_code": "IKEDC", "meter_number": "1111111111111",
			"amount_kobo": 500000, "payment_method": "wallet", "deliver_to": deliver}, idem(uuid.NewString()))
	}
	e.must(order(map[string]any{"sms": true, "phone": "12345"}), 400)

	o := e.must(order(map[string]any{"email": true, "sms": true, "whatsapp": true, "phone": "0803 123 4567"}), 201)
	if err := e.svc.DeliverTokenMessages(context.Background()); err != nil {
		t.Fatal(err)
	}
	tok := strings.ReplaceAll(o.str("token"), " ", "")
	if len(mail.sent) != 1 || mail.sent[0].To != "receipts@example.com" || !strings.Contains(strings.ReplaceAll(mail.sent[0].Text, "-", ""), tok) {
		t.Fatalf("email receipt: %+v (token %s)", mail.sent, tok)
	}
	if len(sms.sent) != 1 || sms.sent[0].To != "2348031234567" || !strings.Contains(strings.ReplaceAll(sms.sent[0].Text, "-", ""), tok) {
		t.Fatalf("sms receipt: %+v", sms.sent)
	}
	// One SMS segment: plain GSM characters (no ₦) and at most 160 of them.
	if txt := sms.sent[0].Text; len(txt) > 160 || strings.ContainsRune(txt, '₦') {
		t.Fatalf("sms must fit one segment: %d chars: %s", len(txt), txt)
	}
	// Sent once only; WhatsApp is not configured, so it is skipped.
	if err := e.svc.DeliverTokenMessages(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mail.sent) != 1 || len(sms.sent) != 1 {
		t.Fatalf("sent twice: %d %d", len(mail.sent), len(sms.sent))
	}
	var statuses []string
	_ = e.svc.DB.System(context.Background(), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT channel||':'||status FROM outbound_messages ORDER BY channel`)
		if err != nil {
			return err
		}
		statuses, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if strings.Join(statuses, ",") != "email:sent,sms:sent,whatsapp:skipped" {
		t.Fatalf("message statuses: %v", statuses)
	}
	// The number given is saved to the profile for next time.
	if me := e.must(e.do("GET", "/v1/me", c.Token, nil), 200); me.str("user", "phone") == "" {
		t.Fatalf("phone not saved: %s", me.Raw)
	}
}
