// Package auth issues and verifies access tokens and hashes passwords.
//
// Access tokens are compact HS256 JWTs. They are short-lived and carry the
// tenant id, so tenant context always comes from a verified credential and
// never from a request header.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const (
	RolePlatformAdmin = "platform_admin"
	RoleTenantAdmin   = "tenant_admin"
	RoleTenantStaff   = "tenant_staff"
	RoleCustomer      = "customer"
)

// Partner API key scopes.
const (
	ScopeSessionsWrite    = "sessions:write"
	ScopeCustomersRead    = "customers:read"
	ScopeCustomersWrite   = "customers:write"
	ScopeMembershipsWrite = "memberships:write"
	ScopeTransactionsRead = "transactions:read"
	ScopeMetersRead       = "meters:read"
)

var AllScopes = []string{ScopeSessionsWrite, ScopeCustomersRead, ScopeCustomersWrite,
	ScopeMembershipsWrite, ScopeTransactionsRead, ScopeMetersRead}

// Principal is the authenticated caller.
type Principal struct {
	Kind     string    // "user" | "api_key" | "device"
	UserID   uuid.UUID // users
	KeyID    uuid.UUID // api keys
	MeterID  uuid.UUID // devices
	TenantID uuid.UUID
	Role     string
	Scopes   []string
	Method   string // "password" | "partner_sso"
}

func (p Principal) ID() string {
	switch p.Kind {
	case "api_key":
		return "key:" + p.KeyID.String()
	case "device":
		return "device:" + p.MeterID.String()
	}
	return "user:" + p.UserID.String()
}

func (p Principal) HasScope(s string) bool {
	for _, x := range p.Scopes {
		if x == s {
			return true
		}
	}
	return false
}

func (p Principal) IsTenantOperator() bool {
	return p.Kind == "user" && (p.Role == RoleTenantAdmin || p.Role == RoleTenantStaff)
}

type Claims struct {
	Sub    string `json:"sub"`
	Tenant string `json:"tid"`
	Role   string `json:"role"`
	AMR    string `json:"amr"`
	Iss    string `json:"iss"`
	Aud    string `json:"aud"`
	Iat    int64  `json:"iat"`
	Exp    int64  `json:"exp"`
	Jti    string `json:"jti"`
}

type Signer struct {
	key   []byte
	kid   string
	ttl   time.Duration
	clock func() time.Time
}

func NewSigner(key []byte, kid string, ttl time.Duration) *Signer {
	return &Signer{key: key, kid: kid, ttl: ttl, clock: time.Now}
}

const issuer = "zieon-platform"
const audience = "zieon-api"

var b64 = base64.RawURLEncoding

func (s *Signer) Issue(userID, tenantID uuid.UUID, role, method string, ttl time.Duration) (string, time.Time, error) {
	if ttl == 0 {
		ttl = s.ttl
	}
	now := s.clock()
	exp := now.Add(ttl)
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT", "kid": s.kid})
	claims, err := json.Marshal(Claims{
		Sub: userID.String(), Tenant: tenantID.String(), Role: role, AMR: method,
		Iss: issuer, Aud: audience, Iat: now.Unix(), Exp: exp.Unix(), Jti: uuid.NewString(),
	})
	if err != nil {
		return "", time.Time{}, err
	}
	unsigned := b64.EncodeToString(header) + "." + b64.EncodeToString(claims)
	return unsigned + "." + b64.EncodeToString(s.mac(unsigned)), exp, nil
}

var ErrInvalidToken = errors.New("invalid or expired token")

func (s *Signer) Verify(token string) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, ErrInvalidToken
	}
	var header struct{ Alg, Kid string }
	hb, err := b64.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &header) != nil || header.Alg != "HS256" || header.Kid != s.kid {
		return Principal{}, ErrInvalidToken
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, s.mac(parts[0]+"."+parts[1])) {
		return Principal{}, ErrInvalidToken
	}
	cb, err := b64.DecodeString(parts[1])
	var c Claims
	if err != nil || json.Unmarshal(cb, &c) != nil {
		return Principal{}, ErrInvalidToken
	}
	if c.Iss != issuer || c.Aud != audience || s.clock().Unix() >= c.Exp {
		return Principal{}, ErrInvalidToken
	}
	uid, err1 := uuid.Parse(c.Sub)
	tid, err2 := uuid.Parse(c.Tenant)
	if err1 != nil || err2 != nil {
		return Principal{}, ErrInvalidToken
	}
	return Principal{Kind: "user", UserID: uid, TenantID: tid, Role: c.Role, Method: c.AMR}, nil
}

func (s *Signer) mac(msg string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

const bcryptCost = 12

func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	return string(b), err
}

func CheckPassword(hash, pw string) bool {
	return hash != "" && bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// dummyHash keeps login timing similar for unknown accounts.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("timing-equaliser"), bcryptCost)

func BurnPasswordCheck(pw string) { _ = bcrypt.CompareHashAndPassword(dummyHash, []byte(pw)) }
