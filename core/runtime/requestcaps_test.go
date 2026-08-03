package runtime

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// RequestCapabilities exposes the request's "using" set (RFC 8620
// section 3.3) to method handlers and type hooks through the context.

func TestRequestCapabilitiesInHandler(t *testing.T) {
	p := NewProcessor()
	p.Register("Probe/caps", jmap.CoreCapability, func(ctx context.Context, call *Call) []jmap.Invocation {
		caps := RequestCapabilities(ctx)
		names := make([]string, 0, len(caps))
		for c := range caps {
			names = append(names, c)
		}
		sort.Strings(names)
		args, _ := json.Marshal(map[string][]string{"caps": names})
		return []jmap.Invocation{{Name: "Probe/caps", Args: args, CallID: call.CallID}}
	})
	req, err := jmap.ParseRequest([]byte(`{
		"using": ["urn:ietf:params:jmap:core"],
		"methodCalls": [["Probe/caps", {}, "c1"]]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	resp := p.Process(context.Background(), req, nil, "s")
	var out struct {
		Caps []string `json:"caps"`
	}
	if err := json.Unmarshal(resp.MethodResponses[0].Args, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Caps) != 1 || out.Caps[0] != jmap.CoreCapability {
		t.Errorf("handler saw caps %v, want [%s]", out.Caps, jmap.CoreCapability)
	}
}

func TestRequestCapabilitiesNilOutsideRequest(t *testing.T) {
	if caps := RequestCapabilities(context.Background()); caps != nil {
		t.Errorf("outside request processing got %v, want nil", caps)
	}
}

// capsCapturingComputed records the using set visible during a derived
// /get's computed-property resolution.
type capsCapturingComputed struct {
	seen map[string]bool
}

func (c *capsCapturingComputed) Accepts(name string) bool { return name == "shadow" }

func (c *capsCapturingComputed) Resolve(ctx context.Context, _ jmap.Id, stored objectdb.Object, names []string, _ map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	c.seen = RequestCapabilities(ctx)
	out := make(map[string]json.RawMessage, len(names))
	for _, name := range names {
		if name == "shadow" {
			if v, has := stored["subject"]; has {
				out["shadow"] = v
			}
		}
	}
	return out, nil
}

// capsCapturingFilter records the using set visible during a derived
// /query's condition matching.
type capsCapturingFilter struct {
	seen map[string]bool
}

func (f *capsCapturingFilter) ValidateCondition(name string, _ json.RawMessage) error {
	if name != "probe" {
		return UnsupportedFilterError{Description: "unknown condition " + name}
	}
	return nil
}

func (f *capsCapturingFilter) MatchCondition(ctx context.Context, _ jmap.Id, _ objectdb.Object, _ string, _ json.RawMessage) (bool, error) {
	f.seen = RequestCapabilities(ctx)
	return true, nil
}

func TestRequestCapabilitiesInDerivedHooks(t *testing.T) {
	comp := &capsCapturingComputed{}
	filt := &capsCapturingFilter{}
	ts := gadgetServer(t, &Extensions{
		Computed: comp,
		Query:    &QueryHooks{Filter: filt},
	})
	id := createGadget(t, ts, `{"subject":"s1"}`)

	callGadget(t, ts, inv("Gadget/get",
		`{"accountId":"Atest1","ids":["`+id+`"],"properties":["shadow"]}`, "0"))
	if !comp.seen[jmap.CoreCapability] || !comp.seen["urn:example:gadget"] {
		t.Errorf("Computed.Resolve saw caps %v, want core and gadget", comp.seen)
	}

	callGadget(t, ts, inv("Gadget/query",
		`{"accountId":"Atest1","filter":{"probe":true}}`, "0"))
	if !filt.seen[jmap.CoreCapability] || !filt.seen["urn:example:gadget"] {
		t.Errorf("MatchCondition saw caps %v, want core and gadget", filt.seen)
	}
}
