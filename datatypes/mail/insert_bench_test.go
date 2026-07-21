package mail

// Benchmarks for the write/ingest path shared by delivery and Email/import
// (insertEmail, materialize.go's commit half): the cost of turning a parsed
// message into a stored Email record - thread assignment, the record
// insert, EmailDelivery's state bump, and per-mailbox counter maintenance.
// Self-contained for *testing.B, same reasoning as query_bench_test.go.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// smallRaw is a short, single-part message: the common case for delivery
// throughput - most mail is not attachment-heavy.
func smallRaw(i int) string {
	return fmt.Sprintf("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: msg %d\r\n\r\nHello there, this is a short message body.\r\n", i)
}

// attachmentRaw is a multipart message with a base64-encoded attachment of
// the given decoded size, to see whether attachment size moves insertEmail
// cost the way it moves parse cost alone.
func attachmentRaw(i, attachSize int) string {
	payload := strings.Repeat("A", attachSize)
	b64 := b64encode(payload)
	return "From: alice@example.com\r\nTo: bob@example.com\r\nSubject: msg " + fmt.Sprint(i) +
		"\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nSee attached.\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		b64 + "\r\n--b--\r\n"
}

func b64encode(s string) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	data := []byte(s)
	for i := 0; i < len(data); i += 3 {
		var n uint32
		rem := len(data) - i
		n = uint32(data[i]) << 16
		if rem > 1 {
			n |= uint32(data[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(data[i+2])
		}
		b.WriteByte(table[(n>>18)&0x3f])
		b.WriteByte(table[(n>>12)&0x3f])
		if rem > 1 {
			b.WriteByte(table[(n>>6)&0x3f])
		} else {
			b.WriteByte('=')
		}
		if rem > 2 {
			b.WriteByte(table[n&0x3f])
		} else {
			b.WriteByte('=')
		}
	}
	return b.String()
}

// BenchmarkInsertEmailSmall is the write side of delivery/import for the
// common case: one short message per call, fresh Thread each time (distinct
// Subject/Message-Id per i, the same as unrelated inbound mail).
func BenchmarkInsertEmailSmall(b *testing.B) {
	ts, db, store, _, _ := benchEmailServer(b)
	inbox := benchCreateMailbox(b, ts, `{"name":"Inbox","role":"inbox"}`)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchPutEmailAt(b, db, store, smallRaw(i), mset(inbox), map[string]bool{"$seen": true}, base.Add(time.Duration(i)*time.Second))
	}
}

// BenchmarkInsertEmailAttachment is the same write path for a message
// carrying a base64 attachment, at a few sizes - to separate "insert cost
// independent of message size" from "insert cost driven by parse cost",
// which are optimized differently.
func BenchmarkInsertEmailAttachment(b *testing.B) {
	for _, size := range []int{10 * 1024, 200 * 1024, 2 * 1024 * 1024} {
		b.Run(fmt.Sprintf("%dKB", size/1024), func(b *testing.B) {
			ts, db, store, _, _ := benchEmailServer(b)
			inbox := benchCreateMailbox(b, ts, `{"name":"Inbox","role":"inbox"}`)
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

			// Build every message before the timer starts: base64-encoding
			// the fixture is test-harness cost, not insertEmail cost, and
			// must not be counted against it.
			raws := make([]string, b.N)
			for i := range raws {
				raws[i] = attachmentRaw(i, size)
			}

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				benchPutEmailAt(b, db, store, raws[i], mset(inbox), map[string]bool{"$seen": true}, base.Add(time.Duration(i)*time.Second))
			}
		})
	}
}

// threadReplyRaw is one reply into the Thread anchored by anchor (every
// reply References the same anchor id and shares its base subject, per
// RFC 8621 section 3's join condition) - anchor must be unique per Thread,
// so distinct benchmark trials do not accidentally join one another's
// Thread through a shared literal reference.
func threadReplyRaw(anchor string, i int) string {
	return fmt.Sprintf("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Re: ongoing thread\r\n"+
		"Message-Id: <msg%s-%d@example.com>\r\nIn-Reply-To: <%s@example.com>\r\nReferences: <%s@example.com>\r\n\r\n"+
		"Reply number %d.\r\n", anchor, i, anchor, anchor, i)
}

// BenchmarkInsertEmailIntoThreadOfSize isolates the marginal cost of one
// insert into a Thread that already has n other members - the case where
// assignThread and adjustCounters have the most prior state to reconcile
// against. A single growing-thread benchmark (every b.N iteration adding to
// the same thread) does not work here: testing.B calibrates iteration count
// assuming roughly constant per-iteration cost, but this operation's cost
// is a function of current thread size, which changes every iteration -
// the reported ns/op would be an uninterpretable blend of many different
// thread sizes. Instead, each timed iteration primes a fresh thread to
// exactly n members (untimed), then measures exactly the marginal (n+1)th
// insert.
func BenchmarkInsertEmailIntoThreadOfSize(b *testing.B) {
	for _, n := range []int{10, 100, 500, 1000} {
		b.Run(fmt.Sprintf("%d_members", n), func(b *testing.B) {
			ts, db, store, _, _ := benchEmailServer(b)
			inbox := benchCreateMailbox(b, ts, `{"name":"Inbox","role":"inbox"}`)
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				anchor := fmt.Sprintf("trial%d", i)
				b.StopTimer()
				for j := 0; j < n; j++ {
					benchPutEmailAt(b, db, store, threadReplyRaw(anchor, j), mset(inbox), map[string]bool{"$seen": true}, base)
				}
				b.StartTimer()
				benchPutEmailAt(b, db, store, threadReplyRaw(anchor, n), mset(inbox), map[string]bool{"$seen": true}, base)
			}
		})
	}
}
