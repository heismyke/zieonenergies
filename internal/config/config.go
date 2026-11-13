// Package config loads runtime configuration from environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env            string // development | test | pilot | production
	Port           string
	DatabaseURL    string
	AllowedOrigins []string
	PublicBaseURL  string // e.g. https://zieon-api.example.com
	EmbedBaseURL   string // hosted electricity module, e.g. https://app.zenerlink.app

	JWTSigningKey []byte
	JWTKeyID      string
	AccessTTL     time.Duration
	RefreshTTL    time.Duration
	EmbedTTL      time.Duration

	// DataKeys holds AES-256 keys by id; DataKeyID is used for new writes.
	DataKeys  map[string][]byte
	DataKeyID string

	PaystackSecretKey    string
	SandboxWebhookSecret string

	// VTpass electricity aggregator (DisCo meters). Empty API key = disabled.
	VTpassBaseURL       string
	VTpassAPIKey        string
	VTpassPublicKey     string
	VTpassSecretKey     string
	VTpassFallbackPhone string

	// Token receipts outside the app. Empty key = that channel is off.
	ResendAPIKey   string
	EmailFrom      string // e.g. "Zenerlink <receipts@zenerlink.com>"
	TermiiAPIKey   string
	TermiiSenderID string
	TermiiBaseURL  string
	TermiiWhatsApp bool // the Termii account has a WhatsApp sender set up

	TrustProxyHeaders bool
	WorkersEnabled    bool

	// MQTT device gateway. Any non-empty listen address enables it.
	MQTTTCPAddr    string   // plain TCP, lab/local only (e.g. 127.0.0.1:1883)
	MQTTTLSAddr    string   // TLS, for devices (e.g. :8883)
	MQTTTLSCert    string   // PEM path or glob; re-read on renewal
	MQTTTLSKey     string   // PEM path or glob
	MQTTWSAddr     string   // websocket listener, put behind a TLS proxy (wss)
	MQTTPublicURLs []string // advertised to devices at provisioning
}

func (c Config) MQTTEnabled() bool {
	return c.MQTTTCPAddr != "" || c.MQTTTLSAddr != "" || c.MQTTWSAddr != ""
}

// SandboxAllowed reports whether simulated payments/vending may be used.
// Production refuses them: real money must never meet simulated credit.
func (c Config) SandboxAllowed() bool { return c.Env != "production" }

func Load() (Config, error) {
	c := Config{
		Env:                  get("APP_ENV", "development"),
		Port:                 get("PORT", "8080"),
		DatabaseURL:          os.Getenv("DATABASE_URL"),
		PublicBaseURL:        strings.TrimRight(get("PUBLIC_BASE_URL", "http://localhost:8080"), "/"),
		EmbedBaseURL:         strings.TrimRight(get("EMBED_BASE_URL", "http://localhost:5173"), "/"),
		JWTKeyID:             get("JWT_KEY_ID", "k1"),
		AccessTTL:            dur("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTTL:           dur("REFRESH_TOKEN_TTL", 30*24*time.Hour),
		EmbedTTL:             dur("EMBED_CODE_TTL", 2*time.Minute),
		PaystackSecretKey:    os.Getenv("PAYSTACK_SECRET_KEY"),
		ResendAPIKey:         os.Getenv("RESEND_API_KEY"),
		EmailFrom:            get("EMAIL_FROM", "Zenerlink <receipts@zenerlink.com>"),
		TermiiAPIKey:         os.Getenv("TERMII_API_KEY"),
		TermiiSenderID:       get("TERMII_SENDER_ID", "Zenerlink"),
		TermiiBaseURL:        get("TERMII_BASE_URL", "https://v3.api.termii.com"),
		TermiiWhatsApp:       os.Getenv("TERMII_WHATSAPP") == "true",
		SandboxWebhookSecret: os.Getenv("SANDBOX_WEBHOOK_SECRET"),
		TrustProxyHeaders:    get("TRUST_PROXY_HEADERS", "false") == "true",
		WorkersEnabled:       get("WORKERS_ENABLED", "true") == "true",
		DataKeys:             map[string][]byte{},
		VTpassBaseURL:        get("VTPASS_BASE_URL", "https://sandbox.vtpass.com"),
		VTpassAPIKey:         os.Getenv("VTPASS_API_KEY"),
		VTpassPublicKey:      os.Getenv("VTPASS_PUBLIC_KEY"),
		VTpassSecretKey:      os.Getenv("VTPASS_SECRET_KEY"),
		VTpassFallbackPhone:  get("VTPASS_FALLBACK_PHONE", "08011111111"),
		MQTTTCPAddr:          os.Getenv("MQTT_TCP_ADDR"),
		MQTTTLSAddr:          os.Getenv("MQTT_TLS_ADDR"),
		MQTTTLSCert:          os.Getenv("MQTT_TLS_CERT_FILE"),
		MQTTTLSKey:           os.Getenv("MQTT_TLS_KEY_FILE"),
		MQTTWSAddr:           os.Getenv("MQTT_WS_ADDR"),
	}
	for _, u := range strings.Split(os.Getenv("MQTT_PUBLIC_URLS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			c.MQTTPublicURLs = append(c.MQTTPublicURLs, u)
		}
	}
	if c.MQTTTLSAddr != "" && (c.MQTTTLSCert == "" || c.MQTTTLSKey == "") {
		return c, errors.New("MQTT_TLS_ADDR requires MQTT_TLS_CERT_FILE and MQTT_TLS_KEY_FILE")
	}
	if c.Env == "production" && c.MQTTTCPAddr != "" && !strings.HasPrefix(c.MQTTTCPAddr, "127.0.0.1:") {
		return c, errors.New("plain-TCP MQTT may only listen on 127.0.0.1 in production")
	}
	for _, o := range strings.Split(os.Getenv("ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.AllowedOrigins = append(c.AllowedOrigins, strings.TrimRight(o, "/"))
		}
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	jwtKey, err := decodeKey(os.Getenv("JWT_SIGNING_KEY"))
	if err != nil || len(jwtKey) < 32 {
		return c, errors.New("JWT_SIGNING_KEY must be base64 for at least 32 random bytes")
	}
	c.JWTSigningKey = jwtKey

	// DATA_ENCRYPTION_KEYS="k1:<base64>,k2:<base64>"; DATA_ENCRYPTION_KEY_ID selects the active key.
	for _, part := range strings.Split(os.Getenv("DATA_ENCRYPTION_KEYS"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, val, ok := strings.Cut(part, ":")
		if !ok {
			return c, fmt.Errorf("DATA_ENCRYPTION_KEYS entry %q must be id:base64", id)
		}
		k, err := decodeKey(val)
		if err != nil || len(k) != 32 {
			return c, fmt.Errorf("data key %q must be base64 for exactly 32 bytes", id)
		}
		c.DataKeys[id] = k
	}
	c.DataKeyID = get("DATA_ENCRYPTION_KEY_ID", "k1")
	if _, ok := c.DataKeys[c.DataKeyID]; !ok {
		return c, fmt.Errorf("DATA_ENCRYPTION_KEYS has no active key %q", c.DataKeyID)
	}
	if c.VTpassAPIKey != "" && c.VTpassSecretKey == "" {
		return c, errors.New("VTPASS_SECRET_KEY is required when VTPASS_API_KEY is set")
	}
	if c.Env == "production" {
		if c.VTpassAPIKey != "" && strings.Contains(c.VTpassBaseURL, "sandbox") {
			return c, errors.New("production must not use the VTpass sandbox")
		}
		if c.PaystackSecretKey == "" {
			return c, errors.New("production requires a live payment provider (PAYSTACK_SECRET_KEY)")
		}
	}
	return c, nil
}

func decodeKey(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

func get(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func dur(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}
