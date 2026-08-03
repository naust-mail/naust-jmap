package quotas

// Registration, session advertisement, and the RFC 9425 worked
// examples: the capability appears as an empty object in both session
// places (section 2.1), and the section 5 request/response pairs
// reproduce against a live server.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
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

// rfcTypeCapabilities covers the illustrative type names the RFC 9425
// examples use ("Mail", "Calendar", "Contact"), which are not entries
// in the IANA "JMAP Types Names" registry the built-in table mirrors.
// Reproducing the worked examples verbatim needs them recognized, and
// Config.TypeCapabilities is the supported way to do it.
func rfcTypeCapabilities() map[string]string {
	tc := DefaultTypeCapabilities()
	tc["Mail"] = "urn:ietf:params:jmap:mail"
	tc["Contact"] = "urn:ietf:params:jmap:contacts"
	return tc
}

// typeCapabilityURIs are the capabilities the quotas tests reference in
// "using". A server only accepts opt-ins to capabilities it advertises
// (RFC 8620 section 3.6.1), so these stand in for the datatype modules
// an embedder would have registered alongside this one.
var typeCapabilityURIs = []string{
	"urn:ietf:params:jmap:mail",
	"urn:ietf:params:jmap:calendars",
	"urn:ietf:params:jmap:contacts",
	"urn:ietf:params:jmap:submission",
}

func newTestServer(t *testing.T) (*httptest.Server, *Service) {
	t.Helper()
	return newTestServerConfig(t, Config{TypeCapabilities: rfcTypeCapabilities()})
}

// newAdminTestServer is the embedder override of RFC 9425 section 8:
// this caller is treated as an administrator, so shared-resource
// scopes are visible. Tests exercising domain or global quotas need
// it; everything else runs against the default.
func newAdminTestServer(t *testing.T) (*httptest.Server, *Service) {
	t.Helper()
	return newTestServerConfig(t, Config{
		TypeCapabilities: rfcTypeCapabilities(),
		ScopeVisible:     func(context.Context, jmap.Id, string) bool { return true },
	})
}

// newTestServerConfig wires a server around cfg, filling in the DB and
// Core the test harness owns.
func newTestServerConfig(t *testing.T, cfg Config) (*httptest.Server, *Service) {
	t.Helper()
	be := memory.New()
	// WithVerifyPreImages enforces objectdb's modify-a-copy contract on
	// every write this module stages.
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithVerifyPreImages())
	p := runtime.NewProcessor()
	cfg.DB = db
	cfg.Core = runtime.DefaultCoreCapabilities()
	svc, err := Register(p, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewServer(staticAuth{}, p, "https://jmap.example.com", runtime.DefaultCoreCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	if err := srv.Capability(CapabilityURI).Advertise(struct{}{}, struct{}{}).Err(); err != nil {
		t.Fatal(err)
	}
	for _, uri := range typeCapabilityURIs {
		if err := srv.Capability(uri).Advertise(struct{}{}, struct{}{}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, svc
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

// callUsing issues one method call under the given capability opt-ins.
func callUsing(t *testing.T, ts *httptest.Server, using []string, name, args string) jmap.Response {
	t.Helper()
	u, err := json.Marshal(using)
	if err != nil {
		t.Fatal(err)
	}
	return call(t, ts, `{"using":`+string(u)+`,"methodCalls":[["`+name+`",`+args+`,"0"]]}`)
}

// mailUsing is the common opt-in pair: core plus mail.
var mailUsing = []string{jmap.CoreCapability, CapabilityURI, "urn:ietf:params:jmap:mail"}

func argsOf(t *testing.T, r jmap.Response, i int, wantName string) map[string]any {
	t.Helper()
	if len(r.MethodResponses) <= i {
		t.Fatalf("response %d missing from %d invocations", i, len(r.MethodResponses))
	}
	if got := r.MethodResponses[i].Name; got != wantName {
		t.Fatalf("response %d is %s (%s), want %s", i, got, r.MethodResponses[i].Args, wantName)
	}
	var out map[string]any
	if err := json.Unmarshal(r.MethodResponses[i].Args, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func u64(v uint64) *uint64 { return &v }
func str(s string) *string { return &s }

// rfcExampleQuota is the section 5.1 example object, minus the
// server-computed used value.
func rfcExampleQuota() Quota {
	return Quota{
		ResourceType: "count",
		HardLimit:    2000,
		WarnLimit:    u64(1600),
		SoftLimit:    u64(1800),
		Scope:        "account",
		Name:         "bob@example.com",
		Types:        []string{"Mail", "Calendar", "Contact"},
		Description: str("Personal account usage. When the soft limit is reached, " +
			"the user is not allowed to send mails or create contacts and calendar events anymore."),
	}
}

func TestSessionAdvertisesCapability(t *testing.T) {
	ts, _ := newTestServer(t)
	var session struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		Accounts     map[jmap.Id]struct {
			AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(get(t, ts, "/.well-known/jmap"), &session); err != nil {
		t.Fatal(err)
	}
	// Both entries are the empty object (RFC 9425 section 2.1).
	if v, ok := session.Capabilities[CapabilityURI]; !ok || string(v) != "{}" {
		t.Errorf("session capabilities[%s] = %s (present %v), want {}", CapabilityURI, v, ok)
	}
	acct, ok := session.Accounts[testAccount]
	if !ok {
		t.Fatalf("account %s missing from session", testAccount)
	}
	if v, ok := acct.AccountCapabilities[CapabilityURI]; !ok || string(v) != "{}" {
		t.Errorf("accountCapabilities[%s] = %s (present %v), want {}", CapabilityURI, v, ok)
	}
}

// RFC 9425 section 5.1: fetching all quotas of an account with a null
// ids argument returns the full list with every property.
func TestRFCExampleFetchingQuotas(t *testing.T) {
	ts, svc := newTestServer(t)
	ctx := context.Background()
	countId, err := svc.Upsert(ctx, testAccount, rfcExampleQuota())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetUsed(ctx, testAccount, countId, 1056); err != nil {
		t.Fatal(err)
	}
	// The example's second entry, abbreviated in the RFC text with "...".
	octets := rfcExampleQuota()
	octets.ResourceType = "octets"
	octets.HardLimit = 25000
	octets.WarnLimit, octets.SoftLimit, octets.Description = nil, nil, nil
	if _, err := svc.Upsert(ctx, testAccount, octets); err != nil {
		t.Fatal(err)
	}

	r := callUsing(t, ts, append(mailUsing, "urn:ietf:params:jmap:calendars", "urn:ietf:params:jmap:contacts"),
		"Quota/get", `{"accountId":"Atest1","ids":null}`)
	args := argsOf(t, r, 0, "Quota/get")
	if args["accountId"] != testAccount {
		t.Errorf("accountId = %v", args["accountId"])
	}
	if _, ok := args["state"].(string); !ok {
		t.Errorf("state = %v, want a string", args["state"])
	}
	if nf, _ := args["notFound"].([]any); len(nf) != 0 {
		t.Errorf("notFound = %v, want empty", nf)
	}
	list, _ := args["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("list has %d entries, want 2", len(list))
	}
	var got map[string]any
	for _, entry := range list {
		e := entry.(map[string]any)
		if e["resourceType"] == "count" {
			got = e
		}
	}
	if got == nil {
		t.Fatalf("no count quota in %v", list)
	}
	want := map[string]any{
		"resourceType": "count",
		"used":         float64(1056),
		"warnLimit":    float64(1600),
		"softLimit":    float64(1800),
		"hardLimit":    float64(2000),
		"scope":        "account",
		"name":         "bob@example.com",
		"description":  *rfcExampleQuota().Description,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if got["id"] != string(countId) {
		t.Errorf("id = %v, want %s", got["id"], countId)
	}
	types, _ := got["types"].([]any)
	if len(types) != 3 || types[0] != "Mail" || types[1] != "Calendar" || types[2] != "Contact" {
		t.Errorf("types = %v, want [Mail Calendar Contact]", types)
	}
}

// RFC 9425 section 5.2: Quota/changes reports updatedProperties
// ["used"] for a usage-only change, and the same request back-
// references it into Quota/get so only that property is returned.
func TestRFCExampleRequestingLatestChanges(t *testing.T) {
	ts, svc := newTestServer(t)
	ctx := context.Background()
	id, err := svc.Upsert(ctx, testAccount, rfcExampleQuota())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetUsed(ctx, testAccount, id, 1056); err != nil {
		t.Fatal(err)
	}
	r := callUsing(t, ts, mailUsing, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	since := argsOf(t, r, 0, "Quota/get")["state"].(string)

	if err := svc.SetUsed(ctx, testAccount, id, 1246); err != nil {
		t.Fatal(err)
	}

	r = call(t, ts, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:quota","urn:ietf:params:jmap:mail"],
		"methodCalls":[
		["Quota/changes",{"accountId":"Atest1","sinceState":"`+since+`","maxChanges":20},"0"],
		["Quota/get",{"accountId":"Atest1",
			"#ids":{"resultOf":"0","name":"Quota/changes","path":"/updated"},
			"#properties":{"resultOf":"0","name":"Quota/changes","path":"/updatedProperties"}},"1"]]}`)

	changes := argsOf(t, r, 0, "Quota/changes")
	if changes["oldState"] != since {
		t.Errorf("oldState = %v, want %s", changes["oldState"], since)
	}
	if changes["hasMoreChanges"] != false {
		t.Errorf("hasMoreChanges = %v, want false", changes["hasMoreChanges"])
	}
	if c, _ := changes["created"].([]any); len(c) != 0 {
		t.Errorf("created = %v, want empty", c)
	}
	if d, _ := changes["destroyed"].([]any); len(d) != 0 {
		t.Errorf("destroyed = %v, want empty", d)
	}
	updated, _ := changes["updated"].([]any)
	if len(updated) != 1 || updated[0] != string(id) {
		t.Errorf("updated = %v, want [%s]", updated, id)
	}
	up, _ := changes["updatedProperties"].([]any)
	if len(up) != 1 || up[0] != "used" {
		t.Errorf("updatedProperties = %v, want [used]", up)
	}

	// The back-referenced /get returns exactly id plus the changed
	// property, which is the point of the section 4.3 optimization.
	got := argsOf(t, r, 1, "Quota/get")
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v, want one entry", list)
	}
	rec := list[0].(map[string]any)
	if len(rec) != 2 || rec["id"] != string(id) || rec["used"] != float64(1246) {
		t.Errorf("record = %v, want id and used 1246 only", rec)
	}
}
