package deliver

// Report ingestion tests (RFC 8621 section 7, RFC 3464, RFC 8098): the
// ENVID stamp at relay, terminal and interim DSN flows into deliveryStatus
// and the computed dsnBlobIds, MDN receipts into displayed and mdnBlobIds,
// the per-recipient state machine swallowing replays, and the forgery
// paths - wrong recipient, guessed ENVID, non-null sender, and the
// Message-ID fallback's inability to finalize.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// msgWithID is a sendable message with a chosen Message-ID.
func msgWithID(mid string) string {
	return "From: john@example.com\r\n" +
		"To: jane@remote.example\r\n" +
		"Subject: Hello\r\n" +
		"Date: Thu, 17 Jul 2026 10:00:00 +0000\r\n" +
		"Message-ID: <" + mid + ">\r\n" +
		"\r\nbody\r\n"
}

// dsnFor builds a multipart/report DSN (RFC 3464) with the given
// per-message and per-recipient fields. envid == "" omits
// Original-Envelope-Id; returnedID != "" appends the returned-content
// third part carrying that Message-ID.
func dsnFor(envid, rcpt, action, status, diag, returnedID string) string {
	var b strings.Builder
	b.WriteString("From: MAILER-DAEMON@remote.example\r\n" +
		"To: john@example.com\r\n" +
		"Subject: Delivery Status Notification\r\n" +
		"Date: Thu, 17 Jul 2026 11:00:00 +0000\r\n" +
		"Message-ID: <" + action + "-" + status + "@remote.example>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/report; report-type=delivery-status; boundary=\"rpt\"\r\n" +
		"\r\n--rpt\r\nContent-Type: text/plain\r\n\r\nDelivery report.\r\n" +
		"--rpt\r\nContent-Type: message/delivery-status\r\n\r\n" +
		"Reporting-MTA: dns; remote.example\r\n")
	if envid != "" {
		b.WriteString("Original-Envelope-Id: " + envid + "\r\n")
	}
	b.WriteString("\r\nFinal-Recipient: rfc822; " + rcpt + "\r\n" +
		"Action: " + action + "\r\n" +
		"Status: " + status + "\r\n")
	if diag != "" {
		b.WriteString("Diagnostic-Code: smtp; " + diag + "\r\n")
	}
	b.WriteString("\r\n")
	if returnedID != "" {
		b.WriteString("--rpt\r\nContent-Type: message/rfc822\r\n\r\n" + msgWithID(returnedID))
	}
	b.WriteString("--rpt--\r\n")
	return b.String()
}

// mdnFor builds a multipart/report MDN (RFC 8098) for the given original
// Message-ID, recipient, and disposition-type.
func mdnFor(origID, rcpt, disposition string) string {
	return "From: jane@remote.example\r\n" +
		"To: john@example.com\r\n" +
		"Subject: Disposition Notification\r\n" +
		"Date: Thu, 17 Jul 2026 11:00:00 +0000\r\n" +
		"Message-ID: <mdn-" + disposition + "@remote.example>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/report; report-type=disposition-notification; boundary=\"rpt\"\r\n" +
		"\r\n--rpt\r\nContent-Type: text/plain\r\n\r\nThe message was " + disposition + ".\r\n" +
		"--rpt\r\nContent-Type: message/disposition-notification\r\n\r\n" +
		"Reporting-UA: mail.remote.example\r\n" +
		"Final-Recipient: rfc822; " + rcpt + "\r\n" +
		"Original-Message-ID: <" + origID + ">\r\n" +
		"Disposition: automatic-action/MDN-sent-automatically; " + disposition + "\r\n" +
		"\r\n--rpt--\r\n"
}

// reportHarness is one wired sending + ingesting server: a relayed
// submission whose reports can be delivered back in.
type reportHarness struct {
	ts    *httptest.Server
	db    *objectdb.DB
	store blob.Store
	d     *Deliverer
	subId string
}

// newReportHarness relays one submission of msgWithID(mid) and returns the
// harness plus the delivering side, with report ingestion enabled and any
// extra options applied.
func newReportHarness(t *testing.T, mid string, opts ...Option) *reportHarness {
	t.Helper()
	ts, db, store, _, w, fake := newEmailServer(t, mail.DefaultAccountCapability())
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, msgWithID(mid), map[string]bool{drafts: true}, nil)
	subId := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	if sent, _, err := w.ProcessDue(context.Background(), 0); err != nil || sent != 1 {
		t.Fatalf("relay: sent=%d err=%v", sent, err)
	}
	// The relay stamped the submission id as the envelope ENVID (RFC 8621
	// section 7 blesses the id as ENVID, overriding client values).
	if got := fake.call(0).env.MailParameters["ENVID"]; got == nil || *got != subId {
		t.Fatalf("ENVID stamp = %v, want %q", got, subId)
	}
	d, err := New(db, store, mapResolver{"john@example.com": testAccount},
		append([]Option{WithReportIngestion()}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return &reportHarness{ts: ts, db: db, store: store, d: d, subId: subId}
}

// deliverReport delivers raw as a report (null sender) to john and returns
// the one event.
func (h *reportHarness) deliverReport(t *testing.T, raw string) Event {
	t.Helper()
	evs := h.d.Deliver(context.Background(), deliveryEnv("", "john@example.com"), strings.NewReader(raw))
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	return evs[0]
}

// getSubmission fetches the submission over JMAP with default properties.
func (h *reportHarness) getSubmission(t *testing.T) map[string]any {
	t.Helper()
	r := callMail(t, h.ts, inv("EmailSubmission/get", fmt.Sprintf(
		`{"accountId":%q,"ids":[%q]}`, testAccount, h.subId), "0"))
	list, ok := methodArgs(t, r, 0, "EmailSubmission/get")["list"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("EmailSubmission/get: %v", r.MethodResponses[0].Args)
	}
	return list[0].(map[string]any)
}

// deliveryStatusObj is one recipient's DeliveryStatus (RFC 8621 section
// 7). Duplicated from submit's own unexported type (same trivial shape;
// submit's test file is unexported and this package cannot import it).
type deliveryStatusObj struct {
	SmtpReply string `json:"smtpReply"`
	Delivered string `json:"delivered"`
	Displayed string `json:"displayed"`
}

func subRecord(t *testing.T, db *objectdb.DB, id string) objectdb.Object {
	t.Helper()
	rec, err := db.Get(context.Background(), testAccount, record.TypeEmailSubmission, jmap.Id(id))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func recDeliveryStatus(t *testing.T, rec objectdb.Object) map[string]deliveryStatusObj {
	t.Helper()
	var ds map[string]deliveryStatusObj
	if err := json.Unmarshal(rec["deliveryStatus"], &ds); err != nil {
		t.Fatal(err)
	}
	return ds
}

func (h *reportHarness) status(t *testing.T) deliveryStatusObj {
	t.Helper()
	rec := subRecord(t, h.db, h.subId)
	return recDeliveryStatus(t, rec)["jane@remote.example"]
}

func blobList(t *testing.T, obj map[string]any, prop string) []any {
	t.Helper()
	l, ok := obj[prop].([]any)
	if !ok {
		t.Fatalf("%s = %v, want a list", prop, obj[prop])
	}
	return l
}

// TestReportIngestDSNTerminal: a failed DSN carrying the stamped ENVID
// finalizes deliveryStatus (delivered no, smtpReply from the smtp
// Diagnostic-Code), lands in the inbox, and surfaces through the computed
// dsnBlobIds; replaying it - even with varied content - is swallowed (the
// terminal slot is sealed, RFC 3464 section 3 + RFC 8621's final
// deliveryStatus).
func TestReportIngestDSNTerminal(t *testing.T) {
	h := newReportHarness(t, "term1@example.com")

	ev := h.deliverReport(t, dsnFor(h.subId, "jane@remote.example", "failed", "5.1.1", "550 5.1.1 user unknown", ""))
	if ev.Outcome != mail.Accepted || ev.EmailId == "" {
		t.Fatalf("advancing DSN not delivered to inbox: %+v", ev)
	}
	st := h.status(t)
	if st.Delivered != "no" || st.SmtpReply != "550 5.1.1 user unknown" {
		t.Fatalf("status after failed DSN = %+v", st)
	}
	sub := h.getSubmission(t)
	if l := blobList(t, sub, "dsnBlobIds"); len(l) != 1 || l[0] != string(ev.BlobId) {
		t.Fatalf("dsnBlobIds = %v, want the report blob", l)
	}
	if l := blobList(t, sub, "mdnBlobIds"); len(l) != 0 {
		t.Fatalf("mdnBlobIds = %v, want empty", l)
	}

	// Replay with varied content (the spammer-adds-a-dot flood): matched,
	// advances nothing, swallowed - accepted on the wire, no inbox Email,
	// no new blob in the list, status untouched.
	ev = h.deliverReport(t, dsnFor(h.subId, "jane@remote.example", "failed", "5.1.1", "550 5.1.1 user unknown NOW REALLY", ""))
	if ev.Outcome != mail.Accepted || ev.EmailId != "" {
		t.Fatalf("replayed DSN not swallowed: %+v", ev)
	}
	if st := h.status(t); st.SmtpReply != "550 5.1.1 user unknown" {
		t.Fatalf("replay changed status: %+v", st)
	}
	if l := blobList(t, h.getSubmission(t), "dsnBlobIds"); len(l) != 1 {
		t.Fatalf("replay grew dsnBlobIds: %v", l)
	}
}

// TestReportIngestDSNInterimThenTerminal: a delayed DSN consumes the one
// interim slot (delivered stays unknown, smtpReply may improve), a second
// delayed is swallowed, and the terminal failed still lands afterwards.
func TestReportIngestDSNInterim(t *testing.T) {
	h := newReportHarness(t, "int1@example.com")

	ev := h.deliverReport(t, dsnFor(h.subId, "jane@remote.example", "delayed", "4.4.1", "451 4.4.1 host unreachable", ""))
	if ev.EmailId == "" {
		t.Fatalf("first delayed DSN not delivered: %+v", ev)
	}
	st := h.status(t)
	if st.Delivered != "unknown" || st.SmtpReply != "451 4.4.1 host unreachable" {
		t.Fatalf("status after delayed DSN = %+v", st)
	}
	if ev = h.deliverReport(t, dsnFor(h.subId, "jane@remote.example", "delayed", "4.4.1", "451 4.4.1 still trying", "")); ev.EmailId != "" {
		t.Fatalf("second delayed DSN not swallowed: %+v", ev)
	}
	if ev = h.deliverReport(t, dsnFor(h.subId, "jane@remote.example", "failed", "5.4.7", "554 5.4.7 giving up", "")); ev.EmailId == "" {
		t.Fatalf("terminal DSN after interim not delivered: %+v", ev)
	}
	if st = h.status(t); st.Delivered != "no" {
		t.Fatalf("status after terminal = %+v", st)
	}
	if l := blobList(t, h.getSubmission(t), "dsnBlobIds"); len(l) != 2 {
		t.Fatalf("dsnBlobIds = %v, want interim + terminal", l)
	}
}

// TestReportIngestDSNForgery: reports that must not correlate or advance -
// a matched ENVID with a recipient the submission never addressed
// (swallowed), a guessed ENVID that matches nothing (ordinary mail), and a
// report from a non-null sender (never even considered; RFC 3464 section 2
// requires the null reverse-path).
func TestReportIngestDSNForgery(t *testing.T) {
	h := newReportHarness(t, "forge1@example.com")

	ev := h.deliverReport(t, dsnFor(h.subId, "victim@elsewhere.example", "failed", "5.1.1", "550 5.1.1 nope", ""))
	if ev.Outcome != mail.Accepted || ev.EmailId != "" {
		t.Fatalf("wrong-recipient DSN not swallowed: %+v", ev)
	}

	ev = h.deliverReport(t, dsnFor("Snonexistent0000", "jane@remote.example", "failed", "5.1.1", "550 5.1.1 nope", ""))
	if ev.EmailId == "" {
		t.Fatalf("unmatched DSN should be ordinary mail: %+v", ev)
	}

	evs := h.d.Deliver(context.Background(),
		deliveryEnv("attacker@evil.example", "john@example.com"),
		strings.NewReader(dsnFor(h.subId, "jane@remote.example", "delivered", "2.0.0", "250 2.0.0 delivered", "")))
	if evs[0].EmailId == "" {
		t.Fatalf("non-null-sender report should be ordinary mail: %+v", evs[0])
	}

	// Nothing above touched the submission.
	if st := h.status(t); st.Delivered != "unknown" {
		t.Fatalf("forgeries changed status: %+v", st)
	}
	if l := blobList(t, h.getSubmission(t), "dsnBlobIds"); len(l) != 0 {
		t.Fatalf("forgeries pinned reports: %v", l)
	}
}

// TestReportIngestMessageIDFallback: without WithMessageIDCorrelation a
// DSN with no ENVID is ordinary mail; with it, the returned content's
// Message-ID pins the report and consumes the slot, but can never flip
// delivered - the weak key must not finalize (RFC 3464 section 4.1).
func TestReportIngestMessageIDFallback(t *testing.T) {
	off := newReportHarness(t, "fb1@example.com")
	ev := off.deliverReport(t, dsnFor("", "jane@remote.example", "failed", "5.1.1", "550 5.1.1 unknown", "fb1@example.com"))
	if ev.EmailId == "" {
		t.Fatalf("fallback off: DSN should be ordinary mail: %+v", ev)
	}
	if l := blobList(t, off.getSubmission(t), "dsnBlobIds"); len(l) != 0 {
		t.Fatalf("fallback off pinned a report: %v", l)
	}

	on := newReportHarness(t, "fb2@example.com", WithMessageIDCorrelation())
	ev = on.deliverReport(t, dsnFor("", "jane@remote.example", "failed", "5.1.1", "550 5.1.1 unknown", "fb2@example.com"))
	if ev.EmailId == "" {
		t.Fatalf("fallback-matched DSN should still reach the inbox: %+v", ev)
	}
	if st := on.status(t); st.Delivered != "unknown" {
		t.Fatalf("weak key finalized delivered: %+v", st)
	}
	if l := blobList(t, on.getSubmission(t), "dsnBlobIds"); len(l) != 1 {
		t.Fatalf("fallback match did not pin: %v", l)
	}
	// The slot is consumed: a second terminal report via the weak key is
	// swallowed like any other replay.
	if ev = on.deliverReport(t, dsnFor("", "jane@remote.example", "failed", "5.1.1", "550 5.1.1 again", "fb2@example.com")); ev.EmailId != "" {
		t.Fatalf("second fallback DSN not swallowed: %+v", ev)
	}
}

// TestReportIngestMDN: a displayed MDN matched by Original-Message-ID (the
// only key RFC 8098 defines) sets displayed yes and fills mdnBlobIds; the
// per-recipient receipt slot admits exactly one MDN ever (RFC 8098 section
// 2.1), and an MDN for an unknown Message-ID is ordinary mail.
func TestReportIngestMDN(t *testing.T) {
	h := newReportHarness(t, "mdn1@example.com")

	ev := h.deliverReport(t, mdnFor("mdn1@example.com", "jane@remote.example", "displayed"))
	if ev.Outcome != mail.Accepted || ev.EmailId == "" {
		t.Fatalf("MDN not delivered: %+v", ev)
	}
	st := h.status(t)
	if st.Displayed != "yes" || st.Delivered != "unknown" {
		t.Fatalf("status after MDN = %+v", st)
	}
	sub := h.getSubmission(t)
	if l := blobList(t, sub, "mdnBlobIds"); len(l) != 1 || l[0] != string(ev.BlobId) {
		t.Fatalf("mdnBlobIds = %v", l)
	}
	if l := blobList(t, sub, "dsnBlobIds"); len(l) != 0 {
		t.Fatalf("dsnBlobIds = %v, want empty", l)
	}

	if ev = h.deliverReport(t, mdnFor("mdn1@example.com", "jane@remote.example", "deleted")); ev.EmailId != "" {
		t.Fatalf("second MDN not swallowed: %+v", ev)
	}
	if ev = h.deliverReport(t, mdnFor("who-knows@elsewhere.example", "jane@remote.example", "displayed")); ev.EmailId == "" {
		t.Fatalf("unmatched MDN should be ordinary mail: %+v", ev)
	}
}

// TestReportIngestDestroyReleasesReports: destroying the submission takes
// its report records with it in the same commit, so nothing references the
// report blobs any more.
func TestReportIngestDestroyReleasesReports(t *testing.T) {
	h := newReportHarness(t, "destroy1@example.com")
	h.deliverReport(t, dsnFor(h.subId, "jane@remote.example", "failed", "5.1.1", "550 5.1.1 unknown", ""))
	ctx := context.Background()
	ids, err := h.db.IdsWhereEqual(ctx, testAccount, record.TypeSubmissionReport, "submissionId", record.MustJSON(jmap.Id(h.subId)), 0)
	if err != nil || len(ids) != 1 {
		t.Fatalf("report rows before destroy: %v %v", ids, err)
	}

	r := callMail(t, h.ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"destroy":[%q]}`, testAccount, h.subId), "0"))
	if destroyed, _ := methodArgs(t, r, 0, "EmailSubmission/set")["destroyed"].([]any); len(destroyed) != 1 {
		t.Fatalf("destroy failed: %v", r.MethodResponses[0].Args)
	}
	ids, err = h.db.IdsWhereEqual(ctx, testAccount, record.TypeSubmissionReport, "submissionId", record.MustJSON(jmap.Id(h.subId)), 0)
	if err != nil || len(ids) != 0 {
		t.Fatalf("report rows survived destroy: %v %v", ids, err)
	}
}
