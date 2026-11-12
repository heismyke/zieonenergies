package vending

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeVTpass mimics the documented VTpass responses.
func fakeVTpass(t *testing.T, pay, requery func(map[string]any) string) *VTpass {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("api-key") != "AK" || r.Header.Get("secret-key") != "SK" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch strings.TrimPrefix(r.URL.Path, "/sandbox") {
		case "/api/pay":
			_, _ = w.Write([]byte(pay(body)))
		case "/api/requery":
			_, _ = w.Write([]byte(requery(body)))
		case "/api/merchant-verify":
			if body["billersCode"] == "1111111111111" && body["serviceID"] == "ikeja-electric" {
				_, _ = w.Write([]byte(`{"code":"000","content":{"Customer_Name":"TESTMETER1","Address":"ABULE EGBA BU ABULE","Meter_Type":"PREPAID","Can_Vend":"yes","WrongBillersCode":false}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"000","content":{"error":"This meter is not correct or is not a valid Ikeja Electric meter.","WrongBillersCode":true}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return NewVTpass(srv.URL+"/sandbox", "AK", "PK", "SK", "08011111111")
}

const delivered = `{"code":"000","content":{"transactions":{"status":"delivered","transactionId":"1741"}},
 "response_description":"TRANSACTION SUCCESSFUL","purchased_code":"Token : 2636 2054 4059 8275 7802","token":"Token : 26362054405982757802",
 "units":"79.9 kWh","tariff":"R2 SINGLE PHASE RESIDENTIAL"}`

func req() Request {
	return Request{Reference: "5f0c6a52-1b7e-4c2b-9a4f-0c8d2e9a1b11", MeterNumber: "1111111111111", ProviderCode: "IKEDC",
		MeterType: "prepaid", AmountKobo: 200000, Phone: "+234 806 123 4567", CreatedAt: time.Date(2026, 11, 12, 21, 59, 30, 0, time.UTC)}
}

func TestVTpassRequestID(t *testing.T) {
	id := RequestID(req().CreatedAt, req().Reference)
	// 21:59 UTC is 22:59 in Lagos; first 12 characters are Lagos YYYYMMDDHHmm.
	if !strings.HasPrefix(id, "202611122259") || strings.Contains(id, "-") || len(id) != 12+32 {
		t.Fatalf("bad request id %q", id)
	}
	if RequestID(req().CreatedAt, req().Reference) != id {
		t.Fatal("request id must be deterministic for retries and requery")
	}
}

func TestVTpassVendOutcomes(t *testing.T) {
	ctx := context.Background()
	var sent map[string]any
	v := fakeVTpass(t, func(b map[string]any) string { sent = b; return delivered }, nil)
	res, err := v.Vend(ctx, req())
	if err != nil || res.Token != "26362054405982757802" || res.EnergyWh != 79900 || res.ProviderReference != "vtpass:1741" {
		t.Fatalf("success: %+v %v", res, err)
	}
	if sent["serviceID"] != "ikeja-electric" || sent["variation_code"] != "prepaid" || sent["amount"] != 2000.0 || sent["phone"] != "08061234567" {
		t.Fatalf("pay body: %v", sent)
	}
	if !v.Sandbox() {
		t.Fatal("sandbox base URL must report Sandbox()")
	}

	cases := []struct {
		name     string
		body     string
		rejected bool
	}{
		{"pending", `{"code":"099","content":{"transactions":{"status":"pending"}}}`, false},
		{"initiated", `{"code":"000","content":{"transactions":{"status":"initiated"}}}`, false},
		{"system error is not a refund", `{"code":"083","response_description":"SYSTEM ERROR"}`, false},
		{"processing is not a refund", `{"code":"089","response_description":"REQUEST IS PROCESSING"}`, false},
		{"failed", `{"code":"016","content":{"transactions":{"status":"failed"}},"response_description":"TRANSACTION FAILED"}`, true},
		{"low wallet", `{"code":"018","response_description":"LOW WALLET BALANCE"}`, true},
		{"not processed", `{"code":"091","response_description":"TRANSACTION NOT PROCESSED"}`, true},
		{"reversed", `{"code":"000","content":{"transactions":{"status":"reversed"}}}`, true},
		{"delivered without token", `{"code":"000","content":{"transactions":{"status":"delivered"}}}`, false},
	}
	for _, c := range cases {
		v := fakeVTpass(t, func(map[string]any) string { return c.body }, nil)
		_, err := v.Vend(ctx, req())
		if err == nil {
			t.Fatalf("%s: expected an error", c.name)
		}
		if errors.Is(err, ErrRejected) != c.rejected {
			t.Fatalf("%s: rejected=%v want %v (%v)", c.name, errors.Is(err, ErrRejected), c.rejected, err)
		}
	}

	// Postpaid succeeds without a token.
	pp := req()
	pp.MeterType = "postpaid"
	v = fakeVTpass(t, func(map[string]any) string {
		return `{"code":"000","content":{"transactions":{"status":"delivered","transactionId":"9"}}}`
	}, nil)
	if res, err := v.Vend(ctx, pp); err != nil || res.Token != "" {
		t.Fatalf("postpaid: %+v %v", res, err)
	}

	// Unknown DisCo is refused before calling VTpass.
	bad := req()
	bad.ProviderCode = "ZIEON"
	if _, err := v.Vend(ctx, bad); !errors.Is(err, ErrRejected) {
		t.Fatalf("unsupported disco must be rejected, got %v", err)
	}
}

func TestVTpassDuplicateRequestIDAndRequery(t *testing.T) {
	ctx := context.Background()
	var requeried string
	v := fakeVTpass(t,
		func(map[string]any) string { return `{"code":"014","response_description":"REQUEST ID ALREADY EXIST"}` },
		func(b map[string]any) string { requeried, _ = b["request_id"].(string); return delivered })
	res, err := v.Vend(ctx, req())
	if err != nil || res.Token == "" || requeried != RequestID(req().CreatedAt, req().Reference) {
		t.Fatalf("014 must resolve via requery with the same id: %+v %v %q", res, err, requeried)
	}
	v = fakeVTpass(t, nil, func(map[string]any) string { return `{"code":"015","response_description":"INVALID REQUEST ID"}` })
	if _, err := v.Lookup(ctx, req()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("015 on requery means VTpass never saw it, got %v", err)
	}
}

func TestVTpassValidateMeter(t *testing.T) {
	v := fakeVTpass(t, nil, nil)
	info, err := v.ValidateMeter(context.Background(), "IKEDC", "1111111111111", "prepaid")
	if err != nil || info.CustomerName != "TESTMETER1" || !info.CanVend {
		t.Fatalf("verify: %+v %v", info, err)
	}
	if _, err := v.ValidateMeter(context.Background(), "IKEDC", "1234567890", "prepaid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong meter must be not found, got %v", err)
	}
	bad := NewVTpass(v.BaseURL, "AK", "PK", "WRONG", "")
	if _, err := bad.ValidateMeter(context.Background(), "IKEDC", "1111111111111", "prepaid"); err == nil {
		t.Fatal("bad credentials must error")
	}
}
