package app

import "testing"

func TestComputeQuote(t *testing.T) {
	base := Tariff{ProviderCode: "AEDC", RateKoboPerKwh: 20000, MinPurchaseKobo: 10000, MaxPurchaseKobo: 50000000}
	q, err := computeQuote(base, 500000, 0)
	if err != nil || q.energyWh != 25000 || q.VatKobo != 0 || q.FeeKobo != 0 {
		t.Fatalf("₦5,000 @ ₦200/kWh: %+v %v", q, err)
	}
	// 7.5% VAT is inclusive and a ₦100 fee comes off the top.
	vat := base
	vat.VatBps, vat.ServiceFeeKobo = 750, 10000
	q, _ = computeQuote(vat, 1085000, 0)
	if q.FeeKobo != 10000 || q.VatKobo != 75000 || q.EnergyValueKobo != 1000000 || q.energyWh != 50000 {
		t.Fatalf("vat/fee split: %+v", q)
	}
	if q.FeeKobo+q.VatKobo+q.EnergyValueKobo != q.AmountKobo {
		t.Fatal("components must sum to the amount paid")
	}
	for _, amt := range []int64{0, 9999, 50000001} {
		if _, err := computeQuote(base, amt, 0); err == nil {
			t.Fatalf("amount %d must be rejected", amt)
		}
	}
	// A payment-method fee is added on top: the energy is bought with the
	// full amount and the customer pays amount + fee.
	q, _ = computeQuote(base, 500000, 15000)
	if q.AmountKobo != 515000 || q.FeeKobo != 15000 || q.EnergyValueKobo != 500000 || q.energyWh != 25000 {
		t.Fatalf("surcharge on top: %+v", q)
	}
	if q.FeeKobo+q.VatKobo+q.EnergyValueKobo != q.AmountKobo {
		t.Fatal("components must sum to the amount paid, surcharge included")
	}
	fee := base
	fee.ServiceFeeKobo = 20000
	if _, err := computeQuote(fee, 15000, 0); err == nil {
		t.Fatal("amount below fee must be rejected")
	}
}

func TestVendStateMachine(t *testing.T) {
	legal := [][2]string{{StPaymentPending, StVending}, {StVending, StVended}, {StVended, StDelivered},
		{StVending, StVendFailed}, {StVendFailed, StRefunded}, {StVending, StNeedsReview}, {StNeedsReview, StRefunded}}
	for _, tr := range legal {
		if !canTransition(tr[0], tr[1]) {
			t.Errorf("%s -> %s should be legal", tr[0], tr[1])
		}
	}
	illegal := [][2]string{{StRefunded, StVending}, {StDelivered, StRefunded}, {StVended, StRefunded},
		{StPaymentPending, StVended}, {StPaymentFailed, StVending}, {StVended, StVending}}
	for _, tr := range illegal {
		if canTransition(tr[0], tr[1]) {
			t.Errorf("%s -> %s must be illegal (would allow double credit or refund after delivery)", tr[0], tr[1])
		}
	}
}

func TestBrandingValidation(t *testing.T) {
	bad := []BrandingInput{
		{Theme: map[string]string{"primary": "red"}},
		{Theme: map[string]string{"evil": "#000000"}},
		{LogoURL: ptr("javascript:alert(1)")},
		{LogoURL: ptr("http://insecure.example/logo.png")},
		{CustomDomain: ptr("not a domain")},
		{Currency: ptr("naira")},
	}
	for i, b := range bad {
		if validateBranding(b) == nil {
			t.Errorf("case %d should be rejected", i)
		}
	}
	if err := validateBranding(BrandingInput{Theme: map[string]string{"primary": "#1F7AE0"}, LogoURL: ptr("https://cdn.pce.example/logo.svg"),
		CustomDomain: ptr("power.pce.example"), Currency: ptr("NGN")}); err != nil {
		t.Fatal(err)
	}
}
