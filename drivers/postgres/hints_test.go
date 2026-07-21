package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/providers/notify/notifytest"
)

// --- Decoder tests (no Postgres needed) ---

func TestDecodeChange(t *testing.T) {
	p, err := decodeChange([]byte(`{"o":"abc","a":"u1","t":{"Email":"s1"}}`))
	if err != nil {
		t.Fatalf("valid change: %v", err)
	}
	if p.Origin != "abc" || p.Account != "u1" || p.Types["Email"] != "s1" {
		t.Fatalf("decoded %+v", p)
	}
	bad := []string{
		``,
		`{}`,
		`not json`,
		`[]`,
		`{"o":"x"}`,           // no account
		`{"a":"y"}`,           // no origin
		`{"o":"","a":"z"}`,    // empty origin
		`{"o":"x","a":""}`,    // empty account
		`{"o":123,"a":"y"}`,   // wrong type
		`{"o":"x","a":"y"}{}`, // trailing garbage
	}
	for _, s := range bad {
		if _, err := decodeChange([]byte(s)); err == nil {
			t.Errorf("decodeChange(%q) accepted, want error", s)
		}
	}
}

func TestDecodeLease(t *testing.T) {
	p, err := decodeLease([]byte(`{"o":"abc","a":"u1"}`))
	if err != nil {
		t.Fatalf("valid lease: %v", err)
	}
	if p.Origin != "abc" || p.Account != "u1" {
		t.Fatalf("decoded %+v", p)
	}
	for _, s := range []string{``, `{}`, `garbage`, `{"o":"x"}`, `{"a":"y"}`, `{"o":"","a":""}`} {
		if _, err := decodeLease([]byte(s)); err == nil {
			t.Errorf("decodeLease(%q) accepted, want error", s)
		}
	}
}

// FuzzDecodeChange asserts the decoder never panics on arbitrary bytes and
// never accepts a payload that violates its invariants (payloads are untrusted:
// any database role can forge one).
func FuzzDecodeChange(f *testing.F) {
	for _, seed := range []string{
		`{"o":"abc","a":"u1","t":{"Email":"s1"}}`,
		`{}`, ``, `not json`, `{"o":"x","a":"y","t":{}}`, `{"o":123}`, `[1,2,3]`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := decodeChange(data)
		if err == nil && (p.Origin == "" || p.Account == "") {
			t.Fatalf("accepted a payload with empty origin/account: %+v", p)
		}
	})
}

// FuzzDecodeLease is the lease-payload counterpart.
func FuzzDecodeLease(f *testing.F) {
	for _, seed := range []string{
		`{"o":"abc","a":"u1"}`, `{}`, ``, `not json`, `{"o":123,"a":456}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := decodeLease(data)
		if err == nil && (p.Origin == "" || p.Account == "") {
			t.Fatalf("accepted a payload with empty origin/account: %+v", p)
		}
	})
}

// TestChangePayloadsSplitsOversize proves a change too large for one NOTIFY is
// split into several payloads, each under the budget, together covering every
// type exactly once.
func TestChangePayloadsSplitsOversize(t *testing.T) {
	h := &Hints{origin: "originhex"}
	types := jmap.TypeState{}
	for i := 0; i < 1000; i++ {
		types[fmt.Sprintf("Type%04d", i)] = fmt.Sprintf("state-value-%04d", i)
	}
	payloads := h.changePayloads("account", types)
	if len(payloads) < 2 {
		t.Fatalf("expected a split, got %d payload(s)", len(payloads))
	}
	merged := jmap.TypeState{}
	for _, p := range payloads {
		if len(p) > notifyPayloadBudget {
			t.Errorf("payload of %d bytes exceeds the %d budget", len(p), notifyPayloadBudget)
		}
		var cp changePayload
		if err := json.Unmarshal([]byte(p), &cp); err != nil {
			t.Fatal(err)
		}
		if cp.Origin != "originhex" || cp.Account != "account" {
			t.Errorf("payload header wrong: %+v", cp)
		}
		for k, v := range cp.Types {
			if _, dup := merged[k]; dup {
				t.Errorf("type %s appeared in two payloads", k)
			}
			merged[k] = v
		}
	}
	if len(merged) != len(types) {
		t.Fatalf("split lost types: got %d, want %d", len(merged), len(types))
	}
	for k, v := range types {
		if merged[k] != v {
			t.Errorf("type %s state = %q, want %q", k, merged[k], v)
		}
	}
}

// TestAfterConsume pins the reconnect throttle: a healthy long-lived connection
// reconnects immediately and resets the backoff, while a run of short-lived
// connections escalates the delay up to the cap rather than re-dialing in a
// tight loop.
func TestAfterConsume(t *testing.T) {
	if delay, next := afterConsume(stableConnThreshold, 8*time.Second); delay != 0 || next != backoffInitial {
		t.Errorf("healthy connection: delay=%v next=%v, want 0 and %v", delay, next, backoffInitial)
	}
	// Every connection dies almost immediately: delays must climb and cap.
	backoff := backoffInitial
	var delays []time.Duration
	for i := 0; i < 8; i++ {
		var d time.Duration
		d, backoff = afterConsume(time.Millisecond, backoff)
		delays = append(delays, d)
	}
	if delays[0] != backoffInitial {
		t.Errorf("first throttled delay = %v, want %v", delays[0], backoffInitial)
	}
	for i := 1; i < len(delays); i++ {
		if delays[i] < delays[i-1] || delays[i] > backoffMax {
			t.Fatalf("delays not monotonic within cap: %v", delays)
		}
	}
	if delays[len(delays)-1] != backoffMax {
		t.Errorf("sustained flapping did not reach the %v cap: %v", backoffMax, delays)
	}
}

// --- Integration tests (require PG_TEST_DSN) ---

// openHints starts one transport over a fresh test database.
func openHints(t *testing.T) *Hints {
	t.Helper()
	store := openTestDB(t)
	t.Cleanup(func() { store.Close() })
	h, err := OpenHints(context.Background(), store)
	if err != nil {
		t.Fatalf("OpenHints: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// openHintsPair starts two transports over one shared test database - the
// cross-instance case. It also returns the database DSN for tests that need to
// disrupt the connections directly.
func openHintsPair(t *testing.T) (a, b *Hints, dsn string) {
	t.Helper()
	storeA := openTestDB(t)
	t.Cleanup(func() { storeA.Close() })

	dsn, err := withDSN(os.Getenv(dsnEnv), dbNameFor(t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(func() { storeB.Close() })

	hA, err := OpenHints(context.Background(), storeA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hA.Close() })
	hB, err := OpenHints(context.Background(), storeB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hB.Close() })
	return hA, hB, dsn
}

// TestHintsNotifierContract runs the shared notifier contract suite against the
// Postgres-backed notifier (single instance).
func TestHintsNotifierContract(t *testing.T) {
	notifytest.Run(t, func(t *testing.T) notify.Notifier {
		return openHints(t).Notifier()
	})
}

// TestHintsNotifierLinked runs the cross-instance contract with two transports
// over one database - a change published on one is observed on the other.
func TestHintsNotifierLinked(t *testing.T) {
	notifytest.RunLinked(t, func(t *testing.T) (a, b notify.Notifier) {
		hA, hB, _ := openHintsPair(t)
		return hA.Notifier(), hB.Notifier()
	})
}

// TestHintsSelfEchoFiltered proves a publisher's own NOTIFY, once it round-trips
// back through the listener, is dropped: the local subscriber sees exactly one
// delivery, not a duplicate from the echo.
func TestHintsSelfEchoFiltered(t *testing.T) {
	n := openHints(t).Notifier()
	ctx := context.Background()
	sub, err := n.Subscribe(ctx, []jmap.Id{"a1"})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	n.Publish(ctx, "a1", jmap.TypeState{"Email": "1"})
	if got := waitState(t, sub, 5*time.Second); got["a1"]["Email"] != "1" {
		t.Fatalf("first delivery = %v, want a1/Email=1", got)
	}
	// Give the self-notify time to round-trip and be dropped; no second delivery.
	c, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	if extra, err := sub.Wait(c); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("self-echo leaked a second delivery: %v (err %v)", extra, err)
	}
}

// TestHintsReconnectsAfterConnectionLoss proves the listener recovers: after
// every backend connection to the database is terminated, cross-instance
// delivery resumes once the listener reconnects.
func TestHintsReconnectsAfterConnectionLoss(t *testing.T) {
	hA, hB, dsn := openHintsPair(t)
	ctx := context.Background()
	subB, err := hB.Notifier().Subscribe(ctx, []jmap.Id{"a1"})
	if err != nil {
		t.Fatal(err)
	}
	defer subB.Close()

	// Baseline: cross-instance delivery works before any disruption.
	hA.Notifier().Publish(ctx, "a1", jmap.TypeState{"Email": "1"})
	if got := waitState(t, subB, 5*time.Second); got["a1"]["Email"] != "1" {
		t.Fatalf("baseline delivery = %v", got)
	}

	terminateBackends(t, dsn)

	// Republish until the reconnected listener delivers, tolerating the lossy
	// window while it is down.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		hA.Notifier().Publish(ctx, "a1", jmap.TypeState{"Email": "2"})
		c, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		got, err := subB.Wait(c)
		cancel()
		if err == nil && got["a1"]["Email"] == "2" {
			return // reconnected and delivered
		}
	}
	t.Fatal("no delivery after connection loss - listener did not reconnect")
}

// waitState waits for one delivery within d and returns it.
func waitState(t *testing.T, sub notify.Subscription, d time.Duration) notify.Changes {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	got, err := sub.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return got
}

// terminateBackends forcibly closes every other connection to the test
// database, simulating a server restart or a dropped network path.
func terminateBackends(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("terminate admin connect: %v", err)
	}
	defer admin.Close()
	_, err = admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()`)
	if err != nil {
		t.Fatalf("terminate backends: %v", err)
	}
}
