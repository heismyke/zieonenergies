// Package payments abstracts payment collection. Paystack is implemented for
// card / bank transfer checkout; Sandbox confirms instantly and is refused in
// production by the service layer.
package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type Intent struct {
	Reference   string
	AmountKobo  int64
	Email       string
	Method      string
	Currency    string
	CallbackURL string
}

type Session struct {
	CheckoutURL string
	// Confirmed is true when the provider has already captured the funds
	// (sandbox only). Live providers confirm via webhook or Verify.
	Confirmed bool
	// BankAccount is filled for bank-transfer style checkouts.
	BankAccount *BankAccount
}

type BankAccount struct {
	BankName      string `json:"bank_name"`
	AccountNumber string `json:"account_number"`
	AccountName   string `json:"account_name"`
}

type Verification struct {
	Status     string // succeeded | failed | pending
	AmountKobo int64
	Currency   string
}

type Provider interface {
	Name() string
	Sandbox() bool
	Initialize(ctx context.Context, in Intent) (Session, error)
	Verify(ctx context.Context, reference string) (Verification, error)
	// Refund returns a captured payment to the payer's original instrument.
	Refund(ctx context.Context, reference string, amountKobo int64) error
}

type Sandbox struct{}

func (Sandbox) Name() string  { return "sandbox" }
func (Sandbox) Sandbox() bool { return true }
func (Sandbox) Initialize(_ context.Context, in Intent) (Session, error) {
	s := Session{Confirmed: true}
	if in.Method == "bank_transfer" {
		s.BankAccount = &BankAccount{BankName: "Sandbox Bank (not real)", AccountNumber: "0000000000", AccountName: "Sandbox Wallet Topup"}
	}
	return s, nil
}
func (Sandbox) Verify(_ context.Context, _ string) (Verification, error) {
	return Verification{Status: "succeeded"}, nil
}
func (Sandbox) Refund(context.Context, string, int64) error { return nil }

// Paystack implements https://paystack.com/docs/api/transaction/.
// It has not been exercised against live Paystack credentials in this codebase.
type Paystack struct {
	SecretKey string
	BaseURL   string
	Client    *http.Client
}

func NewPaystack(secret string) *Paystack {
	return &Paystack{SecretKey: secret, BaseURL: "https://api.paystack.co", Client: &http.Client{Timeout: 15 * time.Second}}
}

func (p *Paystack) Name() string  { return "paystack" }
func (p *Paystack) Sandbox() bool { return false }

func (p *Paystack) Initialize(ctx context.Context, in Intent) (Session, error) {
	req := map[string]any{
		"email": in.Email, "amount": in.AmountKobo, "reference": in.Reference,
		"currency": in.Currency, "callback_url": in.CallbackURL,
	}
	switch in.Method {
	case "card":
		req["channels"] = []string{"card"}
	case "bank_transfer":
		req["channels"] = []string{"bank_transfer"}
	} // "checkout": every channel enabled on the Paystack account
	body, _ := json.Marshal(req)
	var out struct {
		Status  bool   `json:"status"`
		Message string `json:"message"`
		Data    struct {
			AuthorizationURL string `json:"authorization_url"`
		} `json:"data"`
	}
	if err := p.do(ctx, http.MethodPost, "/transaction/initialize", body, &out); err != nil {
		return Session{}, err
	}
	if !out.Status {
		return Session{}, fmt.Errorf("paystack initialize: %s", out.Message)
	}
	return Session{CheckoutURL: out.Data.AuthorizationURL}, nil
}

func (p *Paystack) Verify(ctx context.Context, ref string) (Verification, error) {
	var out struct {
		Status bool `json:"status"`
		Data   struct {
			Status   string `json:"status"`
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		} `json:"data"`
	}
	if err := p.do(ctx, http.MethodGet, "/transaction/verify/"+ref, nil, &out); err != nil {
		return Verification{}, err
	}
	v := Verification{AmountKobo: out.Data.Amount, Currency: out.Data.Currency, Status: "pending"}
	switch out.Data.Status {
	case "success":
		v.Status = "succeeded"
	case "failed", "abandoned", "reversed":
		v.Status = "failed"
	}
	return v, nil
}

// Refund implements https://paystack.com/docs/api/refund/ (full or partial).
func (p *Paystack) Refund(ctx context.Context, reference string, amountKobo int64) error {
	body, _ := json.Marshal(map[string]any{"transaction": reference, "amount": amountKobo})
	var out struct {
		Status  bool   `json:"status"`
		Message string `json:"message"`
	}
	if err := p.do(ctx, http.MethodPost, "/refund", body, &out); err != nil {
		return err
	}
	if !out.Status {
		return fmt.Errorf("paystack refund: %s", out.Message)
	}
	return nil
}

func (p *Paystack) do(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, p.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.SecretKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := p.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 500 {
		return errors.New("paystack unavailable")
	}
	return json.NewDecoder(res.Body).Decode(out)
}
