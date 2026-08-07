package mdn

// Registration and dispatch tests: the capability appears in the
// session object (RFC 9007 section 1.3), MDN/send and MDN/parse are
// callable only under the capability's URI (RFC 8620 section 3.3), and
// Register's startup checks name what is missing - required Config
// fields and the internal Email property wiring.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/datatypes/mail/search"
	"github.com/naust-mail/naust-jmap/datatypes/mail/submit"
)

const testAccount = "Atest1"

// staticAuth authenticates HTTP Basic john@example.com/secret; the
// module cannot reach the core's internal test authenticator.
type staticAuth struct{}

func (staticAuth) Authenticate(r *http.Request) (*auth.Identity, error) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != "john@example.com" || pass != "secret" {
		return nil, auth.ErrUnauthenticated
	}
	return &auth.Identity{
		Username: "john@example.com",
		Accounts: map[jmap.Id]auth.Access{testAccount: {Name: user, Personal: true}},
		Primary:  testAccount,
	}, nil
}

// registerMail wires the mail module the way an embedder would,
// including this package's internal Email properties.
func registerMail(t *testing.T, p *runtime.Processor, db *objectdb.DB, store blob.Store, internal bool) *submit.Queue {
	return registerMailLimits(t, p, db, store, internal, nil)
}

func registerMailLimits(t *testing.T, p *runtime.Processor, db *objectdb.DB, store blob.Store, internal bool, limits *submit.Limits) *submit.Queue {
	return registerMailFull(t, p, db, store, internal, limits, nil)
}

func registerMailFull(t *testing.T, p *runtime.Processor, db *objectdb.DB, store blob.Store, internal bool, limits *submit.Limits, policy mail.SendPolicy) *submit.Queue {
	t.Helper()
	core := runtime.DefaultCoreCapabilities()
	if err := mail.RegisterMailbox(p, mail.MailboxConfig{DB: db, Core: core}); err != nil {
		t.Fatal(err)
	}
	if err := mail.RegisterThread(p, mail.ThreadConfig{DB: db, Core: core}); err != nil {
		t.Fatal(err)
	}
	cfg := mail.EmailConfig{
		DB: db, Store: store, Core: core,
		AccountCapability: mail.DefaultAccountCapability(),
		Searcher:          search.New(store, search.DefaultConfig()),
	}
	if internal {
		cfg.InternalProperties = EmailInternalProperties()
	}
	if err := mail.RegisterEmail(p, cfg); err != nil {
		t.Fatal(err)
	}
	if policy == nil {
		static := mail.NewStaticSendPolicy()
		static.Allow(testAccount, "john@example.com", "*@example.com")
		policy = static
	}
	if err := mail.RegisterIdentity(p, mail.IdentityConfig{DB: db, Core: core, Policy: policy}); err != nil {
		t.Fatal(err)
	}
	queue, err := submit.Register(p, submit.Config{DB: db, Store: store, Core: core, Policy: policy, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return queue
}

// createIdentity makes an Identity for john@example.com and returns its
// id.
func createIdentity(t *testing.T, ts *httptest.Server) jmap.Id {
	t.Helper()
	r := call(t, ts, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:submission"],"methodCalls":[
		["Identity/set",{"accountId":"Atest1","create":{"i1":{"email":"john@example.com"}}},"0"]]}`)
	var out struct {
		Created map[string]struct {
			Id jmap.Id `json:"id"`
		} `json:"created"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Created["i1"].Id == "" {
		t.Fatalf("Identity/set: %s %s", r.MethodResponses[0].Name, r.MethodResponses[0].Args)
	}
	return out.Created["i1"].Id
}

func newTestServer(t *testing.T) *httptest.Server {
	return newTestServerLimits(t, nil)
}

func newTestServerLimits(t *testing.T, limits *submit.Limits) *httptest.Server {
	return newTestServerFull(t, limits, nil)
}

func newTestServerFull(t *testing.T, limits *submit.Limits, policy mail.SendPolicy) *httptest.Server {
	t.Helper()
	be := memory.New()
	// WithVerifyPreImages enforces objectdb's modify-a-copy contract on
	// every write this module's hooks stage.
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithVerifyPreImages())
	store := kvstore.New(memory.New())
	p := runtime.NewProcessor()
	queue := registerMailFull(t, p, db, store, true, limits, policy)
	if err := Register(p, Config{DB: db, Store: store, Core: runtime.DefaultCoreCapabilities(), Queue: queue}); err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewServer(staticAuth{}, p, "https://jmap.example.com", runtime.DefaultCoreCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	srv.EnableBlobs(db, store)
	if err := srv.Capability(mail.CapabilityURI).Advertise(struct{}{}, mail.DefaultAccountCapability()).Err(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Capability(submit.CapabilityURI).Advertise(struct{}{}, submit.AccountCapabilityFor(submit.DefaultLimits())).Err(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Capability(CapabilityURI).Advertise(struct{}{}, struct{}{}).Err(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, ts *httptest.Server, path string) []byte {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	req.SetBasicAuth("john@example.com", "secret")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", path, res.StatusCode)
	}
	var buf bytes.Buffer
	buf.ReadFrom(res.Body)
	return buf.Bytes()
}

func call(t *testing.T, ts *httptest.Server, body string) jmap.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/api", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("john@example.com", "secret")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /api = %d", res.StatusCode)
	}
	var r jmap.Response
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSessionAdvertisesCapability(t *testing.T) {
	ts := newTestServer(t)
	var session struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		Accounts     map[jmap.Id]struct {
			AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(get(t, ts, "/.well-known/jmap"), &session); err != nil {
		t.Fatal(err)
	}
	// Both entries are the empty object (RFC 9007 section 1.3).
	if v, ok := session.Capabilities[CapabilityURI]; !ok || string(v) != "{}" {
		t.Errorf("session capabilities[%s] = %q, want {}", CapabilityURI, v)
	}
	if v, ok := session.Accounts[testAccount].AccountCapabilities[CapabilityURI]; !ok || string(v) != "{}" {
		t.Errorf("accountCapabilities[%s] = %q, want {}", CapabilityURI, v)
	}
}

func TestMethodsDispatch(t *testing.T) {
	ts := newTestServer(t)
	identityId := createIdentity(t, ts)
	r := call(t, ts, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:mdn"],"methodCalls":[
		["MDN/parse",{"accountId":"Atest1","blobIds":[]},"0"],
		["MDN/send",{"accountId":"Atest1","identityId":"`+string(identityId)+`","send":{}},"1"]]}`)
	if len(r.MethodResponses) != 2 {
		t.Fatalf("responses = %d, want 2", len(r.MethodResponses))
	}
	if name := r.MethodResponses[0].Name; name != "MDN/parse" {
		t.Fatalf("response 0 = %s %s", name, r.MethodResponses[0].Args)
	}
	var parse struct {
		AccountID jmap.Id         `json:"accountId"`
		Parsed    json.RawMessage `json:"parsed"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &parse)
	if parse.AccountID != testAccount || string(parse.Parsed) != "null" {
		t.Errorf("MDN/parse = %s", r.MethodResponses[0].Args)
	}
	if name := r.MethodResponses[1].Name; name != "MDN/send" {
		t.Fatalf("response 1 = %s %s", name, r.MethodResponses[1].Args)
	}
}

func TestMethodsGatedOnCapability(t *testing.T) {
	ts := newTestServer(t)
	// A request whose using array lacks urn:ietf:params:jmap:mdn must
	// not reach the methods (RFC 8620 section 3.3).
	r := call(t, ts, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[
		["MDN/parse",{"accountId":"Atest1","blobIds":[]},"0"]]}`)
	if name := r.MethodResponses[0].Name; name != "error" {
		t.Fatalf("ungated dispatch: %s %s", name, r.MethodResponses[0].Args)
	}
	var e jmap.MethodError
	json.Unmarshal(r.MethodResponses[0].Args, &e)
	if e.Type != jmap.ErrUnknownMethod {
		t.Errorf("error type = %q, want %q", e.Type, jmap.ErrUnknownMethod)
	}
}

func TestRegisterMissingFields(t *testing.T) {
	err := Register(runtime.NewProcessor(), Config{})
	if err == nil {
		t.Fatal("Register accepted an empty Config")
	}
	for _, field := range []string{"DB", "Store", "Queue"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error %q does not name missing field %s", err, field)
		}
	}
}

func TestRegisterRequiresInternalProperties(t *testing.T) {
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	store := kvstore.New(memory.New())
	p := runtime.NewProcessor()
	// The embedder forgot EmailConfig.InternalProperties: registration
	// must fail at startup naming the wiring step.
	queue := registerMail(t, p, db, store, false)
	err := Register(p, Config{DB: db, Store: store, Core: runtime.DefaultCoreCapabilities(), Queue: queue})
	if err == nil || !strings.Contains(err.Error(), "InternalProperties") {
		t.Fatalf("Register without the internal property = %v, want an error naming EmailConfig.InternalProperties", err)
	}
}
