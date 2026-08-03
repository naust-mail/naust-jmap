package quotas

// The definition storage paths: the Source socket and its mirror
// discipline, hand-written records, and the used counter.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// fakeSource is an embedder's quota rules, swappable between pulls.
// Refresh may be called concurrently, so the fixture guards its own
// state the way a real Source implementation would have to.
type fakeSource struct {
	mu     sync.Mutex
	quotas []Quota
	err    error
	calls  int
}

func (s *fakeSource) Quotas(ctx context.Context, _ jmap.Id) ([]Quota, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.quotas, nil
}

// callCount reports how many pulls the fixture has served.
func (s *fakeSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// setQuotas replaces the definitions a later pull will return.
func (s *fakeSource) setQuotas(qs []Quota) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotas = qs
}

// sourceServer wires a server whose definitions come from src.
func sourceServer(t *testing.T, src Source) (*Service, func() string) {
	t.Helper()
	_, svc := newTestServerConfig(t, Config{Source: src, TypeCapabilities: rfcTypeCapabilities()})
	state := func() string {
		s, err := svc.db.TypeState(context.Background(), testAccount, TypeName)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	return svc, state
}

// listQuotas returns the stored records by their source id, for
// assertions that do not go through the wire.
func listQuotas(t *testing.T, svc *Service) map[string]jmap.Id {
	t.Helper()
	ctx := context.Background()
	ids, err := svc.db.AllIds(ctx, testAccount, TypeName, 0)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := svc.db.GetMany(ctx, testAccount, TypeName, ids)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]jmap.Id, len(objs))
	for i, obj := range objs {
		sid, _ := decodeString(obj[sourceIdProperty])
		out[sid] = ids[i]
	}
	return out
}

func TestRefreshWithoutSourceErrors(t *testing.T) {
	_, svc := newTestServerConfig(t, Config{})
	err := svc.Refresh(context.Background(), testAccount)
	if err == nil {
		t.Fatal("Refresh without a Source succeeded, want an error")
	}
	if got := err.Error(); got != "quotas: Refresh: no Source configured" {
		t.Errorf("error = %q", got)
	}
}

func TestRefreshMirrorsDefinitions(t *testing.T) {
	src := &fakeSource{quotas: []Quota{
		{Id: "tier-storage", ResourceType: "octets", HardLimit: 20 << 30,
			Scope: "account", Name: "storage", Types: []string{"Email"}},
		{Id: "tier-messages", ResourceType: "count", HardLimit: 50000,
			Scope: "account", Name: "messages", Types: []string{"Email"}},
	}}
	svc, _ := sourceServer(t, src)
	if err := svc.Refresh(context.Background(), testAccount); err != nil {
		t.Fatal(err)
	}
	got := listQuotas(t, svc)
	if len(got) != 2 || got["tier-storage"] == "" || got["tier-messages"] == "" {
		t.Fatalf("records = %v, want both source ids mirrored", got)
	}
}

// An unchanged pull must commit nothing: state strings move only on a
// real change, so /changes and push stay quiet.
func TestRefreshUnchangedLeavesStateAlone(t *testing.T) {
	src := &fakeSource{quotas: []Quota{
		{Id: "s1", ResourceType: "octets", HardLimit: 1000, Scope: "account",
			Name: "storage", Types: []string{"Email"}},
	}}
	svc, state := sourceServer(t, src)
	ctx := context.Background()
	if err := svc.Refresh(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	after := state()
	for i := 0; i < 3; i++ {
		if err := svc.Refresh(ctx, testAccount); err != nil {
			t.Fatal(err)
		}
	}
	if state() != after {
		t.Errorf("state moved from %s to %s across no-op refreshes", after, state())
	}
	if got := src.callCount(); got != 4 {
		t.Errorf("Source called %d times, want 4", got)
	}
}

func TestRefreshAppliesChangesAndRemovals(t *testing.T) {
	src := &fakeSource{quotas: []Quota{
		{Id: "s1", ResourceType: "octets", HardLimit: 1000, Scope: "account",
			Name: "storage", Types: []string{"Email"}},
		{Id: "s2", ResourceType: "count", HardLimit: 10, Scope: "account",
			Name: "messages", Types: []string{"Email"}},
	}}
	svc, state := sourceServer(t, src)
	ctx := context.Background()
	if err := svc.Refresh(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	before := state()
	first := listQuotas(t, svc)

	// The embedder moves the account to a bigger tier and retires s2.
	src.setQuotas([]Quota{
		{Id: "s1", ResourceType: "octets", HardLimit: 5000, Scope: "account",
			Name: "storage", Types: []string{"Email"}},
	})
	if err := svc.Refresh(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	if state() == before {
		t.Error("state did not move after a real definition change")
	}
	got := listQuotas(t, svc)
	if len(got) != 1 {
		t.Fatalf("records = %v, want only s1", got)
	}
	// The surviving record keeps its identity: a limit change is an
	// update, never a destroy and recreate, so client caches and the
	// used counter survive.
	if got["s1"] != first["s1"] {
		t.Errorf("s1 id changed from %s to %s", first["s1"], got["s1"])
	}
	obj, err := svc.db.Get(ctx, testAccount, TypeName, got["s1"])
	if err != nil {
		t.Fatal(err)
	}
	if string(obj["hardLimit"]) != "5000" {
		t.Errorf("hardLimit = %s, want 5000", obj["hardLimit"])
	}
}

// Refresh manages exactly the records carrying a source id;
// hand-written ones coexist untouched.
func TestRefreshLeavesHandWrittenRecords(t *testing.T) {
	src := &fakeSource{quotas: []Quota{
		{Id: "s1", ResourceType: "octets", HardLimit: 1000, Scope: "account",
			Name: "from-source", Types: []string{"Email"}},
	}}
	svc, _ := sourceServer(t, src)
	ctx := context.Background()
	manual := mustUpsert(t, svc, mailQuota("hand-written", "Email"))

	if err := svc.Refresh(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Get(ctx, testAccount, TypeName, manual); err != nil {
		t.Fatalf("hand-written record destroyed by Refresh: %v", err)
	}
	// And a second refresh whose source has emptied still spares it.
	src.setQuotas(nil)
	if err := svc.Refresh(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Get(ctx, testAccount, TypeName, manual); err != nil {
		t.Fatalf("hand-written record destroyed by an emptying Refresh: %v", err)
	}
	if got := listQuotas(t, svc); len(got) != 1 {
		t.Errorf("records = %v, want only the hand-written one", got)
	}
}

// The used counter is server-computed: a definition pull never carries
// it, so refreshes cannot stomp it.
func TestRefreshPreservesUsed(t *testing.T) {
	src := &fakeSource{quotas: []Quota{
		{Id: "s1", ResourceType: "octets", HardLimit: 1000, Scope: "account",
			Name: "storage", Types: []string{"Email"}},
	}}
	svc, _ := sourceServer(t, src)
	ctx := context.Background()
	if err := svc.Refresh(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	id := listQuotas(t, svc)["s1"]
	if err := svc.SetUsed(ctx, testAccount, id, 640); err != nil {
		t.Fatal(err)
	}
	bigger := src.quotas[0]
	bigger.HardLimit = 2000
	src.setQuotas([]Quota{bigger})
	if err := svc.Refresh(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	obj, err := svc.db.Get(ctx, testAccount, TypeName, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(obj["used"]) != "640" {
		t.Errorf("used = %s after a definition refresh, want 640", obj["used"])
	}
}

func TestUpsertUpdatesInPlace(t *testing.T) {
	_, svc := newTestServer(t)
	ctx := context.Background()
	id := mustUpsert(t, svc, mailQuota("original", "Email"))
	if err := svc.SetUsed(ctx, testAccount, id, 77); err != nil {
		t.Fatal(err)
	}
	q := mailQuota("renamed", "Email")
	q.Id = string(id)
	q.HardLimit = 9000
	again, err := svc.Upsert(ctx, testAccount, q)
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Errorf("Upsert minted a new id %s, want %s", again, id)
	}
	obj, err := svc.db.Get(ctx, testAccount, TypeName, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(obj["name"]) != `"renamed"` || string(obj["hardLimit"]) != "9000" {
		t.Errorf("record = %v, want the updated definition", obj)
	}
	if string(obj["used"]) != "77" {
		t.Errorf("used = %s, want the counter preserved at 77", obj["used"])
	}
}

func TestDelete(t *testing.T) {
	_, svc := newTestServer(t)
	ctx := context.Background()
	id := mustUpsert(t, svc, mailQuota("doomed", "Email"))
	if err := svc.Delete(ctx, testAccount, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Get(ctx, testAccount, TypeName, id); err == nil {
		t.Error("record survived Delete")
	}
}

func TestUsedCounter(t *testing.T) {
	_, svc := newTestServer(t)
	ctx := context.Background()
	id := mustUpsert(t, svc, mailQuota("storage", "Email"))

	used := func() string {
		obj, err := svc.db.Get(ctx, testAccount, TypeName, id)
		if err != nil {
			t.Fatal(err)
		}
		return string(obj["used"])
	}
	if used() != "0" {
		t.Errorf("used = %s on a new record, want 0", used())
	}
	if err := svc.AddUsed(ctx, testAccount, id, 1500); err != nil {
		t.Fatal(err)
	}
	if used() != "1500" {
		t.Errorf("used = %s after +1500, want 1500", used())
	}
	if err := svc.AddUsed(ctx, testAccount, id, -500); err != nil {
		t.Fatal(err)
	}
	if used() != "1000" {
		t.Errorf("used = %s after -500, want 1000", used())
	}
	if err := svc.SetUsed(ctx, testAccount, id, 42); err != nil {
		t.Fatal(err)
	}
	if used() != "42" {
		t.Errorf("used = %s after SetUsed(42), want 42", used())
	}
}

// A delta below zero clamps: used is an UnsignedInt (RFC 9425 section
// 4.1), and the reconcile path is what restores the true figure.
func TestAddUsedClampsAtZero(t *testing.T) {
	_, svc := newTestServer(t)
	ctx := context.Background()
	id := mustUpsert(t, svc, mailQuota("storage", "Email"))
	if err := svc.AddUsed(ctx, testAccount, id, 100); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddUsed(ctx, testAccount, id, -5000); err != nil {
		t.Fatalf("underflow returned an error: %v", err)
	}
	obj, err := svc.db.Get(ctx, testAccount, TypeName, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(obj["used"]) != "0" {
		t.Errorf("used = %s, want 0", obj["used"])
	}
}

// A counter bump changes only used, which is what lets Quota/changes
// report updatedProperties ["used"] (RFC 9425 section 4.3).
func TestCounterBumpReportsUsedOnly(t *testing.T) {
	ts, svc := newTestServer(t)
	ctx := context.Background()
	id := mustUpsert(t, svc, mailQuota("storage", "Email"))
	r := callUsing(t, ts, mailUsing, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	since := argsOf(t, r, 0, "Quota/get")["state"].(string)

	if err := svc.AddUsed(ctx, testAccount, id, 900); err != nil {
		t.Fatal(err)
	}
	r = callUsing(t, ts, mailUsing, "Quota/changes",
		`{"accountId":"Atest1","sinceState":"`+since+`"}`)
	args := argsOf(t, r, 0, "Quota/changes")
	up, _ := args["updatedProperties"].([]any)
	if len(up) != 1 || up[0] != "used" {
		t.Errorf("updatedProperties = %v, want [used]", up)
	}
}

// A definition change touches more than used, so the server cannot
// promise a usage-only delta and MUST answer null (section 4.3).
func TestDefinitionChangeReportsNullUpdatedProperties(t *testing.T) {
	ts, svc := newTestServer(t)
	ctx := context.Background()
	id := mustUpsert(t, svc, mailQuota("storage", "Email"))
	r := callUsing(t, ts, mailUsing, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	since := argsOf(t, r, 0, "Quota/get")["state"].(string)

	q := mailQuota("storage", "Email")
	q.Id = string(id)
	q.HardLimit = 99999
	if _, err := svc.Upsert(ctx, testAccount, q); err != nil {
		t.Fatal(err)
	}
	r = callUsing(t, ts, mailUsing, "Quota/changes",
		`{"accountId":"Atest1","sinceState":"`+since+`"}`)
	args := argsOf(t, r, 0, "Quota/changes")
	if args["updatedProperties"] != nil {
		t.Errorf("updatedProperties = %v, want null", args["updatedProperties"])
	}
}

// A create in the window also forces null: the client cannot refetch a
// new record by property list alone.
func TestCreateReportsNullUpdatedProperties(t *testing.T) {
	ts, svc := newTestServer(t)
	mustUpsert(t, svc, mailQuota("first", "Email"))
	r := callUsing(t, ts, mailUsing, "Quota/get", `{"accountId":"Atest1","ids":null}`)
	since := argsOf(t, r, 0, "Quota/get")["state"].(string)

	mustUpsert(t, svc, mailQuota("second", "Email"))
	r = callUsing(t, ts, mailUsing, "Quota/changes",
		`{"accountId":"Atest1","sinceState":"`+since+`"}`)
	args := argsOf(t, r, 0, "Quota/changes")
	if args["updatedProperties"] != nil {
		t.Errorf("updatedProperties = %v, want null", args["updatedProperties"])
	}
}

func TestSourceErrorPropagates(t *testing.T) {
	sentinel := errors.New("tier service unreachable")
	src := &fakeSource{err: sentinel}
	svc, state := sourceServer(t, src)
	before := state()
	err := svc.Refresh(context.Background(), testAccount)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap %v", err, sentinel)
	}
	if state() != before {
		t.Error("state moved despite the pull failing")
	}
}
