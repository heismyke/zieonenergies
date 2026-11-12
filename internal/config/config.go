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

	TrustProxyHeaders bool
	WorkersEnabled    bool
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
		SandboxWebhookSecret: os.Getenv("SANDBOX_WEBHOOK_SECRET"),
		TrustProxyHeaders:    get("TRUST_PROXY_HEADERS", "false") == "true",
		WorkersEnabled:       get("WORKERS_ENABLED", "true") == "true",
		DataKeys:             map[string][]byte{},
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
	if c.Env == "production" {
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
