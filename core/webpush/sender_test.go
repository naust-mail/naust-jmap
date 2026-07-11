package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

func TestSendRejectsNonHTTPS(t *testing.T) {
	var s Sender
	for _, u := range []string{"http://push.example.com/x", "ftp://push.example.com/x", "://bad"} {
		if _, err := s.Send(context.Background(), u, nil, []byte("{}"), 60); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
}

// TestSendBlocksPrivate proves the RFC 8620 section 8.6 SSRF defence:
// the default client refuses to dial loopback/private addresses, and
// because the check runs after name resolution, a hostname pointing at
// one is equally refused.
func TestSendBlocksPrivate(t *testing.T) {
	var s Sender
	for _, u := range []string{"https://127.0.0.1:9/x", "https://[::1]:9/x", "https://localhost:9/x"} {
		_, err := s.Send(context.Background(), u, nil, []byte("{}"), 60)
		if !errors.Is(err, ErrPrivateHost) {
			t.Errorf("%s: %v, want ErrPrivateHost", u, err)
		}
	}
}

// TestSendHeaders checks the RFC 8620 section 7.2 POST shape: JSON
// content type, the RFC 8030 TTL header, and - with keys - a body
// under the aes128gcm content encoding that the keys decrypt.
func TestSendHeaders(t *testing.T) {
	type hit struct {
		header http.Header
		body   []byte
	}
	hits := make(chan hit, 2)
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits <- hit{r.Header.Clone(), body}
		w.WriteHeader(http.StatusCreated)
	}))
	defer endpoint.Close()
	s := Sender{Client: endpoint.Client()}
	payload := []byte(`{"@type":"PushVerification","pushSubscriptionId":"P1","verificationCode":"c"}`)

	status, err := s.Send(context.Background(), endpoint.URL, nil, payload, 43200)
	if err != nil || status != http.StatusCreated {
		t.Fatalf("plain send: %d, %v", status, err)
	}
	h := <-hits
	if got := h.header.Get("TTL"); got != "43200" {
		t.Errorf("TTL %q", got)
	}
	if got := h.header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type %q", got)
	}
	if h.header.Get("Content-Encoding") != "" || string(h.body) != string(payload) {
		t.Errorf("plain send was mangled: %q %q", h.header.Get("Content-Encoding"), h.body)
	}

	uaKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatal(err)
	}
	keys := &jmap.PushKeys{
		P256dh: base64.RawURLEncoding.EncodeToString(uaKey.PublicKey().Bytes()),
		Auth:   base64.RawURLEncoding.EncodeToString(authSecret),
	}
	if _, err := s.Send(context.Background(), endpoint.URL, keys, payload, 60); err != nil {
		t.Fatal(err)
	}
	h = <-hits
	if got := h.header.Get("Content-Encoding"); got != "aes128gcm" {
		t.Fatalf("Content-Encoding %q", got)
	}
	got, err := Decrypt(uaKey, authSecret, h.body)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("encrypted body: %q, %v", got, err)
	}
}
