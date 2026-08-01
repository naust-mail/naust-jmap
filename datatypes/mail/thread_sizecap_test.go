package mail

// Thread size cap hardening: split out from delivery_hardening_test.go, this
// exercises the emailstore engine's thread-join bound directly rather than
// the delivery seam.

import (
	"strconv"
	"testing"

	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailstore"
)

// TestThreadSizeCapSplits: once a thread reaches emailstore.ThreadSizeCap members,
// the next message with the same join keys (Message-ID + base subject) starts a
// fresh threadId instead of joining, bounding the per-insert thread scan. RFC
// 8621 section 3 does not mandate the join algorithm, so this split is allowed.
func TestThreadSizeCapSplits(t *testing.T) {
	// Exercise the split boundary at a small size (the default 1024 would build
	// a 1024-member thread, paying the full per-insert scan the cap exists to
	// bound). Package tests run sequentially, so restoring the default is safe.
	defer func(orig int) { emailstore.ThreadSizeCap = orig }(emailstore.ThreadSizeCap)
	emailstore.ThreadSizeCap = 8

	ts, db, store := emailServer(t)
	// Same Message-ID and subject (identical join keys) with a varied body so
	// each is a distinct blob - the same-thread flood the cap bounds.
	msg := func(i int) string {
		return "Subject: Flood\r\nMessage-ID: <flood@x>\r\n\r\nbody " + strconv.Itoa(i) + "\r\n"
	}

	first := putEmail(t, db, store, msg(0), mbInbox, nil)
	firstThread := threadOf(t, ts, first)

	// Insert up to and including the cap boundary. Messages 1..cap-1 join (the
	// thread fills to the cap); message at the cap is the one that finds a full
	// thread and must split.
	capN := emailstore.ThreadSizeCap
	var capID string
	for i := 1; i <= capN; i++ {
		id := putEmail(t, db, store, msg(i), mbInbox, nil)
		if i == capN {
			capID = id
		} else if i == capN-1 && threadOf(t, ts, id) != firstThread {
			t.Fatalf("message %d did not join the not-yet-full thread", i)
		}
	}
	if threadOf(t, ts, capID) == firstThread {
		t.Fatalf("message at the cap joined the full thread; want a new threadId (cap %d)", capN)
	}
}
