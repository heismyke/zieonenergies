package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/secure"
	"github.com/heismyke/zieonenergies/internal/vending"
)

// Guest checkout: the production "buy electricity" flow. No account: the
// buyer picks a DisCo, verifies the meter, pays through the payment provider
// and reads the token back with a random order key. Internally each order is
// an ordinary vend transaction owned by a passwordless customer record keyed
// by email, so reconciliation, refunds and reporting all apply unchanged.

type Disco struct {
	ProviderCode    string `json:"provider_code"`
	ProviderName    string `json:"provider_name"`
	LogoURL         string `json:"logo_url"`
	MinPurchaseKobo int64  `json:"min_purchase_kobo"`
	MaxPurchaseKobo int64  `json:"max_purchase_kobo"`
	ServiceFeeKobo  int64  `json:"service_fee_kobo"`
	// Convenience fee added on top of the amount, by payment method.
	CheckoutFeeKobo int64 `json:"checkout_fee_kobo"`
	WalletFeeKobo   int64 `json:"wallet_fee_kobo"`
	// For the "≈ kWh" estimate shown while typing; the DisCo's units are final.
	RateKoboPerKwh int64 `json:"rate_kobo_per_kwh"`
	VatBps         int   `json:"vat_bps"`
}

func (s *Service) PublicDiscos(ctx context.Context, t Tenant) ([]Disco, error) {
	if !t.Enabled("guest_checkout") {
		return nil, ErrFeatureOff
	}
	ts, err := s.Tariffs(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	out := []Disco{}
	for _, tr := range ts {
		if tr.ProviderCode == "ZIEON" {
			continue
		}
		logo := s.withDiscoLimits(ctx, &tr)
		out = append(out, Disco{ProviderCode: tr.ProviderCode, ProviderName: tr.ProviderName, LogoURL: logo,
			MinPurchaseKobo: tr.MinPurchaseKobo, MaxPurchaseKobo: tr.MaxPurchaseKobo, ServiceFeeKobo: tr.ServiceFeeKobo,
			CheckoutFeeKobo: t.Fees.CheckoutKobo, WalletFeeKobo: t.Fees.WalletKobo,
			RateKoboPerKwh: tr.RateKoboPerKwh, VatBps: tr.VatBps})
	}
	return out, nil
}

type PublicMeter struct {
	Verified     bool   `json:"verified"`
	CustomerName string `json:"customer_name"`
	Address      string `json:"address"`
	MeterType    string `json:"meter_type"`
	Message      string `json:"message,omitempty"`
}

// VerifyPublicMeter checks a DisCo meter before the buyer pays.
func (s *Service) VerifyPublicMeter(ctx context.Context, t Tenant, providerCode, meterNumber, meterType string) (PublicMeter, error) {
	if !t.Enabled("guest_checkout") {
		return PublicMeter{}, ErrFeatureOff
	}
	return s.verifyMeter(ctx, providerCode, meterNumber, meterType)
}

func (s *Service) verifyMeter(ctx context.Context, providerCode, meterNumber, meterType string) (PublicMeter, error) {
	providerCode = strings.ToUpper(strings.TrimSpace(providerCode))
	meterNumber = strings.TrimSpace(meterNumber)
	if providerCode == "ZIEON" || !meterNumberRe.MatchString(meterNumber) {
		return PublicMeter{}, errf(http.StatusBadRequest, "invalid_meter_number", "enter a valid meter number for your DisCo")
	}
	val, ok := s.Vending["vtpass"].(vending.Validator)
	if !ok {
		return PublicMeter{}, errf(http.StatusServiceUnavailable, "lookup_unavailable", "meter lookup is not available right now")
	}
	info, err := val.ValidateMeter(ctx, providerCode, meterNumber, meterType)
	switch {
	case errors.Is(err, vending.ErrNotFound):
		return PublicMeter{}, errf(http.StatusNotFound, "meter_not_found", "the DisCo did not recognise this meter number")
	case errors.Is(err, vending.ErrUnsupported):
		return PublicMeter{}, errf(http.StatusBadRequest, "unknown_provider", "this DisCo is not supported")
	case err != nil:
		s.Log.Warn("public meter lookup failed", "provider", providerCode, "err", err)
		return PublicMeter{}, errf(http.StatusServiceUnavailable, "lookup_unavailable", "the DisCo lookup is temporarily unavailable; please try again")
	}
	out := PublicMeter{Verified: true, CustomerName: info.CustomerName, Address: info.Address, MeterType: info.MeterType}
	if !info.CanVend {
		out.Message = "The DisCo reports this meter cannot be vended at the moment."
	}
	return out, nil
}

type GuestOrderInput struct {
	ProviderCode string `json:"provider_code"`
	MeterNumber  string `json:"meter_number"`
	MeterType    string `json:"meter_type"`
	AmountKobo   int64  `json:"amount_kobo"`
	Email        string `json:"email"`
	Phone        string `json:"phone"`
	ReturnURL    string `json:"-"` // set by the handler from the approved Origin
}

type GuestOrder struct {
	ID            uuid.UUID `json:"id"`
	Key           string    `json:"key,omitempty"` // returned once, at creation
	Status        string    `json:"status"`
	ProviderCode  string    `json:"provider_code"`
	ProviderName  string    `json:"provider_name"`
	MeterNumber   string    `json:"meter_number"`
	MeterType     string    `json:"meter_type"`
	CustomerName  string    `json:"customer_name"`
	AmountKobo    int64     `json:"amount_kobo"`
	FeeKobo       int64     `json:"fee_kobo"`
	EnergyKwh     float64   `json:"energy_kwh"`
	Token         *string   `json:"token"`
	CheckoutURL   string    `json:"checkout_url,omitempty"`
	PaymentStatus string    `json:"payment_status"`
	Sandbox       bool      `json:"sandbox"`
	Message       string    `json:"message,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

var phoneRe = regexp.MustCompile(`^\+?[0-9]{10,14}$`)

// CreateGuestOrder verifies the meter with the DisCo first (no payment is
// taken for a wrong meter), then starts a checkout for the full amount.
// Off unless the tenant enables guest_orders: by default buyers sign up first.
func (s *Service) CreateGuestOrder(ctx context.Context, t Tenant, in GuestOrderInput) (GuestOrder, error) {
	if !t.Enabled("guest_checkout") || !t.Enabled("guest_orders") {
		return GuestOrder{}, ErrFeatureOff
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.Phone = strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(in.Phone))
	if err := validateEmail(in.Email); err != nil {
		return GuestOrder{}, err
	}
	if !phoneRe.MatchString(in.Phone) {
		return GuestOrder{}, errf(http.StatusBadRequest, "invalid_phone", "enter a valid phone number")
	}
	m, err := s.checkOrder(ctx, t, &in.ProviderCode, in.MeterNumber, &in.MeterType, in.AmountKobo)
	if err != nil {
		return GuestOrder{}, err
	}

	key := "zgo_" + secure.RandomToken(24)
	var p auth.Principal
	var meterID uuid.UUID
	err = s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		// Passwordless customer record keyed by email (reused across orders).
		var userID uuid.UUID
		err := tx.QueryRow(ctx, `INSERT INTO users (tenant_id, email, phone, name, role) VALUES ($1,$2,$3,$4,'customer')
			ON CONFLICT (tenant_id, lower(email)) DO UPDATE SET phone=CASE WHEN users.phone='' THEN EXCLUDED.phone ELSE users.phone END
			RETURNING id`, t.ID, in.Email, in.Phone, m.CustomerName).Scan(&userID)
		if err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1`, userID).Scan(&status); err != nil {
			return err
		}
		if status != "active" {
			return errf(http.StatusForbidden, "account_disabled", "purchases are not available for this email")
		}
		meterID, err = linkExternalMeter(ctx, tx, t.ID, userID, in.ProviderCode, in.MeterNumber, in.MeterType, m)
		p = auth.Principal{Kind: "user", UserID: userID, TenantID: t.ID, Role: auth.RoleCustomer}
		return err
	})
	if err != nil {
		return GuestOrder{}, err
	}

	ret := in.ReturnURL // order id and key are appended once the id exists
	pu, err := s.purchaseWithHook(ctx, p, t, PurchaseInput{MeterID: meterID, AmountKobo: in.AmountKobo, PaymentMethod: "checkout"},
		func(ctx context.Context, tx pgx.Tx, vendID uuid.UUID) (string, error) {
			if _, err := tx.Exec(ctx, `INSERT INTO guest_orders (vend_transaction_id, tenant_id, key_hash, email, phone) VALUES ($1,$2,$3,$4,$5)`,
				vendID, t.ID, secure.HashSecret(key), in.Email, in.Phone); err != nil {
				return "", err
			}
			if ret == "" {
				return "", nil
			}
			return ret + "/order/" + vendID.String() + "?key=" + url.QueryEscape(key), nil
		})
	if err != nil {
		return GuestOrder{}, err
	}
	out, err := s.GuestOrder(ctx, t, pu.ID, key)
	if err != nil {
		return GuestOrder{}, err
	}
	out.Key = key
	return out, nil
}

// OrderInput is a signed-in buyer's purchase: contact details come from the account.
type OrderInput struct {
	ProviderCode string `json:"provider_code"`
	MeterNumber  string `json:"meter_number"`
	MeterType    string `json:"meter_type"`
	AmountKobo   int64  `json:"amount_kobo"`
	// "checkout" (card, transfer or USSD; the default) or "wallet".
	PaymentMethod string `json:"payment_method"`
	ReturnURL     string `json:"-"`
}

// CreateOrder is the account version of the guest order: same checks, the
// meter is saved to the account, and the order shows in the buyer's history.
func (s *Service) CreateOrder(ctx context.Context, p auth.Principal, t Tenant, in OrderInput) (GuestOrder, error) {
	if !t.Enabled("external_meters") {
		return GuestOrder{}, ErrFeatureOff
	}
	if in.PaymentMethod == "" {
		in.PaymentMethod = "checkout"
	}
	if in.PaymentMethod != "checkout" && in.PaymentMethod != "wallet" {
		return GuestOrder{}, errf(http.StatusBadRequest, "invalid_payment_method", "payment_method must be checkout or wallet")
	}
	if err := s.checkMethod(t, in.PaymentMethod); err != nil {
		return GuestOrder{}, err
	}
	m, err := s.checkOrder(ctx, t, &in.ProviderCode, in.MeterNumber, &in.MeterType, in.AmountKobo)
	if err != nil {
		return GuestOrder{}, err
	}
	var meterID uuid.UUID
	if err := s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		var err error
		meterID, err = linkExternalMeter(ctx, tx, t.ID, p.UserID, in.ProviderCode, in.MeterNumber, in.MeterType, m)
		return err
	}); err != nil {
		return GuestOrder{}, err
	}
	ret := in.ReturnURL
	pu, err := s.purchaseWithHook(ctx, p, t, PurchaseInput{MeterID: meterID, AmountKobo: in.AmountKobo, PaymentMethod: in.PaymentMethod},
		func(ctx context.Context, tx pgx.Tx, vendID uuid.UUID) (string, error) {
			if ret == "" {
				return "", nil
			}
			return ret + "/order/" + vendID.String(), nil
		})
	if err != nil {
		return GuestOrder{}, err
	}
	return s.Order(ctx, p, t, pu.ID)
}

// Order returns one of the signed-in buyer's own orders. An order still
// waiting for payment is checked with the payment provider first.
func (s *Service) Order(ctx context.Context, p auth.Principal, t Tenant, id uuid.UUID) (GuestOrder, error) {
	load := func() (GuestOrder, error) {
		var o GuestOrder
		err := s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
			var err error
			o, err = s.orderView(ctx, tx, t, id, p.UserID)
			return err
		})
		return o, err
	}
	o, err := load()
	if err == nil && o.Status == StPaymentPending {
		s.CheckPendingPayments(ctx, t.ID, p.UserID)
		return load()
	}
	return o, err
}

// Orders lists the buyer's account orders, newest first. Orders placed as a
// guest before the account existed are not shown: nothing proved the email.
func (s *Service) Orders(ctx context.Context, p auth.Principal, t Tenant, limit int) ([]GuestOrder, error) {
	out := []GuestOrder{}
	err := s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT v.id FROM vend_transactions v WHERE v.tenant_id=$1 AND v.user_id=$2
			AND NOT EXISTS (SELECT 1 FROM guest_orders g WHERE g.vend_transaction_id=v.id)
			ORDER BY v.created_at DESC LIMIT $3`, t.ID, p.UserID, limit)
		if err != nil {
			return err
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			o, err := s.orderView(ctx, tx, t, id, p.UserID)
			if err != nil {
				return err
			}
			out = append(out, o)
		}
		return nil
	})
	return out, err
}

// checkOrder normalises the order and rejects it before any money moves:
// amounts outside the DisCo's limits, then meters the DisCo does not know.
func (s *Service) checkOrder(ctx context.Context, t Tenant, providerCode *string, meterNumber string, meterType *string, amount int64) (PublicMeter, error) {
	if *meterType == "" {
		*meterType = "prepaid"
	}
	if *meterType != "prepaid" && *meterType != "postpaid" {
		return PublicMeter{}, errf(http.StatusBadRequest, "invalid_meter_type", "meter_type must be prepaid or postpaid")
	}
	*providerCode = strings.ToUpper(strings.TrimSpace(*providerCode))
	ts, err := s.Tariffs(ctx, t.ID)
	if err != nil {
		return PublicMeter{}, err
	}
	for _, tr := range ts {
		if tr.ProviderCode == *providerCode {
			s.withDiscoLimits(ctx, &tr)
			if amount < tr.MinPurchaseKobo || amount > tr.MaxPurchaseKobo {
				return PublicMeter{}, errf(http.StatusBadRequest, "invalid_amount", "for %s the amount must be between ₦%s and ₦%s",
					tr.ProviderName, nairaStr(tr.MinPurchaseKobo), nairaStr(tr.MaxPurchaseKobo))
			}
		}
	}
	return s.verifyMeter(ctx, *providerCode, meterNumber, *meterType)
}

// linkExternalMeter records the DisCo meter (one row per tenant) and links it to the buyer.
func linkExternalMeter(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, providerCode, meterNumber, meterType string, m PublicMeter) (uuid.UUID, error) {
	meterNumber = strings.TrimSpace(meterNumber)
	var meterID uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO meters (tenant_id, meter_number, provider_code, ownership, meter_type, status, customer_name, address)
		VALUES ($1,$2,$3,'external',$4,'active',$5,$6)
		ON CONFLICT (tenant_id, provider_code, meter_number) WHERE ownership='external'
		DO UPDATE SET customer_name=EXCLUDED.customer_name, address=EXCLUDED.address, updated_at=now() RETURNING id`,
		tenantID, meterNumber, providerCode, meterType, m.CustomerName, m.Address).Scan(&meterID)
	if err != nil {
		return meterID, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO meter_links (tenant_id, meter_id, user_id, label, is_owner)
		VALUES ($1,$2,$3,$4, NOT EXISTS (SELECT 1 FROM meter_links WHERE meter_id=$2 AND is_owner))
		ON CONFLICT (meter_id, user_id) DO NOTHING`, tenantID, meterID, userID, "Meter "+meterNumber[max(0, len(meterNumber)-4):])
	return meterID, err
}

// GuestOrder returns an order to whoever holds its key.
func (s *Service) GuestOrder(ctx context.Context, t Tenant, id uuid.UUID, key string) (GuestOrder, error) {
	return s.guestOrder(ctx, t, id, key, true)
}

func (s *Service) guestOrder(ctx context.Context, t Tenant, id uuid.UUID, key string, check bool) (GuestOrder, error) {
	var o GuestOrder
	var guestUser uuid.UUID
	err := s.DB.Tenant(ctx, t.ID, func(tx pgx.Tx) error {
		var hash []byte
		var userID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT g.key_hash, v.user_id FROM guest_orders g JOIN vend_transactions v ON v.id=g.vend_transaction_id
			WHERE g.vend_transaction_id=$1 AND g.tenant_id=$2`, id, t.ID).Scan(&hash, &userID)
		if err != nil || !secure.EqualHex(hexOf(hash), hexOf(secure.HashSecret(key))) {
			return ErrNotFound
		}
		o, err = s.orderView(ctx, tx, t, id, userID)
		if err == nil && o.Status == StPaymentPending {
			guestUser = userID
		}
		return err
	})
	if check && err == nil && guestUser != uuid.Nil {
		s.CheckPendingPayments(ctx, t.ID, guestUser)
		return s.guestOrder(ctx, t, id, key, false)
	}
	return o, err
}

func (s *Service) orderView(ctx context.Context, tx pgx.Tx, t Tenant, id, userID uuid.UUID) (GuestOrder, error) {
	pu, err := s.scanPurchase(tx.QueryRow(ctx, purchaseSelect+`WHERE v.tenant_id=$1 AND v.id=$2 AND v.user_id=$3`, t.ID, id, userID), true)
	if err != nil {
		return GuestOrder{}, err
	}
	var name, mtype string
	if err := tx.QueryRow(ctx, `SELECT coalesce(t.provider_name, m.provider_code), m.meter_type FROM meters m
		LEFT JOIN tariffs t ON t.tenant_id=m.tenant_id AND t.provider_code=m.provider_code WHERE m.id=$1`, pu.MeterID).Scan(&name, &mtype); err != nil {
		return GuestOrder{}, err
	}
	o := GuestOrder{ID: pu.ID, Status: pu.Status, ProviderCode: pu.ProviderCode, ProviderName: name, MeterNumber: pu.MeterNumber,
		MeterType: mtype, CustomerName: pu.CustomerName, AmountKobo: pu.AmountKobo, FeeKobo: pu.FeeKobo, EnergyKwh: pu.EnergyKwh,
		Token: pu.Token, Sandbox: pu.Sandbox, CreatedAt: pu.CreatedAt}
	if pu.Payment != nil {
		o.CheckoutURL, o.PaymentStatus = pu.Payment.CheckoutURL, pu.Payment.Status
	}
	switch pu.Status {
	case StPaymentPending:
		o.Message = "Waiting for your payment to be confirmed."
	case StVending:
		o.Message = "Payment received. Getting your token from the DisCo…"
	case StNeedsReview:
		o.Message = "Your payment was received and your token is delayed. Our team is on it; you will not be charged twice."
	case StRefunded:
		o.Message = "The DisCo could not complete this purchase. Your payment is being refunded to your card or account."
	case StPaymentFailed:
		o.Message = "Payment was not completed. You have not been charged."
	}
	if pu.Status != StVended && pu.Status != StDelivered {
		o.Token = nil
	}
	return o, nil
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2], out[i*2+1] = digits[c>>4], digits[c&15]
	}
	return string(out)
}
