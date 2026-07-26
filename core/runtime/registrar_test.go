package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naust-mail/naust-jmap/core/internal/authtest"
	"github.com/naust-mail/naust-jmap/core/jmap"
)

func registrarServer(t *testing.T) *Server {
	t.Helper()
	a := authtest.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	srv, err := NewServer(a, NewProcessor(), "https://jmap.example.com", DefaultCoreCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// A capability advertised through the registrar must appear under its URI
// in the session capabilities object, in every account's
// accountCapabilities, and in primaryAccounts (RFC 8620 section 2).
func TestCapabilityAdvertiseInSession(t *testing.T) {
	srv := registrarServer(t)
	err := srv.Capability("urn:example:gadget").
		Advertise(map[string]int{"maxWidgets": 3}, map[string]string{"tier": "basic"}).
		Err()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := get(t, ts, "/.well-known/jmap", "john@example.com", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var session struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		Accounts     map[string]struct {
			AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
		} `json:"accounts"`
		PrimaryAccounts map[string]string `json:"primaryAccounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if got := string(session.Capabilities["urn:example:gadget"]); got != `{"maxWidgets":3}` {
		t.Errorf("session capability = %s", got)
	}
	if got := string(session.Accounts["Atest1"].AccountCapabilities["urn:example:gadget"]); got != `{"tier":"basic"}` {
		t.Errorf("account capability = %s", got)
	}
	if session.PrimaryAccounts["urn:example:gadget"] != "Atest1" {
		t.Errorf("primaryAccounts = %v", session.PrimaryAccounts)
	}
}

// A method registered under a capability is callable only when the
// request's "using" array includes that capability (RFC 8620 section 3.3).
func TestCapabilityMethodThroughUsing(t *testing.T) {
	srv := registrarServer(t)
	err := srv.Capability("urn:example:gadget").
		Advertise(struct{}{}, struct{}{}).
		Method("Gadget/ping", func(_ context.Context, call *Call) []jmap.Invocation {
			return []jmap.Invocation{{Name: "Gadget/ping", Args: json.RawMessage(`{"pong":true}`), CallID: call.CallID}}
		}).
		Err()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	call := func(using []string) *jmap.Response {
		body, err := json.Marshal(map[string]any{
			"using":       using,
			"methodCalls": []jmap.Invocation{{Name: "Gadget/ping", Args: json.RawMessage(`{}`), CallID: "0"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		resp := post(t, ts, string(body), "application/json")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		var out jmap.Response
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return &out
	}

	r := call([]string{jmap.CoreCapability, "urn:example:gadget"})
	if len(r.MethodResponses) != 1 || r.MethodResponses[0].Name != "Gadget/ping" {
		t.Fatalf("with capability: %+v", r.MethodResponses)
	}
	r = call([]string{jmap.CoreCapability})
	if len(r.MethodResponses) != 1 || r.MethodResponses[0].Name != "error" {
		t.Fatalf("without capability: %+v", r.MethodResponses)
	}
}

// Handle mounts endpoints beside the built-in ones: exact paths match
// exactly, trailing-slash paths by prefix with the longest prefix
// winning, and unmatched paths still fall through to 404.
func TestCapabilityHandleRoutes(t *testing.T) {
	srv := registrarServer(t)
	serve := func(body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, body)
		})
	}
	err := srv.Capability("urn:example:gadget").
		Handle("/gadget", serve("exact")).
		Handle("/gadgets/", serve("prefix")).
		Handle("/gadgets/deep/", serve("deep")).
		Err()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	for _, tc := range []struct {
		path, want string
		status     int
	}{
		{"/gadget", "exact", http.StatusOK},
		{"/gadgets/a/b", "prefix", http.StatusOK},
		{"/gadgets/deep/x", "deep", http.StatusOK},
		{"/gadget/sub", "", http.StatusNotFound},
		{"/nothing", "", http.StatusNotFound},
	} {
		resp := get(t, ts, tc.path, "", "")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d", tc.path, resp.StatusCode, tc.status)
			continue
		}
		if tc.status == http.StatusOK && string(body) != tc.want {
			t.Errorf("%s: body %q, want %q", tc.path, body, tc.want)
		}
	}
}

// Paths that are, or would shadow, the built-in JMAP endpoints must be
// rejected at registration, as are malformed paths and duplicates.
func TestCapabilityHandleCollisions(t *testing.T) {
	nop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, path := range []string{
		"/api",
		"/.well-known/jmap",
		"/.well-known/",
		"/eventsource",
		"/upload/",
		"/upload/x",
		"/download/",
		"/download/x/",
		"/",
		"noSlash",
		"",
	} {
		srv := registrarServer(t)
		if err := srv.Capability("urn:example:gadget").Handle(path, nop).Err(); err == nil {
			t.Errorf("path %q: expected registration error", path)
		}
	}

	srv := registrarServer(t)
	err := srv.Capability("urn:example:gadget").
		Handle("/gadget", nop).
		Handle("/gadget", nop).
		Err()
	if err == nil {
		t.Error("duplicate path: expected registration error")
	}
}

// The first error in a registration chain sticks: later calls are
// no-ops and Err reports the original failure, so nothing after the
// failure is half-registered.
func TestCapabilityStickyError(t *testing.T) {
	srv := registrarServer(t)
	reg := srv.Capability("urn:example:gadget").
		Advertise(make(chan int), struct{}{}).
		Handle("/gadget", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "should not mount")
		}))
	if reg.Err() == nil {
		t.Fatal("expected marshal error to stick")
	}
	if len(srv.routes) != 0 {
		t.Errorf("route registered after failed Advertise: %v", srv.routes)
	}
	if _, ok := srv.sessionCaps["urn:example:gadget"]; ok {
		t.Error("capability advertised despite marshal failure")
	}
}

func TestBaseURL(t *testing.T) {
	a := authtest.NewStatic()
	srv, err := NewServer(a, NewProcessor(), "https://jmap.example.com/", DefaultCoreCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.BaseURL(); got != "https://jmap.example.com" {
		t.Errorf("BaseURL() = %q", got)
	}
}
