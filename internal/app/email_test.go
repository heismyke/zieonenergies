package app

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Set EMAIL_PREVIEW=/path/file.html to write the email out and look at it.
func TestTokenEmailRenders(t *testing.T) {
	m := tokenMessage{vend: uuid.MustParse("22cf449b-0000-4000-8000-000000000001"), channel: "email", amount: 215000, fee: 15000, energyWh: 79900,
		meter: "1111111111111", disco: "Ikeja Electricity", brand: "Zenerlink", customer: "ADA OKAFOR", method: "card", buyer: "Ada Okafor",
		createdAt: time.Date(2026, 11, 13, 19, 16, 0, 0, time.UTC), accent: "#C8823A", support: "support@zieonenergies.com"}
	msg := renderTokenMessage(m, "26362054405982757802", "https://www.zenerlink.com")
	for _, want := range []string{"2636-2054-4059-8275-7802", "₦2,150", "8275", "https://www.zenerlink.com/order/22cf449b-", "icon-192.png", "#C8823A", "Hello Ada,"} {
		if !strings.Contains(msg.HTML, want) {
			t.Fatalf("email html missing %q", want)
		}
	}
	if p := os.Getenv("EMAIL_PREVIEW"); p != "" {
		os.WriteFile(p, []byte(msg.HTML), 0o644)
	}
}

// Set RESET_PREVIEW=/path/file.html to write the email out and look at it.
func TestResetEmailRenders(t *testing.T) {
	msg := renderResetMessage(brandInfo{name: "Zenerlink", accent: "#C8823A", support: "support@zieonenergies.com"},
		"https://www.zenerlink.com", "ada@example.com", "Ada Okafor", "abc123")
	for _, want := range []string{"Hello Ada,", "https://www.zenerlink.com/reset-password?code=abc123", "ada@example.com", "icon-192.png"} {
		if !strings.Contains(msg.HTML, want) {
			t.Fatalf("reset html missing %q", want)
		}
	}
	if p := os.Getenv("RESET_PREVIEW"); p != "" {
		os.WriteFile(p, []byte(msg.HTML), 0o644)
	}
}
