package mail

// Identity tests (RFC 8621 section 6): the settable lifecycle with the
// spec's defaults, the forbiddenFrom policy gate on create and update,
// wildcard identities, immutability of email, and /changes sync. Plus the
// StaticSendPolicy matching table.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// identityServer wires the Identity type with a policy granting john's
// account one plain address and one whole-domain wildcard.
func identityServer(t *testing.T) *httptest.Server {
	t.Helper()
	a := newStaticAuth()
	a.AddUser("john@example.com", "secret", testAccount)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	policy := NewStaticSendPolicy()
	policy.Allow(testAccount, "joe@example.com", "*@corp.example")
	if err := RegisterIdentity(p, db, policy, core); err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability(SubmissionCapabilityURI, struct{}{}, struct{}{}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// callSubmission is callMail with the submission capability opted in.
func callSubmission(t *testing.T, ts *httptest.Server, calls ...jmap.Invocation) *jmap.Response {
	t.Helper()
	req := map[string]any{
		"using":       []string{jmap.CoreCapability, SubmissionCapabilityURI},
		"methodCalls": calls,
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	hreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api", strings.NewReader(string(body)))
	hreq.SetBasicAuth("john@example.com", "secret")
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
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

// TestIdentityLifecycle: create with defaults filled per section 6, update
// the settable properties, track it through /changes, destroy it.
func TestIdentityLifecycle(t *testing.T) {
	ts := identityServer(t)

	r := callSubmission(t, ts, inv("Identity/get",
		fmt.Sprintf(`{"accountId":%q}`, testAccount), "0"))
	state0 := methodArgs(t, r, 0, "Identity/get")["state"].(string)

	r = callSubmission(t, ts, inv("Identity/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"i1":{"name":"Joe","email":"joe@example.com","bcc":[{"name":null,"email":"joe+archive@example.com"}]}}}`,
		testAccount), "0"))
	created, ok := methodArgs(t, r, 0, "Identity/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", r.MethodResponses[0].Args)
	}
	id := created["i1"].(map[string]any)["id"].(string)

	// The stored object carries the section 6 defaults for everything the
	// client omitted.
	r = callSubmission(t, ts, inv("Identity/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q]}`, testAccount, id), "0"))
	got := methodArgs(t, r, 0, "Identity/get")["list"].([]any)[0].(map[string]any)
	for prop, want := range map[string]any{
		"name": "Joe", "email": "joe@example.com", "replyTo": nil,
		"textSignature": "", "htmlSignature": "", "mayDelete": true,
	} {
		if got[prop] != want {
			t.Errorf("%s = %v, want %v", prop, got[prop], want)
		}
	}
	if bcc := got["bcc"].([]any); len(bcc) != 1 || bcc[0].(map[string]any)["email"] != "joe+archive@example.com" {
		t.Errorf("bcc = %v", got["bcc"])
	}

	// Settable properties update; /changes sees the whole history.
	r = callSubmission(t, ts, inv("Identity/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"textSignature":"-- \nJoe"}}}`, testAccount, id), "0"))
	if _, ok := methodArgs(t, r, 0, "Identity/set")["updated"].(map[string]any); !ok {
		t.Fatalf("update failed: %v", r.MethodResponses[0].Args)
	}
	r = callSubmission(t, ts, inv("Identity/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testAccount, state0), "0"))
	ch := methodArgs(t, r, 0, "Identity/changes")
	if cr := ch["created"].([]any); len(cr) != 1 || cr[0] != id {
		t.Fatalf("changes created: %v", ch)
	}

	r = callSubmission(t, ts, inv("Identity/set", fmt.Sprintf(
		`{"accountId":%q,"destroy":[%q]}`, testAccount, id), "0"))
	if d := methodArgs(t, r, 0, "Identity/set")["destroyed"].([]any); len(d) != 1 || d[0] != id {
		t.Fatalf("destroy failed: %v", r.MethodResponses[0].Args)
	}
}

// TestIdentityPolicyGate: the SendPolicy decides which emails may become
// identities - ungranted addresses and ungranted wildcards are
// forbiddenFrom (section 6.3), granted wildcards work, and the shape
// errors (missing email, bad EmailAddress list) are invalidProperties.
func TestIdentityPolicyGate(t *testing.T) {
	ts := identityServer(t)

	r := callSubmission(t, ts, inv("Identity/set", fmt.Sprintf(
		`{"accountId":%q,"create":{`+
			`"ok1":{"email":"joe@example.com"},`+
			`"ok2":{"email":"*@corp.example"},`+
			`"ok3":{"email":"anyone@corp.example"},`+
			`"bad1":{"email":"evil@other.example"},`+
			`"bad2":{"email":"*@example.com"},`+
			`"bad3":{"name":"no address"},`+
			`"bad4":{"email":"not-an-address"},`+
			`"bad5":{"email":"joe@example.com","replyTo":[{"name":"x"}]}}}`,
		testAccount), "0"))
	args := methodArgs(t, r, 0, "Identity/set")
	created, _ := args["created"].(map[string]any)
	for _, cid := range []string{"ok1", "ok2", "ok3"} {
		if _, ok := created[cid]; !ok {
			t.Errorf("%s not created: %v", cid, args["notCreated"])
		}
	}
	nc := args["notCreated"].(map[string]any)
	for cid, wantType := range map[string]string{
		// A plain grant does not grant the domain (bad2): wildcard
		// identities need the wildcard grant.
		"bad1": "forbiddenFrom", "bad2": "forbiddenFrom",
		"bad3": "invalidProperties", "bad4": "invalidProperties", "bad5": "invalidProperties",
	} {
		serr, ok := nc[cid].(map[string]any)
		if !ok || serr["type"] != wantType {
			t.Errorf("%s: got %v, want %s", cid, nc[cid], wantType)
		}
	}
}

// TestIdentityEmailImmutable: email may not change on update (descriptor
// Immutable, RFC 8620 section 5.3), though echoing the identical value is
// legal; other server-set rules hold (mayDelete rejected on create).
func TestIdentityEmailImmutable(t *testing.T) {
	ts := identityServer(t)
	r := callSubmission(t, ts, inv("Identity/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"i1":{"email":"joe@example.com"}}}`, testAccount), "0"))
	id := methodArgs(t, r, 0, "Identity/set")["created"].(map[string]any)["i1"].(map[string]any)["id"].(string)

	r = callSubmission(t, ts, inv("Identity/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"email":"other@corp.example"}}}`, testAccount, id), "0"))
	nu := methodArgs(t, r, 0, "Identity/set")["notUpdated"].(map[string]any)
	if serr := nu[id].(map[string]any); serr["type"] != "invalidProperties" {
		t.Fatalf("email change: %v", serr)
	}

	r = callSubmission(t, ts, inv("Identity/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"email":"joe@example.com","name":"J"}}}`, testAccount, id), "0"))
	if _, ok := methodArgs(t, r, 0, "Identity/set")["updated"].(map[string]any); !ok {
		t.Fatalf("identical-email update rejected: %v", r.MethodResponses[0].Args)
	}

	r = callSubmission(t, ts, inv("Identity/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"i2":{"email":"joe@example.com","mayDelete":false}}}`, testAccount), "0"))
	nc := methodArgs(t, r, 0, "Identity/set")["notCreated"].(map[string]any)
	if serr := nc["i2"].(map[string]any); serr["type"] != "invalidProperties" {
		t.Fatalf("server-set mayDelete on create: %v", serr)
	}
}

// TestStaticSendPolicyMatching: the plug's matching table - exact match,
// domain case-insensitivity, wildcard grants covering plain and wildcard
// addresses, plain grants covering neither the domain nor other locals,
// and deny-by-default for unknown accounts.
func TestStaticSendPolicyMatching(t *testing.T) {
	ctx := context.Background()
	s := NewStaticSendPolicy()
	s.Allow("Aone", "joe@example.com")
	s.Allow("Atwo", "*@corp.example")

	if ok, _ := s.CanSend(ctx, "Aone"); !ok {
		t.Error("Aone should be allowed to send")
	}
	if ok, reason := s.CanSend(ctx, "Anone"); ok || reason == "" {
		t.Error("unknown account should be denied with a reason")
	}

	cases := []struct {
		acct jmap.Id
		addr string
		want bool
	}{
		{"Aone", "joe@example.com", true},
		{"Aone", "joe@EXAMPLE.COM", true},   // domain compares case-insensitively
		{"Aone", "JOE@example.com", false},  // local part is exact
		{"Aone", "jane@example.com", false}, // plain grant is one address
		{"Aone", "*@example.com", false},    // ...and not the domain
		{"Atwo", "anyone@corp.example", true},
		{"Atwo", "*@corp.example", true}, // wildcard grant covers the wildcard form
		{"Atwo", "anyone@other.example", false},
		{"Anone", "joe@example.com", false},
		{"Aone", "not-an-address", false},
	}
	for _, c := range cases {
		if got := s.CanSendAs(ctx, c.acct, c.addr); got != c.want {
			t.Errorf("CanSendAs(%s, %q) = %v, want %v", c.acct, c.addr, got, c.want)
		}
	}
}
