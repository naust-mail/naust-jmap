package mail

// VacationResponse tests (RFC 8621 section 8): the singleton get/set
// semantics - defaults before any write, id "singleton", create/destroy
// refused, updates round-trip, unknown ids/properties, ifInState. The
// delivery-side RFC 3834 auto-reply (who gets one and how it is formed)
// is deliver/vacationresponder_test.go: it needs a real submission queue
// and a Deliverer, both of which live below root now.

import (
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
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/testsupport"
)

// newVacationResponseServer wires only Mailbox + VacationResponse: the
// singleton get/set surface needs nothing else.
func newVacationResponseServer(t *testing.T) *httptest.Server {
	t.Helper()
	a := testsupport.NewStaticAuth()
	a.AddUser("john@example.com", "secret", testAccount)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithVerifyPreImages())
	p := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	if err := RegisterVacationResponse(p, db); err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Capability(VacationCapabilityURI).Advertise(struct{}{}, struct{}{}).Err(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// callVac posts a request opted into core + vacation.
func callVac(t *testing.T, ts *httptest.Server, calls ...jmap.Invocation) *jmap.Response {
	t.Helper()
	req := map[string]any{
		"using":       []string{jmap.CoreCapability, VacationCapabilityURI},
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
	var out jmap.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func vacGet(t *testing.T, ts *httptest.Server, extra string) map[string]any {
	t.Helper()
	r := callVac(t, ts, inv("VacationResponse/get", fmt.Sprintf(`{"accountId":%q%s}`, testAccount, extra), "0"))
	res := methodArgs(t, r, 0, "VacationResponse/get")
	list, ok := res["list"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("VacationResponse/get list: %v", res)
	}
	return list[0].(map[string]any)
}

func vacSet(t *testing.T, ts *httptest.Server, args string) map[string]any {
	t.Helper()
	r := callVac(t, ts, inv("VacationResponse/set", fmt.Sprintf(`{"accountId":%q,%s}`, testAccount, args), "0"))
	return methodArgs(t, r, 0, "VacationResponse/set")
}

// TestVacationResponseSingleton: the section 8 object semantics - defaults
// before any write, id "singleton", create/destroy refused with the
// singleton SetError, updates round-trip, unknown ids are notFound,
// unknown properties are invalidProperties, and ifInState is honored.
func TestVacationResponseSingleton(t *testing.T) {
	ts := newVacationResponseServer(t)

	obj := vacGet(t, ts, "")
	if obj["id"] != "singleton" || obj["isEnabled"] != false || obj["textBody"] != nil || obj["fromDate"] != nil {
		t.Fatalf("defaults = %v", obj)
	}

	res := vacSet(t, ts, `"create":{"c1":{"isEnabled":true}},"destroy":["singleton"]`)
	nc := res["notCreated"].(map[string]any)["c1"].(map[string]any)
	nd := res["notDestroyed"].(map[string]any)["singleton"].(map[string]any)
	if nc["type"] != "singleton" || nd["type"] != "singleton" {
		t.Fatalf("create/destroy not refused as singleton: %v", res)
	}
	oldState := res["oldState"].(string)

	res = vacSet(t, ts, `"update":{"singleton":{"isEnabled":true,"subject":"Away","textBody":"Back soon.","fromDate":"2026-07-01T00:00:00Z"}}`)
	if _, ok := res["updated"].(map[string]any)["singleton"]; !ok {
		t.Fatalf("update failed: %v", res)
	}
	if res["newState"] == oldState {
		t.Fatal("state did not advance on update")
	}
	obj = vacGet(t, ts, "")
	if obj["isEnabled"] != true || obj["subject"] != "Away" || obj["textBody"] != "Back soon." || obj["fromDate"] != "2026-07-01T00:00:00Z" {
		t.Fatalf("round-trip = %v", obj)
	}
	// Null clears back to default.
	vacSet(t, ts, `"update":{"singleton":{"subject":null}}`)
	if obj = vacGet(t, ts, ""); obj["subject"] != nil || obj["textBody"] != "Back soon." {
		t.Fatalf("null clear = %v", obj)
	}

	// Unknown id, unknown property, bad value, wrong state.
	res = vacSet(t, ts, `"update":{"other":{"isEnabled":true}}`)
	if res["notUpdated"].(map[string]any)["other"].(map[string]any)["type"] != "notFound" {
		t.Fatalf("unknown id: %v", res)
	}
	res = vacSet(t, ts, `"update":{"singleton":{"color":"red"}}`)
	if res["notUpdated"].(map[string]any)["singleton"].(map[string]any)["type"] != "invalidProperties" {
		t.Fatalf("unknown property: %v", res)
	}
	res = vacSet(t, ts, `"update":{"singleton":{"fromDate":"not-a-date"}}`)
	if res["notUpdated"].(map[string]any)["singleton"].(map[string]any)["type"] != "invalidProperties" {
		t.Fatalf("bad date: %v", res)
	}
	r := callVac(t, ts, inv("VacationResponse/set", fmt.Sprintf(
		`{"accountId":%q,"ifInState":"bogus","update":{"singleton":{"isEnabled":false}}}`, testAccount), "0"))
	if r.MethodResponses[0].Name != "error" {
		t.Fatalf("ifInState mismatch not an error: %v", r.MethodResponses[0])
	}

	// get with explicit ids: the singleton resolves, anything else notFound.
	r = callVac(t, ts, inv("VacationResponse/get", fmt.Sprintf(
		`{"accountId":%q,"ids":["singleton","nope"]}`, testAccount), "0"))
	res = methodArgs(t, r, 0, "VacationResponse/get")
	if len(res["list"].([]any)) != 1 || res["notFound"].([]any)[0] != "nope" {
		t.Fatalf("ids get: %v", res)
	}
}
