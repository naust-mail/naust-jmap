package deliver

// HTTP ingest adapter tests: the same delivery verdicts LMTP returns, but
// projected as a JSON results array, plus the surface's own error mapping
// (method, missing recipient, declared oversize).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/datatypes/mail"
)

// postIngest builds an ingest POST with the given envelope headers and body
// and returns the recorded response.
func postIngest(t *testing.T, h *HTTPIngest, from string, rcpts []string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(body))
	if from != "" {
		req.Header.Set(headerMailFrom, from)
	}
	for _, r := range rcpts {
		req.Header.Add(headerRcptTo, r)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeResults parses the JSON results body.
func decodeResults(t *testing.T, rec *httptest.ResponseRecorder) []httpResult {
	t.Helper()
	var out []httpResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode results: %v (body=%q)", err, rec.Body.String())
	}
	return out
}

// TestHTTPIngestHappyPath: a resolvable recipient yields 200 and one accepted
// result carrying the created Email and blob ids.
func TestHTTPIngestHappyPath(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	h := NewHTTPIngest(mustDeliverer(t, db, store, mapResolver{"jane@example.com": testAccount}), DefaultHTTPIngestConfig())

	rec := postIngest(t, h, "joe@example.com", []string{"jane@example.com"}, simpleMessage)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	res := decodeResults(t, rec)
	if len(res) != 1 || res[0].Outcome != "accepted" {
		t.Fatalf("want one accepted result, got %+v", res)
	}
	if res[0].EmailId == "" || res[0].BlobId == "" {
		t.Fatalf("result missing ids: %+v", res[0])
	}
}

// TestHTTPIngestMixedResults: recipients given as one comma-separated header,
// one resolvable and one not, still return 200 with per-recipient verdicts in
// order.
func TestHTTPIngestMixedResults(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	h := NewHTTPIngest(mustDeliverer(t, db, store, mapResolver{"a@example.com": testAccount}), DefaultHTTPIngestConfig())

	rec := postIngest(t, h, "s@example.com", []string{"a@example.com, ghost@example.com"}, simpleMessage)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	res := decodeResults(t, rec)
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(res), res)
	}
	if res[0].Outcome != "accepted" || res[1].Outcome != "rejected" {
		t.Fatalf("wrong verdicts/order: %+v", res)
	}
}

// TestHTTPIngestDeclaredOversize: a client-declared Content-Length over the
// cap is refused up front with 413, before the body is delivered.
func TestHTTPIngestDeclaredOversize(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	h := NewHTTPIngest(mustDeliverer(t, db, store, mapResolver{"jane@example.com": testAccount}, Config{MaxMessageSize: 16}), DefaultHTTPIngestConfig())

	req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(simpleMessage))
	req.Header.Set(headerRcptTo, "jane@example.com")
	req.ContentLength = 1 << 20 // declare far more than the 16-byte cap
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

// TestHTTPIngestNoRecipient: a POST with no recipient header is a 400.
func TestHTTPIngestNoRecipient(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	h := NewHTTPIngest(mustDeliverer(t, db, store, mapResolver{}), DefaultHTTPIngestConfig())

	rec := postIngest(t, h, "s@example.com", nil, simpleMessage)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestHTTPIngestMethodNotAllowed: anything but POST is a 405 with an Allow
// header.
func TestHTTPIngestMethodNotAllowed(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	h := NewHTTPIngest(mustDeliverer(t, db, store, mapResolver{}), DefaultHTTPIngestConfig())

	req := httptest.NewRequest(http.MethodGet, "/ingest", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", rec.Header().Get("Allow"))
	}
}

// TestHTTPIngestVacationReply: the HTTP adapter runs the deferred
// auto-response after writing its results, so a delivery for an
// active-vacation recipient still queues the RFC 3834 reply.
func TestHTTPIngestVacationReply(t *testing.T) {
	ts, db, store, q, _, _ := newEmailServer(t, mail.DefaultAccountCapability())
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
	createIdentity(t, ts, "john@example.com")
	enableVacation(t, ts, `,"textBody":"I am away."`)
	h := NewHTTPIngest(mustDeliverer(t, db, store,
		mapResolver{"john@example.com": testAccount}, Config{MaxMessageSize: defaultMaxMessageSize, VacationQueue: q}), DefaultHTTPIngestConfig())

	raw := bodyMsg("visitor@remote.example", "john@example.com", "hi",
		"are you around?", map[string]string{"Message-ID": "<http-vac-1@remote.example>"})
	rec := postIngest(t, h, "visitor@remote.example", []string{"john@example.com"}, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%q)", rec.Code, rec.Body.String())
	}
	if n := submissionCount(t, db); n != 1 {
		t.Fatalf("queued replies = %d, want 1", n)
	}
}
