package mail

// End-to-end integration: the whole M2 mail slice driven as one system over
// the real HTTP JMAP server. Mail arrives through both delivery adapters
// (HTTP ingest and LMTP), threads together, advances the EmailDelivery push
// state on new mail alone (RFC 8621 section 1.5), and is then read back and
// mutated through the derived JMAP methods with the Mailbox counters and the
// section 2.1.1 trash-aware semantics all staying consistent. Where the
// per-file tests each exercise one surface, this proves they compose.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestMailIntegrationEndToEnd is the step-10 smoke: deliver two threaded
// messages by two different transports, observe the push state, query and
// read them over JMAP, then mark-read and move-to-trash and check every
// counter against the RFC 8621 section 2.1.1 worked example.
func TestMailIntegrationEndToEnd(t *testing.T) {
	ts, db, store := emailServer(t)
	ctx := context.Background()

	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	trash := createMailbox(t, ts, `{"name":"Trash","role":"trash"}`)

	deliveryState := func() string {
		s, err := db.TypeState(ctx, testAccount, TypeEmailDelivery)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// A never-written push type starts at "0" (RFC 8620 section 5.1).
	if got := deliveryState(); got != "0" {
		t.Fatalf("initial EmailDelivery state = %q, want 0", got)
	}

	// The same Deliverer feeds both adapters, so LMTP and HTTP ingest land
	// identical Emails. One address is local; it maps to the test account.
	deliverer := NewDeliverer(db, store, mapResolver{"john@example.com": testAccount})

	// --- transport 1: HTTP ingest delivers the root message ---
	root := bodyMsg("a@example.net", "john@example.com", "Project kickoff",
		"Let us begin the project on Monday.",
		map[string]string{"Message-ID": "<m1@example.net>"})
	rec := postIngest(t, NewHTTPIngest(deliverer), "a@example.net", []string{"john@example.com"}, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	res := decodeResults(t, rec)
	if len(res) != 1 || res[0].Outcome != "accepted" || res[0].EmailId == "" {
		t.Fatalf("ingest result = %+v, want one accepted with an EmailId", res)
	}
	m1 := string(res[0].EmailId)

	afterFirst := deliveryState()
	if afterFirst == "0" {
		t.Fatal("EmailDelivery state did not advance after the first delivery")
	}

	// --- transport 2: LMTP delivers a reply into the same thread ---
	reply := bodyMsg("john@example.com", "a@example.net", "Re: Project kickoff",
		"Monday works for me.",
		map[string]string{
			"Message-ID":  "<m2@example.net>",
			"In-Reply-To": "<m1@example.net>",
			"References":  "<m1@example.net>",
		})
	c, done := lmtpDial(t, deliverer, "mailserver.example")
	wantCode(t, c, "LHLO mailserver.example", 250)
	wantCode(t, c, "MAIL FROM:<john@example.com>", 250)
	wantCode(t, c, "RCPT TO:<john@example.com>", 250)
	wantCode(t, c, "DATA", 354)
	writeBody(t, c, reply)
	if code, _ := readReply(t, c); code != 250 {
		t.Fatalf("LMTP DATA reply = %d, want 250", code)
	}
	wantCode(t, c, "QUIT", 221)
	done()

	// New mail again: the push state advances a second time.
	afterSecond := deliveryState()
	if afterSecond == afterFirst {
		t.Fatalf("EmailDelivery state did not advance after the second delivery (still %q)", afterSecond)
	}

	// --- query: the two Emails are one collapsed Thread ---
	all := qIds(t, emailQuery(t, ts, fmt.Sprintf(
		`{"accountId":%q,"sort":[{"property":"receivedAt","isAscending":true}]}`, testAccount)))
	if len(all) != 2 {
		t.Fatalf("uncollapsed query returned %d ids, want 2: %v", len(all), all)
	}
	collapsed := emailQuery(t, ts, fmt.Sprintf(
		`{"accountId":%q,"collapseThreads":true,"calculateTotal":true}`, testAccount))
	if ids := qIds(t, collapsed); len(ids) != 1 {
		t.Fatalf("collapsed query returned %d ids, want 1: %v", len(ids), ids)
	}
	// The fast total for a collapsed query is the Thread count (section 4.4).
	if total, _ := collapsed["total"].(float64); total != 1 {
		t.Fatalf("collapsed total = %v, want 1", collapsed["total"])
	}

	// m2 is the id that is not m1; both share one Thread.
	var m2 string
	for _, id := range all {
		if id != m1 {
			m2 = id
		}
	}
	if m2 == "" {
		t.Fatalf("second Email id not found among %v (m1=%s)", all, m1)
	}
	if threadOf(t, ts, m1) != threadOf(t, ts, m2) {
		t.Fatal("the two delivered Emails are not in one Thread")
	}

	// --- counters: two unread Emails, one unread Thread, all in the inbox ---
	wantCounters(t, "inbox after delivery", readCounters(t, ts, inbox), 2, 2, 1, 1)

	// --- mark the root read: the unread Email count drops but the Thread is
	// still unread (the reply is unread), and the push state does NOT move ---
	callMail(t, ts, inv("Email/set",
		fmt.Sprintf(`{"accountId":%q,"update":{%q:{"keywords/$seen":true}}}`, testAccount, m1), "0"))
	wantCounters(t, "inbox after mark-read", readCounters(t, ts, inbox), 2, 1, 1, 1)
	if got := deliveryState(); got != afterSecond {
		t.Fatalf("EmailDelivery moved on mark-read: %q -> %q (section 1.5 SHOULD NOT)", afterSecond, got)
	}

	// --- move the unread reply to trash: this reproduces section 2.1.1 - a
	// read Email in the inbox, an unread one in the trash, one Thread. The
	// inbox loses its unread Thread; the trash gains one ---
	callMail(t, ts, inv("Email/set",
		fmt.Sprintf(`{"accountId":%q,"update":{%q:{"mailboxIds":{%q:true}}}}`, testAccount, m2, trash), "0"))
	wantCounters(t, "inbox after move", readCounters(t, ts, inbox), 1, 0, 1, 0)
	wantCounters(t, "trash after move", readCounters(t, ts, trash), 1, 1, 1, 1)
	// Moving mail between mailboxes is not new mail: the push state holds.
	if got := deliveryState(); got != afterSecond {
		t.Fatalf("EmailDelivery moved on a mailbox move: %q -> %q", afterSecond, got)
	}

	// --- search: a body term both messages share highlights in each ---
	snips := snippetByEmail(t, snippetGet(t, ts, `{"body":"Monday"}`, m1, m2))
	for _, id := range []string{m1, m2} {
		prev, _ := snips[id]["preview"].(string)
		if !strings.Contains(prev, "<mark>Monday</mark>") {
			t.Errorf("Email %s snippet = %q, want a <mark>Monday</mark>", id, prev)
		}
	}
}
