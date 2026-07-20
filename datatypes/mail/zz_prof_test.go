package mail

// Throwaway: where does a delivery's CPU actually go? The harness uses the
// in-memory backend, so storage is out of the picture - whatever this burns is
// the per-message work itself (read, parse, decode, hash, index).

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"

	"github.com/naust-mail/naust-jmap/core/providers/blob/fsstore"
	"strings"
	"testing"
	"time"
)

// profPath returns a stable location for profile output, creating the
// directory on first use. Profiles must outlive the test run for
// go tool pprof, so t.TempDir (removed at cleanup) is not usable.
func profPath(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(os.TempDir(), "naust-prof")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, name)
}

func benchMessage(seq int) string {
	payload := make([]byte, 1<<20) // 1 MiB, as the ingest benchmark uses
	for i := range payload {
		payload[i] = byte(i)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: joe@example.com\r\nTo: jane@example.com\r\n")
	fmt.Fprintf(&b, "Subject: profile %d\r\nMessage-ID: <%d@example.com>\r\n", seq, seq)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=\"BOUND\"\r\n\r\n")
	b.WriteString("--BOUND\r\nContent-Type: text/plain\r\n\r\nhello\r\n")
	b.WriteString("--BOUND\r\nContent-Type: application/octet-stream\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	enc := base64.StdEncoding.EncodeToString(payload)
	for i := 0; i < len(enc); i += 76 {
		end := i + 76
		if end > len(enc) {
			end = len(enc)
		}
		b.WriteString(enc[i:end])
		b.WriteString("\r\n")
	}
	b.WriteString("--BOUND--\r\n")
	return b.String()
}

func TestZZProfileDelivery(t *testing.T) {
	ts, db, _ := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	// NOT the harness's kvstore: that one buffers and hashes twice, which is
	// its own flaw, not the delivery path's. fsstore is what the default does -
	// stream, hash once.
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := NewDeliverer(db, store, mapResolver{"jane@example.com": testAccount})

	const n = 30
	msgs := make([]string, n)
	for i := range msgs {
		msgs[i] = benchMessage(i) // distinct, so dedup never short-circuits
	}
	wire := len(msgs[0])

	f, err := os.Create(profPath(t, "deliver.prof"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < n; i++ {
		evs := d.Deliver(context.Background(),
			deliveryEnv("joe@example.com", "jane@example.com"),
			strings.NewReader(msgs[i]))
		if evs[0].Outcome != Accepted {
			t.Fatalf("delivery %d: %v %q", i, evs[0].Outcome, evs[0].Reason)
		}
	}
	elapsed := time.Since(start)
	pprof.StopCPUProfile()

	per := elapsed / n
	fmt.Printf("\n  wire size %d KiB, in-memory backend (NO disk, NO sqlite)\n", wire>>10)
	fmt.Printf("  Deliver(): %.1f ms/op  %.1f op/s  %.1f MiB/s\n\n",
		float64(per.Microseconds())/1000,
		float64(time.Second)/float64(per),
		float64(wire)/(1<<20)/per.Seconds())
}
