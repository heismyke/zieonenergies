package mqttgw

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCert(t *testing.T, dir, cn string, mod time.Time) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{cn}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	cp, kp := filepath.Join(dir, "site.crt"), filepath.Join(dir, "site.key")
	_ = os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	_ = os.Chtimes(cp, mod, mod)
	_ = os.Chtimes(kp, mod, mod)
}

func TestCertReloaderPicksUpRenewal(t *testing.T) {
	dir := t.TempDir()
	writeCert(t, dir, "first.example", time.Now().Add(-time.Hour))
	r := &certReloader{certGlob: filepath.Join(dir, "*.crt"), keyGlob: filepath.Join(dir, "*.key")}
	c1, err := r.get(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf1, _ := x509.ParseCertificate(c1.Certificate[0])
	writeCert(t, dir, "renewed.example", time.Now())
	if c, _ := r.get(nil); c != c1 {
		t.Fatal("cert should be cached for a minute")
	}
	r.checked = time.Now().Add(-2 * time.Minute)
	c2, err := r.get(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf2, _ := x509.ParseCertificate(c2.Certificate[0])
	if leaf1.Subject.CommonName != "first.example" || leaf2.Subject.CommonName != "renewed.example" {
		t.Fatalf("renewal not picked up: %s -> %s", leaf1.Subject.CommonName, leaf2.Subject.CommonName)
	}
	if _, err := (&certReloader{certGlob: filepath.Join(dir, "missing*.crt"), keyGlob: "x"}).get(nil); err == nil {
		t.Fatal("missing certificate must fail at startup")
	}
}
