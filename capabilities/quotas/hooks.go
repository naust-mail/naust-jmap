package quotas

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// recognizedTypes returns the stored types list filtered to the
// entries whose capability the current request opted in to (RFC 9425
// section 4.1: the server MUST filter out any types for which the
// client did not request the associated capability in "using").
// Outside request processing there is no "using" set and nothing is
// recognized. Order of the stored list is preserved.
func recognizedTypes(ctx context.Context, typeCaps map[string]string, obj objectdb.Object) []string {
	var all []string
	if raw, has := obj[typesProperty]; has {
		// The stored list was validated at write time; a decode failure
		// leaves it empty, which hides the record rather than leaking it.
		_ = json.Unmarshal(raw, &all)
	}
	using := runtime.RequestCapabilities(ctx)
	out := make([]string, 0, len(all))
	for _, t := range all {
		if cap, known := typeCaps[t]; known && using[cap] {
			out = append(out, t)
		}
	}
	return out
}

// quotaComputed resolves the wire-visible "types" property from the
// stored full list, filtered per request (RFC 9425 section 4.1).
type quotaComputed struct {
	typeCaps map[string]string
}

func (quotaComputed) Accepts(name string) bool { return name == "types" }

func (c quotaComputed) Resolve(ctx context.Context, _ jmap.Id, stored objectdb.Object, names []string, _ map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(names))
	for _, name := range names {
		if name != "types" {
			continue
		}
		raw, err := json.Marshal(recognizedTypes(ctx, c.typeCaps, stored))
		if err != nil {
			return nil, err
		}
		out[name] = raw
	}
	return out, nil
}

// quotaChangesExtra adds updatedProperties to Quota/changes (RFC 9425
// section 4.3): a list containing only "used" when every change in the
// window was an update touching nothing else, null when the server
// cannot tell - so clients refetch just the counter on the frequent
// usage-only change.
func quotaChangesExtra(_ context.Context, _ jmap.Id, view *runtime.ChangesView, _ map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{"updatedProperties": json.RawMessage("null")}
	// An empty changed-property set is not evidence that only "used"
	// moved: internal properties are stripped from the change record, so
	// a commit that touched nothing else visible still arrives empty.
	// Claiming ["used"] there would tell a client to refetch the counter
	// and keep a stale copy of whatever really changed.
	if len(view.Updated) == 0 || len(view.Created) > 0 || len(view.Destroyed) > 0 || len(view.UpdatedProps) == 0 {
		return out, nil
	}
	for _, p := range view.UpdatedProps {
		if p != "used" {
			return out, nil
		}
	}
	out["updatedProperties"] = json.RawMessage(`["used"]`)
	return out, nil
}

// quotaFilter is the Quota FilterCondition language (RFC 9425 section
// 4.4): name contains, scope exact, resourceType exact, type
// membership. A condition object matches when every given condition
// does; zero conditions match every object.
type quotaFilter struct {
	typeCaps map[string]string
}

func (quotaFilter) ValidateCondition(name string, value json.RawMessage) error {
	switch name {
	case "name", "scope", "resourceType", "type":
		if _, ok := decodeString(value); !ok {
			return fmt.Errorf("%s must be a String", name)
		}
	default:
		return runtime.UnsupportedFilterError{Description: fmt.Sprintf("cannot filter on %q", name)}
	}
	return nil
}

// MatchCondition needs no I/O: every Quota condition reads the record
// in hand. ctx carries the request's "using" set for the type
// condition, which matches against the client-visible types list -
// the section 4.1 filtered view, never the raw stored one.
func (f quotaFilter) MatchCondition(ctx context.Context, _ jmap.Id, obj objectdb.Object, name string, value json.RawMessage) (bool, error) {
	want, _ := decodeString(value)
	switch name {
	case "name":
		got, _ := decodeString(obj["name"])
		return strings.Contains(got, want), nil
	case "scope", "resourceType":
		got, ok := decodeString(obj[name])
		return ok && got == want, nil
	case "type":
		for _, t := range recognizedTypes(ctx, f.typeCaps, obj) {
			if t == want {
				return true, nil
			}
		}
		return false, nil
	}
	return false, nil
}

// decodeString unmarshals a JSON string, reporting failure instead of
// comparing raw bytes: two encodings of one string must compare equal.
// null is not a string and is reported as such - unmarshaling null
// into a string succeeds silently in Go, which would otherwise let a
// null filter value pass as the empty string and match everything.
func decodeString(raw json.RawMessage) (string, bool) {
	var s string
	if raw == nil || isNullRaw(raw) || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// isNullRaw reports whether raw is the JSON null literal, ignoring the
// insignificant whitespace a client may pad it with.
func isNullRaw(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}
