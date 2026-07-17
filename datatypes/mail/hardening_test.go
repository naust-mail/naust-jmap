package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
)

// countingStore wraps a blob.Store and counts Open calls.
type countingStore struct {
	blob.Store
	opens int
}

func (c *countingStore) Open(ctx context.Context, acct, blobID jmap.Id) (io.ReadCloser, int64, error) {
	c.opens++
	return c.Store.Open(ctx, acct, blobID)
}

// TestBodyPropertiesCap: a bodyProperties list longer than
// maxBodyProperties is invalidArguments; a list at the cap is accepted.
func TestBodyPropertiesCap(t *testing.T) {
	build := func(n int) map[string]json.RawMessage {
		entries := make([]string, n)
		for i := range entries {
			entries[i] = fmt.Sprintf(`"header:X-%d"`, i)
		}
		raw := json.RawMessage("[" + strings.Join(entries, ",") + "]")
		return map[string]json.RawMessage{"bodyProperties": raw}
	}

	if err := checkEmailGetArgs(build(maxBodyProperties)); err != nil {
		t.Fatalf("list at the cap rejected: %v", err)
	}
	if err := checkEmailGetArgs(build(maxBodyProperties + 1)); err == nil {
		t.Fatal("oversized bodyProperties accepted")
	}
}

// TestCompileBodyPropsDedup: duplicate names collapse and each header
// form is parsed once (no duplicate entries in the compiled plan).
func TestCompileBodyPropsDedup(t *testing.T) {
	plan := compileBodyProps([]string{"type", "type", "header:X-Foo", "header:X-Foo", "size"})
	if got := len(plan.standard); got != 2 { // type, size
		t.Fatalf("standard props: want 2, got %d (%v)", got, plan.standard)
	}
	if got := len(plan.headerProps); got != 1 {
		t.Fatalf("header props: want 1, got %d", got)
	}
	if plan.headerProps[0].name != "header:X-Foo" {
		t.Fatalf("header prop name: %q", plan.headerProps[0].name)
	}
}

// TestThreadKeysNotExposed: the internal threadKeys index that threading
// uses is never visible on the Email/get wire, and cannot be requested.
func TestThreadKeysNotExposed(t *testing.T) {
	ts, db, store := emailServer(t)
	id := putEmail(t, db, store, simpleMessage, map[string]bool{"MBinbox": true}, nil)

	// It never appears in a normal (default-property) response.
	if _, has := emailGet(t, ts, id, "")["threadKeys"]; has {
		t.Fatal("threadKeys leaked into Email/get response")
	}

	// Explicitly requesting it is invalidArguments: to a client it does
	// not exist.
	r := callMail(t, ts, inv("Email/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q],"properties":["threadKeys"]}`, testAccount, id), "0"))
	if r.MethodResponses[0].Name != "error" {
		t.Fatalf("requesting threadKeys should error, got %s", r.MethodResponses[0].Name)
	}
	if got := methodArgs(t, r, 0, "error")["type"]; got != "invalidArguments" {
		t.Fatalf("want invalidArguments, got %v", got)
	}
}

// TestSearcherParseCachePerRecord: within one record's per-record scope the
// blob is read a fixed number of times however many text conditions the filter
// has - once for the structure, once per distinct set of terms searched for -
// and without a scope every condition pays afresh. Both conditions here search
// for the same term, so they share one body scan.
func TestSearcherParseCachePerRecord(t *testing.T) {
	cs := &countingStore{Store: kvstore.New(memory.New())}
	raw := []byte(simpleMessage)
	blobID := blob.IdFor(raw)
	if err := cs.Put(context.Background(), testAccount, blobID, raw); err != nil {
		t.Fatal(err)
	}
	s := naiveSearcher{store: cs}
	obj := objectdb.Object{"blobId": mustJSON(blobID)}

	ctx := emailFilter{}.EnterRecord(context.Background())
	for _, f := range []string{"body", "text"} {
		if _, err := s.Match(ctx, testAccount, obj, f, json.RawMessage(`"free"`)); err != nil {
			t.Fatal(err)
		}
	}
	if cs.opens != 2 {
		t.Fatalf("per-record cache: want 2 blob opens (structure, body scan), got %d", cs.opens)
	}

	cs.opens = 0
	for _, f := range []string{"body", "text"} {
		if _, err := s.Match(context.Background(), testAccount, obj, f, json.RawMessage(`"free"`)); err != nil {
			t.Fatal(err)
		}
	}
	if cs.opens != 4 {
		t.Fatalf("no scope: want 4 blob opens, got %d", cs.opens)
	}
}

// TestEmailParsePropertiesCap: Email/parse with a properties list over the
// cap is invalidArguments.
func TestEmailParsePropertiesCap(t *testing.T) {
	ts, _, _ := emailServer(t)
	var b strings.Builder
	fmt.Fprintf(&b, `{"accountId":%q,"blobIds":[],"properties":[`, testAccount)
	for i := 0; i <= maxParseProperties; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"header:X-%d"`, i)
	}
	b.WriteString(`]}`)
	r := callMail(t, ts, inv("Email/parse", b.String(), "0"))
	if r.MethodResponses[0].Name != "error" {
		t.Fatalf("oversized properties should error, got %s", r.MethodResponses[0].Name)
	}
	if got := methodArgs(t, r, 0, "error")["type"]; got != "invalidArguments" {
		t.Fatalf("want invalidArguments, got %v", got)
	}
}
