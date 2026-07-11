package webpush

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRFC8291Vector drives the deterministic core with the published
// key pair and salt of RFC 8291 section 5 / appendix A and demands the
// exact body from the RFC.
func TestRFC8291Vector(t *testing.T) {
	asKey, err := ecdh.P256().NewPrivateKey(b64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	uaPublic, authSecret, err := DecodeKeys(
		"BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
		"BTBZMqHH6r4Tts7J_aSIgg")
	if err != nil {
		t.Fatal(err)
	}
	salt := b64(t, "DGv6ra1nlYgDCS1FRnbzlw")
	plaintext := []byte("When I grow up, I want to be a watermelon")

	body, err := encrypt(uaPublic, authSecret, asKey, salt, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	want := b64(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml"+
		"mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT"+
		"pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN")
	if !bytes.Equal(body, want) {
		t.Fatalf("body mismatch:\n got %x\nwant %x", body, want)
	}

	// The user agent side decrypts the same body with its private key.
	uaKey, err := ecdh.P256().NewPrivateKey(b64(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decrypt(uaKey, authSecret, body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("decrypt: %q", got)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	uaKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(`{"@type":"StateChange","changed":{"A1":{"Email":"7"}}}`)

	body, err := Encrypt(uaKey.PublicKey(), authSecret, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decrypt(uaKey, authSecret, body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round trip: %q", got)
	}

	// A flipped ciphertext bit or the wrong auth secret must not open.
	body[len(body)-1] ^= 1
	if _, err := Decrypt(uaKey, authSecret, body); err == nil {
		t.Fatal("tampered body decrypted")
	}
	body[len(body)-1] ^= 1
	authSecret[0] ^= 1
	if _, err := Decrypt(uaKey, authSecret, body); err == nil {
		t.Fatal("wrong auth secret decrypted")
	}
}

func TestEncryptTooLarge(t *testing.T) {
	uaKey, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	// MaxPlaintext fits exactly in the RFC 8030 4096-octet limit.
	body, err := Encrypt(uaKey.PublicKey(), auth, make([]byte, MaxPlaintext))
	if err != nil || len(body) != 4096 {
		t.Fatalf("at MaxPlaintext: len %d, %v", len(body), err)
	}
	if _, err := Encrypt(uaKey.PublicKey(), auth, make([]byte, MaxPlaintext+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over MaxPlaintext: %v", err)
	}
}

func TestDecodeKeysRejects(t *testing.T) {
	uaKey, _ := ecdh.P256().GenerateKey(rand.Reader)
	goodPub := base64.RawURLEncoding.EncodeToString(uaKey.PublicKey().Bytes())
	goodAuth := base64.RawURLEncoding.EncodeToString(make([]byte, 16))

	// A 65-octet blob that is not a point on P-256 MUST be rejected
	// (RFC 8291 section 7: failure to validate can leak the private key).
	offCurve := make([]byte, 65)
	offCurve[0] = 0x04
	for _, tc := range []struct{ name, p256dh, auth string }{
		{"off-curve point", base64.RawURLEncoding.EncodeToString(offCurve), goodAuth},
		{"short auth", goodPub, base64.RawURLEncoding.EncodeToString(make([]byte, 15))},
		{"bad base64", "!!!", goodAuth},
	} {
		if _, _, err := DecodeKeys(tc.p256dh, tc.auth); !errors.Is(err, ErrKeys) {
			t.Errorf("%s: %v, want ErrKeys", tc.name, err)
		}
	}

	// Padded base64 is fine (RFC 4648 allows either).
	paddedAuth := base64.URLEncoding.EncodeToString(make([]byte, 16))
	if _, _, err := DecodeKeys(goodPub, paddedAuth); err != nil {
		t.Errorf("padded auth: %v", err)
	}
}
