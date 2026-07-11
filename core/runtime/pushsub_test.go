package runtime

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/pushsub"
	"github.com/naust-mail/naust-jmap/core/webpush"
)

// pushHit is one POST captured by the fake push service.
type pushHit struct {
	header http.Header
	body   []byte
}

// newPushEndpoint is a fake RFC 8030 push service: TLS (the url MUST
// begin with "https://" per section 7.2), records every POST, and
// answers 201.
func newPushEndpoint(t *testing.T) (*httptest.Server, chan pushHit) {
	t.Helper()
	hits := make(chan pushHit, 64)
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits <- pushHit{r.Header.Clone(), body}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(endpoint.Close)
	return endpoint, hits
}

// pushRig is a push-enabled server plus the pieces a test needs to
// reach behind it.
type pushRig struct {
	ts    *httptest.Server
	srv   *Server
	store *pushsub.Store
	be    *memory.Store
	lm    *lease.InProcess
}

// newPushRig builds a push-enabled server whose Sender trusts the fake
// endpoint's TLS certificate (a custom Client also skips the SSRF
// guard, which would otherwise refuse 127.0.0.1; the guard has its own
// tests in webpush). A non-nil prior shares its backend, leases, and
// subscription store, simulating a restart of the same server.
func newPushRig(t *testing.T, client *http.Client, prior *pushRig) *pushRig {
	t.Helper()
	core := DefaultCoreCapabilities()
	a := auth.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	a.AddUser("jane@example.com", "secret2", "Atest2")
	rig := &pushRig{}
	if prior != nil {
		rig.be, rig.lm, rig.store = prior.be, prior.lm, prior.store
	} else {
		rig.be = memory.New()
		rig.lm = lease.NewInProcess(rig.be)
		rig.store = pushsub.NewStore(rig.be, rig.lm)
	}
	db := objectdb.New(rig.be, rig.lm)
	p := NewProcessor()
	if err := RegisterStandardType(p, db, testNoteType(), core); err != nil {
		t.Fatal(err)
	}
	taskType := &descriptor.Type{
		Name:       "TestTask",
		Capability: "urn:example:testnote",
		Properties: map[string]descriptor.Property{
			"title": {Kind: descriptor.KindString},
		},
	}
	if err := RegisterStandardType(p, db, taskType, core); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability("urn:example:testnote", struct{}{}, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if err := srv.EnablePush(db, notify.NewInProcess(), rig.store, &webpush.Sender{Client: client}); err != nil {
		t.Fatal(err)
	}
	rig.srv = srv
	t.Cleanup(srv.Close)
	rig.ts = httptest.NewServer(srv)
	t.Cleanup(rig.ts.Close)
	return rig
}

// callAPIAs is callAPI with explicit credentials, for the second user.
func callAPIAs(t *testing.T, ts *httptest.Server, user, pass string, calls ...jmap.Invocation) *jmap.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"using":       []string{jmap.CoreCapability, "urn:example:testnote"},
		"methodCalls": calls,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
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

// nextHit returns the endpoint's next captured POST.
func nextHit(t *testing.T, hits chan pushHit) pushHit {
	t.Helper()
	select {
	case h := <-hits:
		return h
	case <-time.After(5 * time.Second):
		t.Fatal("no push POST arrived")
		return pushHit{}
	}
}

// noHit asserts the endpoint stays quiet.
func noHit(t *testing.T, hits chan pushHit) {
	t.Helper()
	select {
	case h := <-hits:
		t.Fatalf("unexpected push POST: %s", h.body)
	case <-time.After(200 * time.Millisecond):
	}
}

func decodeStateChange(t *testing.T, body []byte) map[string]map[string]string {
	t.Helper()
	var sc struct {
		Type    string                       `json:"@type"`
		Changed map[string]map[string]string `json:"changed"`
	}
	if err := json.Unmarshal(body, &sc); err != nil {
		t.Fatalf("StateChange body %q: %v", body, err)
	}
	if sc.Type != "StateChange" {
		t.Fatalf("@type %q, want StateChange", sc.Type)
	}
	return sc.Changed
}

// createPushSub registers a subscription for john and returns its id
// with the verification code captured from the immediate
// PushVerification POST (7.2.2).
func createPushSub(t *testing.T, ts *httptest.Server, hits chan pushHit, props string) (jmap.Id, string) {
	t.Helper()
	r := callAPI(t, ts, inv("PushSubscription/set", `{"create":{"p1":`+props+`}}`, "0"))
	args := methodArgs(t, r, 0, "PushSubscription/set")
	created, ok := args["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", args)
	}
	echo, ok := created["p1"].(map[string]any)
	if !ok {
		t.Fatalf("no created echo: %v", created)
	}
	id := jmap.Id(echo["id"].(string))
	h := nextHit(t, hits)
	var pv jmap.PushVerification
	if err := json.Unmarshal(h.body, &pv); err != nil {
		t.Fatalf("verification body %q: %v", h.body, err)
	}
	if pv.Type != "PushVerification" || pv.PushSubscriptionId != id || pv.VerificationCode == "" {
		t.Fatalf("verification object: %+v", pv)
	}
	return id, pv.VerificationCode
}

func verifyPushSub(t *testing.T, ts *httptest.Server, id jmap.Id, code string) {
	t.Helper()
	r := callAPI(t, ts, inv("PushSubscription/set",
		fmt.Sprintf(`{"update":{%q:{"verificationCode":%q}}}`, id, code), "0"))
	args := methodArgs(t, r, 0, "PushSubscription/set")
	updated, ok := args["updated"].(map[string]any)
	if !ok {
		t.Fatalf("verify failed: %v", args)
	}
	if _, ok := updated[string(id)]; !ok {
		t.Fatalf("verify not applied: %v", args)
	}
}

// wantSetError asserts m[key] is a SetError of the given type naming
// prop in its properties list (when prop is non-empty).
func wantSetError(t *testing.T, args map[string]any, group string, key jmap.Id, errType, prop string) {
	t.Helper()
	m, ok := args[group].(map[string]any)
	if !ok {
		t.Fatalf("no %s in %v", group, args)
	}
	e, ok := m[string(key)].(map[string]any)
	if !ok {
		t.Fatalf("no %s entry for %s: %v", group, key, m)
	}
	if e["type"] != errType {
		t.Fatalf("%s[%s].type = %v, want %s (%v)", group, key, e["type"], errType, e)
	}
	if prop != "" {
		props, _ := e["properties"].([]any)
		if !slices.Contains(props, any(prop)) {
			t.Fatalf("%s[%s].properties = %v, want %q listed", group, key, props, prop)
		}
	}
}

// TestPushSubscriptionLifecycle walks the whole section 7.2 story:
// create with server-set defaults, the immediate PushVerification POST,
// silence until verified, verification, StateChange delivery, expiry
// clamping, destroy.
func TestPushSubscriptionLifecycle(t *testing.T) {
	endpoint, hits := newPushEndpoint(t)
	rig := newPushRig(t, endpoint.Client(), nil)
	ts := rig.ts

	r := callAPI(t, ts, inv("PushSubscription/set",
		fmt.Sprintf(`{"create":{"p1":{"deviceClientId":"dev1","url":%q}}}`, endpoint.URL), "0"))
	args := methodArgs(t, r, 0, "PushSubscription/set")
	// PushSubscription/set has no state arguments or responses (7.2.2).
	if _, has := args["oldState"]; has {
		t.Fatalf("oldState in response: %v", args)
	}
	if _, has := args["newState"]; has {
		t.Fatalf("newState in response: %v", args)
	}
	echo := args["created"].(map[string]any)["p1"].(map[string]any)
	id := jmap.Id(echo["id"].(string))
	// Unsubmitted properties come back: keys and types default to null.
	if v, has := echo["keys"]; !has || v != nil {
		t.Fatalf("keys echo: %v (%v)", v, has)
	}
	if v, has := echo["types"]; !has || v != nil {
		t.Fatalf("types echo: %v (%v)", v, has)
	}
	// The server set expires (7.2: at least 48h, SHOULD be 7 days or
	// more for these non-time-bounded credentials).
	exp, err := time.Parse(time.RFC3339, echo["expires"].(string))
	if err != nil {
		t.Fatalf("expires %v: %v", echo["expires"], err)
	}
	if d := time.Until(exp); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour+time.Minute {
		t.Fatalf("default expires %v from now", d)
	}

	// The PushVerification POST arrives immediately with the RFC 8030
	// TTL header, and its code has real entropy (8.6).
	h := nextHit(t, hits)
	if got := h.header.Get("TTL"); got == "" {
		t.Error("no TTL header on push POST")
	}
	if got := h.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q", got)
	}
	var pv jmap.PushVerification
	if err := json.Unmarshal(h.body, &pv); err != nil {
		t.Fatal(err)
	}
	if pv.Type != "PushVerification" || pv.PushSubscriptionId != id || len(pv.VerificationCode) < 16 {
		t.Fatalf("verification: %+v", pv)
	}

	// Until the client verifies, the server MUST NOT make any further
	// requests to the URL (7.2): a data change stays undelivered.
	createNote(t, ts, `{"subject":"before verification"}`)
	noHit(t, hits)

	// /get (7.2.1): no accountId, no state, url and keys never returned,
	// verificationCode still null.
	r = callAPI(t, ts, inv("PushSubscription/get", `{"ids":null}`, "0"))
	g := methodArgs(t, r, 0, "PushSubscription/get")
	if _, has := g["accountId"]; has {
		t.Fatalf("accountId in /get response: %v", g)
	}
	if _, has := g["state"]; has {
		t.Fatalf("state in /get response: %v", g)
	}
	list := g["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list: %v", list)
	}
	obj := list[0].(map[string]any)
	if obj["id"] != string(id) || obj["deviceClientId"] != "dev1" || obj["verificationCode"] != nil {
		t.Fatalf("object: %v", obj)
	}
	for _, secret := range []string{"url", "keys"} {
		if _, has := obj[secret]; has {
			t.Fatalf("%s returned by /get: %v", secret, obj)
		}
	}

	// A wrong verification code MUST be rejected (7.2.2).
	r = callAPI(t, ts, inv("PushSubscription/set",
		fmt.Sprintf(`{"update":{%q:{"verificationCode":"wrong"}}}`, id), "0"))
	args = methodArgs(t, r, 0, "PushSubscription/set")
	wantSetError(t, args, "notUpdated", id, jmap.SetErrInvalidProperties, "verificationCode")

	verifyPushSub(t, ts, id, pv.VerificationCode)
	r = callAPI(t, ts, inv("PushSubscription/get", `{"ids":null}`, "0"))
	g = methodArgs(t, r, 0, "PushSubscription/get")
	if got := g["list"].([]any)[0].(map[string]any)["verificationCode"]; got != pv.VerificationCode {
		t.Fatalf("verificationCode after verify: %v", got)
	}

	// Now data changes are pushed as StateChange objects (7.1/7.2).
	createNote(t, ts, `{"subject":"after verification"}`)
	h = nextHit(t, hits)
	changed := decodeStateChange(t, h.body)
	if changed["Atest1"]["TestNote"] != "2" {
		t.Fatalf("changed: %v", changed)
	}

	// Extending the lifetime needs no re-verification, and the server
	// clamps the value, reporting what it kept in updated (7.2.2).
	far := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	r = callAPI(t, ts, inv("PushSubscription/set",
		fmt.Sprintf(`{"update":{%q:{"expires":%q}}}`, id, far), "0"))
	args = methodArgs(t, r, 0, "PushSubscription/set")
	amended, ok := args["updated"].(map[string]any)[string(id)].(map[string]any)
	if !ok {
		t.Fatalf("clamped update: %v", args)
	}
	kept, err := time.Parse(time.RFC3339, amended["expires"].(string))
	if err != nil || kept.After(time.Now().Add(7*24*time.Hour+time.Minute)) {
		t.Fatalf("clamped expires %v: %v", amended["expires"], err)
	}

	// Destroy stops delivery for good.
	r = callAPI(t, ts, inv("PushSubscription/set", fmt.Sprintf(`{"destroy":[%q]}`, id), "0"))
	args = methodArgs(t, r, 0, "PushSubscription/set")
	destroyed, _ := args["destroyed"].([]any)
	if !slices.Contains(destroyed, any(string(id))) {
		t.Fatalf("destroyed: %v", args)
	}
	createNote(t, ts, `{"subject":"after destroy"}`)
	noHit(t, hits)
}

// TestPushDeliveryEncrypted: when the client supplies keys, everything
// the server sends - the PushVerification included - MUST be encrypted
// per RFC 8291 (section 8.7).
func TestPushDeliveryEncrypted(t *testing.T) {
	endpoint, hits := newPushEndpoint(t)
	rig := newPushRig(t, endpoint.Client(), nil)
	ts := rig.ts

	uaKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatal(err)
	}
	r := callAPI(t, ts, inv("PushSubscription/set", fmt.Sprintf(
		`{"create":{"p1":{"deviceClientId":"dev-enc","url":%q,"keys":{"p256dh":%q,"auth":%q}}}}`,
		endpoint.URL,
		base64.RawURLEncoding.EncodeToString(uaKey.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(authSecret)), "0"))
	args := methodArgs(t, r, 0, "PushSubscription/set")
	echo, ok := args["created"].(map[string]any)["p1"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", args)
	}
	id := jmap.Id(echo["id"].(string))

	h := nextHit(t, hits)
	if got := h.header.Get("Content-Encoding"); got != "aes128gcm" {
		t.Fatalf("verification Content-Encoding %q", got)
	}
	plain, err := webpush.Decrypt(uaKey, authSecret, h.body)
	if err != nil {
		t.Fatal(err)
	}
	var pv jmap.PushVerification
	if err := json.Unmarshal(plain, &pv); err != nil {
		t.Fatal(err)
	}
	if pv.PushSubscriptionId != id {
		t.Fatalf("verification: %+v", pv)
	}
	verifyPushSub(t, ts, id, pv.VerificationCode)

	createNote(t, ts, `{"subject":"secret"}`)
	h = nextHit(t, hits)
	if got := h.header.Get("Content-Encoding"); got != "aes128gcm" {
		t.Fatalf("StateChange Content-Encoding %q", got)
	}
	plain, err = webpush.Decrypt(uaKey, authSecret, h.body)
	if err != nil {
		t.Fatal(err)
	}
	if changed := decodeStateChange(t, plain); changed["Atest1"]["TestNote"] != "1" {
		t.Fatalf("changed: %v", changed)
	}
}

// TestPushSubscriptionGetRules covers the 7.2.1 argument rules and the
// credential scoping of 7.2.
func TestPushSubscriptionGetRules(t *testing.T) {
	endpoint, hits := newPushEndpoint(t)
	rig := newPushRig(t, endpoint.Client(), nil)
	ts := rig.ts
	id, _ := createPushSub(t, ts, hits, fmt.Sprintf(`{"deviceClientId":"d1","url":%q}`, endpoint.URL))

	// url and keys may be secret to the device: requesting them is
	// forbidden; unknown properties and the (absent) accountId argument
	// are invalidArguments.
	for _, tc := range []struct{ name, argsJSON, want string }{
		{"url property", `{"properties":["url"]}`, jmap.ErrForbidden},
		{"keys property", `{"properties":["keys"]}`, jmap.ErrForbidden},
		{"unknown property", `{"properties":["bogus"]}`, jmap.ErrInvalidArguments},
		{"accountId argument", `{"accountId":"Atest1"}`, jmap.ErrInvalidArguments},
	} {
		r := callAPI(t, ts, inv("PushSubscription/get", tc.argsJSON, "0"))
		e := methodArgs(t, r, 0, "error")
		if e["type"] != tc.want {
			t.Errorf("%s: %v, want %s", tc.name, e["type"], tc.want)
		}
	}

	// Duplicate ids dedupe; unknown ids land in notFound.
	r := callAPI(t, ts, inv("PushSubscription/get",
		fmt.Sprintf(`{"ids":[%q,%q,"nope"]}`, id, id), "0"))
	g := methodArgs(t, r, 0, "PushSubscription/get")
	if list := g["list"].([]any); len(list) != 1 {
		t.Fatalf("list: %v", list)
	}
	if nf := g["notFound"].([]any); len(nf) != 1 || nf[0] != "nope" {
		t.Fatalf("notFound: %v", g["notFound"])
	}

	// A properties selection still always includes id.
	r = callAPI(t, ts, inv("PushSubscription/get", `{"properties":["expires"]}`, "0"))
	g = methodArgs(t, r, 0, "PushSubscription/get")
	obj := g["list"].([]any)[0].(map[string]any)
	if obj["id"] != string(id) || obj["expires"] == nil {
		t.Fatalf("selected: %v", obj)
	}
	if _, has := obj["deviceClientId"]; has {
		t.Fatalf("unselected property returned: %v", obj)
	}

	// Another user's credentials see nothing of john's subscription and
	// cannot touch it (7.2: tied to the creating credentials).
	r = callAPIAs(t, ts, "jane@example.com", "secret2",
		inv("PushSubscription/get", `{"ids":null}`, "0"))
	g = methodArgs(t, r, 0, "PushSubscription/get")
	if list := g["list"].([]any); len(list) != 0 {
		t.Fatalf("jane sees john's subscription: %v", list)
	}
	r = callAPIAs(t, ts, "jane@example.com", "secret2", inv("PushSubscription/set",
		fmt.Sprintf(`{"update":{%q:{"types":null}},"destroy":[%q]}`, id, id), "0"))
	args := methodArgs(t, r, 0, "PushSubscription/set")
	wantSetError(t, args, "notUpdated", id, jmap.SetErrNotFound, "")
	wantSetError(t, args, "notDestroyed", id, jmap.SetErrNotFound, "")
}

// TestPushSubscriptionSetValidation is the 7.2/7.2.2 property-rule
// table: every bad create or update is rejected with invalidProperties
// naming the property.
func TestPushSubscriptionSetValidation(t *testing.T) {
	endpoint, hits := newPushEndpoint(t)
	rig := newPushRig(t, endpoint.Client(), nil)
	ts := rig.ts

	for _, tc := range []struct{ name, props, wantProp string }{
		{"non-https url", `{"deviceClientId":"d","url":"http://push.example.com/x"}`, "url"},
		{"missing url", `{"deviceClientId":"d"}`, "url"},
		{"missing deviceClientId", `{"url":"https://push.example.com/x"}`, "deviceClientId"},
		{"verificationCode on create", `{"deviceClientId":"d","url":"https://push.example.com/x","verificationCode":"x"}`, "verificationCode"},
		{"server-set id", `{"id":"X","deviceClientId":"d","url":"https://push.example.com/x"}`, "id"},
		{"unknown property", `{"deviceClientId":"d","url":"https://push.example.com/x","frobnicate":1}`, "frobnicate"},
		{"unknown type name", `{"deviceClientId":"d","url":"https://push.example.com/x","types":["Bogus"]}`, "types"},
		{"malformed keys", `{"deviceClientId":"d","url":"https://push.example.com/x","keys":{"p256dh":"!!!","auth":"!!!"}}`, "keys"},
		{"invalid expires", `{"deviceClientId":"d","url":"https://push.example.com/x","expires":"tomorrow"}`, "expires"},
	} {
		r := callAPI(t, ts, inv("PushSubscription/set", `{"create":{"c":`+tc.props+`}}`, "0"))
		args := methodArgs(t, r, 0, "PushSubscription/set")
		wantSetError(t, args, "notCreated", "c", jmap.SetErrInvalidProperties, tc.wantProp)
	}
	// None of the rejected creates may have POSTed anything.
	noHit(t, hits)

	// url, keys, and deviceClientId are immutable (7.2.2).
	id, _ := createPushSub(t, ts, hits, fmt.Sprintf(`{"deviceClientId":"d","url":%q}`, endpoint.URL))
	for _, tc := range []struct{ name, patch, wantProp string }{
		{"url", `{"url":"https://other.example.com/x"}`, "url"},
		{"keys", `{"keys":{"p256dh":"x","auth":"y"}}`, "keys"},
		{"deviceClientId", `{"deviceClientId":"d2"}`, "deviceClientId"},
	} {
		r := callAPI(t, ts, inv("PushSubscription/set",
			fmt.Sprintf(`{"update":{%q:%s}}`, id, tc.patch), "0"))
		args := methodArgs(t, r, 0, "PushSubscription/set")
		wantSetError(t, args, "notUpdated", id, jmap.SetErrInvalidProperties, tc.wantProp)
	}

	// Unknown ids on update and destroy.
	r := callAPI(t, ts, inv("PushSubscription/set",
		`{"update":{"Pmissing":{"types":null}},"destroy":["Pmissing"]}`, "0"))
	args := methodArgs(t, r, 0, "PushSubscription/set")
	wantSetError(t, args, "notUpdated", "Pmissing", jmap.SetErrNotFound, "")
	wantSetError(t, args, "notDestroyed", "Pmissing", jmap.SetErrNotFound, "")
}

// TestPushSubscriptionLimits covers the section 8.6 MUSTs: a cap on
// subscriptions per credential and a cap on creation rate.
func TestPushSubscriptionLimits(t *testing.T) {
	t.Run("cap", func(t *testing.T) {
		endpoint, hits := newPushEndpoint(t)
		rig := newPushRig(t, endpoint.Client(), nil)
		rig.store.MaxPerCredential = 2
		for i := range 2 {
			createPushSub(t, rig.ts, hits, fmt.Sprintf(`{"deviceClientId":"d%d","url":%q}`, i, endpoint.URL))
		}
		r := callAPI(t, rig.ts, inv("PushSubscription/set",
			fmt.Sprintf(`{"create":{"c":{"deviceClientId":"d-over","url":%q}}}`, endpoint.URL), "0"))
		args := methodArgs(t, r, 0, "PushSubscription/set")
		wantSetError(t, args, "notCreated", "c", jmap.SetErrOverQuota, "")
	})

	t.Run("creation rate", func(t *testing.T) {
		endpoint, hits := newPushEndpoint(t)
		rig := newPushRig(t, endpoint.Client(), nil)
		for i := range createRateMax {
			createPushSub(t, rig.ts, hits, fmt.Sprintf(`{"deviceClientId":"d%d","url":%q}`, i, endpoint.URL))
		}
		r := callAPI(t, rig.ts, inv("PushSubscription/set",
			fmt.Sprintf(`{"create":{"c":{"deviceClientId":"d-fast","url":%q}}}`, endpoint.URL), "0"))
		args := methodArgs(t, r, 0, "PushSubscription/set")
		wantSetError(t, args, "notCreated", "c", jmap.SetErrRateLimit, "")
	})
}

// TestPushTypesFilter: a subscription with a types list only receives
// StateChange objects for those types (7.2).
func TestPushTypesFilter(t *testing.T) {
	endpoint, hits := newPushEndpoint(t)
	rig := newPushRig(t, endpoint.Client(), nil)
	ts := rig.ts

	id, code := createPushSub(t, ts, hits, fmt.Sprintf(
		`{"deviceClientId":"d","url":%q,"types":["TestTask"]}`, endpoint.URL))
	verifyPushSub(t, ts, id, code)

	// A TestNote change is filtered out entirely; the following TestTask
	// change is delivered without any TestNote entry.
	createNote(t, ts, `{"subject":"quiet"}`)
	r := callAPI(t, ts, inv("TestTask/set",
		`{"accountId":"Atest1","create":{"c":{"title":"loud"}}}`, "0"))
	if _, ok := methodArgs(t, r, 0, "TestTask/set")["created"].(map[string]any); !ok {
		t.Fatal("task create failed")
	}
	h := nextHit(t, hits)
	changed := decodeStateChange(t, h.body)
	if changed["Atest1"]["TestTask"] != "2" {
		t.Fatalf("changed: %v", changed)
	}
	if _, has := changed["Atest1"]["TestNote"]; has {
		t.Fatalf("filtered type delivered: %v", changed)
	}
}

// TestPushWatcherRestart: deliveries survive a server restart - a new
// Server over the same backend resumes watching every verified
// subscription (EnablePush's resume path).
func TestPushWatcherRestart(t *testing.T) {
	endpoint, hits := newPushEndpoint(t)
	rig1 := newPushRig(t, endpoint.Client(), nil)
	id, code := createPushSub(t, rig1.ts, hits, fmt.Sprintf(`{"deviceClientId":"d","url":%q}`, endpoint.URL))
	verifyPushSub(t, rig1.ts, id, code)
	rig1.ts.Close()
	rig1.srv.Close()

	rig2 := newPushRig(t, endpoint.Client(), rig1)
	createNote(t, rig2.ts, `{"subject":"after restart"}`)
	h := nextHit(t, hits)
	if changed := decodeStateChange(t, h.body); changed["Atest1"]["TestNote"] != "1" {
		t.Fatalf("changed: %v", changed)
	}
}
