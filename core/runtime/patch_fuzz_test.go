package runtime

import (
	"encoding/json"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// FuzzApplyPatch throws arbitrary PatchObjects at a well-formed record.
// Invariants from RFC 8620 section 5.3: nothing panics; the outcome is
// exactly one of a patched object or a SetError of a known type; a
// successful patch never changes id, immutable, or server-set values,
// never stores an undeclared property, and every stored value still
// satisfies its declared kind.
func FuzzApplyPatch(f *testing.F) {
	typ := testNoteType()
	current := objectdb.Object{
		"id":       json.RawMessage(`"A1"`),
		"subject":  json.RawMessage(`"hello"`),
		"body":     json.RawMessage(`"text"`),
		"kind":     json.RawMessage(`"memo"`),
		"revision": json.RawMessage(`1`),
		"flagged":  json.RawMessage(`false`),
	}

	f.Add([]byte(`{"subject":"new"}`))
	f.Add([]byte(`{"body":null,"flagged":true}`))
	f.Add([]byte(`{"subject":"a","subject/x":"b"}`))
	f.Add([]byte(`{"subject/deep":"x"}`))
	f.Add([]byte(`{"a~1b":1,"a~0b":2}`))
	f.Add([]byte(`{"id":"A1","kind":"memo","revision":1}`))
	f.Add([]byte(`{"id":"other"}`))
	f.Add([]byte(`{"revision":null}`))
	f.Add([]byte(`{"parentId":"#c1"}`))
	f.Add([]byte(`{"":"empty pointer"}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var patch map[string]json.RawMessage
		if json.Unmarshal(data, &patch) != nil {
			return
		}
		obj, serr := applyPatch(typ, current, patch, nil)
		if (obj == nil) == (serr == nil) {
			t.Fatalf("patch %s: want exactly one of object/error, got obj=%v err=%v", data, obj, serr)
		}
		if serr != nil {
			if serr.Type != jmap.SetErrInvalidPatch && serr.Type != jmap.SetErrInvalidProperties {
				t.Fatalf("patch %s: unexpected SetError type %q", data, serr.Type)
			}
			return
		}
		for _, fixedProp := range []string{"id", "kind", "revision"} {
			if !jsonEqual(obj[fixedProp], current[fixedProp]) {
				t.Fatalf("patch %s: changed %s from %s to %s",
					data, fixedProp, current[fixedProp], obj[fixedProp])
			}
		}
		for name, v := range obj {
			if name == "id" {
				continue
			}
			p, declared := typ.Properties[name]
			if !declared {
				t.Fatalf("patch %s: stored undeclared property %q", data, name)
			}
			if err := p.CheckValue(v); err != nil {
				t.Fatalf("patch %s: stored invalid value for %q: %v", data, name, err)
			}
		}
	})
}
