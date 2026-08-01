package deliver

// The sink that reads a whole body part for the body value of an Email/get
// (RFC 8621 section 4.2) must not hold the part to read it: it is reachable by
// a request, so a message the server accepted at delivery becomes a memory
// cost per request against it if it buffers, and a client that asks for a
// small value of a large part must pay for the small value. The matching
// claim for the text a search term is matched against (section 4.4.1) is
// tested with the search package it belongs to now
// (search/parsestream_test.go); this file keeps the delivery-pipeline half,
// which shares its heap-watching fixtures with that one through
// internal/testsupport.
//
// These tests measure that rather than assume it: they run a real parse over a
// large part and watch the live heap as its octets go past.

import (
	"context"
	"crypto/sha256"
	"hash"
	"io"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/testsupport"
)

// TestDeliveryDoesNotHoldTheMessage: an ingest is the unauthenticated surface,
// and it is where holding the message would hurt most - anyone who can reach the
// MTA can send one. The message must flow through the blob store and the parser
// together, so the server pays for a buffer and not for what it was sent, and
// the Email must still come out right at the end of it.
func TestDeliveryDoesNotHoldTheMessage(t *testing.T) {
	const size = 8 << 20
	ts, db, _ := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	d := mustDeliverer(t, db, streamingStore{}, mapResolver{"jane@example.com": testAccount})

	msg := io.MultiReader(
		strings.NewReader("From: joe@example.com\r\nSubject: large\r\n"),
		testsupport.TextMessage("utf-8", testsupport.Filler(size)),
	)
	w := testsupport.WatchHeap(msg)
	evs := d.Deliver(context.Background(), deliveryEnv("joe@example.com", "jane@example.com"), w)

	if len(evs) != 1 || evs[0].Outcome != mail.Accepted {
		t.Fatalf("delivery failed: %+v", evs)
	}
	if evs[0].Size < size {
		t.Errorf("stored size %d, want the whole message", evs[0].Size)
	}
	if used := w.Held(); used > size/4 {
		t.Errorf("delivering an %d octet message held %d octets of heap", size, used)
	}
}

// TestDeliveryOversizeIsRejectedAsItArrives: the size limit is enforced on the
// octets as they pass, not on a buffer already full of them, so a sender cannot
// make the server hold a message it is going to refuse. The blob that was being
// written is aborted, so the refused message leaves nothing behind.
func TestDeliveryOversizeIsRejectedAsItArrives(t *testing.T) {
	const limit = 1 << 20
	const sent = 16 << 20
	ts, db, _ := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	d := mustDeliverer(t, db, streamingStore{}, mapResolver{"jane@example.com": testAccount}, WithMaxMessageSize(limit))

	w := testsupport.WatchHeap(testsupport.TextMessage("utf-8", testsupport.Filler(sent)))
	evs := d.Deliver(context.Background(), deliveryEnv("joe@example.com", "jane@example.com"), w)

	if len(evs) != 1 || evs[0].Outcome != mail.Rejected || evs[0].Reason != "message too large" {
		t.Fatalf("want a rejection for the size limit, got %+v", evs)
	}
	if evs[0].EmailId != "" {
		t.Error("a rejected message created an Email")
	}
	// The parse stops at the limit, so the octets behind it are never even read:
	// what is held cannot be more than the limit itself.
	if used := w.Held(); used > limit*2 {
		t.Errorf("rejecting a %d octet message held %d octets of heap (limit %d)", sent, used, limit)
	}
}

// streamingStore is a blob store that retains nothing: it computes the content
// address (RFC 8620 section 6.1) as the octets pass and drops them. The store the
// server ships with streams too, but the in-memory one the rest of these tests
// use holds every blob by design - measuring a delivery against it would measure
// the store, not the pipeline, which is what these two tests are about.
type streamingStore struct{}

func (streamingStore) Create(context.Context, jmap.Id) (blob.BlobWriter, error) {
	return &streamingWriter{h: sha256.New()}, nil
}

func (streamingStore) Put(context.Context, jmap.Id, jmap.Id, []byte) error { return nil }

func (streamingStore) Open(context.Context, jmap.Id, jmap.Id) (io.ReadCloser, int64, error) {
	return io.NopCloser(strings.NewReader("")), 0, nil
}

func (streamingStore) Delete(context.Context, jmap.Id, jmap.Id) error { return nil }

type streamingWriter struct {
	h hash.Hash
	n int64
}

func (w *streamingWriter) Write(b []byte) (int, error) {
	w.n += int64(len(b))
	return w.h.Write(b)
}

func (w *streamingWriter) ID() jmap.Id {
	var sum [sha256.Size]byte
	w.h.Sum(sum[:0])
	return blob.IdFromDigest(sum)
}

func (w *streamingWriter) Commit() (jmap.Id, error) {
	return w.ID(), nil
}

func (w *streamingWriter) Abort() error { return nil }
