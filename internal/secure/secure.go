// Package secure holds the small cryptographic primitives the platform relies
// on: envelope encryption with key rotation, hashing of bearer secrets, HMAC
// signatures and random token generation.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Keyring encrypts with the active key and decrypts with any known key, so
// keys can be rotated by adding a new id, switching the active id, and
// re-encrypting rows at leisure (see docs/SECURITY_AND_THREAT_MODEL.md).
type Keyring struct {
	keys   map[string][]byte
	active string
}

func NewKeyring(keys map[string][]byte, active string) (*Keyring, error) {
	if _, ok := keys[active]; !ok {
		return nil, fmt.Errorf("active key %q not in keyring", active)
	}
	return &Keyring{keys: keys, active: active}, nil
}

// Encrypt returns "<keyid>:" || nonce || AES-256-GCM ciphertext.
func (k *Keyring) Encrypt(plain []byte) ([]byte, error) {
	gcm, err := k.gcm(k.active)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte(k.active+":"), nonce...)
	return gcm.Seal(out, nonce, plain, []byte(k.active)), nil
}

func (k *Keyring) Decrypt(blob []byte) ([]byte, error) {
	id, rest, ok := strings.Cut(string(blob), ":")
	if !ok {
		return nil, errors.New("secure: malformed ciphertext")
	}
	gcm, err := k.gcm(id)
	if err != nil {
		return nil, err
	}
	raw := []byte(rest)
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("secure: short ciphertext")
	}
	return gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(id))
}

func (k *Keyring) gcm(id string) (cipher.AEAD, error) {
	key, ok := k.keys[id]
	if !ok {
		return nil, fmt.Errorf("secure: unknown key id %q", id)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// HashSecret is used for high-entropy bearer secrets (refresh tokens, API keys,
// embed codes, energy tokens). Passwords use bcrypt instead.
func HashSecret(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func RandomToken(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// RandomDigits returns n cryptographically random decimal digits.
func RandomDigits(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		d, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			panic(err)
		}
		sb.WriteByte(byte('0' + d.Int64()))
	}
	return sb.String()
}

func HMACSHA256Hex(key []byte, msg string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func HMACSHA512Hex(key []byte, msg []byte) string {
	m := hmac.New(sha512.New, key)
	m.Write(msg)
	return hex.EncodeToString(m.Sum(nil))
}

func EqualHex(a, b string) bool {
	x, err1 := hex.DecodeString(a)
	y, err2 := hex.DecodeString(b)
	return err1 == nil && err2 == nil && hmac.Equal(x, y)
}

// SignWebhook produces the Zieon-Signature header value: t=<unix>,v1=<hex>,
// where v1 = HMAC-SHA256(secret, "<unix>.<body>").
func SignWebhook(secret []byte, ts time.Time, body []byte) string {
	t := strconv.FormatInt(ts.Unix(), 10)
	return "t=" + t + ",v1=" + HMACSHA256Hex(secret, t+"."+string(body))
}

// VerifyWebhook checks a Zieon-Signature header within the tolerance window.
func VerifyWebhook(secret []byte, header string, body []byte, now time.Time, tolerance time.Duration) bool {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sig == "" {
		return false
	}
	if d := now.Sub(time.Unix(unix, 0)); d > tolerance || d < -tolerance {
		return false
	}
	return EqualHex(sig, HMACSHA256Hex(secret, ts+"."+string(body)))
}
