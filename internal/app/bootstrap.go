package app

import (
	"context"
	"fmt"

	"github.com/heismyke/zieonenergies/internal/secure"
)

// BuyOnlyFeatures is the production product: buy DisCo electricity with a
// guest checkout, like BuyPower. Dashboard features stay off until launched.
var BuyOnlyFeatures = map[string]bool{
	"guest_checkout": true, "card_payments": true, "external_meters": true,
	"wallet": false, "bank_transfer": false, "spark_share": false, "auto_topup": false, "benefits": false,
	"subscriptions": false, "support_chat": false, "properties": false, "energy_exchange": false, "crypto_payments": false,
}

type BootstrapInput struct {
	AdminEmail     string
	Origins        []string
	ServiceFeeKobo int64
	MinKobo        int64
}

// Bootstrap creates the production platform tenant once. Re-running is a no-op.
func (s *Service) Bootstrap(ctx context.Context, in BootstrapInput) ([]string, error) {
	if existing, err := s.TenantBySlug(ctx, "zenerlink"); err == nil {
		return []string{fmt.Sprintf("tenant zenerlink already exists (%s); nothing changed", existing.ID)}, nil
	}
	if in.MinKobo == 0 {
		in.MinKobo = 50000 // ₦500: below this most DisCos refuse to vend
	}
	var tariffs []TariffInput
	for _, d := range NigerianDiscos {
		if d[0] == "ZIEON" {
			continue
		}
		// rate is an estimate for quotes only; the DisCo's returned units are authoritative.
		tariffs = append(tariffs, TariffInput{ProviderCode: d[0], ProviderName: d[1], RateKoboPerKwh: 20000,
			ServiceFeeKobo: in.ServiceFeeKobo, MinPurchaseKobo: in.MinKobo, MaxPurchaseKobo: 50000000})
	}
	pw := secure.RandomToken(15)
	name := "Zenerlink"
	in2 := ProvisionInput{Slug: "zenerlink", Name: "Zieon Energies", IsPlatform: true, Features: BuyOnlyFeatures,
		AllowedOrigins: in.Origins, Tariffs: tariffs, Branding: BrandingInput{DisplayName: &name}}
	in2.Admin.Email, in2.Admin.Name, in2.Admin.Password = in.AdminEmail, "Zieon Platform Admin", pw
	t, err := s.ProvisionTenant(ctx, in2)
	if err != nil {
		return nil, err
	}
	out := []string{fmt.Sprintf("tenant zenerlink %s created (buy-only)", t.ID),
		fmt.Sprintf("platform admin %s / %s", in.AdminEmail, pw)}
	if s.LiveRouteAvailable() {
		if _, err := s.SetTenantStatus(ctx, t.ID, "", "live"); err != nil {
			return out, err
		}
		out = append(out, "vending_mode live (live vending route configured)")
	} else {
		out = append(out, "WARNING: no live vending route; tenant left in sandbox mode")
	}
	return out, nil
}
