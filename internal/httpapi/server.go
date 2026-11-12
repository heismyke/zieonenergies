// Package httpapi exposes the platform over a versioned JSON REST API (/v1).
// Handlers decode and validate input, resolve the caller, and delegate to
// app.Service; they contain no business rules.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/heismyke/zieonenergies/internal/app"
	"github.com/heismyke/zieonenergies/internal/auth"
	"github.com/heismyke/zieonenergies/internal/ratelimit"
)

type Server struct {
	svc           *app.Service
	log           *slog.Logger
	defaultTenant string
	mux           *http.ServeMux

	authLimiter    *ratelimit.Limiter // per IP on credential endpoints
	userLimiter    *ratelimit.Limiter // per principal
	partnerLimiter *ratelimit.Limiter // per API key
	deviceLimiter  *ratelimit.Limiter // per meter
	publicLimiter  *ratelimit.Limiter // per IP, unauthenticated reads
	lookupLimiter  *ratelimit.Limiter // per IP, public meter verification
	orderLimiter   *ratelimit.Limiter // per IP, guest order creation
}

func New(svc *app.Service, log *slog.Logger, defaultTenant string) *Server {
	s := &Server{
		svc: svc, log: log, defaultTenant: defaultTenant, mux: http.NewServeMux(),
		authLimiter:    ratelimit.New(20, 10),
		userLimiter:    ratelimit.New(240, 60),
		partnerLimiter: ratelimit.New(600, 100),
		deviceLimiter:  ratelimit.New(60, 20),
		publicLimiter:  ratelimit.New(120, 40),
		lookupLimiter:  ratelimit.New(30, 10),
		orderLimiter:   ratelimit.New(10, 5),
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.recoverer(s.requestMeta(s.securityHeaders(s.cors(s.mux))))
}

// ---------------------------------------------------------------------------
// Handler plumbing
// ---------------------------------------------------------------------------

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

func (s *Server) handle(pattern string, h handlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			s.writeError(w, r, err)
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, status int, body []byte) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, err := w.Write(body)
	return err
}

type apiErr struct {
	Error     *app.Error `json:"error"`
	RequestID string     `json:"request_id"`
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *app.Error
	if !errors.As(err, &ae) {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.log.Error("request failed", "path", r.URL.Path, "request_id", app.Meta(r.Context()).RequestID, "err", err)
		ae = &app.Error{Status: http.StatusInternalServerError, Code: "internal_error", Message: "something went wrong; please retry"}
	}
	_ = writeJSON(w, ae.Status, apiErr{Error: ae, RequestID: app.Meta(r.Context()).RequestID})
}

func badRequest(code, msg string) error {
	return &app.Error{Status: http.StatusBadRequest, Code: code, Message: msg}
}

const maxBody = 1 << 20

// decode reads a strict JSON body: unknown fields and trailing data are rejected.
func decode(r *http.Request, v any) error {
	body, err := readBody(r)
	if err != nil {
		return err
	}
	return decodeBytes(body, v)
}

func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, badRequest("invalid_body", "could not read request body")
	}
	if len(body) > maxBody {
		return nil, &app.Error{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large", Message: "request body exceeds 1 MiB"}
	}
	return body, nil
}

func decodeBytes(body []byte, v any) error {
	if len(body) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid_json", "invalid JSON body: "+strings.TrimPrefix(err.Error(), "json: "))
	}
	if dec.More() {
		return badRequest("invalid_json", "unexpected data after JSON body")
	}
	return nil
}

func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, app.ErrNotFound
	}
	return id, nil
}

func queryInt(r *http.Request, name string, def, maxV int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, maxV)
}

func queryUUID(r *http.Request, name string) (*uuid.UUID, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil, badRequest("invalid_query", name+" must be a UUID")
	}
	return &id, nil
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

func (s *Server) clientIP(r *http.Request) string {
	if s.svc.Cfg.TrustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) requestMeta(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-Id")
		if rid == "" || len(rid) > 64 {
			rid = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", rid)
		ctx := app.WithMeta(r.Context(), app.RequestMeta{IP: s.clientIP(r), RequestID: rid})
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r.WithContext(ctx))
		if r.URL.Path == "/healthz" {
			return
		}
		m := app.Meta(ctx)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds(),
			"request_id", rid, "ip", m.IP)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "err", fmt.Sprint(rec), "stack", string(debug.Stack()))
				http.Error(w, `{"error":{"code":"internal_error","message":"internal error"}}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if s.svc.Cfg.Env != "development" {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// cors allows the configured first-party apps plus each tenant's approved origins.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(r.Header.Get("Origin"), "/")
		if origin != "" && s.originAllowed(r.Context(), origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Vary", "Origin")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Tenant, X-Request-Id")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Expose-Headers", "X-Request-Id, Idempotent-Replayed")
			h.Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) originAllowed(ctx context.Context, origin string) bool {
	for _, o := range s.svc.Cfg.AllowedOrigins {
		if o == origin {
			return true
		}
	}
	return s.svc.OriginAllowed(ctx, origin)
}

func tooMany() error {
	return &app.Error{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "too many requests; slow down"}
}

// ---------------------------------------------------------------------------
// Caller resolution
// ---------------------------------------------------------------------------

// publicTenant resolves the tenant for unauthenticated calls (login, sign-up,
// branding). It is a public identifier only; once a user authenticates, the
// tenant comes exclusively from the signed token.
func (s *Server) publicTenant(r *http.Request) (app.Tenant, error) {
	if slug := r.Header.Get("X-Tenant"); slug != "" {
		return s.svc.TenantBySlug(r.Context(), slug)
	}
	if slug := r.URL.Query().Get("tenant"); slug != "" {
		return s.svc.TenantBySlug(r.Context(), slug)
	}
	if t, err := s.svc.TenantByHost(r.Context(), r.Host); err == nil {
		return t, nil
	}
	return s.svc.TenantBySlug(r.Context(), s.defaultTenant)
}

type caller struct {
	P      auth.Principal
	User   app.User
	Tenant app.Tenant
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

var errUnauthenticated = &app.Error{Status: http.StatusUnauthorized, Code: "unauthenticated", Message: "a valid access token is required"}

// user authenticates a bearer access token and enforces roles. An X-Tenant
// header that disagrees with the token is rejected rather than ignored.
func (s *Server) user(r *http.Request, roles ...string) (caller, error) {
	tok := bearer(r)
	if tok == "" || strings.HasPrefix(tok, "zk_") {
		return caller{}, errUnauthenticated
	}
	p, err := s.svc.Signer.Verify(tok)
	if err != nil {
		return caller{}, errUnauthenticated
	}
	if !s.userLimiter.Allow(p.ID()) {
		return caller{}, tooMany()
	}
	u, t, err := s.svc.ActiveUser(r.Context(), p)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			return caller{}, errUnauthenticated
		}
		return caller{}, err
	}
	if h := r.Header.Get("X-Tenant"); h != "" && !strings.EqualFold(h, t.Slug) {
		return caller{}, &app.Error{Status: http.StatusForbidden, Code: "tenant_mismatch", Message: "X-Tenant does not match your session"}
	}
	if len(roles) > 0 {
		ok := false
		for _, role := range roles {
			ok = ok || p.Role == role
		}
		if !ok {
			return caller{}, app.ErrForbidden
		}
	}
	if p.Role == auth.RolePlatformAdmin && !t.IsPlatform {
		return caller{}, app.ErrForbidden
	}
	*r = *r.WithContext(app.WithMeta(r.Context(), app.RequestMeta{IP: app.Meta(r.Context()).IP, RequestID: app.Meta(r.Context()).RequestID, Principal: p}))
	return caller{P: p, User: u, Tenant: t}, nil
}

func (s *Server) customer(r *http.Request) (caller, error) { return s.user(r, auth.RoleCustomer) }

func (s *Server) operator(r *http.Request, adminOnly bool) (caller, error) {
	if adminOnly {
		return s.user(r, auth.RoleTenantAdmin)
	}
	return s.user(r, auth.RoleTenantAdmin, auth.RoleTenantStaff)
}

func (s *Server) platform(r *http.Request) (caller, error) { return s.user(r, auth.RolePlatformAdmin) }

func (s *Server) apiKey(r *http.Request, scope string) (caller, error) {
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		key = bearer(r)
	}
	if !strings.HasPrefix(key, "zk_") {
		return caller{}, &app.Error{Status: http.StatusUnauthorized, Code: "invalid_api_key", Message: "partner API key required"}
	}
	p, err := s.svc.AuthenticateAPIKey(r.Context(), key)
	if err != nil {
		return caller{}, err
	}
	if !s.partnerLimiter.Allow(p.ID()) {
		return caller{}, tooMany()
	}
	if !p.HasScope(scope) {
		return caller{}, &app.Error{Status: http.StatusForbidden, Code: "insufficient_scope", Message: "API key lacks scope " + scope}
	}
	t, err := s.svc.TenantByID(r.Context(), p.TenantID)
	if err != nil {
		return caller{}, err
	}
	*r = *r.WithContext(app.WithMeta(r.Context(), app.RequestMeta{IP: app.Meta(r.Context()).IP, RequestID: app.Meta(r.Context()).RequestID, Principal: p}))
	return caller{P: p, Tenant: t}, nil
}

// idempotent wraps a mutating handler with Idempotency-Key semantics.
func (s *Server) idempotent(w http.ResponseWriter, r *http.Request, c caller, body []byte, fn func() (int, any, error)) error {
	status, resp, err := s.svc.Idempotent(r.Context(), c.P.TenantID, c.P.ID()+":"+r.Method+" "+r.URL.Path, r.Header.Get("Idempotency-Key"), body, fn)
	if err != nil {
		return err
	}
	return writeRaw(w, status, resp)
}
