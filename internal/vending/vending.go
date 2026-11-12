// Package vending abstracts the authorised routes that turn a paid order into
// electricity credit (an STS token from a licensed vending system, or a credit
// command to Zieon hardware).
//
// Only the Sandbox provider is implemented. It exists for development and
// pilot testing: its tokens are flagged sandbox, start with "99", and are not
// valid on any real meter. A live provider must be supplied by an authorised
// STS vending partner (see docs/METER_VENDING_FLOW.md); this package never
// generates or derives real STS tokens and holds no STS keys.
package vending

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/heismyke/zieonenergies/internal/secure"
)

type Request struct {
	Reference    string // our vend transaction id; providers must treat it as idempotent
	MeterNumber  string
	ProviderCode string
	EnergyWh     int64
	AmountKobo   int64
}

type Result struct {
	Token             string
	EnergyWh          int64
	ProviderReference string
}

var (
	// ErrRejected: the provider definitively refused; safe to refund.
	ErrRejected = errors.New("vend rejected by provider")
	// ErrNotFound: the provider has no record of the reference; safe to retry or refund.
	ErrNotFound = errors.New("vend reference not found at provider")
)

type Provider interface {
	Name() string
	Sandbox() bool
	Vend(ctx context.Context, r Request) (Result, error)
	// Lookup returns the outcome of an earlier Vend with the same reference.
	Lookup(ctx context.Context, reference string) (Result, error)
}

// Sandbox simulates a vending switch. Meter numbers ending in "0000" are
// rejected so failure and refund paths can be exercised end to end.
type Sandbox struct {
	mu   sync.Mutex
	done map[string]Result
	// FailNext, when set, makes the next Vend call return this error (tests).
	FailNext error
}

func NewSandbox() *Sandbox { return &Sandbox{done: map[string]Result{}} }

func (s *Sandbox) Name() string  { return "sandbox" }
func (s *Sandbox) Sandbox() bool { return true }

func (s *Sandbox) Vend(_ context.Context, r Request) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.FailNext; err != nil {
		s.FailNext = nil
		return Result{}, err
	}
	if prev, ok := s.done[r.Reference]; ok {
		return prev, nil // idempotent on reference, like a real switch
	}
	if strings.HasSuffix(r.MeterNumber, "0000") {
		return Result{}, ErrRejected
	}
	res := Result{Token: "99" + secure.RandomDigits(18), EnergyWh: r.EnergyWh, ProviderReference: "sbx_" + r.Reference}
	s.done[r.Reference] = res
	return res, nil
}

func (s *Sandbox) Lookup(_ context.Context, ref string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.done[ref]; ok {
		return r, nil
	}
	return Result{}, ErrNotFound
}
