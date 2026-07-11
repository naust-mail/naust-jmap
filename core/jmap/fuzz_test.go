package jmap

import (
	"encoding/json"
	"errors"
	"testing"
)

// FuzzParseRequest drives arbitrary bytes through the same gate the API
// endpoint uses (RFC 8620 section 3.1/3.6.1: I-JSON check, then Request
// signature check). Invariants: nothing panics; every rejection is a
// classified notRequest; whatever is accepted round-trips through the
// Invocation wire encoding of section 3.2.
func FuzzParseRequest(f *testing.F) {
	f.Add([]byte(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"hi":true},"c1"]]}`))
	f.Add([]byte(`{"using":[],"methodCalls":[]}`))
	f.Add([]byte(`{"methodCalls":[["a",{},"b"]]}`))
	f.Add([]byte(`{"using":["x"],"methodCalls":[["only-two",{}]]}`))
	f.Add([]byte(`{"using":["x"],"methodCalls":[],"createdIds":{"bad id!":"A1"}}`))
	f.Add([]byte(`{"using":["x"],"using":["y"],"methodCalls":[]}`))
	f.Add([]byte(`[1,2,3]`))
	f.Add([]byte(`{"using":["x"],"methodCalls":[["n",{"a":1},"c","extra"]]}`))

	f.Fuzz(func(t *testing.T, body []byte) {
		if CheckIJSON(body) != nil {
			return
		}
		req, err := ParseRequest(body)
		if err != nil {
			if !errors.Is(err, ErrNotRequest) {
				t.Fatalf("unclassified parse error: %v", err)
			}
			return
		}
		for _, call := range req.MethodCalls {
			wire, err := json.Marshal(call)
			if err != nil {
				t.Fatalf("accepted invocation does not marshal: %v", err)
			}
			var back Invocation
			if err := json.Unmarshal(wire, &back); err != nil {
				t.Fatalf("marshaled invocation does not parse: %s: %v", wire, err)
			}
			if back.Name != call.Name || back.CallID != call.CallID {
				t.Fatalf("invocation round trip changed %q/%q to %q/%q",
					call.Name, call.CallID, back.Name, back.CallID)
			}
		}
		for id := range req.CreatedIds {
			if !id.Valid() {
				t.Fatalf("accepted invalid creation id %q", id)
			}
		}
	})
}
