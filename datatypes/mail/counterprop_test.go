package mail

// Property-based counter invariant. The four Mailbox counters (RFC 8621
// section 2.1) are maintained incrementally, as signed deltas applied per
// operation by the Email create/set hooks. That delta arithmetic is the
// bug-prone part: a missed increment, a double count, or a thread-membership
// change that goes undetected drifts silently and is never noticed by an
// example-shaped test. This test runs random sequences of create / flag /
// move / destroy and, after every operation, recomputes all four counters
// from scratch straight from the current Email set (an independent oracle,
// written from the section 2.1 definitions, NOT from the delta code) and
// asserts the stored counters equal it. Any drift fails immediately, with the
// seed and step to reproduce.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"testing"
	"time"
)

// oracleEmail is the counter-relevant projection of one Email, read back
// from the server (so threadId is the id the server actually assigned, and
// threading correctness is not re-derived here).
type oracleEmail struct {
	mailboxes map[string]bool // mailbox id -> member
	unread    bool            // neither $seen nor $draft (section 2.1)
	threadId  string
}

// oracleConsidered applies the section 2.1 trash rules that decide whether an
// Email counts toward unreadThreads for mailbox mb. It is deliberately a
// separate implementation from the package's consideredForUnread, so the two
// cross-check each other. trash is "" when the account has no trash Mailbox.
func oracleConsidered(e oracleEmail, mb, trash string) bool {
	if trash != "" && mb == trash {
		return e.mailboxes[trash] // trash: only Emails in the trash (rule 2)
	}
	if trash != "" && len(e.mailboxes) == 1 && e.mailboxes[trash] {
		return false // other Mailbox: ignore Emails only in the trash (rule 1)
	}
	return true
}

// oracleCounters recomputes [totalEmails, unreadEmails, totalThreads,
// unreadThreads] for mailbox mb from the whole Email set, per section 2.1.
func oracleCounters(emails map[string]oracleEmail, mb, trash string) [4]int64 {
	var totalE, unreadE int64
	totalThreads := map[string]bool{}
	inThreadHere := map[string]bool{}    // thread has a considered Email in mb
	threadHasUnread := map[string]bool{} // thread has a considered unread Email
	for _, e := range emails {
		if e.mailboxes[mb] {
			totalE++
			if e.unread {
				unreadE++
			}
			totalThreads[e.threadId] = true
		}
		if !oracleConsidered(e, mb, trash) {
			continue
		}
		if e.mailboxes[mb] {
			inThreadHere[e.threadId] = true
		}
		if e.unread {
			threadHasUnread[e.threadId] = true
		}
	}
	var unreadT int64
	for tid := range inThreadHere {
		if threadHasUnread[tid] {
			unreadT++
		}
	}
	return [4]int64{totalE, unreadE, int64(len(totalThreads)), unreadT}
}

// readEmailStates reads every live Email's counter-relevant fields.
func readEmailStates(t *testing.T, ts *httptest.Server) map[string]oracleEmail {
	t.Helper()
	ids := qIds(t, emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q}`, testAccount)))
	out := map[string]oracleEmail{}
	if len(ids) == 0 {
		return out
	}
	idsJSON, _ := json.Marshal(ids)
	r := callMail(t, ts, inv("Email/get", fmt.Sprintf(
		`{"accountId":%q,"ids":%s,"properties":["mailboxIds","keywords","threadId"]}`,
		testAccount, idsJSON), "0"))
	for _, v := range methodArgs(t, r, 0, "Email/get")["list"].([]any) {
		m := v.(map[string]any)
		e := oracleEmail{mailboxes: map[string]bool{}, threadId: m["threadId"].(string)}
		for mb := range m["mailboxIds"].(map[string]any) {
			e.mailboxes[mb] = true
		}
		kw, _ := m["keywords"].(map[string]any)
		e.unread = !hasKeyword(kw, "$seen") && !hasKeyword(kw, "$draft")
		out[m["id"].(string)] = e
	}
	return out
}

func hasKeyword(kw map[string]any, name string) bool {
	v, ok := kw[name]
	return ok && v == true
}

// TestCounterInvariantProperty drives random op sequences over a fixed set of
// Mailboxes and checks the stored counters against the oracle after each step.
func TestCounterInvariantProperty(t *testing.T) {
	for _, seed := range []int64{1, 2, 3} {
		seed := seed
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			runCounterProperty(t, seed)
		})
	}
}

func runCounterProperty(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	ts, db, store := emailServer(t)

	// A fixed set of Mailboxes; trash triggers the section 2.1.1 rules.
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	trash := createMailbox(t, ts, `{"name":"Trash","role":"trash"}`)
	archive := createMailbox(t, ts, `{"name":"Archive","role":"archive"}`)
	work := createMailbox(t, ts, `{"name":"Work"}`)
	mailboxes := []string{inbox, trash, archive, work}

	// nonEmptySubset returns a random non-empty subset of the Mailboxes
	// (Email/set enforces mailboxIds has at least one entry).
	nonEmptySubset := func() map[string]bool {
		s := map[string]bool{}
		for _, mb := range mailboxes {
			if rng.Intn(2) == 0 {
				s[mb] = true
			}
		}
		if len(s) == 0 {
			s[mailboxes[rng.Intn(len(mailboxes))]] = true
		}
		return s
	}
	subsetJSON := func(s map[string]bool) string {
		b, _ := json.Marshal(s)
		return string(b)
	}

	var live []string
	seq := 0

	check := func(step int, op string) {
		emails := readEmailStates(t, ts)
		for _, mb := range mailboxes {
			got := readCounters(t, ts, mb)
			want := oracleCounters(emails, mb, trash)
			g := [4]any{got["totalEmails"], got["unreadEmails"], got["totalThreads"], got["unreadThreads"]}
			for i := range mailboxCounters {
				if int64(g[i].(float64)) != want[i] {
					t.Fatalf("seed=%d step=%d op=%s mailbox=%s: %s stored=%v oracle=%d (stored all=%v oracle all=%v)",
						seed, step, op, mb, mailboxCounters[i], g[i], want[i], g, want)
				}
			}
		}
	}

	const steps = 140
	for step := 0; step < steps; step++ {
		op := "create"
		if len(live) > 0 {
			switch rng.Intn(10) {
			case 0, 1, 2, 3:
				op = "create"
			case 4, 5:
				op = "flag"
			case 6, 7:
				op = "move"
			default:
				op = "destroy"
			}
		}

		switch op {
		case "create":
			group := rng.Intn(4) // shared roots so Emails thread together
			seq++
			headers := map[string]string{
				"Message-ID": fmt.Sprintf("<m%d@x>", seq),
				"References": fmt.Sprintf("<root%d@x>", group),
			}
			keywords := map[string]bool{}
			if rng.Intn(2) == 0 {
				keywords["$seen"] = true
			}
			if rng.Intn(4) == 0 {
				keywords["$draft"] = true
			}
			raw := threadMsg(fmt.Sprintf("Subject %d", group), headers)
			id := putEmailAt(t, db, store, raw, nonEmptySubset(), keywords, testReceivedAt.Add(sequenceOffset(seq)))
			live = append(live, id)

		case "flag":
			id := live[rng.Intn(len(live))]
			kw := "$seen"
			if rng.Intn(2) == 0 {
				kw = "$draft"
			}
			val := "true"
			if rng.Intn(2) == 0 {
				val = "null" // remove the keyword
			}
			callMail(t, ts, inv("Email/set", fmt.Sprintf(
				`{"accountId":%q,"update":{%q:{"keywords/%s":%s}}}`, testAccount, id, kw, val), "0"))

		case "move":
			id := live[rng.Intn(len(live))]
			callMail(t, ts, inv("Email/set", fmt.Sprintf(
				`{"accountId":%q,"update":{%q:{"mailboxIds":%s}}}`, testAccount, id, subsetJSON(nonEmptySubset())), "0"))

		case "destroy":
			idx := rng.Intn(len(live))
			id := live[idx]
			callMail(t, ts, inv("Email/set", fmt.Sprintf(
				`{"accountId":%q,"destroy":[%q]}`, testAccount, id), "0"))
			live = append(live[:idx], live[idx+1:]...)
		}

		check(step, op)
	}
}

// sequenceOffset spreads receivedAt across creates so ordering is stable; the
// exact value is irrelevant to the counters.
func sequenceOffset(seq int) time.Duration {
	return time.Duration(seq) * time.Minute
}
