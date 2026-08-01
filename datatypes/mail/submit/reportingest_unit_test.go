package submit

// Direct unit tests of IngestReport and its helpers (RFC 3464, RFC 8098,
// RFC 8621 section 7), complementing the package's own end-to-end coverage
// with white-box assertions on the (matched, deliver, err) contract and the
// resulting deliveryStatus / SubmissionReport record mutations. The
// end-to-end flows covering the same correlation rules live in
// deliver/reportingest_test.go; these tests must agree with what that file
// already proves about the semantics.

import (
	"context"
	"fmt"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
	"github.com/naust-mail/naust-jmap/datatypes/mail/report"
)

// ingestFor runs IngestReport inside one account Update against the
// package's real db.Update path, returning its result.
func ingestFor(t *testing.T, db *objectdb.DB, rep *report.Inbound, blobID jmap.Id, opts IngestOptions) (matched, deliver bool) {
	t.Helper()
	_, err := db.Update(context.Background(), testAccount, func(u *objectdb.Update) error {
		var ierr error
		matched, deliver, ierr = IngestReport(u, rep, blobID, testReceivedAt, opts)
		return ierr
	})
	if err != nil {
		t.Fatal(err)
	}
	return matched, deliver
}

// reportRowCount counts the SubmissionReport records referring to subId.
func reportRowCount(t *testing.T, db *objectdb.DB, subId string) int {
	t.Helper()
	ids, err := db.IdsWhereEqual(context.Background(), testAccount, record.TypeSubmissionReport, "submissionId", record.MustJSON(jmap.Id(subId)), 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(ids)
}

// TestIngestReportDSNCorrelatesByEnvid: a DSN carrying the submission id as
// Original-Envelope-Id matches, and each recipient's deliveryStatus is
// updated per its own Action - delivered and failed finalize Delivered and
// take the terminal slot, delayed leaves Delivered alone (only the interim
// slot, and smtpReply when the correlation is exact) - RFC 8621 section 7.
func TestIngestReportDSNCorrelatesByEnvid(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	subId := submitEnvelope(t, ts, identityId, emailId, `{"mailFrom":{"email":"john@example.com"},"rcptTo":[`+
		`{"email":"a@remote.example"},{"email":"b@remote.example"},{"email":"c@remote.example"}]}`)

	matched, deliver := ingestFor(t, db, &report.Inbound{
		Kind:  report.KindDSN,
		Envid: subId,
		Rcpts: []report.Recipient{
			{Addr: "a@remote.example", Action: "delivered", Status: "2.0.0", SMTPDiag: "250 2.0.0 ok"},
			{Addr: "b@remote.example", Action: "failed", Status: "5.1.1", SMTPDiag: "550 5.1.1 unknown user"},
			{Addr: "c@remote.example", Action: "delayed", Status: "4.4.1", SMTPDiag: "451 4.4.1 host unreachable"},
		},
	}, jmap.Id("Bdsn1"), IngestOptions{})
	if !matched || !deliver {
		t.Fatalf("matched=%v deliver=%v, want both true", matched, deliver)
	}

	ds := recDeliveryStatus(t, subRecord(t, db, subId))
	if st := ds["a@remote.example"]; st.Delivered != "yes" || st.SmtpReply != "250 2.0.0 ok" {
		t.Errorf("delivered recipient status = %+v", st)
	}
	if st := ds["b@remote.example"]; st.Delivered != "no" || st.SmtpReply != "550 5.1.1 unknown user" {
		t.Errorf("failed recipient status = %+v", st)
	}
	if st := ds["c@remote.example"]; st.Delivered != "queued" || st.SmtpReply != "451 4.4.1 host unreachable" {
		t.Errorf("delayed recipient status = %+v, want Delivered unchanged with the improved reply", st)
	}
	if n := reportRowCount(t, db, subId); n != 3 {
		t.Errorf("report rows = %d, want 3 (two terminal, one interim)", n)
	}
}

// TestIngestReportMDNCorrelatesByOrigMessageID: an MDN correlates by
// Original-Message-ID (RFC 8098 section 3.2.5, the only key it defines), a
// displayed disposition records Displayed=yes, and delivered stays whatever
// it already was - an MDN never speaks to delivery outcome.
func TestIngestReportMDNCorrelatesByOrigMessageID(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	subId := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)

	matched, deliver := ingestFor(t, db, &report.Inbound{
		Kind:           report.KindMDN,
		OrigMessageID:  "m1@example.com",
		FinalRecipient: "jane@remote.example",
		Disposition:    "displayed",
	}, jmap.Id("Bmdn1"), IngestOptions{})
	if !matched || !deliver {
		t.Fatalf("matched=%v deliver=%v, want both true", matched, deliver)
	}

	st := recDeliveryStatus(t, subRecord(t, db, subId))["jane@remote.example"]
	if st.Displayed != "yes" {
		t.Errorf("displayed = %q, want yes", st.Displayed)
	}
	if st.Delivered != "queued" {
		t.Errorf("delivered = %q, an MDN must not change it", st.Delivered)
	}
	if n := reportRowCount(t, db, subId); n != 1 {
		t.Errorf("report rows = %d, want 1 (the receipt slot)", n)
	}
}

// TestIngestReportNoMatch: a DSN with an unrecognized ENVID and no fallback
// enabled correlates with nothing - matched and deliver both false, no
// records touched.
func TestIngestReportNoMatch(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	subId := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)

	before := recDeliveryStatus(t, subRecord(t, db, subId))["jane@remote.example"]
	matched, deliver := ingestFor(t, db, &report.Inbound{
		Kind:  report.KindDSN,
		Envid: "Snonexistent0000000000000",
		Rcpts: []report.Recipient{{Addr: "jane@remote.example", Action: "failed", Status: "5.1.1"}},
	}, jmap.Id("Bnomatch1"), IngestOptions{})
	if matched || deliver {
		t.Fatalf("matched=%v deliver=%v, want both false for an unrecognized ENVID", matched, deliver)
	}
	after := recDeliveryStatus(t, subRecord(t, db, subId))["jane@remote.example"]
	if after != before {
		t.Errorf("unmatched report changed status: before=%+v after=%+v", before, after)
	}
	if n := reportRowCount(t, db, subId); n != 0 {
		t.Errorf("report rows = %d, want 0", n)
	}
}

// TestIngestReportMessageIDFallback: without MessageIDFallback a DSN
// lacking Original-Envelope-Id never correlates by the returned content's
// Message-ID; with it enabled, the weak key pins the report and consumes
// the terminal slot but must never finalize Delivered (RFC 3464 section
// 4.1: only the ENVID this server stamped may do that).
func TestIngestReportMessageIDFallback(t *testing.T) {
	newSub := func(t *testing.T) (*objectdb.DB, string) {
		t.Helper()
		ts, db, store, _ := submissionServer(t)
		drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
		identityId := createIdentity(t, ts, "john@example.com")
		emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
		subId := submitEnvelope(t, ts, identityId, emailId,
			`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
		return db, subId
	}
	dsn := &report.Inbound{
		Kind:              report.KindDSN,
		ReturnedMessageID: "m1@example.com",
		Rcpts:             []report.Recipient{{Addr: "jane@remote.example", Action: "failed", Status: "5.1.1", SMTPDiag: "550 5.1.1 unknown"}},
	}

	db, subId := newSub(t)
	matched, deliver := ingestFor(t, db, dsn, jmap.Id("Bfb-off"), IngestOptions{MessageIDFallback: false})
	if matched || deliver {
		t.Fatalf("fallback off: matched=%v deliver=%v, want both false", matched, deliver)
	}
	if n := reportRowCount(t, db, subId); n != 0 {
		t.Errorf("fallback off pinned a report: %d rows", n)
	}

	db, subId = newSub(t)
	matched, deliver = ingestFor(t, db, dsn, jmap.Id("Bfb-on"), IngestOptions{MessageIDFallback: true})
	if !matched || !deliver {
		t.Fatalf("fallback on: matched=%v deliver=%v, want both true", matched, deliver)
	}
	st := recDeliveryStatus(t, subRecord(t, db, subId))["jane@remote.example"]
	if st.Delivered != "queued" {
		t.Errorf("weak key finalized delivered: %+v", st)
	}
	if n := reportRowCount(t, db, subId); n != 1 {
		t.Errorf("fallback on did not pin the report: %d rows", n)
	}
}

// TestMatchEnvelopeRecipient: the deliveryStatus key lookup for a reported
// address - present verbatim, present only case-differently, and absent
// (an address this submission never addressed).
func TestMatchEnvelopeRecipient(t *testing.T) {
	ds := map[string]deliveryStatusObj{
		"jane@remote.example": {Delivered: "queued"},
	}
	if got := matchEnvelopeRecipient(ds, "jane@remote.example"); got != "jane@remote.example" {
		t.Errorf("exact match = %q", got)
	}
	if got := matchEnvelopeRecipient(ds, "Jane@Remote.Example"); got != "jane@remote.example" {
		t.Errorf("case-insensitive match = %q, want the stored key", got)
	}
	if got := matchEnvelopeRecipient(ds, "victim@elsewhere.example"); got != "" {
		t.Errorf("unaddressed recipient matched: %q", got)
	}
	if got := matchEnvelopeRecipient(ds, ""); got != "" {
		t.Errorf("empty address matched: %q", got)
	}
}

// TestSubmissionDestroyReleasesReportRows: destroying a submission that
// has accumulated SubmissionReport rows destroys them in the same commit
// (submissionDestroy's loop body, otherwise unreached by this package's
// own tests since none of them ingest a report first).
func TestSubmissionDestroyReleasesReportRows(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	subId := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)

	matched, _ := ingestFor(t, db, &report.Inbound{
		Kind:  report.KindDSN,
		Envid: subId,
		Rcpts: []report.Recipient{{Addr: "jane@remote.example", Action: "failed", Status: "5.1.1"}},
	}, jmap.Id("Bdestroy1"), IngestOptions{})
	if !matched {
		t.Fatal("setup: DSN did not correlate")
	}
	if n := reportRowCount(t, db, subId); n != 1 {
		t.Fatalf("setup: report rows = %d, want 1", n)
	}

	r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"destroy":[%q]}`, testAccount, subId), "0"))
	if destroyed, _ := methodArgs(t, r, 0, "EmailSubmission/set")["destroyed"].([]any); len(destroyed) != 1 {
		t.Fatalf("destroy failed: %v", r.MethodResponses[0].Args)
	}
	if n := reportRowCount(t, db, subId); n != 0 {
		t.Errorf("report rows survived destroy: %d", n)
	}
}
