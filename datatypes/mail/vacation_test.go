package mail

// VacationResponse tests (RFC 8621 section 8) and the RFC 3834 responder:
// the singleton get/set semantics, and the delivery-side auto-reply - who
// gets one (through the real submission queue, with the section 3 reply
// form) and, mostly, who never does (the section 2 refusal rules).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// newVacationServer is newWorkerServer plus the VacationResponse type and
// capability, returning the queue for the responder option.
func newVacationServer(t *testing.T) (*httptest.Server, *objectdb.DB, blob.Store, *SubmissionQueue, *SubmissionWorker, *fakeSubmitter) {
	t.Helper()
	limits := DefaultSubmissionLimits()
	a := newStaticAuth()
	a.AddUser("john@example.com", "secret", testAccount)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithVerifyPreImages())
	store := kvstore.New(memory.New())
	p := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	policy := &submissionPolicy{StaticSendPolicy: NewStaticSendPolicy()}
	policy.Allow(testAccount, "john@example.com", "*@corp.example")
	fake := &fakeSubmitter{}
	if err := RegisterMailbox(p, db, core); err != nil {
		t.Fatal(err)
	}
	if err := RegisterThread(p, db, core); err != nil {
		t.Fatal(err)
	}
	if err := RegisterEmail(p, db, store, core, DefaultAccountCapability(), nil); err != nil {
		t.Fatal(err)
	}
	if err := RegisterIdentity(p, db, policy, core); err != nil {
		t.Fatal(err)
	}
	q, err := RegisterEmailSubmission(p, db, store, core, policy, limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterVacationResponse(p, db); err != nil {
		t.Fatal(err)
	}
	w, err := NewSubmissionWorker(q, fake, SubmissionWorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: time.Now().Add(2 * time.Second)}
	w.now = clock.now
	w.rand = func() float64 { return 0.5 }
	srv, err := runtime.NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability(CapabilityURI, struct{}{}, DefaultAccountCapability()); err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability(SubmissionCapabilityURI, struct{}{}, SubmissionAccountCapabilityFor(limits)); err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability(VacationCapabilityURI, struct{}{}, struct{}{}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, db, store, q, w, fake
}

// callVac posts a request opted into core + mail + submission + vacation.
func callVac(t *testing.T, ts *httptest.Server, calls ...jmap.Invocation) *jmap.Response {
	t.Helper()
	req := map[string]any{
		"using":       []string{jmap.CoreCapability, CapabilityURI, SubmissionCapabilityURI, VacationCapabilityURI},
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

// enableVacation turns the responder on with the given extra properties.
func enableVacation(t *testing.T, ts *httptest.Server, extraProps string) {
	t.Helper()
	res := vacSet(t, ts, `"update":{"singleton":{"isEnabled":true`+extraProps+`}}`)
	if _, ok := res["updated"].(map[string]any)["singleton"]; !ok {
		t.Fatalf("enable failed: %v", res)
	}
}

// TestVacationResponseSingleton: the section 8 object semantics - defaults
// before any write, id "singleton", create/destroy refused with the
// singleton SetError, updates round-trip, unknown ids are notFound,
// unknown properties are invalidProperties, and ifInState is honored.
func TestVacationResponseSingleton(t *testing.T) {
	ts, _, _, _, _, _ := newVacationServer(t)

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

// vacationDeliver delivers raw for sender -> john@example.com through a
// responder-enabled Deliverer.
func vacationDeliver(t *testing.T, db *objectdb.DB, store blob.Store, q *SubmissionQueue, sender, raw string) {
	t.Helper()
	d := NewDeliverer(db, store, mapResolver{
		"john@example.com": testAccount,
		"jane@example.com": testAccount,
	}, WithVacationResponder(q))
	evs := d.Deliver(context.Background(), deliveryEnv(sender, "john@example.com"), strings.NewReader(raw))
	if evs[0].Outcome != Accepted {
		t.Fatalf("delivery not accepted: %+v", evs[0])
	}
}

func submissionCount(t *testing.T, db *objectdb.DB) int {
	t.Helper()
	ids, err := db.AllIds(context.Background(), testAccount, TypeEmailSubmission, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(ids)
}

// inboundMsg is a plain message from sender addressed To john.
func inboundMsg(extraHeaders string) string {
	return "From: Visitor <visitor@remote.example>\r\n" +
		"To: John <john@example.com>\r\n" +
		"Subject: Question\r\n" +
		"Date: Thu, 17 Jul 2026 10:00:00 +0000\r\n" +
		"Message-ID: <q1@remote.example>\r\n" +
		extraHeaders +
		"\r\nAre you around?\r\n"
}

// TestVacationResponderReplies: the full RFC 3834 section 3 reply through
// the real queue - a real Email in the sent mailbox, a real
// EmailSubmission with the null reverse-path and NOTIFY=NEVER, relayed by
// the worker with the reply headers the spec mandates - and the section 2
// per-sender suppression admitting one reply per sender per period.
func TestVacationResponderReplies(t *testing.T) {
	ts, db, store, q, w, fake := newVacationServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	sent := createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
	createIdentity(t, ts, "john@example.com")
	enableVacation(t, ts, `,"textBody":"I am away until August."`)

	vacationDeliver(t, db, store, q, "visitor@remote.example", inboundMsg(""))
	if n := submissionCount(t, db); n != 1 {
		t.Fatalf("submissions after delivery = %d, want 1", n)
	}
	// The reply Email sits in the sent mailbox, seen.
	sentIds, err := db.IdsWhereMember(context.Background(), testAccount, TypeEmail, "mailboxIds", sent)
	if err != nil || len(sentIds) != 1 {
		t.Fatalf("sent mailbox emails: %v %v", sentIds, err)
	}

	// The worker relays it like any queued submission.
	if n, _, err := w.ProcessDue(context.Background(), 0); err != nil || n != 1 {
		t.Fatalf("relay: %d %v", n, err)
	}
	call := fake.call(0)
	if call.env.MailFrom != "" {
		t.Fatalf("reply MAIL FROM = %q, want the null reverse-path", call.env.MailFrom)
	}
	if len(call.env.Recipients) != 1 || call.env.Recipients[0].Email != "visitor@remote.example" {
		t.Fatalf("reply recipients = %+v", call.env.Recipients)
	}
	if v := call.env.Recipients[0].Parameters["NOTIFY"]; v == nil || *v != "NEVER" {
		t.Fatalf("reply NOTIFY = %v, want NEVER", v)
	}
	msg := call.msg
	for _, want := range []string{
		"Auto-Submitted: auto-replied\r\n", // RFC 3834 section 3.1.7 MUST
		"Subject: Auto: Question\r\n",      // section 3.1.5 prefix on the original subject
		"To: <visitor@remote.example>\r\n", // section 3.1.3: the Return-Path address only
		"From: <john@example.com>\r\n",
		"In-Reply-To: <q1@remote.example>\r\n", // section 3.1.4
		"I am away until August.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("reply missing %q:\n%s", want, msg)
		}
	}

	// Same sender again inside the period: suppressed (section 2's
	// per-sender history, one reply per period).
	vacationDeliver(t, db, store, q, "visitor@remote.example", inboundMsg(""))
	if n := submissionCount(t, db); n != 1 {
		t.Fatalf("suppression failed: %d submissions", n)
	}
	// A different sender gets their own reply.
	other := strings.ReplaceAll(inboundMsg(""), "visitor@remote.example", "other@remote.example")
	vacationDeliver(t, db, store, q, "other@remote.example", other)
	if n := submissionCount(t, db); n != 2 {
		t.Fatalf("second sender not answered: %d submissions", n)
	}
}

// TestVacationResponderRefusals: every RFC 3834 section 2 rule and RFC
// 8621 section 8 gate under which no reply may be produced. Each case
// delivers successfully and must leave the queue empty.
func TestVacationResponderRefusals(t *testing.T) {
	cases := []struct {
		name   string
		setup  string // extra vacation properties
		sender string
		raw    string
	}{
		{"null reverse-path (a bounce)", "", "", inboundMsg("")},
		{"Auto-Submitted: auto-generated", "", "visitor@remote.example", inboundMsg("Auto-Submitted: auto-generated\r\n")},
		{"Auto-Submitted: auto-replied", "", "visitor@remote.example", inboundMsg("Auto-Submitted: auto-replied\r\n")},
		{"list mail (List-Id)", "", "visitor@remote.example", inboundMsg("List-Id: Fans <fans.lists.example>\r\n")},
		{"list mail (List-Unsubscribe)", "", "visitor@remote.example", inboundMsg("List-Unsubscribe: <mailto:leave@lists.example>\r\n")},
		{"recipient not in To or Cc", "", "visitor@remote.example",
			"From: Visitor <visitor@remote.example>\r\nTo: Someone <else@remote.example>\r\n" +
				"Subject: Bcc-style\r\nDate: Thu, 17 Jul 2026 10:00:00 +0000\r\n\r\nhello\r\n"},
		{"MAILER-DAEMON sender", "", "MAILER-DAEMON@remote.example", inboundMsg("")},
		{"owner- sender", "", "owner-fans@lists.example", inboundMsg("")},
		{"-request sender", "", "fans-request@lists.example", inboundMsg("")},
		{"own address as sender", "", "john@example.com", inboundMsg("")},
		{"before fromDate", `,"fromDate":"2030-01-01T00:00:00Z"`, "visitor@remote.example", inboundMsg("")},
		{"after toDate", `,"toDate":"2020-01-01T00:00:00Z"`, "visitor@remote.example", inboundMsg("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, db, store, q, _, _ := newVacationServer(t)
			createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
			createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
			createIdentity(t, ts, "john@example.com")
			enableVacation(t, ts, tc.setup)
			vacationDeliver(t, db, store, q, tc.sender, tc.raw)
			if n := submissionCount(t, db); n != 0 {
				t.Fatalf("%s: %d submissions queued, want 0", tc.name, n)
			}
		})
	}

	t.Run("disabled", func(t *testing.T) {
		ts, db, store, q, _, _ := newVacationServer(t)
		createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
		createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
		createIdentity(t, ts, "john@example.com")
		vacationDeliver(t, db, store, q, "visitor@remote.example", inboundMsg(""))
		if n := submissionCount(t, db); n != 0 {
			t.Fatalf("disabled vacation replied: %d", n)
		}
	})
	t.Run("no identity for the address", func(t *testing.T) {
		ts, db, store, q, _, _ := newVacationServer(t)
		createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
		createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
		enableVacation(t, ts, "")
		vacationDeliver(t, db, store, q, "visitor@remote.example", inboundMsg(""))
		if n := submissionCount(t, db); n != 0 {
			t.Fatalf("identityless account replied: %d", n)
		}
	})
	t.Run("no sent mailbox", func(t *testing.T) {
		ts, db, store, q, _, _ := newVacationServer(t)
		createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
		createIdentity(t, ts, "john@example.com")
		enableVacation(t, ts, "")
		vacationDeliver(t, db, store, q, "visitor@remote.example", inboundMsg(""))
		if n := submissionCount(t, db); n != 0 {
			t.Fatalf("sentless account replied: %d", n)
		}
	})
}

// TestVacationReplyHeaderHardening: a hostile or non-ASCII original
// subject cannot break the reply's header block - controls are refused by
// the ASCII gate and everything non-ASCII rides in RFC 2047 encoded-words.
func TestVacationReplyHeaderHardening(t *testing.T) {
	full := vacationDefaults()
	raw, _ := buildVacationReply("john@example.com", "visitor@remote.example",
		mustParse(t, "From: v@remote.example\r\nTo: john@example.com\r\n"+
			"Subject: =?utf-8?B?xJxlbmVyaWM=?= r\u00e9sum\u00e9\r\nMessage-ID: <x@remote.example>\r\n\r\nhi\r\n"),
		full, time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC))
	header, _, _ := strings.Cut(raw, "\r\n\r\n")
	for _, line := range strings.Split(header, "\r\n") {
		for i := 0; i < len(line); i++ {
			if line[i] < 0x20 && line[i] != '\t' || line[i] > 0x7e {
				t.Fatalf("reply header carries a raw non-ASCII or control octet: %q", line)
			}
		}
	}
	if !strings.Contains(raw, "=?utf-8?B?") {
		t.Fatalf("non-ASCII subject not encoded:\n%s", raw)
	}
}

func mustParse(t *testing.T, raw string) *parsed {
	t.Helper()
	p, err := parseMessage(strings.NewReader(raw), newCapture())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
