package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
)

func testServer(t *testing.T, core jmap.CoreCapabilities) *httptest.Server {
	t.Helper()
	a := auth.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	srv, err := NewServer(a, NewProcessor(), "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, ts *httptest.Server, path, user, pass string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func post(t *testing.T, ts *httptest.Server, body, contentType string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api", strings.NewReader(body))
	req.SetBasicAuth("john@example.com", "secret")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSessionRequiresAuth(t *testing.T) {
	ts := testServer(t, DefaultCoreCapabilities())
	resp := get(t, ts, "/.well-known/jmap", "", "")
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Errorf("status %d, WWW-Authenticate %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	resp = get(t, ts, "/.well-known/jmap", "john@example.com", "wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad password -> %d", resp.StatusCode)
	}
}

// RFC 8620 section 2: the session resource shape.
func TestSessionShape(t *testing.T) {
	ts := testServer(t, DefaultCoreCapabilities())
	resp := get(t, ts, "/.well-known/jmap", "john@example.com", "secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store per section 2", cc)
	}
	var session jmap.Session
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	coreRaw, ok := session.Capabilities[jmap.CoreCapability]
	if !ok {
		t.Fatal("capabilities must include urn:ietf:params:jmap:core")
	}
	var core jmap.CoreCapabilities
	if err := json.Unmarshal(coreRaw, &core); err != nil {
		t.Fatal(err)
	}
	if core.MaxSizeRequest != 10_000_000 || core.MaxCallsInRequest != 16 || len(core.CollationAlgorithms) == 0 {
		t.Errorf("core capability object = %+v", core)
	}
	acc, ok := session.Accounts["Atest1"]
	if !ok || !acc.IsPersonal || acc.IsReadOnly || acc.Name != "john@example.com" {
		t.Errorf("accounts = %+v", session.Accounts)
	}
	if _, hasCore := session.PrimaryAccounts[jmap.CoreCapability]; hasCore {
		t.Error("core SHOULD NOT appear in primaryAccounts")
	}
	if session.Username != "john@example.com" {
		t.Errorf("username = %q", session.Username)
	}
	// URL template variables required by section 2.
	for url, vars := range map[string][]string{
		session.DownloadURL:    {"{accountId}", "{blobId}", "{type}", "{name}"},
		session.UploadURL:      {"{accountId}"},
		session.EventSourceURL: {"{types}", "{closeafter}", "{ping}"},
	} {
		for _, v := range vars {
			if !strings.Contains(url, v) {
				t.Errorf("url %q missing template variable %s", url, v)
			}
		}
	}
	if session.APIURL != "https://jmap.example.com/api" {
		t.Errorf("apiUrl = %q", session.APIURL)
	}
	if session.State == "" {
		t.Error("session state must be non-empty")
	}
}

func TestAPIEchoEndToEnd(t *testing.T) {
	ts := testServer(t, DefaultCoreCapabilities())
	resp := post(t, ts, `{
		"using": ["urn:ietf:params:jmap:core"],
		"methodCalls": [["Core/echo", {"hello": true}, "c1"]]
	}`, "application/json")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out jmap.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.MethodResponses) != 1 || out.MethodResponses[0].Name != "Core/echo" {
		t.Errorf("responses = %+v", out.MethodResponses)
	}
	// sessionState on the response equals the session resource's state.
	sresp := get(t, ts, "/.well-known/jmap", "john@example.com", "secret")
	var session jmap.Session
	if err := json.NewDecoder(sresp.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if out.SessionState != session.State {
		t.Errorf("sessionState %q != session state %q", out.SessionState, session.State)
	}
}

func problemType(t *testing.T, resp *http.Response) (string, jmap.RequestError) {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("problem content type = %q", ct)
	}
	var p jmap.RequestError
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	return p.Type, p
}

func TestAPIRequestLevelErrors(t *testing.T) {
	small := DefaultCoreCapabilities()
	small.MaxCallsInRequest = 1
	small.MaxSizeRequest = 200
	ts := testServer(t, small)

	// Duplicate member -> notJSON (I-JSON, section 1.5).
	resp := post(t, ts, `{"using": [], "using": [], "methodCalls": []}`, "application/json")
	if typ, _ := problemType(t, resp); typ != jmap.ProblemNotJSON || resp.StatusCode != 400 {
		t.Errorf("duplicate members -> %s (%d)", typ, resp.StatusCode)
	}
	// Wrong content type -> notJSON.
	resp = post(t, ts, `{"using": [], "methodCalls": []}`, "text/plain")
	if typ, _ := problemType(t, resp); typ != jmap.ProblemNotJSON {
		t.Errorf("wrong content type -> %s", typ)
	}
	// Valid JSON, wrong shape -> notRequest.
	resp = post(t, ts, `{"methodCalls": []}`, "application/json")
	if typ, _ := problemType(t, resp); typ != jmap.ProblemNotRequest {
		t.Errorf("wrong shape -> %s", typ)
	}
	// Unknown capability in using -> unknownCapability.
	resp = post(t, ts, `{"using": ["https://example.com/apis/foobar"], "methodCalls": []}`, "application/json")
	if typ, _ := problemType(t, resp); typ != jmap.ProblemUnknownCapability {
		t.Errorf("unknown capability -> %s", typ)
	}
	// Too many calls -> limit with the limit named (section 3.6.1).
	resp = post(t, ts, `{"using": [], "methodCalls": [["Core/echo",{},"a"],["Core/echo",{},"b"]]}`, "application/json")
	typ, p := problemType(t, resp)
	if typ != jmap.ProblemLimit || p.Limit != "maxCallsInRequest" {
		t.Errorf("too many calls -> %s limit=%q", typ, p.Limit)
	}
	// Oversized body -> limit maxSizeRequest.
	resp = post(t, ts, `{"using": [], "methodCalls": [["Core/echo", {"pad": "`+strings.Repeat("x", 300)+`"}, "c1"]]}`, "application/json")
	typ, p = problemType(t, resp)
	if typ != jmap.ProblemLimit || p.Limit != "maxSizeRequest" {
		t.Errorf("oversized -> %s limit=%q", typ, p.Limit)
	}
}

func TestUnbuiltEndpointsAnd404(t *testing.T) {
	ts := testServer(t, DefaultCoreCapabilities())
	for _, path := range []string{"/eventsource", "/upload/Atest1/", "/download/Atest1/x/y"} {
		if resp := get(t, ts, path, "john@example.com", "secret"); resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s -> %d, want 501", path, resp.StatusCode)
		}
	}
	if resp := get(t, ts, "/nope", "john@example.com", "secret"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path -> %d", resp.StatusCode)
	}
}
