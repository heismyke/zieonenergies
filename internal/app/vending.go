package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/payments"
	"github.com/heismyke/zieonenergies/internal/secure"
	"github.com/heismyke/zieonenergies/internal/vending"
)

// Vend transaction states. Allowed transitions:
//
//	payment_pending -> vending | payment_failed
//	vending         -> vended | vend_failed -> refunded | needs_review
//	vended          -> delivered
//	needs_review    -> refunded | vending (operator retry)
const (
	StPaymentPending = "payment_pending"
	StVending        = "vending"
	StVended         = "vended"
	StDelivered      = "delivered"
	StPaymentFailed  = "payment_failed"
	StVendFailed     = "vend_failed"
	StNeedsReview    = "needs_review"
	StRefunded       = "refunded"
)

var transitions = map[string][]string{
	StPaymentPending: {StVending, StPaymentFailed},
	StVending:        {StVended, StVendFailed, StNeedsReview},
	StVended:         {StDelivered},
	StVendFailed:     {StRefunded, StNeedsReview},
	StNeedsReview:    {StRefunded, StVending},
}

func canTransition(from, to string) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

func setStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID, from, to, reason string) error {
	if !canTransition(from, to) {
		return fmt.Errorf("illegal vend transition %s -> %s", from, to)
	}
	tag, err := tx.Exec(ctx, `UPDATE vend_transactions SET status=$3, failure_reason=CASE WHEN $4='' THEN failure_reason ELSE $4 END,
		updated_at=now() WHERE id=$1 AND status=$2`, id, from, to, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("vend %s no longer in state %s", id, from)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Quote
// ---------------------------------------------------------------------------

type Quote struct {
	ProviderCode    string  `json:"provider_code"`
	AmountKobo      int64   `json:"amount_kobo"`
	FeeKobo         int64   `json:"fee_kobo"`
	VatKobo         int64   `json:"vat_kobo"`
	EnergyValueKobo int64   `json:"energy_value_kobo"`
	RateKoboPerKwh  int64   `json:"rate_kobo_per_kwh"`
	EnergyKwh       float64 `json:"energy_kwh"`
	energyWh        int64
}

// computeQuote splits a gross amount into fee, VAT and energy value. Rewards
// and discounts never change this: they are separate funding legs.
func computeQuote(t Tariff, amount int64) (Quote, error) {
	if amount < t.MinPurchaseKobo || amount > t.MaxPurchaseKobo {
		return Quote{}, errf(http.StatusBadRequest, "invalid_amount", "amount must be between %d and %d kobo", t.MinPurchaseKobo, t.MaxPurchaseKobo)
	}
	q := Quote{ProviderCode: t.ProviderCode, AmountKobo: amount, FeeKobo: t.ServiceFeeKobo, RateKoboPerKwh: t.RateKoboPerKwh}
	net := amount - q.FeeKobo
	if net <= 0 {
		return Quote{}, errf(http.StatusBadRequest, "invalid_amount", "amount does not cover the service fee")
	}
	q.VatKobo = net * int64(t.VatBps) / int64(10000+t.VatBps)
	q.EnergyValueKobo = net - q.VatKobo
	q.energyWh = q.EnergyValueKobo * 1000 / t.RateKoboPerKwh
	if q.energyWh <= 0 {
		return Quote{}, errf(http.StatusBadRequest, "invalid_amount", "amount is too small to buy any energy")
	}
	q.EnergyKwh = WhToKwh(q.energyWh)
	return q, nil
}

func tariffFor(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, provider string) (Tariff, error) {
	t, err := scanTariff(tx.QueryRow(ctx, `SELECT `+tariffCols+` FROM tariffs WHERE tenant_id=$1 AND provider_code=$2 AND active`, tenant, provider))
	if errors.Is(err, pgx.ErrNoRows) {
		return t, errf(http.StatusBadRequest, "no_tariff", "no active tariff for provider %s", provider)
	}
	return t, err
}

func (s *Service) Quote(ctx context.Context, p auth.Principal, providerCode string, amountKobo int64, kwh float64) (Quote, error) {
	var q Quote
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		t, err := tariffFor(ctx, tx, p.TenantID, strings.ToUpper(providerCode))
		if err != nil {
			return err
		}
		if amountKobo == 0 && kwh > 0 {
			// Inverse: gross amount needed for kwh of energy, rounded up to the kobo.
			energy := (KwhToWh(kwh)*t.RateKoboPerKwh + 999) / 1000
			net := energy + (energy*int64(t.VatBps)+9999)/10000
			amountKobo = net + t.ServiceFeeKobo
		}
		q, err = computeQuote(t, amountKobo)
		return err
	})
	return q, err
}

// ---------------------------------------------------------------------------
// Purchase
// ---------------------------------------------------------------------------

type PurchaseInput struct {
	MeterID       uuid.UUID `json:"meter_id"`
	AmountKobo    int64     `json:"amount_kobo"`
	PaymentMethod string    `json:"payment_method"` // wallet | card | bank_transfer | crypto
	UseRewards    bool      `json:"use_rewards"`
	CallbackURL   string    `json:"callback_url"`
}

type Purchase struct {
	ID              uuid.UUID    `json:"id"`
	Status          string       `json:"status"`
	MeterID         uuid.UUID    `json:"meter_id"`
	MeterNumber     string       `json:"meter_number"`
	MeterLabel      string       `json:"meter_label"`
	ProviderCode    string       `json:"provider_code"`
	CustomerName    string       `json:"customer_name"`
	PaymentMethod   string       `json:"payment_method"`
	AmountKobo      int64        `json:"amount_kobo"`
	WalletKobo      int64        `json:"wallet_kobo"`
	RewardKobo      int64        `json:"reward_kobo"`
	ExternalKobo    int64        `json:"external_kobo"`
	FeeKobo         int64        `json:"fee_kobo"`
	VatKobo         int64        `json:"vat_kobo"`
	EnergyValueKobo int64        `json:"energy_value_kobo"`
	RateKoboPerKwh  int64        `json:"rate_kobo_per_kwh"`
	EnergyKwh       float64      `json:"energy_kwh"`
	Route           string       `json:"route"`
	Sandbox         bool         `json:"sandbox"`
	Token           *string      `json:"token"`
	TokenStatus     *string      `json:"token_status"`
	FailureReason   string       `json:"failure_reason,omitempty"`
	RewardEarned    int64        `json:"reward_earned_kobo"`
	Payment         *PaymentInfo `json:"payment,omitempty"`
	CreatedAt       time.Time    `json:"created_at"`
	VendedAt        *time.Time   `json:"vended_at"`
	UserID          uuid.UUID    `json:"user_id"`
}

type PaymentInfo struct {
	Reference   string                `json:"reference"`
	Provider    string                `json:"provider"`
	Status      string                `json:"status"`
	CheckoutURL string                `json:"checkout_url,omitempty"`
	BankAccount *payments.BankAccount `json:"bank_account,omitempty"`
}

// vendProvider picks the route for a meter. DisCo meters go to the
// aggregator ("vtpass"); Zieon meters to the STS route ("live"). A sandbox
// tenant never reaches a live provider (no real money), and a live tenant
// never reaches a sandbox one (no fake tokens).
func (s *Service) vendProvider(t Tenant, ownership string) (vending.Provider, error) {
	var route vending.Provider
	if ownership == "external" {
		route = s.Vending["vtpass"]
	} else {
		route = s.Vending["live"]
	}
	if t.VendingMode == "live" {
		if route == nil || route.Sandbox() {
			return nil, errf(http.StatusServiceUnavailable, "vending_unavailable", "no authorised vending route is configured for this meter")
		}
		return route, nil
	}
	if !s.Cfg.SandboxAllowed() {
		return nil, errf(http.StatusServiceUnavailable, "vending_unavailable", "sandbox vending is disabled in production")
	}
	if route != nil && route.Sandbox() {
		return route, nil // e.g. the VTpass sandbox: real API, test tokens
	}
	return s.Vending["sandbox"], nil
}

// LiveRouteAvailable reports whether any non-sandbox vending route exists.
func (s *Service) LiveRouteAvailable() bool {
	for _, k := range []string{"live", "vtpass"} {
		if p := s.Vending[k]; p != nil && !p.Sandbox() {
			return true
		}
	}
	return false
}

func (s *Service) paymentProvider(method string) (payments.Provider, error) {
	if p, ok := s.Payments["paystack"]; ok && p != nil {
		return p, nil
	}
	if s.Cfg.SandboxAllowed() {
		return s.Payments["sandbox"], nil
	}
	return nil, errf(http.StatusServiceUnavailable, "payments_unavailable", "%s payments are not configured", method)
}

func (s *Service) checkMethod(t Tenant, method string) error {
	switch method {
	case "wallet":
		if !t.Enabled("wallet") {
			return ErrFeatureOff
		}
	case "card":
		if !t.Enabled("card_payments") {
			return ErrFeatureOff
		}
	case "bank_transfer":
		if !t.Enabled("bank_transfer") {
			return ErrFeatureOff
		}
	case "crypto":
		return errf(http.StatusBadRequest, "unsupported_method", "crypto payments are not supported")
	default:
		return errf(http.StatusBadRequest, "invalid_payment_method", "payment_method must be wallet, card or bank_transfer")
	}
	return nil
}

func (s *Service) Purchase(ctx context.Context, p auth.Principal, t Tenant, in PurchaseInput) (Purchase, error) {
	if err := s.checkMethod(t, in.PaymentMethod); err != nil {
		return Purchase{}, err
	}
	if in.UseRewards && !t.Enabled("benefits") {
		return Purchase{}, ErrFeatureOff
	}
	var txnID uuid.UUID
	var pay *paymentRow
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		m, err := myMeter(ctx, tx, p, in.MeterID)
		if err != nil {
			return err
		}
		vp, err := s.vendProvider(t, m.Ownership)
		if err != nil {
			return err
		}
		if m.Status != "active" {
			return errf(http.StatusConflict, "meter_inactive", "this meter is not active")
		}
		tariff, err := tariffFor(ctx, tx, p.TenantID, m.ProviderCode)
		if err != nil {
			return err
		}
		q, err := computeQuote(tariff, in.AmountKobo)
		if err != nil {
			return err
		}
		var rewardKobo int64
		if in.UseRewards {
			avail, err := balance(ctx, tx, AcctReward, p.UserID)
			if err != nil {
				return err
			}
			rewardKobo = min(avail, in.AmountKobo)
		}
		rest := in.AmountKobo - rewardKobo
		walletKobo, externalKobo := int64(0), int64(0)
		if in.PaymentMethod == "wallet" {
			walletKobo = rest
		} else {
			externalKobo = rest
		}
		status := StVending
		if externalKobo > 0 {
			status = StPaymentPending
		}
		route := "zieon_meter"
		if m.Ownership == "external" {
			route = "disco_aggregator"
		}
		source := "customer"
		if Meta(ctx).Principal.Kind == "system" {
			source = "auto_topup"
		}
		err = tx.QueryRow(ctx, `INSERT INTO vend_transactions (tenant_id, user_id, meter_id, status, route, provider, payment_method,
			amount_kobo, wallet_kobo, reward_kobo, external_kobo, fee_kobo, vat_kobo, energy_value_kobo, rate_kobo_per_kwh, energy_wh, sandbox, source)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18) RETURNING id`,
			p.TenantID, p.UserID, m.ID, status, route, vp.Name(), in.PaymentMethod, in.AmountKobo, walletKobo, rewardKobo, externalKobo,
			q.FeeKobo, q.VatKobo, q.EnergyValueKobo, q.RateKoboPerKwh, q.energyWh, vp.Sandbox(), source).Scan(&txnID)
		if err != nil {
			return err
		}
		ref := txnID.String()
		if rewardKobo > 0 {
			if err := consumeRewards(ctx, tx, p.TenantID, p.UserID, rewardKobo, ref); err != nil {
				return err
			}
		}
		if walletKobo > 0 {
			if err := transfer(ctx, tx, p.TenantID, AcctWallet, p.UserID, AcctClearing, p.TenantID, walletKobo, "purchase", ref,
				"Electricity purchase "+m.MeterNumber); err != nil {
				return err
			}
		}
		if externalKobo > 0 {
			pp, err := s.paymentProvider(in.PaymentMethod)
			if err != nil {
				return err
			}
			pay = &paymentRow{Provider: pp.Name(), Method: in.PaymentMethod, AmountKobo: externalKobo, Purpose: "purchase", VendID: &txnID}
			if err := insertPayment(ctx, tx, p, pay); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Purchase{}, err
	}
	if pay != nil {
		if err := s.startPayment(ctx, p, pay, in.CallbackURL); err != nil {
			return Purchase{}, err
		}
	} else {
		s.executeVend(ctx, p.TenantID, txnID)
	}
	return s.GetPurchase(ctx, p, txnID)
}

// executeVend calls the vending route for a transaction in state vending.
// It is safe to call repeatedly: providers are idempotent on our reference
// and the result is only applied while the row is still in state vending.
func (s *Service) executeVend(ctx context.Context, tenant, txnID uuid.UUID) {
	ctx = context.WithoutCancel(ctx)
	var req vending.Request
	var providerName string
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		var status string
		var err error
		if req, providerName, status, err = loadVendRequest(ctx, tx, txnID, true); err != nil {
			return err
		}
		if status != StVending {
			return errSkip
		}
		_, err = tx.Exec(ctx, `UPDATE vend_transactions SET attempts=attempts+1, updated_at=now() WHERE id=$1`, txnID)
		return err
	})
	if err != nil {
		if !errors.Is(err, errSkip) {
			s.Log.Error("vend: load", "txn", txnID, "err", err)
		}
		return
	}
	vp := s.Vending[providerName]
	if vp == nil {
		s.Log.Error("vend: provider missing", "txn", txnID, "provider", providerName)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
	res, verr := vp.Vend(callCtx, req)
	cancel()
	s.applyVendResult(ctx, tenant, txnID, res, verr)
}

var errSkip = errors.New("skip")

// loadVendRequest builds the provider request for a vend transaction. The
// amount sent to the DisCo excludes our service fee.
func loadVendRequest(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (vending.Request, string, string, error) {
	var r vending.Request
	var provider, status string
	var fee int64
	q := `SELECT v.status, v.provider, m.meter_number, m.provider_code, m.meter_type, v.energy_wh, v.amount_kobo, v.fee_kobo,
		u.phone, v.created_at FROM vend_transactions v JOIN meters m ON m.id=v.meter_id JOIN users u ON u.id=v.user_id WHERE v.id=$1`
	if lock {
		q += ` FOR UPDATE OF v`
	}
	err := tx.QueryRow(ctx, q, id).Scan(&status, &provider, &r.MeterNumber, &r.ProviderCode, &r.MeterType, &r.EnergyWh,
		&r.AmountKobo, &fee, &r.Phone, &r.CreatedAt)
	r.AmountKobo -= fee
	r.Reference = id.String()
	return r, provider, status, err
}

// VendRequest is used by reconciliation to re-ask the provider about a vend.
func (s *Service) VendRequest(ctx context.Context, tenant, id uuid.UUID) (vending.Request, error) {
	var r vending.Request
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		r, _, _, err = loadVendRequest(ctx, tx, id, false)
		return err
	})
	return r, err
}

func (s *Service) applyVendResult(ctx context.Context, tenant, txnID uuid.UUID, res vending.Result, verr error) {
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		v, err := lockVend(ctx, tx, txnID)
		if err != nil {
			return err
		}
		if v.status != StVending {
			return nil // already resolved by another worker
		}
		switch {
		case verr == nil:
			return s.completeVend(ctx, tx, v, res)
		case errors.Is(verr, vending.ErrRejected):
			return s.failAndRefund(ctx, tx, v, StVending, "vending route rejected the purchase")
		default:
			// Outcome unknown (timeout, network). Leave in vending for reconciliation.
			_, err := tx.Exec(ctx, `UPDATE vend_transactions SET failure_reason=$2, updated_at=now() WHERE id=$1`, txnID, "pending confirmation: "+verr.Error())
			return err
		}
	})
	if err != nil {
		s.Log.Error("vend: apply result", "txn", txnID, "err", err)
	}
}

type vendRow struct {
	id, tenant, user, meter          uuid.UUID
	status, method                   string
	amount, wallet, reward, external int64
	energyWh                         int64
	sandbox                          bool
	attempts                         int
	updatedAt                        time.Time
}

func lockVend(ctx context.Context, tx pgx.Tx, id uuid.UUID) (vendRow, error) {
	var v vendRow
	err := tx.QueryRow(ctx, `SELECT id, tenant_id, user_id, meter_id, status, payment_method, amount_kobo, wallet_kobo, reward_kobo,
		external_kobo, energy_wh, sandbox, attempts, updated_at FROM vend_transactions WHERE id=$1 FOR UPDATE`, id).
		Scan(&v.id, &v.tenant, &v.user, &v.meter, &v.status, &v.method, &v.amount, &v.wallet, &v.reward, &v.external, &v.energyWh,
			&v.sandbox, &v.attempts, &v.updatedAt)
	return v, notFoundIfNoRows(err)
}

func (s *Service) completeVend(ctx context.Context, tx pgx.Tx, v vendRow, res vending.Result) error {
	if res.Token == "" {
		var mt string
		if err := tx.QueryRow(ctx, `SELECT meter_type FROM meters WHERE id=$1`, v.meter).Scan(&mt); err != nil {
			return err
		}
		if mt != "postpaid" {
			return s.markReview(ctx, tx, v, "provider returned success without a token")
		}
		// Postpaid: the bill is paid at the DisCo; there is no token to deliver.
		if err := setStatus(ctx, tx, v.id, StVending, StVended, ""); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE vend_transactions SET provider_reference=$2, vended_at=now(), delivered_at=now(), status='delivered',
			failure_reason='' WHERE id=$1`, v.id, res.ProviderReference); err != nil {
			return err
		}
		if err := notify(ctx, tx, v.tenant, &v.user, "Bill paid", fmt.Sprintf("Your postpaid bill payment of ₦%.2f was received.", float64(v.amount)/100), "purchase", false); err != nil {
			return err
		}
		return emit(ctx, tx, v.tenant, "purchase.vended", map[string]any{"purchase_id": v.id, "user_id": v.user, "meter_id": v.meter,
			"amount_kobo": v.amount, "postpaid": true, "sandbox": v.sandbox})
	}
	var dup bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM energy_tokens WHERE token_hash=$1)`,
		secure.HashSecret(normalizeToken(res.Token))).Scan(&dup); err != nil {
		return err
	}
	salt := ""
	if dup {
		if !v.sandbox {
			// A real token we already issued must never be credited twice.
			return s.markReview(ctx, tx, v, "provider returned a token that was already issued")
		}
		// Sandbox aggregators reuse one test token; keep each purchase distinct.
		salt = v.id.String()
	}
	if err := setStatus(ctx, tx, v.id, StVending, StVended, ""); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE vend_transactions SET provider_reference=$2, vended_at=now(), failure_reason='' WHERE id=$1`,
		v.id, res.ProviderReference); err != nil {
		return err
	}
	energy := v.energyWh
	if res.EnergyWh > 0 {
		// The DisCo is authoritative for units issued; our quote was an estimate.
		energy = res.EnergyWh
		if _, err := tx.Exec(ctx, `UPDATE vend_transactions SET energy_wh=$2 WHERE id=$1`, v.id, energy); err != nil {
			return err
		}
	}
	if _, err := s.insertTokenSalted(ctx, tx, v.tenant, v.meter, v.user, "purchase", &v.id, res.Token, salt, energy, "", v.sandbox); err != nil {
		return err
	}
	earned, err := s.grantPurchaseRewards(ctx, tx, v)
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("Your %.1f kWh token is ready.", WhToKwh(energy))
	if earned > 0 {
		msg += fmt.Sprintf(" You earned ₦%.2f in rewards.", float64(earned)/100)
	}
	if err := notify(ctx, tx, v.tenant, &v.user, "Token ready", msg, "purchase", false); err != nil {
		return err
	}
	return emit(ctx, tx, v.tenant, "purchase.vended", map[string]any{"purchase_id": v.id, "user_id": v.user, "meter_id": v.meter,
		"amount_kobo": v.amount, "energy_kwh": WhToKwh(energy), "sandbox": v.sandbox})
}

func (s *Service) markReview(ctx context.Context, tx pgx.Tx, v vendRow, reason string) error {
	if err := setStatus(ctx, tx, v.id, v.status, StNeedsReview, reason); err != nil {
		return err
	}
	if err := s.audit(ctx, tx, AuditEntry{TenantID: &v.tenant, Action: "vend.needs_review", TargetType: "vend_transaction", TargetID: v.id.String(),
		Metadata: map[string]any{"reason": reason}}); err != nil {
		return err
	}
	return emit(ctx, tx, v.tenant, "purchase.needs_review", map[string]any{"purchase_id": v.id, "reason": reason})
}

// failAndRefund returns every funding leg. Card / transfer legs are refunded
// to the customer's wallet (instant and auditable); a cash-out to the
// original instrument is an operator action.
func (s *Service) failAndRefund(ctx context.Context, tx pgx.Tx, v vendRow, from, reason string) error {
	if err := setStatus(ctx, tx, v.id, from, StVendFailed, reason); err != nil {
		return err
	}
	return s.refund(ctx, tx, v, StVendFailed, reason)
}

func (s *Service) refund(ctx context.Context, tx pgx.Tx, v vendRow, from, reason string) error {
	ref := v.id.String()
	if v.reward > 0 {
		if err := refundRewards(ctx, tx, v.tenant, v.user, v.reward, ref); err != nil {
			return err
		}
	}
	if toWallet := v.wallet + v.external; toWallet > 0 {
		if err := transfer(ctx, tx, v.tenant, AcctClearing, v.tenant, AcctWallet, v.user, toWallet, "refund", ref, "Refund: "+reason); err != nil {
			return err
		}
	}
	if err := setStatus(ctx, tx, v.id, from, StRefunded, reason); err != nil {
		return err
	}
	if err := notify(ctx, tx, v.tenant, &v.user, "Purchase refunded",
		fmt.Sprintf("Your purchase could not be completed (%s). ₦%.2f has been returned.", reason, float64(v.amount)/100), "purchase", true); err != nil {
		return err
	}
	if err := s.audit(ctx, tx, AuditEntry{TenantID: &v.tenant, Action: "vend.refund", TargetType: "vend_transaction", TargetID: ref,
		Metadata: map[string]any{"reason": reason, "wallet_kobo": v.wallet, "external_kobo": v.external, "reward_kobo": v.reward}}); err != nil {
		return err
	}
	return emit(ctx, tx, v.tenant, "purchase.refunded", map[string]any{"purchase_id": v.id, "user_id": v.user, "amount_kobo": v.amount, "reason": reason})
}

const purchaseSelect = `SELECT v.id, v.status, v.meter_id, m.meter_number, coalesce(l.label, m.meter_number), m.provider_code, m.customer_name,
	v.payment_method, v.amount_kobo, v.wallet_kobo, v.reward_kobo, v.external_kobo, v.fee_kobo, v.vat_kobo, v.energy_value_kobo,
	v.rate_kobo_per_kwh, v.energy_wh, v.route, v.sandbox, v.failure_reason, v.created_at, v.vended_at, v.user_id,
	tk.token_enc, tk.status,
	coalesce((SELECT sum(g.amount_kobo) FROM reward_grants g WHERE g.user_id=v.user_id AND g.source_ref=v.id::text AND g.kind IN ('cashback','fixed_reward')),0),
	py.reference, py.provider, py.status, py.checkout_url, py.provider_payload->'bank_account'
	FROM vend_transactions v
	JOIN meters m ON m.id=v.meter_id
	LEFT JOIN meter_links l ON l.meter_id=v.meter_id AND l.user_id=v.user_id
	LEFT JOIN energy_tokens tk ON tk.vend_transaction_id=v.id
	LEFT JOIN payments py ON py.vend_transaction_id=v.id `

func (s *Service) scanPurchase(row pgx.Row, reveal bool) (Purchase, error) {
	var pu Purchase
	var energyWh int64
	var tokenEnc []byte
	var payRef, payProvider, payStatus, payURL *string
	var bank *payments.BankAccount
	err := row.Scan(&pu.ID, &pu.Status, &pu.MeterID, &pu.MeterNumber, &pu.MeterLabel, &pu.ProviderCode, &pu.CustomerName, &pu.PaymentMethod,
		&pu.AmountKobo, &pu.WalletKobo, &pu.RewardKobo, &pu.ExternalKobo, &pu.FeeKobo, &pu.VatKobo, &pu.EnergyValueKobo, &pu.RateKoboPerKwh,
		&energyWh, &pu.Route, &pu.Sandbox, &pu.FailureReason, &pu.CreatedAt, &pu.VendedAt, &pu.UserID, &tokenEnc, &pu.TokenStatus,
		&pu.RewardEarned, &payRef, &payProvider, &payStatus, &payURL, &bank)
	if err != nil {
		return pu, notFoundIfNoRows(err)
	}
	pu.EnergyKwh = WhToKwh(energyWh)
	if tokenEnc != nil && reveal {
		plain, err := s.Keys.Decrypt(tokenEnc)
		if err != nil {
			return pu, err
		}
		pu.Token = ptr(string(plain))
	}
	if payRef != nil {
		pu.Payment = &PaymentInfo{Reference: *payRef, Provider: *payProvider, Status: *payStatus, CheckoutURL: *payURL, BankAccount: bank}
	}
	return pu, nil
}

// GetPurchase reveals the token only to the customer who bought it.
func (s *Service) GetPurchase(ctx context.Context, p auth.Principal, id uuid.UUID) (Purchase, error) {
	var pu Purchase
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		if p.Role == auth.RoleCustomer {
			pu, err = s.scanPurchase(tx.QueryRow(ctx, purchaseSelect+`WHERE v.tenant_id=$1 AND v.id=$2 AND v.user_id=$3`, p.TenantID, id, p.UserID), true)
		} else {
			pu, err = s.scanPurchase(tx.QueryRow(ctx, purchaseSelect+`WHERE v.tenant_id=$1 AND v.id=$2`, p.TenantID, id), false)
		}
		return err
	})
	return pu, err
}

type PurchaseFilter struct {
	UserID  *uuid.UUID
	MeterID *uuid.UUID
	Status  string
	Limit   int
}

func (s *Service) ListPurchases(ctx context.Context, tenant uuid.UUID, f PurchaseFilter, reveal bool) ([]Purchase, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	out := []Purchase{}
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, purchaseSelect+`WHERE v.tenant_id=$1 AND ($2::uuid IS NULL OR v.user_id=$2) AND ($3::uuid IS NULL OR v.meter_id=$3)
			AND ($4='' OR v.status=$4) ORDER BY v.created_at DESC LIMIT $5`, tenant, f.UserID, f.MeterID, f.Status, f.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			pu, err := s.scanPurchase(rows, reveal)
			if err != nil {
				return err
			}
			out = append(out, pu)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Payments
// ---------------------------------------------------------------------------

type paymentRow struct {
	ID         uuid.UUID
	Reference  string
	Provider   string
	Method     string
	Purpose    string
	AmountKobo int64
	VendID     *uuid.UUID
	Email      string
}

func insertPayment(ctx context.Context, tx pgx.Tx, p auth.Principal, pr *paymentRow) error {
	pr.Reference = "zp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := tx.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, p.UserID).Scan(&pr.Email); err != nil {
		return err
	}
	return tx.QueryRow(ctx, `INSERT INTO payments (tenant_id, user_id, purpose, provider, method, reference, amount_kobo, vend_transaction_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, p.TenantID, p.UserID, pr.Purpose, pr.Provider, pr.Method, pr.Reference, pr.AmountKobo, pr.VendID).Scan(&pr.ID)
}

// startPayment calls the provider outside any DB transaction. Sandbox
// payments confirm immediately through the same path a webhook would use.
func (s *Service) startPayment(ctx context.Context, p auth.Principal, pr *paymentRow, callback string) error {
	pp := s.Payments[pr.Provider]
	sess, err := pp.Initialize(ctx, payments.Intent{Reference: pr.Reference, AmountKobo: pr.AmountKobo, Email: pr.Email, Method: pr.Method,
		Currency: "NGN", CallbackURL: callback})
	if err != nil {
		s.Log.Error("payment initialize", "ref", pr.Reference, "err", err)
		_ = s.FailPayment(ctx, pr.Reference, "payment provider unavailable")
		return errf(http.StatusBadGateway, "payment_provider_error", "could not start the payment; you have not been charged")
	}
	err = s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payments SET checkout_url=$2, provider_payload=jsonb_build_object('bank_account', $3::jsonb), updated_at=now() WHERE id=$1`,
			pr.ID, sess.CheckoutURL, sess.BankAccount)
		return err
	})
	if err != nil {
		return err
	}
	if sess.Confirmed {
		if !pp.Sandbox() {
			return errors.New("live provider claimed synchronous confirmation")
		}
		return s.ConfirmPayment(ctx, pr.Provider, pr.Reference, pr.AmountKobo)
	}
	return nil
}

// ConfirmPayment is the single path for captured funds (webhook, verify,
// sandbox). It is idempotent: a payment already resolved is a no-op.
func (s *Service) ConfirmPayment(ctx context.Context, provider, reference string, amountKobo int64) error {
	ctx = context.WithoutCancel(ctx)
	tenant, err := s.paymentTenant(ctx, reference)
	if err != nil {
		return err
	}
	var vendID *uuid.UUID
	err = s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		var id, user uuid.UUID
		var status, purpose, prov string
		var amount int64
		err := tx.QueryRow(ctx, `SELECT id, user_id, status, purpose, provider, amount_kobo, vend_transaction_id FROM payments WHERE reference=$1 FOR UPDATE`,
			reference).Scan(&id, &user, &status, &purpose, &prov, &amount, &vendID)
		if err != nil {
			return notFoundIfNoRows(err)
		}
		if status != "pending" {
			vendID = nil
			return nil // duplicate notification
		}
		if prov != provider {
			return errf(http.StatusBadRequest, "provider_mismatch", "payment belongs to a different provider")
		}
		if amountKobo != amount {
			vendID = nil
			if _, err := tx.Exec(ctx, `UPDATE payments SET status='failed', updated_at=now(), provider_payload = provider_payload || jsonb_build_object('mismatch_amount', $2::bigint) WHERE id=$1`, id, amountKobo); err != nil {
				return err
			}
			return s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "payment.amount_mismatch", TargetType: "payment", TargetID: reference,
				Metadata: map[string]any{"expected": amount, "received": amountKobo}})
		}
		if _, err := tx.Exec(ctx, `UPDATE payments SET status='succeeded', confirmed_at=now(), updated_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
		switch purpose {
		case "wallet_fund":
			if err := transfer(ctx, tx, tenant, AcctClearing, tenant, AcctWallet, user, amount, "wallet_fund", reference, "Wallet funded"); err != nil {
				return err
			}
			if err := notify(ctx, tx, tenant, &user, "Wallet funded", fmt.Sprintf("₦%.2f was added to your wallet.", float64(amount)/100), "wallet", false); err != nil {
				return err
			}
			return emit(ctx, tx, tenant, "wallet.funded", map[string]any{"user_id": user, "amount_kobo": amount, "reference": reference})
		case "purchase":
			if vendID == nil {
				return errors.New("purchase payment without vend transaction")
			}
			v, err := lockVend(ctx, tx, *vendID)
			if err != nil {
				return err
			}
			if v.status != StPaymentPending {
				vendID = nil
				return nil
			}
			return setStatus(ctx, tx, v.id, StPaymentPending, StVending, "")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if vendID != nil {
		s.executeVend(ctx, tenant, *vendID)
	}
	return nil
}

// FailPayment marks a pending payment failed and unwinds a linked purchase.
func (s *Service) FailPayment(ctx context.Context, reference, reason string) error {
	ctx = context.WithoutCancel(ctx)
	tenant, err := s.paymentTenant(ctx, reference)
	if err != nil {
		return err
	}
	return s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		var id uuid.UUID
		var status string
		var vendID *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id, status, vend_transaction_id FROM payments WHERE reference=$1 FOR UPDATE`, reference).Scan(&id, &status, &vendID); err != nil {
			return notFoundIfNoRows(err)
		}
		if status != "pending" {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE payments SET status='failed', updated_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
		if vendID == nil {
			return nil
		}
		v, err := lockVend(ctx, tx, *vendID)
		if err != nil || v.status != StPaymentPending {
			return err
		}
		if err := setStatus(ctx, tx, v.id, StPaymentPending, StPaymentFailed, reason); err != nil {
			return err
		}
		// Only the reward leg can have been taken before payment; return it.
		if v.reward > 0 {
			if err := refundRewards(ctx, tx, v.tenant, v.user, v.reward, v.id.String()); err != nil {
				return err
			}
		}
		return emit(ctx, tx, tenant, "purchase.payment_failed", map[string]any{"purchase_id": v.id, "reason": reason})
	})
}

func (s *Service) paymentTenant(ctx context.Context, reference string) (uuid.UUID, error) {
	var tenant uuid.UUID
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT tenant_id FROM payments WHERE reference=$1`, reference).Scan(&tenant)
	})
	return tenant, notFoundIfNoRows(err)
}

// RecordWebhookEvent de-duplicates inbound provider events. It returns false
// when the event was already processed.
func (s *Service) RecordWebhookEvent(ctx context.Context, provider, eventID string, payload []byte) (bool, error) {
	var fresh bool
	err := s.DB.System(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO webhook_events (provider, event_id, payload) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, provider, eventID, payload)
		fresh = err == nil && tag.RowsAffected() == 1
		return err
	})
	return fresh, err
}

// ---------------------------------------------------------------------------
// Wallet
// ---------------------------------------------------------------------------

type Wallet struct {
	BalanceKobo       int64  `json:"balance_kobo"`
	RewardBalanceKobo int64  `json:"reward_balance_kobo"`
	Currency          string `json:"currency"`
}

func (s *Service) Wallet(ctx context.Context, p auth.Principal) (Wallet, error) {
	w := Wallet{Currency: "NGN"}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		if w.BalanceKobo, err = balance(ctx, tx, AcctWallet, p.UserID); err != nil {
			return err
		}
		w.RewardBalanceKobo, err = balance(ctx, tx, AcctReward, p.UserID)
		return err
	})
	return w, err
}

type FundResult struct {
	Payment PaymentInfo `json:"payment"`
	Wallet  Wallet      `json:"wallet"`
	Sandbox bool        `json:"sandbox"`
}

func (s *Service) FundWallet(ctx context.Context, p auth.Principal, t Tenant, amountKobo int64, method, callback string) (FundResult, error) {
	if !t.Enabled("wallet") {
		return FundResult{}, ErrFeatureOff
	}
	if method != "card" && method != "bank_transfer" {
		return FundResult{}, errf(http.StatusBadRequest, "invalid_payment_method", "method must be card or bank_transfer")
	}
	if err := s.checkMethod(t, method); err != nil {
		return FundResult{}, err
	}
	if amountKobo < 10000 || amountKobo > 100000000 {
		return FundResult{}, errf(http.StatusBadRequest, "invalid_amount", "amount must be between ₦100 and ₦1,000,000")
	}
	pp, err := s.paymentProvider(method)
	if err != nil {
		return FundResult{}, err
	}
	pr := &paymentRow{Provider: pp.Name(), Method: method, AmountKobo: amountKobo, Purpose: "wallet_fund"}
	if err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error { return insertPayment(ctx, tx, p, pr) }); err != nil {
		return FundResult{}, err
	}
	if err := s.startPayment(ctx, p, pr, callback); err != nil {
		return FundResult{}, err
	}
	out := FundResult{Sandbox: pp.Sandbox()}
	err = s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		var bank *payments.BankAccount
		out.Payment.Reference, out.Payment.Provider = pr.Reference, pr.Provider
		return tx.QueryRow(ctx, `SELECT status, checkout_url, provider_payload->'bank_account' FROM payments WHERE id=$1`, pr.ID).
			Scan(&out.Payment.Status, &out.Payment.CheckoutURL, &bank)
	})
	if err != nil {
		return out, err
	}
	out.Wallet, err = s.Wallet(ctx, p)
	return out, err
}

// ---------------------------------------------------------------------------
// Tokens (DIU delivery)
// ---------------------------------------------------------------------------

func normalizeToken(t string) string {
	var b strings.Builder
	for _, c := range t {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func (s *Service) insertToken(ctx context.Context, tx pgx.Tx, tenant, meter, user uuid.UUID, source string, vendID *uuid.UUID, token string, wh int64, note string, sandbox bool) (uuid.UUID, error) {
	return s.insertTokenSalted(ctx, tx, tenant, meter, user, source, vendID, token, "", wh, note, sandbox)
}

// tokenHash is the lookup key. Real tokens hash on their digits alone, so a
// token can exist once; salted (sandbox-only) rows allow a reused test token.
func tokenHash(token, salt string) []byte {
	if salt == "" {
		return secure.HashSecret(token)
	}
	return secure.HashSecret(token + "|" + salt)
}

func (s *Service) insertTokenSalted(ctx context.Context, tx pgx.Tx, tenant, meter, user uuid.UUID, source string, vendID *uuid.UUID, token, salt string, wh int64, note string, sandbox bool) (uuid.UUID, error) {
	token = normalizeToken(token)
	enc, err := s.Keys.Encrypt([]byte(token))
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO energy_tokens (tenant_id, meter_id, issued_to_user_id, source, vend_transaction_id, token_enc, token_hash, token_last4, energy_wh, note, sandbox)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`, tenant, meter, user, source, vendID, enc, tokenHash(token, salt), token[len(token)-4:], wh, note, sandbox).Scan(&id)
	return id, err
}

type PendingToken struct {
	ID        uuid.UUID `json:"id"`
	Token     string    `json:"token"`
	MeterID   uuid.UUID `json:"meter_id"`
	EnergyKwh float64   `json:"energy_kwh"`
	Source    string    `json:"source"`
	Note      string    `json:"note"`
	Status    string    `json:"status"`
	Sandbox   bool      `json:"sandbox"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) PendingTokens(ctx context.Context, p auth.Principal, meterID *uuid.UUID) ([]PendingToken, error) {
	out := []PendingToken{}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT t.id, t.token_enc, t.meter_id, t.energy_wh, t.source, t.note, t.status, t.sandbox, t.created_at
			FROM energy_tokens t JOIN meter_links l ON l.meter_id=t.meter_id AND l.user_id=$2
			WHERE t.tenant_id=$1 AND t.status IN ('issued','submitted') AND ($3::uuid IS NULL OR t.meter_id=$3)
			AND (t.issued_to_user_id=$2 OR t.source='purchase') ORDER BY t.created_at DESC LIMIT 50`, p.TenantID, p.UserID, meterID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var pt PendingToken
			var enc []byte
			var wh int64
			if err := rows.Scan(&pt.ID, &enc, &pt.MeterID, &wh, &pt.Source, &pt.Note, &pt.Status, &pt.Sandbox, &pt.CreatedAt); err != nil {
				return err
			}
			plain, err := s.Keys.Decrypt(enc)
			if err != nil {
				return err
			}
			pt.Token, pt.EnergyKwh = string(plain), WhToKwh(wh)
			out = append(out, pt)
		}
		return rows.Err()
	})
	return out, err
}

type RedeemResult struct {
	Status    string  `json:"status"` // delivered | submitted
	EnergyKwh float64 `json:"energy_kwh"`
	Simulated bool    `json:"simulated"`
	Message   string  `json:"message"`
	Meter     Meter   `json:"meter"`
}

// RedeemToken is the DIU step. Sandbox tokens are credited to the platform
// ledger immediately and flagged simulated. Live tokens are only marked
// "submitted": delivery is confirmed when the meter reports accepting it.
func (s *Service) RedeemToken(ctx context.Context, p auth.Principal, meterID uuid.UUID, token string) (RedeemResult, error) {
	token = normalizeToken(token)
	if len(token) != 20 {
		return RedeemResult{}, errf(http.StatusBadRequest, "invalid_token", "token must be 20 digits")
	}
	var out RedeemResult
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		m, err := myMeter(ctx, tx, p, meterID)
		if err != nil {
			return err
		}
		var id uuid.UUID
		var tokMeter uuid.UUID
		var status string
		var wh int64
		var sandbox bool
		err = tx.QueryRow(ctx, `SELECT id, meter_id, status, energy_wh, sandbox FROM energy_tokens WHERE tenant_id=$1 AND token_hash=$2 FOR UPDATE`,
			p.TenantID, secure.HashSecret(token)).Scan(&id, &tokMeter, &status, &wh, &sandbox)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (status == "delivered" || tokMeter != meterID)) {
			// Sandbox tokens may repeat; match the oldest undelivered one on this meter.
			if sid, ok, ferr := s.findSandboxToken(ctx, tx, p.TenantID, meterID, token); ferr != nil {
				return ferr
			} else if ok {
				err = tx.QueryRow(ctx, `SELECT id, meter_id, status, energy_wh, sandbox FROM energy_tokens WHERE id=$1 FOR UPDATE`, sid).
					Scan(&id, &tokMeter, &status, &wh, &sandbox)
			}
		}
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && tokMeter != meterID) {
			return errf(http.StatusBadRequest, "invalid_token", "token is invalid for this meter")
		}
		if err != nil {
			return err
		}
		if status == "delivered" {
			return errf(http.StatusConflict, "token_used", "this token has already been delivered")
		}
		if status == "void" {
			return errf(http.StatusBadRequest, "invalid_token", "token is invalid for this meter")
		}
		out.EnergyKwh = WhToKwh(wh)
		if m.CommsMode == "mqtt" && m.Capabilities.RemoteCredit {
			// Connected meter: the token is pushed as a signed command and only
			// counts as delivered when the meter acknowledges executing it.
			if status == "submitted" {
				return errf(http.StatusConflict, "token_pending", "this token is already being sent to your meter")
			}
			plain := token
			if _, err := enqueueCommand(ctx, tx, p.TenantID, meterID, "credit_token",
				map[string]any{"token": plain, "token_id": id, "energy_kwh": WhToKwh(wh)}, &p.UserID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE energy_tokens SET status='submitted', submitted_at=now() WHERE id=$1`, id); err != nil {
				return err
			}
			out.Status, out.Simulated = "submitted", false
			out.Message = "Token sent to your meter. Your balance updates as soon as the meter confirms it."
		} else if sandbox {
			if _, err := s.confirmTokenID(ctx, tx, p.TenantID, meterID, id); err != nil {
				return err
			}
			out.Status, out.Simulated = "delivered", true
			out.Message = "Sandbox token: credit applied to the platform ledger only. No real meter was credited."
		} else {
			if status == "issued" {
				if _, err := tx.Exec(ctx, `UPDATE energy_tokens SET status='submitted', submitted_at=now() WHERE id=$1`, id); err != nil {
					return err
				}
			}
			out.Status = "submitted"
			if m.Capabilities.Keypad || !m.Capabilities.RemoteCredit {
				out.Message = "Enter this token on your meter keypad. Your balance updates once the meter confirms it."
			} else {
				out.Message = "Token sent. Your balance updates once the meter confirms it."
			}
		}
		out.Meter, err = myMeter(ctx, tx, p, meterID)
		return err
	})
	return out, err
}

// confirmTokenDelivery credits the platform energy ledger exactly once.
func (s *Service) confirmTokenDelivery(ctx context.Context, tx pgx.Tx, tenant, meterID uuid.UUID, token string) (int, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM energy_tokens WHERE tenant_id=$1 AND meter_id=$2 AND token_hash=$3 AND status IN ('issued','submitted')`,
		tenant, meterID, secure.HashSecret(token)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		sid, ok, ferr := s.findSandboxToken(ctx, tx, tenant, meterID, token)
		if ferr != nil || !ok {
			return 0, ferr
		}
		id, err = sid, nil
	}
	if err != nil {
		return 0, err
	}
	return s.confirmTokenID(ctx, tx, tenant, meterID, id)
}

// findSandboxToken matches a reused sandbox token by decrypting this meter's
// undelivered sandbox tokens (a handful at most). Never used for live tokens.
func (s *Service) findSandboxToken(ctx context.Context, tx pgx.Tx, tenant, meterID uuid.UUID, token string) (uuid.UUID, bool, error) {
	rows, err := tx.Query(ctx, `SELECT id, token_enc FROM energy_tokens WHERE tenant_id=$1 AND meter_id=$2 AND sandbox
		AND status IN ('issued','submitted') AND token_last4=$3 ORDER BY created_at LIMIT 50`, tenant, meterID, token[max(0, len(token)-4):])
	if err != nil {
		return uuid.Nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var enc []byte
		if err := rows.Scan(&id, &enc); err != nil {
			return uuid.Nil, false, err
		}
		if plain, err := s.Keys.Decrypt(enc); err == nil && string(plain) == token {
			return id, true, nil
		}
	}
	return uuid.Nil, false, rows.Err()
}

// confirmTokenID marks one token delivered and credits the energy ledger exactly once.
func (s *Service) confirmTokenID(ctx context.Context, tx pgx.Tx, tenant, meterID, tokenID uuid.UUID) (int, error) {
	var id uuid.UUID
	var wh int64
	var vendID *uuid.UUID
	var user uuid.UUID
	var source string
	err := tx.QueryRow(ctx, `UPDATE energy_tokens SET status='delivered', delivered_at=now()
		WHERE tenant_id=$1 AND meter_id=$2 AND id=$3 AND status IN ('issued','submitted')
		RETURNING id, energy_wh, vend_transaction_id, issued_to_user_id, source`, tenant, meterID, tokenID).Scan(&id, &wh, &vendID, &user, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if err := postEnergy(ctx, tx, tenant, meterID, wh, "token_delivered", id.String(), source); err != nil {
		return 0, err
	}
	if vendID != nil {
		if _, err := tx.Exec(ctx, `UPDATE vend_transactions SET status='delivered', delivered_at=now(), updated_at=now() WHERE id=$1 AND status='vended'`, *vendID); err != nil {
			return 0, err
		}
	}
	return 1, emit(ctx, tx, tenant, "token.delivered", map[string]any{"meter_id": meterID, "energy_kwh": WhToKwh(wh), "source": source})
}

// ---------------------------------------------------------------------------
// Spark Share
// ---------------------------------------------------------------------------

type ShareInput struct {
	FromMeterID   uuid.UUID `json:"from_meter_id"`
	ToMeterNumber string    `json:"to_meter_number"`
	Kwh           float64   `json:"kwh"`
	Note          string    `json:"note"`
	RecipientName string    `json:"recipient_name"`
	Mode          string    `json:"mode"` // direct | token
}

type ShareResult struct {
	Mode      string  `json:"mode"`
	Kwh       float64 `json:"kwh"`
	Token     string  `json:"token,omitempty"`
	Simulated bool    `json:"simulated"`
	FromMeter Meter   `json:"from_meter"`
}

// Share moves units between meters. The sending meter must support remote
// debit; otherwise units physically on the meter cannot be taken back, and
// pretending otherwise would create energy from nothing.
func (s *Service) Share(ctx context.Context, p auth.Principal, t Tenant, in ShareInput) (ShareResult, error) {
	if !t.Enabled("spark_share") {
		return ShareResult{}, ErrFeatureOff
	}
	in.ToMeterNumber = strings.TrimSpace(in.ToMeterNumber)
	if in.Mode == "" {
		in.Mode = "direct"
	}
	if in.Mode != "direct" && in.Mode != "token" {
		return ShareResult{}, errf(http.StatusBadRequest, "invalid_mode", "mode must be direct or token")
	}
	if !meterNumberRe.MatchString(in.ToMeterNumber) {
		return ShareResult{}, errf(http.StatusBadRequest, "invalid_meter_number", "recipient meter number must be 10-20 digits")
	}
	wh := KwhToWh(in.Kwh)
	if wh <= 0 || in.Kwh > 10000 {
		return ShareResult{}, errf(http.StatusBadRequest, "invalid_amount", "kwh must be between 0.001 and 10000")
	}
	if len(in.Note) > 120 || len(in.RecipientName) > 60 {
		return ShareResult{}, errf(http.StatusBadRequest, "invalid_input", "note max 120, recipient_name max 60 characters")
	}
	out := ShareResult{Mode: in.Mode, Kwh: WhToKwh(wh), Simulated: t.VendingMode == "sandbox"}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		from, err := myMeter(ctx, tx, p, in.FromMeterID)
		if err != nil {
			return err
		}
		if !from.IsOwner {
			return errf(http.StatusForbidden, "not_owner", "only the meter owner can share its units")
		}
		if !from.Capabilities.RemoteDebit {
			return errf(http.StatusConflict, "meter_capability", "this meter cannot release units remotely, so its units cannot be shared")
		}
		// Lock the sender's energy by locking its meter row.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM meters WHERE id=$1 FOR UPDATE`, from.ID); err != nil {
			return err
		}
		var available int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(wh),0) FROM energy_entries WHERE meter_id=$1`, from.ID).Scan(&available); err != nil {
			return err
		}
		if from.DeviceCreditKwh != nil {
			available = min(available, KwhToWh(*from.DeviceCreditKwh))
		}
		if wh > available {
			return errf(http.StatusConflict, "insufficient_units", "only %.1f kWh available to share", WhToKwh(available))
		}
		var toID uuid.UUID
		var toCaps []byte
		err = tx.QueryRow(ctx, `SELECT id, capabilities FROM meters WHERE tenant_id=$1 AND meter_number=$2 AND status='active' ORDER BY ownership='zieon' DESC LIMIT 1`,
			p.TenantID, in.ToMeterNumber).Scan(&toID, &toCaps)
		if errors.Is(err, pgx.ErrNoRows) {
			return errf(http.StatusNotFound, "recipient_not_found", "recipient meter is not on this platform")
		}
		if err != nil {
			return err
		}
		if toID == from.ID {
			return errf(http.StatusBadRequest, "invalid_recipient", "cannot share to the same meter")
		}
		ref := uuid.NewString()
		if in.Mode == "direct" {
			if err := postEnergy(ctx, tx, p.TenantID, from.ID, -wh, "share_out", ref, in.Note); err != nil {
				return err
			}
			if err := postEnergy(ctx, tx, p.TenantID, toID, wh, "share_in", ref, in.Note); err != nil {
				return err
			}
			if err := notifyMeterOwners(ctx, tx, p.TenantID, toID, "Units received", fmt.Sprintf("%.1f kWh was shared to your meter.", WhToKwh(wh))); err != nil {
				return err
			}
		} else {
			if err := postEnergy(ctx, tx, p.TenantID, from.ID, -wh, "share_token", ref, in.Note); err != nil {
				return err
			}
			token := "98" + secure.RandomDigits(18)
			if _, err := s.insertToken(ctx, tx, p.TenantID, toID, p.UserID, "share", nil, token, wh, in.Note, t.VendingMode == "sandbox"); err != nil {
				return err
			}
			out.Token = token
		}
		name := in.RecipientName
		if name == "" {
			name = in.Note
		}
		if name == "" {
			name = "Recipient"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO share_recipients (tenant_id, user_id, name, meter_number, last_note, last_wh, last_shared_at)
			VALUES ($1,$2,$3,$4,$5,$6,now()) ON CONFLICT (user_id, meter_number) DO UPDATE SET name=EXCLUDED.name, last_note=EXCLUDED.last_note,
			last_wh=EXCLUDED.last_wh, last_shared_at=now()`, p.TenantID, p.UserID, name, in.ToMeterNumber, in.Note, wh); err != nil {
			return err
		}
		out.FromMeter, err = myMeter(ctx, tx, p, from.ID)
		return err
	})
	return out, err
}

func notifyMeterOwners(ctx context.Context, tx pgx.Tx, tenant, meterID uuid.UUID, title, msg string) error {
	_, err := tx.Exec(ctx, `INSERT INTO notifications (tenant_id, user_id, title, message, kind)
		SELECT $1, user_id, $3, $4, 'share' FROM meter_links WHERE meter_id=$2`, tenant, meterID, title, msg)
	return err
}

type Recipient struct {
	Name         string     `json:"name"`
	MeterNumber  string     `json:"meter_number"`
	LastNote     string     `json:"last_note"`
	LastKwh      float64    `json:"last_kwh"`
	LastSharedAt *time.Time `json:"last_shared_at"`
}

func (s *Service) Recipients(ctx context.Context, p auth.Principal) ([]Recipient, error) {
	out := []Recipient{}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT name, meter_number, last_note, last_wh, last_shared_at FROM share_recipients
			WHERE tenant_id=$1 AND user_id=$2 ORDER BY coalesce(last_shared_at, created_at) DESC LIMIT 20`, p.TenantID, p.UserID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Recipient
			var wh int64
			if err := rows.Scan(&r.Name, &r.MeterNumber, &r.LastNote, &wh, &r.LastSharedAt); err != nil {
				return err
			}
			r.LastKwh = WhToKwh(wh)
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Service) AddRecipient(ctx context.Context, p auth.Principal, name, meterNumber string) (Recipient, error) {
	name, meterNumber = strings.TrimSpace(name), strings.TrimSpace(meterNumber)
	if name == "" || len(name) > 60 || !meterNumberRe.MatchString(meterNumber) {
		return Recipient{}, errf(http.StatusBadRequest, "invalid_input", "name (max 60) and a 10-20 digit meter number are required")
	}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO share_recipients (tenant_id, user_id, name, meter_number) VALUES ($1,$2,$3,$4)
			ON CONFLICT (user_id, meter_number) DO UPDATE SET name=EXCLUDED.name`, p.TenantID, p.UserID, name, meterNumber)
		return err
	})
	return Recipient{Name: name, MeterNumber: meterNumber}, err
}

func (s *Service) RemoveRecipient(ctx context.Context, p auth.Principal, meterNumber string) error {
	return s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM share_recipients WHERE tenant_id=$1 AND user_id=$2 AND meter_number=$3`, p.TenantID, p.UserID, meterNumber)
		return err
	})
}

// ---------------------------------------------------------------------------
// Activity feed (profile "History")
// ---------------------------------------------------------------------------

type Activity struct {
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	Subtitle   string    `json:"subtitle"`
	AmountKobo *int64    `json:"amount_kobo,omitempty"`
	Kwh        *float64  `json:"kwh,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *Service) Activity(ctx context.Context, p auth.Principal, limit int) ([]Activity, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	out := []Activity{}
	err := s.DB.Tenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT kind, memo, amount_kobo, NULL::bigint, created_at FROM ledger_entries
			  WHERE tenant_id=$1 AND account_id=$2 AND account_type IN ('wallet','reward')
			UNION ALL
			SELECT e.kind, coalesce(l.label,'') || CASE WHEN e.memo <> '' THEN ' • ' || e.memo ELSE '' END, NULL, e.wh, e.created_at
			  FROM energy_entries e JOIN meter_links l ON l.meter_id=e.meter_id AND l.user_id=$2 WHERE e.tenant_id=$1
			UNION ALL
			SELECT 'meter_added', l.label || ' • ' || m.meter_number, NULL, NULL, l.created_at
			  FROM meter_links l JOIN meters m ON m.id=l.meter_id WHERE l.tenant_id=$1 AND l.user_id=$2
			ORDER BY 5 DESC LIMIT $3`, p.TenantID, p.UserID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a Activity
			var amt, wh *int64
			if err := rows.Scan(&a.Kind, &a.Subtitle, &amt, &wh, &a.CreatedAt); err != nil {
				return err
			}
			a.AmountKobo = amt
			if wh != nil {
				a.Kwh = ptr(WhToKwh(*wh))
			}
			a.Title = activityTitles[a.Kind]
			if a.Title == "" {
				a.Title = strings.ReplaceAll(a.Kind, "_", " ")
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

var activityTitles = map[string]string{
	"wallet_fund": "Wallet funded", "purchase": "Electricity purchase", "refund": "Purchase refunded",
	"token_delivered": "DIU credit delivered", "share_out": "Units shared", "share_in": "Units received",
	"share_token": "Spark Share token generated", "meter_added": "Meter added", "cashback": "Cashback earned",
	"fixed_reward": "Reward earned", "monthly_credit": "Monthly electricity credit", "reward_spend": "Rewards applied",
	"reward_refund": "Rewards returned", "reward_expired": "Rewards expired",
}

// ---------------------------------------------------------------------------
// Operator recovery
// ---------------------------------------------------------------------------

// ResolveReview lets a platform operator settle a needs_review purchase after
// checking the vending partner's records: "refund" returns funds, "retry"
// re-attempts the vend under the same idempotent reference.
func (s *Service) ResolveReview(ctx context.Context, tenant, id uuid.UUID, action, note string) error {
	if action != "refund" && action != "retry" {
		return errf(http.StatusBadRequest, "invalid_action", "action must be refund or retry")
	}
	retry := false
	err := s.DB.Tenant(ctx, tenant, func(tx pgx.Tx) error {
		v, err := lockVend(ctx, tx, id)
		if err != nil {
			return err
		}
		if v.status != StNeedsReview {
			return errf(http.StatusConflict, "invalid_state", "purchase is %s, not needs_review", v.status)
		}
		if err := s.audit(ctx, tx, AuditEntry{TenantID: &tenant, Action: "vend.review." + action, TargetType: "vend_transaction", TargetID: id.String(),
			Metadata: map[string]any{"note": note}}); err != nil {
			return err
		}
		if action == "refund" {
			return s.refund(ctx, tx, v, StNeedsReview, "refunded after review")
		}
		retry = true
		return setStatus(ctx, tx, id, StNeedsReview, StVending, "")
	})
	if err == nil && retry {
		s.executeVend(ctx, tenant, id)
	}
	return err
}
