package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTokenRoundTripAndTamper(t *testing.T) {
	s := NewSigner([]byte(strings.Repeat("k", 32)), "k1", time.Minute)
	uid, tid := uuid.New(), uuid.New()
	tok, _, err := s.Issue(uid, tid, RoleCustomer, "password", 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Verify(tok)
	if err != nil || p.UserID != uid || p.TenantID != tid || p.Role != RoleCustomer {
		t.Fatalf("verify: %v %+v", err, p)
	}
	// Changing the tenant claim must break the signature.
	parts := strings.Split(tok, ".")
	forged := parts[0] + "." + strings.Replace(parts[1], "a", "b", 1) + "." + parts[2]
	if _, err := s.Verify(forged); err == nil {
		t.Fatal("forged token accepted")
	}
	other := NewSigner([]byte(strings.Repeat("x", 32)), "k1", time.Minute)
	if _, err := other.Verify(tok); err == nil {
		t.Fatal("token from other key accepted")
	}
	s.clock = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, err := s.Verify(tok); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestPassword(t *testing.T) {
	h, _ := HashPassword("correct horse")
	if !CheckPassword(h, "correct horse") || CheckPassword(h, "wrong") || CheckPassword("", "") {
		t.Fatal("password check")
	}
}
