package deliver

// The RFC 3834 auto-reply tests: who gets one (through the real submission
// queue, with the section 3 reply form) and, mostly, who never does (the
// section 2 refusal rules). The VacationResponse singleton get/set surface
// itself is root's vacation_test.go - this needs a real submission queue
// and Deliverer, both of which live below root.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
	"github.com/naust-mail/naust-jmap/datatypes/mail/submit"
)

func vacGet(t *testing.T, ts *httptest.Server, extra string) map[string]any {
	t.Helper()
	r := callMail(t, ts, inv("VacationResponse/get", fmt.Sprintf(`{"accountId":%q%s}`, testAccount, extra), "0"))
	res := methodArgs(t, r, 0, "VacationResponse/get")
	list, ok := res["list"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("VacationResponse/get list: %v", res)
	}
	return list[0].(map[string]any)
}

func vacSet(t testing.TB, ts *httptest.Server, args string) map[string]any {
	t.Helper()
	r := callMail(t, ts, inv("VacationResponse/set", fmt.Sprintf(`{"accountId":%q,%s}`, testAccount, args), "0"))
	return methodArgs(t, r, 0, "VacationResponse/set")
}

// enableVacation turns the responder on with the given extra properties.
func enableVacation(t testing.TB, ts *httptest.Server, extraProps string) {
	t.Helper()
	res := vacSet(t, ts, `"update":{"singleton":{"isEnabled":true`+extraProps+`}}`)
	if _, ok := res["updated"].(map[string]any)["singleton"]; !ok {
		t.Fatalf("enable failed: %v", res)
	}
}

// newVacationDeliverer builds one responder-enabled Deliverer for a test:
// New registers the suppression ledger type, which errors on a second
// registration, so callers build one and reuse it across deliveries rather
// than one per call - which also matches a real server (one long-lived
// Deliverer, not one per message).
func newVacationDeliverer(t *testing.T, db *objectdb.DB, store blob.Store, q *submit.Queue) *Deliverer {
	t.Helper()
	return mustDeliverer(t, db, store, mapResolver{
		"john@example.com": testAccount,
		"jane@example.com": testAccount,
	}, Config{MaxMessageSize: defaultMaxMessageSize, VacationQueue: q})
}

// vacationDeliver delivers raw for sender -> john@example.com through d.
func vacationDeliver(t *testing.T, d *Deliverer, sender, raw string) {
	t.Helper()
	evs := d.Deliver(context.Background(), deliveryEnv(sender, "john@example.com"), strings.NewReader(raw))
	if evs[0].Outcome != mail.Accepted {
		t.Fatalf("delivery not accepted: %+v", evs[0])
	}
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
	ts, db, store, q, w, fake := newEmailServer(t, mail.DefaultAccountCapability())
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	sent := createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
	createIdentity(t, ts, "john@example.com")
	enableVacation(t, ts, `,"textBody":"I am away until August."`)
	d := newVacationDeliverer(t, db, store, q)

	vacationDeliver(t, d, "visitor@remote.example", inboundMsg(""))
	if n := submissionCount(t, db); n != 1 {
		t.Fatalf("submissions after delivery = %d, want 1", n)
	}
	// The reply Email sits in the sent mailbox, seen.
	sentIds, err := db.IdsWhereMember(context.Background(), testAccount, mail.TypeEmail, "mailboxIds", sent)
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
	vacationDeliver(t, d, "visitor@remote.example", inboundMsg(""))
	if n := submissionCount(t, db); n != 1 {
		t.Fatalf("suppression failed: %d submissions", n)
	}
	// A different sender gets their own reply.
	other := strings.ReplaceAll(inboundMsg(""), "visitor@remote.example", "other@remote.example")
	vacationDeliver(t, d, "other@remote.example", other)
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
			ts, db, store, q, _, _ := newEmailServer(t, mail.DefaultAccountCapability())
			createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
			createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
			createIdentity(t, ts, "john@example.com")
			enableVacation(t, ts, tc.setup)
			d := newVacationDeliverer(t, db, store, q)
			vacationDeliver(t, d, tc.sender, tc.raw)
			if n := submissionCount(t, db); n != 0 {
				t.Fatalf("%s: %d submissions queued, want 0", tc.name, n)
			}
		})
	}

	t.Run("disabled", func(t *testing.T) {
		ts, db, store, q, _, _ := newEmailServer(t, mail.DefaultAccountCapability())
		createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
		createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
		createIdentity(t, ts, "john@example.com")
		d := newVacationDeliverer(t, db, store, q)
		vacationDeliver(t, d, "visitor@remote.example", inboundMsg(""))
		if n := submissionCount(t, db); n != 0 {
			t.Fatalf("disabled vacation replied: %d", n)
		}
	})
	t.Run("no identity for the address", func(t *testing.T) {
		ts, db, store, q, _, _ := newEmailServer(t, mail.DefaultAccountCapability())
		createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
		createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
		enableVacation(t, ts, "")
		d := newVacationDeliverer(t, db, store, q)
		vacationDeliver(t, d, "visitor@remote.example", inboundMsg(""))
		if n := submissionCount(t, db); n != 0 {
			t.Fatalf("identityless account replied: %d", n)
		}
	})
	t.Run("no sent mailbox", func(t *testing.T) {
		ts, db, store, q, _, _ := newEmailServer(t, mail.DefaultAccountCapability())
		createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
		createIdentity(t, ts, "john@example.com")
		enableVacation(t, ts, "")
		d := newVacationDeliverer(t, db, store, q)
		vacationDeliver(t, d, "visitor@remote.example", inboundMsg(""))
		if n := submissionCount(t, db); n != 0 {
			t.Fatalf("sentless account replied: %d", n)
		}
	})
}

// TestVacationReplyHeaderHardening: a hostile or non-ASCII original
// subject cannot break the reply's header block - controls are refused by
// the ASCII gate and everything non-ASCII rides in RFC 2047 encoded-words.
func TestVacationReplyHeaderHardening(t *testing.T) {
	view := &mail.VacationView{}
	raw, _ := buildVacationReply("john@example.com", "visitor@remote.example",
		mustParse(t, "From: v@remote.example\r\nTo: john@example.com\r\n"+
			"Subject: =?utf-8?B?xJxlbmVyaWM=?= r\u00e9sum\u00e9\r\nMessage-ID: <x@remote.example>\r\n\r\nhi\r\n"),
		view, time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC))
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

func mustParse(t *testing.T, raw string) *parse.Parsed {
	t.Helper()
	p, err := parse.ParseMessage(strings.NewReader(raw), parse.NewCapture())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestVacationReplyDeferred: deliverDeferred settles and reports every
// verdict with no reply created until the returned respond closure runs -
// the adapter answers its peer before any responder work - and a
// cancelled context loses the courtesy without failing anything.
func TestVacationReplyDeferred(t *testing.T) {
	ts, db, store, q, _, _ := newEmailServer(t, mail.DefaultAccountCapability())
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
	createIdentity(t, ts, "john@example.com")
	enableVacation(t, ts, `,"textBody":"I am away until August."`)
	d := newVacationDeliverer(t, db, store, q)

	events, respond := d.deliverDeferred(context.Background(),
		deliveryEnv("visitor@remote.example", "john@example.com"), strings.NewReader(inboundMsg("")))
	if events[0].Outcome != mail.Accepted {
		t.Fatalf("delivery not accepted: %+v", events[0])
	}
	if respond == nil {
		t.Fatal("no respond closure for an accepted delivery with vacation on")
	}
	if n := submissionCount(t, db); n != 0 {
		t.Fatalf("submissions before respond = %d, want 0", n)
	}
	respond(context.Background())
	if n := submissionCount(t, db); n != 1 {
		t.Fatalf("submissions after respond = %d, want 1", n)
	}

	// A cancelled context: the delivery already succeeded, the courtesy is
	// simply lost.
	other := strings.ReplaceAll(inboundMsg(""), "visitor@remote.example", "other@remote.example")
	events, respond = d.deliverDeferred(context.Background(),
		deliveryEnv("other@remote.example", "john@example.com"), strings.NewReader(other))
	if events[0].Outcome != mail.Accepted || respond == nil {
		t.Fatalf("second delivery: %+v, respond nil=%v", events[0], respond == nil)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	respond(cancelled)
	if n := submissionCount(t, db); n != 1 {
		t.Fatalf("submissions after cancelled respond = %d, want 1", n)
	}
}
