package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/heismyke/zieonenergies/internal/vending"
)

// fakeAggregator is a stateful stand-in for VTpass using its sandbox test
// meters: 1111111111111 succeeds, 201000000000 goes pending, others fail.
type fakeAggregator struct {
	mu      sync.Mutex
	settled map[string]string // request_id -> response body for requery
	pays    int
}

func (f *fakeAggregator) handler(w http.ResponseWriter, r *http.Request) {
	var b map[string]any
	_ = json.NewDecoder(r.Body).Decode(&b)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch strings.TrimPrefix(r.URL.Path, "/sandbox") {
	case "/api/merchant-verify":
		if n := b["billersCode"]; n == "1111111111111" || n == "2010000000000" || n == "3000000000000" {
			_, _ = w.Write([]byte(`{"code":"000","content":{"Customer_Name":"TESTMETER1","Address":"ABULE EGBA","Meter_Type":"PREPAID","Can_Vend":"yes"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"000","content":{"error":"invalid meter","WrongBillersCode":true}}`))
	case "/api/pay":
		id := b["request_id"].(string)
		f.pays++
		tok := fmt.Sprintf("1234 5678 9012 3456 %04d", f.pays)
		ok := `{"code":"000","content":{"transactions":{"status":"delivered","transactionId":"T` + id[12:20] + `"}},
			"purchased_code":"Token : ` + tok + `","units":"9.5 kWh"}`
		switch b["billersCode"] {
		case "1111111111111":
			f.settled[id] = ok
			_, _ = w.Write([]byte(ok))
		case "2010000000000":
			f.settled[id] = ok // completes later; requery will see it
			_, _ = w.Write([]byte(`{"code":"099","content":{"transactions":{"status":"pending"}}}`))
		default:
			_, _ = w.Write([]byte(`{"code":"016","content":{"transactions":{"status":"failed"}},"response_description":"TRANSACTION FAILED"}`))
		}
	case "/api/requery":
		if body, ok := f.settled[b["request_id"].(string)]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{"code":"015","response_description":"INVALID REQUEST ID"}`))
	}
}

func TestVTpassAggregatorFlow(t *testing.T) {
	e := newEnv(t, false)
	agg := &fakeAggregator{settled: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(agg.handler))
	defer srv.Close()
	e.svc.Vending["vtpass"] = vending.NewVTpass(srv.URL+"/sandbox", "AK", "PK", "SK", "08011111111")

	p := e.provision("zen", nil, nil)
	c := e.customer(p.Slug, "c@zen.test")
	e.must(e.do("PATCH", "/v1/me", c.Token, map[string]string{"phone": "+2348061234567"}), 200)

	// The DisCo lookup now returns the real customer.
	v := e.must(e.do("POST", "/v1/meters/validate", c.Token, map[string]string{"provider_code": "IKEDC", "meter_number": "1111111111111"}), 200)
	if v.get("verified") != true || v.str("customer_name") != "TESTMETER1" || v.str("source") != "disco" {
		t.Fatalf("validate: %s", v.Raw)
	}
	e.must(e.do("POST", "/v1/meters/validate", c.Token, map[string]string{"provider_code": "IKEDC", "meter_number": "9999999999"}), 404)

	link := func(number string) meter {
		r := e.must(e.do("POST", "/v1/meters", c.Token, map[string]string{"provider_code": "IKEDC", "meter_number": number, "label": "Home"}), 201)
		return meter{ID: r.str("id"), Number: number}
	}
	ok, pending, failing := link("1111111111111"), link("2010000000000"), link("3000000000000")
	e.fund(c, 1000000)

	r := e.must(e.buy(c, ok, 200000), 201)
	if r.str("status") != "vended" || r.str("token") != "12345678901234560001" || r.num("energy_kwh") != 9.5 || r.get("sandbox") != true {
		t.Fatalf("aggregator purchase: %s", r.Raw)
	}

	// DisCo rejected: refunded in full, nothing to deliver.
	f := e.must(e.buy(c, failing, 100000), 201)
	if f.str("status") != "refunded" || f.get("token") != nil {
		t.Fatalf("failed vend must refund: %s", f.Raw)
	}

	// Pending at the DisCo: stays vending, then reconciliation's requery completes it.
	pd := e.must(e.buy(c, pending, 100000), 201)
	if pd.str("status") != "vending" {
		t.Fatalf("pending vend must wait: %s", pd.Raw)
	}
	_, _ = e.svc.DB.Pool.Exec(context.Background(), `UPDATE vend_transactions SET updated_at = now() - interval '5 minutes' WHERE id=$1`, pd.str("id"))
	paysBefore := agg.pays
	if err := e.svc.ReconcileVends(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g := e.must(e.do("GET", "/v1/purchases/"+pd.str("id"), c.Token, nil), 200); g.str("status") != "vended" || len(g.str("token")) != 20 {
		t.Fatalf("reconciliation via requery: %s", g.Raw)
	}
	if agg.pays != paysBefore {
		t.Fatal("reconciliation must requery, never pay again")
	}
	// ₦10,000 funded - ₦2,000 - ₦1,000 (pending, now vended); the failed ₦1,000 was refunded.
	if e.walletKobo(c) != 700000 {
		t.Fatalf("wallet %v", e.walletKobo(c))
	}

	// A sandbox aggregator can never serve a live tenant.
	e.must(e.do("PATCH", "/v1/platform/tenants/"+p.ID, e.platform, map[string]string{"vending_mode": "live"}), 409)
	// A live aggregator is never used for a sandbox tenant (no real money in test mode).
	e.svc.Vending["vtpass"] = vending.NewVTpass(srv.URL, "AK", "PK", "SK", "")
	paysBefore = agg.pays
	sb := e.must(e.buy(c, ok, 100000), 201)
	if agg.pays != paysBefore || !strings.HasPrefix(sb.str("token"), "99") {
		t.Fatalf("sandbox tenant must use the internal simulator when the aggregator is live: %s", sb.Raw)
	}
}
