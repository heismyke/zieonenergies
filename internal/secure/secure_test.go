package secure

import (
	"bytes"
	"crypto/rand"
	"testing"
	"time"
)

func key() []byte { b := make([]byte, 32); rand.Read(b); return b }

func TestKeyringRotation(t *testing.T) {
	k1, k2 := key(), key()
	old, _ := NewKeyring(map[string][]byte{"k1": k1}, "k1")
	blob, err := old.Encrypt([]byte("meter-secret"))
	if err != nil {
		t.Fatal(err)
	}
	rotated, _ := NewKeyring(map[string][]byte{"k1": k1, "k2": k2}, "k2")
	got, err := rotated.Decrypt(blob)
	if err != nil || !bytes.Equal(got, []byte("meter-secret")) {
		t.Fatalf("decrypt old ciphertext after rotation: %v %q", err, got)
	}
	nb, _ := rotated.Encrypt([]byte("x"))
	if !bytes.HasPrefix(nb, []byte("k2:")) {
		t.Fatal("new writes must use active key")
	}
	tampered := append([]byte{}, blob...)
	tampered[len(tampered)-1] ^= 1
	if _, err := rotated.Decrypt(tampered); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
}

func TestWebhookSignature(t *testing.T) {
	s := []byte("whsec")
	now := time.Now()
	h := SignWebhook(s, now, []byte(`{"a":1}`))
	if !VerifyWebhook(s, h, []byte(`{"a":1}`), now, 5*time.Minute) {
		t.Fatal("valid signature rejected")
	}
	if VerifyWebhook(s, h, []byte(`{"a":2}`), now, 5*time.Minute) {
		t.Fatal("modified body accepted")
	}
	if VerifyWebhook(s, h, []byte(`{"a":1}`), now.Add(10*time.Minute), 5*time.Minute) {
		t.Fatal("replayed (stale) signature accepted")
	}
}

func TestRandomDigits(t *testing.T) {
	d := RandomDigits(20)
	if len(d) != 20 {
		t.Fatal(d)
	}
	for _, c := range d {
		if c < '0' || c > '9' {
			t.Fatal(d)
		}
	}
}
