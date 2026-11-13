package vending

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VTpass vends DisCo electricity through the VTpass aggregator
// (https://www.vtpass.com/documentation/). VTpass holds the DisCo vending
// agreements; Zieon acts as its merchant. Tokens come from the DisCo's own
// vending system — this code never generates them.
//
// Idempotency: VTpass de-duplicates on request_id, which must start with the
// Lagos-time minute it was created (YYYYMMDDHHmm). We derive it from the vend
// transaction's created_at and id, so Vend and Lookup always use the same id.
type VTpass struct {
	BaseURL   string // https://sandbox.vtpass.com or https://vtpass.com
	APIKey    string
	PublicKey string
	SecretKey string
	// FallbackPhone is sent when the customer has no phone on file (VTpass requires one).
	FallbackPhone string
	HTTP          *http.Client

	catMu    sync.Mutex
	catalog  map[string]CatalogEntry
	catUntil time.Time
}

func NewVTpass(baseURL, apiKey, publicKey, secretKey, fallbackPhone string) *VTpass {
	if baseURL == "" {
		baseURL = "https://sandbox.vtpass.com"
	}
	return &VTpass{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, PublicKey: publicKey, SecretKey: secretKey,
		FallbackPhone: fallbackPhone, HTTP: &http.Client{Timeout: 45 * time.Second}}
}

func (v *VTpass) Name() string { return "vtpass" }

// Sandbox is true for the VTpass sandbox environment: its tokens are not real.
func (v *VTpass) Sandbox() bool { return strings.Contains(v.BaseURL, "sandbox") }

// ServiceIDs maps our DisCo provider codes to VTpass serviceIDs.
var ServiceIDs = map[string]string{
	"IKEDC": "ikeja-electric", "EKEDC": "eko-electric", "AEDC": "abuja-electric", "KEDCO": "kano-electric",
	"PHED": "portharcourt-electric", "JED": "jos-electric", "KEDC": "kaduna-electric", "EEDC": "enugu-electric",
	"IBEDC": "ibadan-electric", "BEDC": "benin-electric", "YEDC": "yola-electric", "ABA": "aba-electric",
}

var lagos = time.FixedZone("WAT", 3600) // Africa/Lagos, UTC+1, no DST

// RequestID is deterministic per transaction: Lagos YYYYMMDDHHmm + reference hex.
func RequestID(createdAt time.Time, reference string) string {
	return createdAt.In(lagos).Format("200601021504") + strings.ReplaceAll(reference, "-", "")
}

type vtTxn struct {
	Status        string `json:"status"`
	TransactionID string `json:"transactionId"`
}

type vtResponse struct {
	Code                string          `json:"code"`
	ResponseDescription string          `json:"response_description"`
	Content             json.RawMessage `json:"content"`
	PurchasedCode       string          `json:"purchased_code"`
	Token               string          `json:"token"`
	MainToken           string          `json:"mainToken"`
	Units               string          `json:"units"`
	Tariff              string          `json:"tariff"`
	ExchangeReference   string          `json:"exchangeReference"`
}

func (r vtResponse) txn() vtTxn {
	var c struct {
		Transactions vtTxn `json:"transactions"`
	}
	_ = json.Unmarshal(r.Content, &c)
	return c.Transactions
}

func (v *VTpass) serviceID(code string) (string, error) {
	id, ok := ServiceIDs[strings.ToUpper(code)]
	if !ok {
		return "", ErrUnsupported
	}
	return id, nil
}

func meterType(t string) string {
	if t == "postpaid" {
		return "postpaid"
	}
	return "prepaid"
}

var digitsRe = regexp.MustCompile(`\d[\d\s-]{18,}\d`)

// extractToken finds the STS token in "Token : 2636 2054 …" style strings.
func extractToken(fields ...string) string {
	for _, f := range fields {
		if m := digitsRe.FindString(f); m != "" {
			d := strings.NewReplacer(" ", "", "-", "").Replace(m)
			if len(d) >= 20 {
				return d[:20]
			}
		}
	}
	return ""
}

var unitsRe = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*kwh`)

func parseUnitsWh(s string) int64 {
	m := unitsRe.FindStringSubmatch(strings.ToLower(s))
	if m == nil {
		return 0
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	return int64(f*1000 + 0.5)
}

// interpret maps a pay or requery response to our outcome semantics:
// success, definitive rejection (safe to refund), or unknown (reconcile later).
func (v *VTpass) interpret(r vtResponse, req Request) (Result, error) {
	t := r.txn()
	status := strings.ToLower(t.Status)
	switch {
	case r.Code == "000" && status == "delivered":
		res := Result{Token: extractToken(r.Token, r.PurchasedCode, r.MainToken), EnergyWh: parseUnitsWh(r.Units),
			ProviderReference: "vtpass:" + t.TransactionID, Receipt: strings.TrimSpace(r.Tariff + " " + r.Units)}
		if res.Token == "" && meterType(req.MeterType) == "prepaid" {
			// Paid but no token returned: never refund automatically.
			return Result{}, fmt.Errorf("vtpass delivered without a token (request %s)", RequestID(req.CreatedAt, req.Reference))
		}
		return res, nil
	case status == "failed" || status == "reversed" || definitiveRejects[r.Code]:
		return Result{}, fmt.Errorf("%w: %s", ErrRejected, nonEmpty(r.ResponseDescription, status))
	case r.Code == "099" || r.Code == "089" || status == "pending" || status == "initiated":
		return Result{}, errors.New("vtpass transaction pending")
	}
	return Result{}, fmt.Errorf("vtpass: unexpected response code %q (%s)", r.Code, r.ResponseDescription)
}

// Codes meaning VTpass did not process the request and charged nothing, so
// the customer can be refunded immediately. From
// https://www.vtpass.com/documentation/response-codes/. Codes whose outcome
// is unclear (083 system error, 089 processing, 044 resolved, 099) are NOT
// here: they go to reconciliation and, failing that, human review.
var definitiveRejects = map[string]bool{
	"010": true, // variation code does not exist
	"011": true, // invalid arguments
	"012": true, // product does not exist
	"013": true, // below minimum amount
	"016": true, // transaction failed
	"017": true, // above maximum amount
	"018": true, // low wallet balance
	"019": true, // likely duplicate (same service within 30 s, different request id)
	"021": true, // account locked
	"022": true, // account suspended
	"023": true, // API access not enabled
	"024": true, // account inactive
	"027": true, // IP not whitelisted
	"028": true, // product not whitelisted on account
	"030": true, // biller not reachable
	"034": true, // service suspended
	"035": true, // service inactive
	"040": true, // transaction reversal (funds returned to wallet)
	"085": true, // improper request id
	"087": true, // invalid credentials
	"091": true, // transaction not processed, no charge applied
}

func (v *VTpass) Vend(ctx context.Context, req Request) (Result, error) {
	sid, err := v.serviceID(req.ProviderCode)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %s", ErrRejected, err)
	}
	phone := strings.TrimPrefix(digitsOnly(req.Phone), "234")
	if len(phone) == 10 {
		phone = "0" + phone
	}
	if len(phone) < 10 {
		phone = v.FallbackPhone
	}
	body := map[string]any{
		"request_id": RequestID(req.CreatedAt, req.Reference), "serviceID": sid, "billersCode": req.MeterNumber,
		"variation_code": meterType(req.MeterType), "amount": float64(req.AmountKobo) / 100, "phone": phone,
	}
	var r vtResponse
	if err := v.post(ctx, "/api/pay", body, &r); err != nil {
		return Result{}, err // network/timeout: outcome unknown
	}
	if r.Code == "014" {
		// Our id already exists at VTpass: an earlier attempt reached them. Ask what happened.
		return v.Lookup(ctx, req)
	}
	if definitiveRejects[r.Code] {
		return Result{}, fmt.Errorf("%w: %s (%s)", ErrRejected, r.ResponseDescription, r.Code)
	}
	return v.interpret(r, req)
}

func (v *VTpass) Lookup(ctx context.Context, req Request) (Result, error) {
	var r vtResponse
	if err := v.post(ctx, "/api/requery", map[string]string{"request_id": RequestID(req.CreatedAt, req.Reference)}, &r); err != nil {
		return Result{}, err
	}
	if r.Code == "015" {
		return Result{}, ErrNotFound // invalid request id: VTpass never saw it
	}
	return v.interpret(r, req)
}

func (v *VTpass) ValidateMeter(ctx context.Context, providerCode, meterNumber, mt string) (MeterInfo, error) {
	sid, err := v.serviceID(providerCode)
	if err != nil {
		return MeterInfo{}, err
	}
	var r struct {
		Code    string `json:"code"`
		Content struct {
			CustomerName     string `json:"Customer_Name"`
			Address          string `json:"Address"`
			MeterType        string `json:"Meter_Type"`
			CanVend          string `json:"Can_Vend"`
			WrongBillersCode bool   `json:"WrongBillersCode"`
			Error            string `json:"error"`
		} `json:"content"`
	}
	if err := v.post(ctx, "/api/merchant-verify", map[string]string{"billersCode": meterNumber, "serviceID": sid, "type": meterType(mt)}, &r); err != nil {
		return MeterInfo{}, err
	}
	c := r.Content
	if r.Code != "000" || c.WrongBillersCode || c.Error != "" || c.CustomerName == "" {
		return MeterInfo{}, ErrNotFound
	}
	return MeterInfo{CustomerName: c.CustomerName, Address: c.Address, MeterType: strings.ToLower(c.MeterType),
		CanVend: c.CanVend == "" || strings.EqualFold(c.CanVend, "yes")}, nil
}

// Catalog returns VTpass's electricity services keyed by our DisCo code,
// cached for an hour (GET /api/services?identifier=electricity-bill).
func (v *VTpass) Catalog(ctx context.Context) (map[string]CatalogEntry, error) {
	v.catMu.Lock()
	defer v.catMu.Unlock()
	if v.catalog != nil && time.Now().Before(v.catUntil) {
		return v.catalog, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.BaseURL+"/api/services?identifier=electricity-bill", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("api-key", v.APIKey)
	req.Header.Set("public-key", v.PublicKey)
	res, err := v.HTTP.Do(req)
	if err != nil {
		return v.catalog, fmt.Errorf("vtpass services: %w", err)
	}
	defer res.Body.Close()
	var out struct {
		Content []struct {
			ServiceID string `json:"serviceID"`
			Image     string `json:"image"`
			Min       string `json:"minimium_amount"` // sic: VTpass's spelling
			Max       string `json:"maximum_amount"`
		} `json:"content"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&out) != nil {
		return v.catalog, fmt.Errorf("vtpass services: HTTP %d", res.StatusCode)
	}
	byService := map[string]string{}
	for code, sid := range ServiceIDs {
		byService[sid] = code
	}
	cat := map[string]CatalogEntry{}
	for _, s := range out.Content {
		code, ok := byService[s.ServiceID]
		if !ok {
			continue
		}
		e := CatalogEntry{ImageURL: s.Image}
		if n, err := strconv.ParseFloat(strings.TrimSpace(s.Min), 64); err == nil {
			e.MinKobo = int64(n * 100)
		}
		if n, err := strconv.ParseFloat(strings.TrimSpace(s.Max), 64); err == nil {
			e.MaxKobo = int64(n * 100)
		}
		cat[code] = e
	}
	v.catalog, v.catUntil = cat, time.Now().Add(time.Hour)
	return cat, nil
}

func (v *VTpass) post(ctx context.Context, path string, body any, out any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", v.APIKey)
	req.Header.Set("secret-key", v.SecretKey)
	res, err := v.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("vtpass %s: %w", path, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 500 {
		return fmt.Errorf("vtpass %s: HTTP %d", path, res.StatusCode)
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: vtpass rejected our credentials (HTTP %d)", ErrRejected, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("vtpass %s: decode: %w", path, err)
	}
	return nil
}

func digitsOnly(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b = append(b, s[i])
		}
	}
	return string(b)
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
