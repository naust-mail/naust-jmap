package quotas

// Hostile and degenerate inputs: definitions a Source has no business
// returning, filter values of the wrong shape, boundary numbers,
// concurrency, and the states the spec permits but implementations
// often mishandle.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// A definition outside the closed value sets is refused with the
// offending field named, and nothing is committed.
func TestRefreshRejectsInvalidDefinitions(t *testing.T) {
	valid := Quota{Id: "ok", ResourceType: "octets", HardLimit: 10,
		Scope: "account", Name: "fine", Types: []string{"Email"}}
	cases := []struct {
		name  string
		quota Quota
		want  string
	}{
		{"unknown resourceType", Quota{Id: "x", ResourceType: "bytes", Scope: "account", Name: "n"}, "ResourceType"},
		{"empty resourceType", Quota{Id: "x", Scope: "account", Name: "n"}, "ResourceType"},
		{"unknown scope", Quota{Id: "x", ResourceType: "count", Scope: "planet", Name: "n"}, "Scope"},
		{"empty scope", Quota{Id: "x", ResourceType: "count", Name: "n"}, "Scope"},
		{"scope wrong case", Quota{Id: "x", ResourceType: "count", Scope: "Account", Name: "n"}, "Scope"},
		{"resourceType wrong case", Quota{Id: "x", ResourceType: "Octets", Scope: "account", Name: "n"}, "ResourceType"},
		{"empty name", Quota{Id: "x", ResourceType: "count", Scope: "account"}, "Name"},
		{"missing source id", Quota{ResourceType: "count", Scope: "account", Name: "n"}, "Id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{quotas: []Quota{valid, tc.quota}}
			svc, state := sourceServer(t, src)
			before := state()
			err := svc.Refresh(context.Background(), testAccount)
			if err == nil {
				t.Fatal("Refresh accepted the definition, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
			// The valid sibling must not have been committed either:
			// one bad definition aborts the whole account.
			if state() != before {
				t.Error("state moved despite the pull being rejected")
			}
			if got := listQuotas(t, svc); len(got) != 0 {
				t.Errorf("records = %v, want none", got)
			}
		})
	}
}

// Two definitions sharing a source id would make the mirror ambiguous.
func TestRefreshRejectsDuplicateSourceIds(t *testing.T) {
	q := Quota{Id: "same", ResourceType: "count", HardLimit: 1,
		Scope: "account", Name: "a", Types: []string{"Email"}}
	other := q
	other.Name = "b"
	src := &fakeSource{quotas: []Quota{q, other}}
	svc, _ := sourceServer(t, src)
	err := svc.Refresh(context.Background(), testAccount)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error = %v, want a duplicate-id complaint", err)
	}
}

// Usage above the hard limit is a legal state - it is how an
// enforcement layer knows the account is over - and is stored verbatim.
func TestUsedMayExceedHardLimit(t *testing.T) {
	ts, svc := newTestServer(t)
	ctx := context.Background()
	q := mailQuota("storage", "Email")
	q.HardLimit = 100
	id := mustUpsert(t, svc, q)
	if err := svc.SetUsed(ctx, testAccount, id, 999999); err != nil {
		t.Fatal(err)
	}
	r := callUsing(t, ts, mailUsing, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	list, _ := argsOf(t, r, 0, "Quota/get")["list"].([]any)
	rec := list[0].(map[string]any)
	if rec["used"] != float64(999999) || rec["hardLimit"] != float64(100) {
		t.Errorf("record = %v, want used 999999 over hardLimit 100", rec)
	}
}

// The limits are UnsignedInt, so the boundary is the largest value
// JSON round-trips exactly rather than anything this package imposes.
func TestExtremeValues(t *testing.T) {
	ts, svc := newTestServer(t)
	ctx := context.Background()
	const maxExact = uint64(1)<<53 - 1
	q := mailQuota("huge", "Email")
	q.HardLimit = maxExact
	q.WarnLimit = u64(maxExact - 2)
	q.SoftLimit = u64(maxExact - 1)
	id := mustUpsert(t, svc, q)
	if err := svc.SetUsed(ctx, testAccount, id, maxExact); err != nil {
		t.Fatal(err)
	}
	obj, err := svc.db.Get(ctx, testAccount, TypeName, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(obj["used"]); got != fmt.Sprint(maxExact) {
		t.Errorf("used = %s, want %d", got, maxExact)
	}
	r := callUsing(t, ts, mailUsing, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	list, _ := argsOf(t, r, 0, "Quota/get")["list"].([]any)
	if got := list[0].(map[string]any)["hardLimit"]; got != float64(maxExact) {
		t.Errorf("hardLimit = %v, want %d", got, maxExact)
	}
}

// An absurd delta fails and leaves the counter alone. Overflow is
// deliberately not clamped the way underflow is: a usage figure below
// zero is reachable in ordinary operation and zero is the nearest true
// value, whereas a figure past the representable range only arrives
// through a nonsense delta, and pinning it at the maximum would assert
// an enormous usage the account does not have.
func TestAddUsedOverflowRefused(t *testing.T) {
	_, svc := newTestServer(t)
	ctx := context.Background()
	id := mustUpsert(t, svc, mailQuota("storage", "Email"))
	if err := svc.SetUsed(ctx, testAccount, id, 5000); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int64{math.MaxInt64, math.MaxInt64 - 4999, 1 << 60} {
		if err := svc.AddUsed(ctx, testAccount, id, delta); err == nil {
			t.Errorf("AddUsed(%d) succeeded, want an error", delta)
		}
		obj, err := svc.db.Get(ctx, testAccount, TypeName, id)
		if err != nil {
			t.Fatal(err)
		}
		var used int64
		if err := json.Unmarshal(obj["used"], &used); err != nil {
			t.Fatal(err)
		}
		if used != 5000 {
			t.Fatalf("used = %d after a refused AddUsed(%d), want 5000 untouched", used, delta)
		}
	}
	// The most negative delta representable is still an ordinary
	// underflow rather than a wrap - a non-negative counter plus a
	// negative delta cannot leave the range - so it clamps.
	if err := svc.AddUsed(ctx, testAccount, id, math.MinInt64); err != nil {
		t.Fatalf("AddUsed(MinInt64): %v", err)
	}
	obj, err := svc.db.Get(ctx, testAccount, TypeName, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(obj["used"]); got != "0" {
		t.Errorf("used = %s, want 0", got)
	}
}

// Filter values of the wrong JSON shape are rejected as
// invalidArguments, never coerced.
func TestFilterRejectsWrongValueTypes(t *testing.T) {
	ts, svc := newTestServer(t)
	mustUpsert(t, svc, mailQuota("q", "Email"))
	cases := []string{
		`{"name":123}`,
		`{"name":null}`,
		`{"name":true}`,
		`{"name":["a"]}`,
		`{"name":{"nested":"object"}}`,
		`{"scope":0}`,
		`{"resourceType":false}`,
		`{"type":null}`,
	}
	for _, filter := range cases {
		t.Run(filter, func(t *testing.T) {
			r := callUsing(t, ts, mailUsing, "Quota/query",
				`{"accountId":"Atest1","filter":`+filter+`}`)
			if r.MethodResponses[0].Name != "error" {
				t.Fatalf("got %s, want error", r.MethodResponses[0].Args)
			}
			if got := argsOf(t, r, 0, "error")["type"]; got != jmap.ErrInvalidArguments {
				t.Errorf("error type = %v, want %s", got, jmap.ErrInvalidArguments)
			}
		})
	}
}

// String comparison happens on decoded values: two JSON spellings of
// one string must match, and an escape must not match its literal
// backslash form.
func TestFilterComparesDecodedStrings(t *testing.T) {
	ts, svc := newTestServer(t)
	mustUpsert(t, svc, mailQuota("café-quota", "Email"))

	// The same name written with a \u escape must match.
	r := callUsing(t, ts, mailUsing, "Quota/query",
		`{"accountId":"Atest1","filter":{"name":"café"}}`)
	if ids, _ := argsOf(t, r, 0, "Quota/query")["ids"].([]any); len(ids) != 1 {
		t.Errorf("ids = %v, want the escaped spelling to match", ids)
	}
	// A literal backslash-u sequence is a different string.
	r = callUsing(t, ts, mailUsing, "Quota/query",
		`{"accountId":"Atest1","filter":{"name":"caf\\u00e9"}}`)
	if ids, _ := argsOf(t, r, 0, "Quota/query")["ids"].([]any); len(ids) != 0 {
		t.Errorf("ids = %v, want no match for the literal escape text", ids)
	}
}

// Strings that break naive implementations: empty, control characters,
// astral-plane runes, and a name that looks like JSON.
func TestHostileStringsRoundTrip(t *testing.T) {
	ts, svc := newTestServer(t)
	names := []string{
		`"},{"injected":`,
		"tab\there",
		"emoji \U0001F600 name",
		strings.Repeat("long", 500),
		"null",
		"nul\x00inside",
	}
	for _, name := range names {
		q := mailQuota(name, "Email")
		mustUpsert(t, svc, q)
	}
	r := callUsing(t, ts, mailUsing, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	list, _ := argsOf(t, r, 0, "Quota/get")["list"].([]any)
	if len(list) != len(names) {
		t.Fatalf("list has %d records, want %d", len(list), len(names))
	}
	got := make(map[string]bool, len(list))
	for _, e := range list {
		got[e.(map[string]any)["name"].(string)] = true
	}
	for _, name := range names {
		if !got[name] {
			t.Errorf("name %q did not round-trip", name)
		}
	}
}

// An empty filter condition object matches every visible record
// (RFC 9425 section 4.4).
func TestEmptyFilterMatchesAll(t *testing.T) {
	ts, svc := newTestServer(t)
	mustUpsert(t, svc, mailQuota("a", "Email"))
	mustUpsert(t, svc, mailQuota("b", "Email"))
	r := callUsing(t, ts, mailUsing, "Quota/query",
		`{"accountId":"Atest1","filter":{}}`)
	if ids, _ := argsOf(t, r, 0, "Quota/query")["ids"].([]any); len(ids) != 2 {
		t.Errorf("ids = %v, want both records", ids)
	}
}

// Types the mapping table does not know are simply unrecognized; a
// quota listing only such types is invisible to everyone.
func TestUnknownTypeNamesAreNotRecognized(t *testing.T) {
	ts, svc := newTestServer(t)
	mustUpsert(t, svc, mailQuota("vendor", "AcmeWidget", "NotARealType"))
	mixed := mustUpsert(t, svc, mailQuota("mixed", "AcmeWidget", "Email"))

	r := callUsing(t, ts, mailUsing, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	ids := getIds(t, argsOf(t, r, 0, "Quota/get"))
	if len(ids) != 1 || ids[0] != string(mixed) {
		t.Fatalf("ids = %v, want only the record with a recognized type", ids)
	}
	list, _ := argsOf(t, r, 0, "Quota/get")["list"].([]any)
	types, _ := list[0].(map[string]any)["types"].([]any)
	if len(types) != 1 || types[0] != "Email" {
		t.Errorf("types = %v, want the unknown name filtered out", types)
	}
}

// A Source that never returns must not hold the caller past its
// deadline, and must commit nothing.
func TestSourceRespectsContextDeadline(t *testing.T) {
	src := &hangingSource{released: make(chan struct{})}
	svc, state := sourceServer(t, src)
	defer close(src.released)
	before := state()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- svc.Refresh(ctx, testAccount) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Refresh succeeded despite the deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Refresh ignored its context deadline")
	}
	if state() != before {
		t.Error("state moved despite the pull never completing")
	}
}

// hangingSource blocks until the test releases it or ctx ends.
type hangingSource struct{ released chan struct{} }

func (s *hangingSource) Quotas(ctx context.Context, _ jmap.Id) ([]Quota, error) {
	select {
	case <-s.released:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Concurrent refreshes of one account must not corrupt the mirror or
// duplicate records.
func TestConcurrentRefresh(t *testing.T) {
	src := &fakeSource{quotas: []Quota{
		{Id: "s1", ResourceType: "octets", HardLimit: 1000, Scope: "account",
			Name: "storage", Types: []string{"Email"}},
		{Id: "s2", ResourceType: "count", HardLimit: 10, Scope: "account",
			Name: "messages", Types: []string{"Email"}},
	}}
	svc, _ := sourceServer(t, src)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.Refresh(context.Background(), testAccount); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Refresh: %v", err)
	}
	if got := listQuotas(t, svc); len(got) != 2 {
		t.Errorf("records = %v, want exactly the two definitions", got)
	}
}

// Concurrent counter bumps must not lose updates: the write path reads
// and writes inside one account update.
func TestConcurrentAddUsed(t *testing.T) {
	_, svc := newTestServer(t)
	ctx := context.Background()
	id := mustUpsert(t, svc, mailQuota("storage", "Email"))
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.AddUsed(ctx, testAccount, id, 10); err != nil {
				t.Errorf("AddUsed: %v", err)
			}
		}()
	}
	wg.Wait()
	obj, err := svc.db.Get(ctx, testAccount, TypeName, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(obj["used"]); got != fmt.Sprint(n*10) {
		t.Errorf("used = %s after %d concurrent bumps, want %d", got, n, n*10)
	}
}

// Writes against a record that is not there fail rather than
// resurrecting it.
func TestCounterOnMissingRecord(t *testing.T) {
	_, svc := newTestServer(t)
	ctx := context.Background()
	if err := svc.AddUsed(ctx, testAccount, "Nsuchthing", 1); err == nil {
		t.Error("AddUsed on a missing record succeeded")
	}
	if err := svc.SetUsed(ctx, testAccount, "Nsuchthing", 1); err == nil {
		t.Error("SetUsed on a missing record succeeded")
	}
	q := mailQuota("ghost", "Email")
	q.Id = "Nsuchthing"
	if _, err := svc.Upsert(ctx, testAccount, q); err == nil {
		t.Error("Upsert against a missing id succeeded")
	}
}

// The internal properties backing the types filtering and the mirror
// must never appear on the wire, and must not be requestable.
func TestInternalPropertiesHidden(t *testing.T) {
	ts, svc := newTestServer(t)
	mustUpsert(t, svc, mailQuota("q", "Email"))
	r := callUsing(t, ts, mailUsing, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	list, _ := argsOf(t, r, 0, "Quota/get")["list"].([]any)
	rec := list[0].(map[string]any)
	for _, name := range []string{typesProperty, sourceIdProperty} {
		if _, present := rec[name]; present {
			t.Errorf("internal property %q reached the wire: %v", name, rec)
		}
	}
	for _, name := range []string{typesProperty, sourceIdProperty} {
		r = callUsing(t, ts, mailUsing, "Quota/get",
			`{"accountId":"Atest1","ids":null,"properties":["`+name+`"]}`)
		if r.MethodResponses[0].Name != "error" {
			t.Errorf("requesting %q returned %s, want an error", name, r.MethodResponses[0].Args)
		}
	}
	// Filtering on them is unsupported too.
	r = callUsing(t, ts, mailUsing, "Quota/query",
		`{"accountId":"Atest1","filter":{"`+sourceIdProperty+`":"s1"}}`)
	if got := argsOf(t, r, 0, "error")["type"]; got != jmap.ErrUnsupportedFilter {
		t.Errorf("error type = %v, want %s", got, jmap.ErrUnsupportedFilter)
	}
}

// A quota whose limits contradict the section 4.1 SHOULD ordering is
// still stored verbatim: the ordering is advice, not a constraint the
// server may silently rewrite.
func TestContradictoryLimitsStoredVerbatim(t *testing.T) {
	_, svc := newTestServer(t)
	q := mailQuota("inverted", "Email")
	q.HardLimit = 10
	q.WarnLimit = u64(900)
	q.SoftLimit = u64(500)
	id := mustUpsert(t, svc, q)
	obj, err := svc.db.Get(context.Background(), testAccount, TypeName, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(obj["warnLimit"]) != "900" || string(obj["softLimit"]) != "500" || string(obj["hardLimit"]) != "10" {
		t.Errorf("record = %v, want the values stored as supplied", obj)
	}
}

// Optional properties absent from a definition are stored as null,
// which is what section 4.1 types them as.
func TestOptionalPropertiesNull(t *testing.T) {
	ts, svc := newTestServer(t)
	mustUpsert(t, svc, mailQuota("bare", "Email"))
	r := callUsing(t, ts, mailUsing, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	list, _ := argsOf(t, r, 0, "Quota/get")["list"].([]any)
	rec := list[0].(map[string]any)
	for _, name := range []string{"warnLimit", "softLimit", "description"} {
		v, present := rec[name]
		if !present || v != nil {
			t.Errorf("%s = %v (present %v), want null", name, v, present)
		}
	}
}
