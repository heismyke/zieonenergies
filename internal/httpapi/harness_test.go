package httpapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/heismyke/zieonenergies/internal/app"
	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/config"
	"github.com/heismyke/zieonenergies/internal/db"
	"github.com/heismyke/zieonenergies/internal/httpapi"
	"github.com/heismyke/zieonenergies/internal/payments"
	"github.com/heismyke/zieonenergies/internal/secure"
	"github.com/heismyke/zieonenergies/internal/vending"
)

// Tests need PostgreSQL. TEST_DATABASE_URL points at a server where the user
// may CREATE DATABASE; each test run gets a fresh database that is dropped after.
//
//	TEST_DATABASE_URL=postgres://zieon:zieon_dev@localhost:55432/zieon?sslmode=disable go test ./...

type env struct {
	t        *testing.T
	svc      *app.Service
	srv      *httptest.Server
	vend     *vending.Sandbox
	pay      *deferredPay
	platform string // platform admin access token
}

// deferredPay is a non-sandbox-looking provider that never confirms
// synchronously, so tests exercise the webhook path.
type deferredPay struct {
	mu       sync.Mutex
	verified map[string]payments.Verification
}

func (d *deferredPay) Name() string  { return "sandbox" }
func (d *deferredPay) Sandbox() bool { return true }
func (d *deferredPay) Initialize(_ context.Context, in payments.Intent) (payments.Session, error) {
	return payments.Session{CheckoutURL: "https://pay.test/" + in.Reference}, nil
}
func (d *deferredPay) Verify(_ context.Context, ref string) (payments.Verification, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if v, ok := d.verified[ref]; ok {
		return v, nil
	}
	return payments.Verification{Status: "pending"}, nil
}

const webhookSecret = "test-webhook-secret"

var (
	setupOnce sync.Once
	baseURL   string
)

func newEnv(t *testing.T, deferPayments bool) *env {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := "zieon_test_" + strings.ReplaceAll(uuid.NewString()[:13], "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	store, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		admin.Close(context.Background())
	})
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	key := func(n int) []byte { b := make([]byte, n); rand.Read(b); return b }
	keys, _ := secure.NewKeyring(map[string][]byte{"k1": key(32)}, "k1")
	cfg := config.Config{Env: "test", AccessTTL: 15 * time.Minute, RefreshTTL: time.Hour, EmbedTTL: 2 * time.Minute,
		EmbedBaseURL: "https://embed.test", SandboxWebhookSecret: webhookSecret}
	e := &env{t: t, vend: vending.NewSandbox(), pay: &deferredPay{verified: map[string]payments.Verification{}}}
	e.svc = &app.Service{DB: store, Keys: keys, Cfg: cfg, Now: time.Now,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Signer:   auth.NewSigner(key(32), "k1", cfg.AccessTTL),
		Vending:  map[string]vending.Provider{"sandbox": e.vend},
		Payments: map[string]payments.Provider{"sandbox": payments.Sandbox{}},
	}
	if deferPayments {
		e.svc.Payments["sandbox"] = e.pay
	}
	e.srv = httptest.NewServer(httpapi.New(e.svc, e.svc.Log, "zieon").Handler())
	t.Cleanup(e.srv.Close)

	_, err = e.svc.ProvisionTenant(ctx, app.ProvisionInput{Slug: "zieon", Name: "Zieon", IsPlatform: true,
		Admin: struct {
			Email    string `json:"email"`
			Name     string `json:"name"`
			Password string `json:"password"`
		}{"root@zieon.test", "Root", "platform-pass-1"}})
	if err != nil {
		t.Fatal(err)
	}
	e.platform = e.login("zieon", "root@zieon.test", "platform-pass-1")
	return e
}

type resp struct {
	Status int
	Body   map[string]any
	Raw    []byte
}

func (r resp) str(path ...string) string {
	v := r.get(path...)
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func (r resp) num(path ...string) float64 {
	f, _ := r.get(path...).(float64)
	return f
}

func (r resp) get(path ...string) any {
	var cur any = r.Body
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

func (r resp) list(key string) []any {
	l, _ := r.Body[key].([]any)
	return l
}

type reqOpt func(*http.Request)

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func idem(k string) reqOpt          { return withHeader("Idempotency-Key", k) }
func tenantHdr(slug string) reqOpt  { return withHeader("X-Tenant", slug) }

func (e *env) do(method, path, token string, body any, opts ...reqOpt) resp {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		js, _ := json.Marshal(b)
		rd = bytes.NewReader(js)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, o := range opts {
		o(req)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{Status: res.StatusCode, Raw: raw}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func (e *env) must(r resp, status int) resp {
	e.t.Helper()
	if r.Status != status {
		e.t.Fatalf("want %d, got %d: %s", status, r.Status, r.Raw)
	}
	return r
}

func (e *env) login(slug, email, pw string) string {
	e.t.Helper()
	r := e.must(e.do("POST", "/v1/auth/login", "", map[string]string{"email": email, "password": pw}, tenantHdr(slug)), 200)
	return r.str("access_token")
}

// partner provisions a tenant through the platform API and returns its admin token.
type partner struct {
	ID    string
	Slug  string
	Admin string
}

func (e *env) provision(slug string, features map[string]bool, theme map[string]string) partner {
	e.t.Helper()
	r := e.must(e.do("POST", "/v1/platform/tenants", e.platform, map[string]any{
		"slug": slug, "name": slug + " Ltd", "features": features,
		"branding": map[string]any{"display_name": slug + " Power", "theme": theme, "support_email": "help@" + slug + ".test"},
		"admin":    map[string]string{"email": "admin@" + slug + ".test", "name": "Admin", "password": "admin-pass-1"},
	}), 201)
	return partner{ID: r.str("id"), Slug: slug, Admin: e.login(slug, "admin@"+slug+".test", "admin-pass-1")}
}

type customer struct {
	Token string
	ID    string
	Email string
}

func (e *env) customer(slug, email string) customer {
	e.t.Helper()
	r := e.must(e.do("POST", "/v1/auth/register", "", map[string]string{"email": email, "password": "customer-pass-1", "name": "C", "phone": "0800"},
		tenantHdr(slug)), 201)
	return customer{Token: r.str("access_token"), ID: r.str("user", "id"), Email: email}
}

type meter struct {
	ID     string
	Number string
	Serial string
	Secret string
}

// zieonMeter registers sandbox hardware, assigns it to a tenant and a customer.
func (e *env) zieonMeter(p partner, c customer, number string, caps map[string]bool) meter {
	e.t.Helper()
	if caps == nil {
		caps = map[string]bool{"telemetry": true, "remote_credit": true, "remote_debit": true, "tamper": true}
	}
	serial := "SBX-" + number
	comms := "gsm"
	if len(caps) == 0 {
		comms = "none" // keypad-only meter: no device identity, no telemetry
	}
	r := e.must(e.do("POST", "/v1/platform/meters", e.platform, map[string]any{"serial_number": serial, "meter_number": number,
		"comms_mode": comms, "capabilities": caps, "firmware_version": "1.0"}), 201)
	m := meter{ID: r.str("id"), Number: number, Serial: serial, Secret: r.str("device_secret")}
	e.must(e.do("POST", "/v1/platform/meters/"+m.ID+"/assign", e.platform, map[string]string{"tenant_id": p.ID}), 204)
	e.must(e.do("POST", "/v1/admin/meters/"+m.ID+"/assign", p.Admin, map[string]any{"user_id": c.ID, "label": "Home"}), 204)
	return m
}

func (e *env) fund(c customer, kobo int64) {
	e.t.Helper()
	e.must(e.do("POST", "/v1/wallet/fund", c.Token, map[string]any{"amount_kobo": kobo, "method": "bank_transfer"}, idem(uuid.NewString())), 201)
}

func (e *env) buy(c customer, m meter, kobo int64, opts ...map[string]any) resp {
	e.t.Helper()
	body := map[string]any{"meter_id": m.ID, "amount_kobo": kobo, "payment_method": "wallet"}
	for _, o := range opts {
		for k, v := range o {
			body[k] = v
		}
	}
	return e.do("POST", "/v1/purchases", c.Token, body, idem(uuid.NewString()))
}

func (e *env) walletKobo(c customer) float64 {
	return e.must(e.do("GET", "/v1/wallet", c.Token, nil), 200).num("balance_kobo")
}

func (e *env) signedWebhook(body map[string]any) resp {
	raw, _ := json.Marshal(body)
	return e.do("POST", "/v1/webhooks/payments/sandbox", "", raw, withHeader("Zieon-Signature", secure.SignWebhook([]byte(webhookSecret), time.Now(), raw)))
}

func (e *env) deviceReport(m meter, body map[string]any, nonce string) resp {
	raw, _ := json.Marshal(body)
	ts := fmt.Sprint(time.Now().Unix())
	sig := secure.HMACSHA256Hex([]byte(m.Secret), m.Serial+"\n"+ts+"\n"+nonce+"\n"+string(raw))
	return e.do("POST", "/v1/devices/telemetry", "", raw, withHeader("X-Device-Serial", m.Serial), withHeader("X-Device-Timestamp", ts),
		withHeader("X-Device-Nonce", nonce), withHeader("X-Device-Signature", sig))
}
