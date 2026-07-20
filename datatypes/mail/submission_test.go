package mail

// EmailSubmission tests (RFC 8621 sections 7 and 7.5): the send flow
// modeled on the section 7.5.1 example (submit + onSuccessUpdateEmail
// moving the draft), envelope derivation, the create error taxonomy, the
// FUTURERELEASE hold, cancel semantics, the onSuccess destroy
// continuation, and query/sort.

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

// submissionPolicy wraps StaticSendPolicy with a switchable CanSend, so a
// test can revoke sending after identities exist (forbiddenToSend).
type submissionPolicy struct {
	*StaticSendPolicy
	denySend bool
}

func (p *submissionPolicy) CanSend(ctx context.Context, acct jmap.Id) (bool, string) {
	if p.denySend {
		return false, "sending suspended for this account"
	}
	return p.StaticSendPolicy.CanSend(ctx, acct)
}

// newSubmissionServer wires the full mail + submission surface: Mailbox,
// Thread, Email, Identity, and EmailSubmission on one db, with the given
// limits. The returned policy grants john@example.com and the
// *@corp.example wildcard.
func newSubmissionServer(t *testing.T, limits SubmissionLimits) (*httptest.Server, *objectdb.DB, blob.Store, *submissionPolicy) {
	t.Helper()
	a := newStaticAuth()
	a.AddUser("john@example.com", "secret", testAccount)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	store := kvstore.New(memory.New())
	p := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	policy := &submissionPolicy{StaticSendPolicy: NewStaticSendPolicy()}
	policy.Allow(testAccount, "john@example.com", "*@corp.example")
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
	if _, err := RegisterEmailSubmission(p, db, store, core, policy, limits); err != nil {
		t.Fatal(err)
	}
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
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, db, store, policy
}

func submissionServer(t *testing.T) (*httptest.Server, *objectdb.DB, blob.Store, *submissionPolicy) {
	return newSubmissionServer(t, DefaultSubmissionLimits())
}

// callSub posts a request opted into core + mail + submission.
func callSub(t *testing.T, ts *httptest.Server, calls ...jmap.Invocation) *jmap.Response {
	t.Helper()
	req := map[string]any{
		"using":       []string{jmap.CoreCapability, CapabilityURI, SubmissionCapabilityURI},
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

// sendableMsg is a strictly valid RFC 5322 message; extra headers extend
// or override nothing (they are appended).
func sendableMsg(extra map[string]string) string {
	h := "From: john@example.com\r\n" +
		"To: jane@remote.example\r\n" +
		"Subject: Hello\r\n" +
		"Date: Thu, 17 Jul 2026 10:00:00 +0000\r\n" +
		"Message-ID: <m1@example.com>\r\n"
	for k, v := range extra {
		h += k + ": " + v + "\r\n"
	}
	return h + "\r\nbody\r\n"
}

// createIdentity makes an Identity for email and returns its id.
func createIdentity(t *testing.T, ts *httptest.Server, email string) string {
	t.Helper()
	r := callSub(t, ts, inv("Identity/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"i1":{"email":%q}}}`, testAccount, email), "0"))
	created, ok := methodArgs(t, r, 0, "Identity/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("identity create failed: %v", r.MethodResponses[0].Args)
	}
	return created["i1"].(map[string]any)["id"].(string)
}

// notCreatedErr digs the SetError for cid out of an EmailSubmission/set
// response.
func notCreatedErr(t *testing.T, r *jmap.Response, cid string) map[string]any {
	t.Helper()
	nc, ok := methodArgs(t, r, 0, "EmailSubmission/set")["notCreated"].(map[string]any)
	if !ok {
		t.Fatalf("expected notCreated: %v", r.MethodResponses[0].Args)
	}
	serr, ok := nc[cid].(map[string]any)
	if !ok {
		t.Fatalf("no notCreated entry for %s: %v", cid, nc)
	}
	return serr
}

// TestEmailSubmissionSendFlow is the RFC 8621 section 7.5.1 example
// reshaped onto this server: a saved draft is submitted with a derived
// envelope, and onSuccessUpdateEmail moves it from Drafts to Sent and
// clears $draft through the implicit Email/set, both responses sharing
// the call id.
func TestEmailSubmissionSendFlow(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	sent := createMailbox(t, ts, `{"name":"Sent"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil),
		map[string]bool{drafts: true}, map[string]bool{"$draft": true})

	r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"k1490":{"identityId":%q,"emailId":%q}},
		  "onSuccessUpdateEmail":{"#k1490":{
		    "mailboxIds/%s":null,"mailboxIds/%s":true,"keywords/$draft":null}}}`,
		testAccount, identityId, emailId, drafts, sent), "0"))
	if len(r.MethodResponses) != 2 {
		t.Fatalf("want EmailSubmission/set + implicit Email/set, got %v", r.MethodResponses)
	}
	if r.MethodResponses[1].CallID != "0" {
		t.Fatalf("implicit set call id = %q", r.MethodResponses[1].CallID)
	}

	created, ok := methodArgs(t, r, 0, "EmailSubmission/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", r.MethodResponses[0].Args)
	}
	echo := created["k1490"].(map[string]any)
	if echo["id"] == "" || echo["undoStatus"] != "pending" {
		t.Fatalf("echo = %v", echo)
	}
	// The derived envelope is echoed: mailFrom from the From header,
	// rcptTo from To.
	env := echo["envelope"].(map[string]any)
	if env["mailFrom"].(map[string]any)["email"] != "john@example.com" {
		t.Errorf("mailFrom = %v", env["mailFrom"])
	}
	rcpts := env["rcptTo"].([]any)
	if len(rcpts) != 1 || rcpts[0].(map[string]any)["email"] != "jane@remote.example" {
		t.Errorf("rcptTo = %v", rcpts)
	}
	ds := echo["deliveryStatus"].(map[string]any)["jane@remote.example"].(map[string]any)
	if ds["delivered"] != "queued" || ds["displayed"] != "unknown" {
		t.Errorf("deliveryStatus = %v", ds)
	}
	if !jmap.ValidUTCDate(echo["sendAt"].(string)) {
		t.Errorf("sendAt = %v", echo["sendAt"])
	}
	if len(echo["dsnBlobIds"].([]any)) != 0 || len(echo["mdnBlobIds"].([]any)) != 0 {
		t.Errorf("DSN/MDN lists not empty: %v", echo)
	}

	// The implicit Email/set applied the requested patch.
	upd, ok := methodArgs(t, r, 1, "Email/set")["updated"].(map[string]any)
	if !ok {
		t.Fatalf("implicit set failed: %v", r.MethodResponses[1].Args)
	}
	if _, has := upd[emailId]; !has {
		t.Fatalf("implicit set updated %v, want %s", upd, emailId)
	}
	got := emailGet(t, ts, emailId, `,"properties":["mailboxIds","keywords","threadId"]`)
	boxes := got["mailboxIds"].(map[string]any)
	if len(boxes) != 1 || boxes[sent] != true {
		t.Errorf("mailboxIds after send = %v", boxes)
	}
	if len(got["keywords"].(map[string]any)) != 0 {
		t.Errorf("keywords after send = %v", got["keywords"])
	}

	// The stored submission: threadId snapshotted from the Email, and the
	// internal queue properties never on the wire.
	subId := echo["id"].(string)
	r = callSub(t, ts, inv("EmailSubmission/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q]}`, testAccount, subId), "0"))
	sub := methodArgs(t, r, 0, "EmailSubmission/get")["list"].([]any)[0].(map[string]any)
	if sub["threadId"] != got["threadId"] || sub["emailId"] != emailId || sub["identityId"] != identityId {
		t.Errorf("stored submission = %v", sub)
	}
	for _, hidden := range []string{"attempts", "nextAttemptAt", "claimedAt", "blobId"} {
		if _, has := sub[hidden]; has {
			t.Errorf("internal property %q on the wire", hidden)
		}
	}
	// But the queue state is in the record: nextAttemptAt = sendAt.
	stored, err := db.Get(context.Background(), testAccount, TypeEmailSubmission, jmap.Id(subId))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored["nextAttemptAt"]) == "" || !jsonEq(stored["nextAttemptAt"], stored["sendAt"]) {
		t.Errorf("nextAttemptAt = %s, sendAt = %s", stored["nextAttemptAt"], stored["sendAt"])
	}
}

func jsonEq(a, b json.RawMessage) bool { return string(a) == string(b) }

// TestEmailSubmissionComposeAndSend: one request composes a draft
// (Email/set create) and submits it via a "#" creation reference - the
// request-wide creation-id map crossing method boundaries.
func TestEmailSubmissionComposeAndSend(t *testing.T) {
	ts, _, _, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")

	r := callSub(t, ts,
		inv("Email/set", fmt.Sprintf(
			`{"accountId":%q,"create":{"d1":{"mailboxIds":{%q:true},
			  "from":[{"email":"john@example.com"}],"to":[{"email":"jane@remote.example"}],
			  "subject":"composed","bodyValues":{"b":{"value":"hi"}},
			  "textBody":[{"partId":"b","type":"text/plain"}]}}}`, testAccount, drafts), "0"),
		inv("EmailSubmission/set", fmt.Sprintf(
			`{"accountId":%q,"create":{"s1":{"identityId":%q,"emailId":"#d1"}}}`,
			testAccount, identityId), "1"))
	if _, ok := methodArgs(t, r, 0, "Email/set")["created"].(map[string]any); !ok {
		t.Fatalf("compose failed: %v", r.MethodResponses[0].Args)
	}
	created, ok := methodArgs(t, r, 1, "EmailSubmission/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("submit failed: %v", r.MethodResponses[1].Args)
	}
	env := created["s1"].(map[string]any)["envelope"].(map[string]any)
	if env["mailFrom"].(map[string]any)["email"] != "john@example.com" {
		t.Errorf("mailFrom = %v", env["mailFrom"])
	}
}

// TestEmailSubmissionEnvelopeDerivation: Sender wins over From, the
// Identity's address substitutes for a sender it does not allow, and
// rcptTo is the deduplicated union of To/Cc/Bcc (RFC 8621 section 7).
func TestEmailSubmissionEnvelopeDerivation(t *testing.T) {
	ts, db, store, policy := submissionServer(t)
	policy.Allow(testAccount, "mailer@service.example")
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")

	// Sender present and allowed by policy but NOT by the exact Identity:
	// the Identity's email is substituted as mailFrom (section 7).
	msg := "From: john@example.com\r\n" +
		"Sender: mailer@service.example\r\n" +
		"To: a@x.example, b@x.example\r\n" +
		"Cc: b@x.example, c@x.example\r\n" +
		"Bcc: d@x.example\r\n" +
		"Subject: derive\r\n" +
		"Date: Thu, 17 Jul 2026 10:00:00 +0000\r\n" +
		"\r\nbody\r\n"
	emailId := putEmail(t, db, store, msg, map[string]bool{drafts: true}, nil)

	r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"s":{"identityId":%q,"emailId":%q}}}`,
		testAccount, identityId, emailId), "0"))
	created, ok := methodArgs(t, r, 0, "EmailSubmission/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", r.MethodResponses[0].Args)
	}
	env := created["s"].(map[string]any)["envelope"].(map[string]any)
	if env["mailFrom"].(map[string]any)["email"] != "john@example.com" {
		t.Errorf("substituted mailFrom = %v", env["mailFrom"])
	}
	var got []string
	for _, r := range env["rcptTo"].([]any) {
		got = append(got, r.(map[string]any)["email"].(string))
	}
	want := []string{"a@x.example", "b@x.example", "c@x.example", "d@x.example"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("rcptTo = %v, want %v", got, want)
	}
}

// TestEmailSubmissionCreateErrors: the section 7.5 create error taxonomy.
func TestEmailSubmissionCreateErrors(t *testing.T) {
	ts, db, store, policy := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	goodEmail := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	mb := map[string]bool{drafts: true}

	submit := func(createObj string) *jmap.Response {
		return callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
			`{"accountId":%q,"create":{"s":%s}}`, testAccount, createObj), "0"))
	}

	// Server-set and unknown properties are rejected up front.
	r := submit(fmt.Sprintf(`{"identityId":%q,"emailId":%q,"undoStatus":"final"}`, identityId, goodEmail))
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidProperties" || e["properties"].([]any)[0] != "undoStatus" {
		t.Errorf("undoStatus at create: %v", e)
	}
	// Unresolvable Email and Identity ids are invalidProperties (7.5).
	r = submit(fmt.Sprintf(`{"identityId":%q,"emailId":"Enope"}`, identityId))
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidProperties" {
		t.Errorf("bad emailId: %v", e)
	}
	r = submit(fmt.Sprintf(`{"identityId":"Inope","emailId":%q}`, goodEmail))
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidProperties" {
		t.Errorf("bad identityId: %v", e)
	}

	// invalidEmail lists the invalid Email properties: a draft may lack a
	// Date or From (legal at rest, 4.6), but not at submission.
	noDate := putEmail(t, db, store,
		"From: john@example.com\r\nTo: j@x.example\r\nSubject: s\r\n\r\nb\r\n", mb, nil)
	r = submit(fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityId, noDate))
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidEmail" || e["properties"].([]any)[0] != "sentAt" {
		t.Errorf("missing Date: %v", e)
	}
	noFrom := putEmail(t, db, store,
		"To: j@x.example\r\nSubject: s\r\nDate: Thu, 17 Jul 2026 10:00:00 +0000\r\n\r\nb\r\n", mb, nil)
	r = submit(fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityId, noFrom))
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidEmail" || e["properties"].([]any)[0] != "from" {
		t.Errorf("missing From: %v", e)
	}
	// Multi-address From without Sender violates RFC 5322 3.6.2.
	multiFrom := putEmail(t, db, store,
		"From: john@example.com, boss@example.com\r\nTo: j@x.example\r\nDate: Thu, 17 Jul 2026 10:00:00 +0000\r\n\r\nb\r\n", mb, nil)
	r = submit(fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityId, multiFrom))
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidEmail" || e["properties"].([]any)[0] != "sender" {
		t.Errorf("multi-From without Sender: %v", e)
	}

	// noRecipients: a message with no To/Cc/Bcc derives an empty rcptTo.
	noRcpt := putEmail(t, db, store,
		"From: john@example.com\r\nSubject: s\r\nDate: Thu, 17 Jul 2026 10:00:00 +0000\r\n\r\nb\r\n", mb, nil)
	r = submit(fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityId, noRcpt))
	if e := notCreatedErr(t, r, "s"); e["type"] != "noRecipients" {
		t.Errorf("noRecipients: %v", e)
	}
	// invalidRecipients names the bad addresses.
	r = submit(fmt.Sprintf(`{"identityId":%q,"emailId":%q,
		"envelope":{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"ok@x.example"},{"email":"bogus"}]}}`,
		identityId, goodEmail))
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidRecipients" || e["invalidRecipients"].([]any)[0] != "bogus" {
		t.Errorf("invalidRecipients: %v", e)
	}
	// A malformed envelope is invalidProperties.
	r = submit(fmt.Sprintf(`{"identityId":%q,"emailId":%q,"envelope":42}`, identityId, goodEmail))
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidProperties" {
		t.Errorf("junk envelope: %v", e)
	}

	// forbiddenMailFrom: an envelope sender the policy does not grant.
	r = submit(fmt.Sprintf(`{"identityId":%q,"emailId":%q,
		"envelope":{"mailFrom":{"email":"stranger@evil.example"},"rcptTo":[{"email":"j@x.example"}]}}`,
		identityId, goodEmail))
	if e := notCreatedErr(t, r, "s"); e["type"] != "forbiddenMailFrom" {
		t.Errorf("forbiddenMailFrom: %v", e)
	}
	// forbiddenFrom: the message's From is outside the Identity, even
	// with a permitted envelope sender.
	policy.Allow(testAccount, "boss@example.com")
	bossFrom := putEmail(t, db, store,
		"From: boss@example.com\r\nTo: j@x.example\r\nDate: Thu, 17 Jul 2026 10:00:00 +0000\r\n\r\nb\r\n", mb, nil)
	r = submit(fmt.Sprintf(`{"identityId":%q,"emailId":%q,
		"envelope":{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"j@x.example"}]}}`,
		identityId, bossFrom))
	if e := notCreatedErr(t, r, "s"); e["type"] != "forbiddenFrom" {
		t.Errorf("forbiddenFrom: %v", e)
	}
	// forbiddenToSend carries the policy's reason for the user.
	policy.denySend = true
	r = submit(fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityId, goodEmail))
	if e := notCreatedErr(t, r, "s"); e["type"] != "forbiddenToSend" || e["description"] != "sending suspended for this account" {
		t.Errorf("forbiddenToSend: %v", e)
	}
	policy.denySend = false
}

// TestEmailSubmissionLimits: tooManyRecipients carries maxRecipients and
// tooLarge carries maxSize (section 7.5).
func TestEmailSubmissionLimits(t *testing.T) {
	limits := DefaultSubmissionLimits()
	limits.MaxRecipients = 2
	limits.MaxMessageBytes = 60
	ts, db, store, _ := newSubmissionServer(t, limits)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	// sendableMsg is comfortably over 60 octets.
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"s":{"identityId":%q,"emailId":%q,
		  "envelope":{"mailFrom":{"email":"john@example.com"},
		    "rcptTo":[{"email":"a@x.example"},{"email":"b@x.example"},{"email":"c@x.example"}]}}}}`,
		testAccount, identityId, emailId), "0"))
	if e := notCreatedErr(t, r, "s"); e["type"] != "tooManyRecipients" || e["maxRecipients"] != float64(2) {
		t.Errorf("tooManyRecipients: %v", e)
	}
	r = callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"s":{"identityId":%q,"emailId":%q}}}`,
		testAccount, identityId, emailId), "0"))
	if e := notCreatedErr(t, r, "s"); e["type"] != "tooLarge" || e["maxSize"] != float64(60) {
		t.Errorf("tooLarge: %v", e)
	}
}

// TestEmailSubmissionFutureRelease: the RFC 4865 hold sets sendAt, is
// consumed from the stored envelope, and over-limit or conflicting holds
// are refused.
func TestEmailSubmissionFutureRelease(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	submit := func(params string) *jmap.Response {
		return callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
			`{"accountId":%q,"create":{"s":{"identityId":%q,"emailId":%q,
			  "envelope":{"mailFrom":{"email":"john@example.com","parameters":%s},
			    "rcptTo":[{"email":"j@x.example"}]}}}}`,
			testAccount, identityId, emailId, params), "0"))
	}

	// HOLDFOR: sendAt lands the interval in the future, and the stored
	// envelope no longer carries the parameter (the queue owns the hold).
	before := time.Now().UTC()
	r := submit(`{"HOLDFOR":"3600"}`)
	created, ok := methodArgs(t, r, 0, "EmailSubmission/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("HOLDFOR create failed: %v", r.MethodResponses[0].Args)
	}
	echo := created["s"].(map[string]any)
	sendAt, err := time.Parse(time.RFC3339, echo["sendAt"].(string))
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := before.Add(3590*time.Second), before.Add(3620*time.Second)
	if sendAt.Before(lo) || sendAt.After(hi) {
		t.Errorf("sendAt = %v, want about %v", sendAt, before.Add(time.Hour))
	}
	env := echo["envelope"].(map[string]any)
	if params := env["mailFrom"].(map[string]any)["parameters"]; params != nil {
		t.Errorf("hold parameter not consumed: %v", params)
	}

	// HOLDUNTIL: sendAt is the given instant.
	until := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	r = submit(fmt.Sprintf(`{"HOLDUNTIL":%q}`, until.Format(time.RFC3339)))
	created, ok = methodArgs(t, r, 0, "EmailSubmission/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("HOLDUNTIL create failed: %v", r.MethodResponses[0].Args)
	}
	if got := created["s"].(map[string]any)["sendAt"].(string); got != until.Format(time.RFC3339) {
		t.Errorf("sendAt = %v, want %v", got, until.Format(time.RFC3339))
	}

	// Over-limit and conflicting holds are rejected, not clamped (RFC
	// 4865 section 4.2).
	for _, params := range []string{
		fmt.Sprintf(`{"HOLDFOR":"%d"}`, DefaultSubmissionLimits().MaxDelayedSend+1),
		`{"HOLDFOR":"3600","HOLDUNTIL":"2027-01-01T00:00:00Z"}`,
		`{"HOLDFOR":"not-a-number"}`,
		`{"HOLDFOR":null}`,
	} {
		r = submit(params)
		if e := notCreatedErr(t, r, "s"); e["type"] != "invalidProperties" {
			t.Errorf("hold %s: %v", params, e)
		}
	}

	// A server with delayed send disabled refuses every hold.
	ts2, db2, store2, _ := newSubmissionServer(t, SubmissionLimits{MaxRecipients: 10, MaxMessageBytes: 1 << 20})
	drafts2 := createMailbox(t, ts2, `{"name":"Drafts"}`)
	identity2 := createIdentity(t, ts2, "john@example.com")
	email2 := putEmail(t, db2, store2, sendableMsg(nil), map[string]bool{drafts2: true}, nil)
	r = callSub(t, ts2, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"s":{"identityId":%q,"emailId":%q,
		  "envelope":{"mailFrom":{"email":"john@example.com","parameters":{"HOLDFOR":"60"}},
		    "rcptTo":[{"email":"j@x.example"}]}}}}`,
		testAccount, identity2, email2), "0"))
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidProperties" {
		t.Errorf("hold with delayed send disabled: %v", e)
	}
}

// TestEmailSubmissionCancel: the undoStatus state machine (section 7.5)
// and the queue-exit side effects.
func TestEmailSubmissionCancel(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")

	create := func() string {
		emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
		r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
			`{"accountId":%q,"create":{"s":{"identityId":%q,"emailId":%q}}}`,
			testAccount, identityId, emailId), "0"))
		created, ok := methodArgs(t, r, 0, "EmailSubmission/set")["created"].(map[string]any)
		if !ok {
			t.Fatalf("create failed: %v", r.MethodResponses[0].Args)
		}
		return created["s"].(map[string]any)["id"].(string)
	}
	update := func(subId, patch string) map[string]any {
		r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
			`{"accountId":%q,"update":{%q:%s}}`, testAccount, subId, patch), "0"))
		return methodArgs(t, r, 0, "EmailSubmission/set")
	}

	// Cancel: the finalized deliveryStatus is echoed as a server side
	// effect, the record leaves the queue, and undoStatus is canceled.
	subId := create()
	args := update(subId, `{"undoStatus":"canceled"}`)
	side, ok := args["updated"].(map[string]any)[subId].(map[string]any)
	if !ok {
		t.Fatalf("cancel failed: %v", args)
	}
	ds := side["deliveryStatus"].(map[string]any)["jane@remote.example"].(map[string]any)
	if ds["delivered"] != "no" || !strings.Contains(ds["smtpReply"].(string), "canceled") {
		t.Errorf("canceled deliveryStatus = %v", ds)
	}
	stored, err := db.Get(context.Background(), testAccount, TypeEmailSubmission, jmap.Id(subId))
	if err != nil {
		t.Fatal(err)
	}
	if _, queued := stored["nextAttemptAt"]; queued {
		t.Error("canceled submission still queued")
	}
	// A redundant re-cancel is a no-op success.
	args = update(subId, `{"undoStatus":"canceled"}`)
	if v, has := args["updated"].(map[string]any)[subId]; !has || v != nil {
		t.Errorf("re-cancel: %v", args)
	}
	// canceled -> pending is not a legal client transition.
	args = update(subId, `{"undoStatus":"pending"}`)
	if e := args["notUpdated"].(map[string]any)[subId].(map[string]any); e["type"] != "invalidProperties" {
		t.Errorf("uncancel: %v", e)
	}

	// A final submission cannot be canceled; a claimed one is already
	// being transmitted. Both states are staged directly (the worker that
	// sets them is the next build step).
	finalId, claimedId := create(), create()
	_, err = db.Update(context.Background(), testAccount, func(u *objectdb.Update) error {
		obj, err := u.Get(TypeEmailSubmission, jmap.Id(finalId))
		if err != nil {
			return err
		}
		obj["undoStatus"] = json.RawMessage(`"final"`)
		if err := u.Put(TypeEmailSubmission, jmap.Id(finalId), obj); err != nil {
			return err
		}
		obj, err = u.Get(TypeEmailSubmission, jmap.Id(claimedId))
		if err != nil {
			return err
		}
		obj["claimedAt"] = mustJSON(time.Now().UTC().Format(time.RFC3339))
		return u.Put(TypeEmailSubmission, jmap.Id(claimedId), obj)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{finalId, claimedId} {
		args = update(id, `{"undoStatus":"canceled"}`)
		if e := args["notUpdated"].(map[string]any)[id].(map[string]any); e["type"] != "cannotUnsend" {
			t.Errorf("cancel %s: %v", id, e)
		}
	}

	// Everything else is immutable or server-set.
	args = update(subId, `{"envelope":{"mailFrom":{"email":"x@y.example"},"rcptTo":[]}}`)
	if e := args["notUpdated"].(map[string]any)[subId].(map[string]any); e["type"] != "invalidProperties" {
		t.Errorf("envelope update: %v", e)
	}
}

// TestEmailSubmissionOnSuccessDestroy: onSuccessDestroyEmail destroys the
// sent Email through the implicit set, including for a submission
// destroyed in the same call (its emailId comes from the destroyed
// record's last value); the submission record itself never needs the
// Email (section 7.5).
func TestEmailSubmissionOnSuccessDestroy(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")

	// Create + destroy-the-email in one call, via the creation reference.
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"s1":{"identityId":%q,"emailId":%q}},
		  "onSuccessDestroyEmail":["#s1"]}`, testAccount, identityId, emailId), "0"))
	created, ok := methodArgs(t, r, 0, "EmailSubmission/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", r.MethodResponses[0].Args)
	}
	subId := created["s1"].(map[string]any)["id"].(string)
	destroyed := methodArgs(t, r, 1, "Email/set")["destroyed"].([]any)
	if len(destroyed) != 1 || destroyed[0] != emailId {
		t.Fatalf("implicit destroy = %v", destroyed)
	}
	// The submission stands, still naming the destroyed Email (7.5:
	// destroying the Email MUST NOT affect the submission).
	r = callSub(t, ts, inv("EmailSubmission/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q]}`, testAccount, subId), "0"))
	sub := methodArgs(t, r, 0, "EmailSubmission/get")["list"].([]any)[0].(map[string]any)
	if sub["emailId"] != emailId {
		t.Errorf("submission emailId = %v", sub["emailId"])
	}

	// Destroy the submission and its Email in the same call: the emailId
	// is resolved from the destroyed record.
	emailId2 := putEmail(t, db, store, sendableMsg(map[string]string{"Message-ID": "<m2@example.com>"}), map[string]bool{drafts: true}, nil)
	r = callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"s2":{"identityId":%q,"emailId":%q}}}`,
		testAccount, identityId, emailId2), "0"))
	sub2 := methodArgs(t, r, 0, "EmailSubmission/set")["created"].(map[string]any)["s2"].(map[string]any)["id"].(string)
	r = callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"destroy":[%q],"onSuccessDestroyEmail":[%q]}`,
		testAccount, sub2, sub2), "0"))
	if d := methodArgs(t, r, 0, "EmailSubmission/set")["destroyed"].([]any); len(d) != 1 {
		t.Fatalf("submission destroy failed: %v", r.MethodResponses[0].Args)
	}
	if d := methodArgs(t, r, 1, "Email/set")["destroyed"].([]any); len(d) != 1 || d[0] != emailId2 {
		t.Fatalf("implicit destroy after submission destroy = %v", d)
	}

	// An entry whose item failed requests nothing: no implicit call.
	r = callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"bad":{"identityId":%q,"emailId":"Enope"}},
		  "onSuccessDestroyEmail":["#bad"]}`, testAccount, identityId), "0"))
	if len(r.MethodResponses) != 1 {
		t.Fatalf("implicit set ran for a failed create: %v", r.MethodResponses)
	}
}

// TestEmailSubmissionQuery: the section 7.3 filter conditions and sort
// properties, including the "sentAt" alias for sendAt.
func TestEmailSubmissionQuery(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")

	mkSub := func(msgid, holdUntil string) string {
		msg := sendableMsg(map[string]string{"Message-ID": msgid})
		emailId := putEmail(t, db, store, msg, map[string]bool{drafts: true}, nil)
		params := "null"
		if holdUntil != "" {
			params = fmt.Sprintf(`{"HOLDUNTIL":%q}`, holdUntil)
		}
		r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
			`{"accountId":%q,"create":{"s":{"identityId":%q,"emailId":%q,
			  "envelope":{"mailFrom":{"email":"john@example.com","parameters":%s},
			    "rcptTo":[{"email":"j@x.example"}]}}}}`,
			testAccount, identityId, emailId, params), "0"))
		created, ok := methodArgs(t, r, 0, "EmailSubmission/set")["created"].(map[string]any)
		if !ok {
			t.Fatalf("create failed: %v", r.MethodResponses[0].Args)
		}
		return created["s"].(map[string]any)["id"].(string)
	}
	farA := time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339)
	farB := time.Now().UTC().Add(90 * time.Minute).Format(time.RFC3339)
	subNow := mkSub("<q1@x>", "")
	subA := mkSub("<q2@x>", farA)
	subB := mkSub("<q3@x>", farB)
	// Cancel subNow so undoStatus distinguishes it.
	callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"undoStatus":"canceled"}}}`, testAccount, subNow), "0"))

	query := func(args string) []any {
		r := callSub(t, ts, inv("EmailSubmission/query", fmt.Sprintf(
			`{"accountId":%q,%s}`, testAccount, args), "0"))
		return methodArgs(t, r, 0, "EmailSubmission/query")["ids"].([]any)
	}
	if ids := query(`"filter":{"undoStatus":"pending"},"sort":[{"property":"sentAt"}]`); len(ids) != 2 || ids[0] != subA || ids[1] != subB {
		t.Errorf("pending by sentAt = %v, want [%s %s]", ids, subA, subB)
	}
	if ids := query(`"filter":{"undoStatus":"pending"},"sort":[{"property":"sendAt","isAscending":false}]`); len(ids) != 2 || ids[0] != subB {
		t.Errorf("pending by sendAt desc = %v", ids)
	}
	if ids := query(fmt.Sprintf(`"filter":{"before":%q}`, farB)); len(ids) != 2 {
		t.Errorf("before = %v", ids)
	}
	if ids := query(fmt.Sprintf(`"filter":{"after":%q}`, farB)); len(ids) != 1 || ids[0] != subB {
		t.Errorf("after = %v", ids)
	}
	sub, err := db.Get(context.Background(), testAccount, TypeEmailSubmission, jmap.Id(subA))
	if err != nil {
		t.Fatal(err)
	}
	var wantEmail string
	json.Unmarshal(sub["emailId"], &wantEmail)
	if ids := query(fmt.Sprintf(`"filter":{"emailIds":[%q],"identityIds":[%q]}`, wantEmail, identityId)); len(ids) != 1 || ids[0] != subA {
		t.Errorf("emailIds+identityIds = %v", ids)
	}

	// Unknown filter and sort properties are rejected; internal queue
	// properties are invisible.
	r := callSub(t, ts, inv("EmailSubmission/query", fmt.Sprintf(
		`{"accountId":%q,"filter":{"attempts":0}}`, testAccount), "0"))
	if args := methodArgs(t, r, 0, "error"); args["type"] != "unsupportedFilter" {
		t.Errorf("internal filter: %v", args)
	}
	r = callSub(t, ts, inv("EmailSubmission/query", fmt.Sprintf(
		`{"accountId":%q,"sort":[{"property":"nextAttemptAt"}]}`, testAccount), "0"))
	if args := methodArgs(t, r, 0, "error"); args["type"] != "unsupportedSort" {
		t.Errorf("internal sort: %v", args)
	}
}
