// Package messaging sends customer messages (token receipts) by email, SMS
// and WhatsApp. Each channel is enabled by its provider's API key; with no
// key the channel is simply absent and messages for it are skipped.
package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	Email    = "email"
	SMS      = "sms"
	WhatsApp = "whatsapp"
)

type Message struct {
	To      string // email address, or phone number in international form (2348012345678)
	Subject string // email only
	Text    string
	HTML    string // email only; Text is used when empty
}

type Sender interface {
	// Send delivers one message and returns the provider's reference.
	Send(ctx context.Context, m Message) (string, error)
}

// ErrPermanent marks a failure that retrying will not fix (bad number, rejected sender).
var ErrPermanent = errors.New("messaging: permanent failure")

var client = &http.Client{Timeout: 15 * time.Second}

func postJSON(ctx context.Context, url string, headers map[string]string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode >= 400 {
		err := fmt.Errorf("messaging: %s returned %d: %s", url, res.StatusCode, strings.TrimSpace(string(raw)))
		if res.StatusCode < 500 && res.StatusCode != http.StatusTooManyRequests {
			return errors.Join(ErrPermanent, err)
		}
		return err
	}
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
	return nil
}

// Resend sends email through resend.com.
type Resend struct {
	APIKey  string
	From    string // e.g. "Zenerlink <receipts@zenerlink.com>"
	BaseURL string
}

func (r Resend) Send(ctx context.Context, m Message) (string, error) {
	base := r.BaseURL
	if base == "" {
		base = "https://api.resend.com"
	}
	body := map[string]any{"from": r.From, "to": []string{m.To}, "subject": m.Subject, "text": m.Text}
	if m.HTML != "" {
		body["html"] = m.HTML
	}
	var out struct {
		ID string `json:"id"`
	}
	err := postJSON(ctx, base+"/emails", map[string]string{"Authorization": "Bearer " + r.APIKey}, body, &out)
	return out.ID, err
}

// Termii sends SMS (DND route, delivered even to numbers on Do-Not-Disturb)
// or WhatsApp messages through termii.com.
type Termii struct {
	APIKey   string
	SenderID string // approved sender ID, e.g. "Zenerlink"
	BaseURL  string // account-specific, e.g. https://v3.api.termii.com
	Channel  string // "dnd" for SMS, "whatsapp" for WhatsApp
}

func (t Termii) Send(ctx context.Context, m Message) (string, error) {
	base := t.BaseURL
	if base == "" {
		base = "https://v3.api.termii.com"
	}
	var out struct {
		MessageID string `json:"message_id"`
		Code      string `json:"code"`
		Message   string `json:"message"`
	}
	err := postJSON(ctx, strings.TrimRight(base, "/")+"/api/sms/send", nil, map[string]any{
		"api_key": t.APIKey, "to": m.To, "from": t.SenderID, "sms": m.Text, "type": "plain", "channel": t.Channel,
	}, &out)
	if err == nil && out.Code != "" && out.Code != "ok" {
		err = errors.Join(ErrPermanent, fmt.Errorf("termii: %s", out.Message))
	}
	return out.MessageID, err
}

// NormalizePhone turns a Nigerian number (08012345678, +234 801 234 5678,
// 2348012345678) into 2348012345678. It returns "" if it is not a valid number.
func NormalizePhone(p string) string {
	d := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, p)
	switch {
	case len(d) == 11 && d[0] == '0':
		d = "234" + d[1:]
	case len(d) == 10 && d[0] != '0':
		d = "234" + d
	}
	if len(d) != 13 || !strings.HasPrefix(d, "234") {
		return ""
	}
	return d
}
