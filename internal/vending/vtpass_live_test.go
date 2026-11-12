package vending

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Contract test against the real VTpass sandbox. Skipped unless keys are set:
//
//	set -a; . ./.env.vtpass; set +a; VTPASS_LIVE_TEST=1 go test -run TestVTpassSandboxContract -v ./internal/vending/
//
// Uses VTpass's documented sandbox test meters, which move only test money.
func TestVTpassSandboxContract(t *testing.T) {
	if os.Getenv("VTPASS_LIVE_TEST") != "1" {
		t.Skip("set VTPASS_LIVE_TEST=1 and VTPASS_* keys to run against the VTpass sandbox")
	}
	v := NewVTpass(os.Getenv("VTPASS_BASE_URL"), os.Getenv("VTPASS_API_KEY"), os.Getenv("VTPASS_PUBLIC_KEY"),
		os.Getenv("VTPASS_SECRET_KEY"), "08011111111")
	if !v.Sandbox() {
		t.Fatal("refusing to run the contract test against live VTpass")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	info, err := v.ValidateMeter(ctx, "IKEDC", "1111111111111", "prepaid")
	if err != nil || info.CustomerName == "" || !info.CanVend {
		t.Fatalf("verify test meter: %+v %v", info, err)
	}
	t.Logf("verify: %s, %s", info.CustomerName, info.Address)

	newReq := func(meter string) Request {
		return Request{Reference: uuid.NewString(), MeterNumber: meter, ProviderCode: "IKEDC", MeterType: "prepaid",
			AmountKobo: 200000, Phone: "08011111111", CreatedAt: time.Now()}
	}

	// Success: real response shape, token and units parsed.
	ok := newReq("1111111111111")
	res, err := v.Vend(ctx, ok)
	if err != nil || len(res.Token) != 20 || res.EnergyWh <= 0 {
		t.Fatalf("vend success: %+v %v", res, err)
	}
	t.Logf("vend: token %s… units %.1f kWh ref %s (%s)", res.Token[:4], float64(res.EnergyWh)/1000, res.ProviderReference, res.Receipt)

	// Requery of the same request returns the same token (reconciliation path).
	again, err := v.Lookup(ctx, ok)
	if err != nil || again.Token != res.Token {
		t.Fatalf("requery: %+v %v", again, err)
	}
	// Re-sending the same request id must not create a second purchase.
	dup, err := v.Vend(ctx, ok)
	if err != nil || dup.Token != res.Token {
		t.Fatalf("duplicate request id must resolve to the original purchase: %+v %v", dup, err)
	}

	// A request VTpass never saw is "not found" (safe to retry or refund).
	if _, err := v.Lookup(ctx, newReq("1111111111111")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown request id: want ErrNotFound, got %v", err)
	}

	// Pending test meter: outcome unknown, must NOT be treated as a rejection.
	if _, err := v.Vend(ctx, newReq("201000000000")); err == nil || errors.Is(err, ErrRejected) {
		t.Fatalf("pending meter must be an unknown outcome, got %v", err)
	} else {
		t.Logf("pending: %v", err)
	}

	// Any other number fails at the DisCo: definitive, refundable.
	if _, err := v.Vend(ctx, newReq("1234567890123")); !errors.Is(err, ErrRejected) {
		t.Fatalf("failing meter must be a rejection, got %v", err)
	} else {
		t.Logf("failed: %v", err)
	}
}
