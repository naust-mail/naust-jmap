package jmap

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// TestResponseConstructionSitesAreAudited is a deliberate tripwire, not a
// behavioral test: it lists every place this module builds a response
// Invocation's Args, so WriteJSON's "Args is always CompactJSON by the time
// it reaches a response" assumption stays a checked claim, not a stale
// comment. If a new construction site is added elsewhere and this list isn't
// updated, that is a reviewer's problem to catch - this test only proves the
// ones known about here still go through MarshalCompactJSON.
func TestResponseConstructionSitesAreAudited(t *testing.T) {
	// 1. ErrorInvocation (errors.go): always MarshalCompactJSON(e), or the
	//    hardcoded-valid literal fallback.
	inv := ErrorInvocation("c", MethodError{Type: ErrServerFail})
	if !json.Valid(inv.Args) {
		t.Errorf("ErrorInvocation produced invalid JSON: %s", inv.Args)
	}
	// 2. core/runtime/standard.go's reply() and core/runtime/processor.go's
	//    echo() are audited by inspection (both call MarshalCompactJSON) and
	//    exercised end-to-end by core/runtime's own test suite; they cannot
	//    be called from this package without an import cycle.
}

// FuzzResponseWriteJSON differentially tests Response.AppendJSON against
// json.Marshal (Response has no custom MarshalJSON, so this is still the
// plain reflection-based encoder - the same one the codebase relied on
// before WriteJSON existed) across arbitrary field content. CallID gets the
// deepest coverage: unlike Args (always CompactJSON by construction - see
// TestResponseConstructionSitesAreAudited) and Name (a fixed method-dispatch
// string), CallID is the one field carrying the client's own unaudited
// string straight into a response.
func FuzzResponseWriteJSON(f *testing.F) {
	seeds := []struct {
		name, args, callID, session string
		createdIDKey, createdIDVal  string
	}{
		{"", "", "", "", "", ""},
		{"Email/get", `{"a":1}`, "c1", "s1", "", ""},
		{"m", `{"a":"quote\""}`, `has"quote`, "s", "", ""},
		{"m", `{}`, "has\nnewline", "s", "", ""},
		{"m", `{}`, "has\x00null", "s", "", ""},
		{"m", `{}`, "has�replacement", "s", "", ""},
		{"m", `{}`, "has\\backslash", "s", "", ""},
		{"m", `{}`, "has</script>html", "s", "", ""},
		{"m", `{}`, strings.Repeat("x", 10000), "s", "", ""},
		{"m", `{}`, "unicodeé中\U0001F600", "s", "", ""},
		{"m", "", "c", "s", "", ""},         // empty Args: must default to "{}"
		{"m", "not json", "c", "s", "", ""}, // caught, replaced with {} below
		{"m", `{}`, "c", "s", "id1", "new1"},
		{"m", `{}`, "c", "s", "id with \"quote", "new"},
	}
	for _, s := range seeds {
		f.Add(s.name, s.args, s.callID, s.session, s.createdIDKey, s.createdIDVal)
	}
	f.Fuzz(func(t *testing.T, name, args, callID, session, createdIDKey, createdIDVal string) {
		// AppendJSON's documented precondition is that Args is CompactJSON
		// representing a JMAP method-args object: every real construction
		// site (reply()'s marshaled structs, ErrorInvocation's MethodError,
		// echo()'s call.Args, compacted via MarshalCompactJSON before it ever
		// reaches an Invocation) satisfies "already-compact JSON object", not
		// just "valid JSON" (a bare number is valid JSON but breaks
		// Invocation's decode contract) and not just "starts with { " (a
		// valid but non-compact object, like "{ }", is valid JSON breaking a
		// *different* part of the contract - the "already compact" part).
		// Both gaps were found by this fuzz target before this normalization
		// existed; neither was a bug in AppendJSON, which matched
		// json.Marshal(resp) byte for byte in both cases - only this
		// harness's simulated Args wasn't representative of what a real
		// construction site produces. So: actually compact fuzzed args
		// through the same function real callers use, exactly like a real
		// construction site would, rather than approximating its shape.
		// Malformed/non-object Args on their own are covered separately by
		// TestMalformedArgsNeverPanics.
		raw := json.RawMessage(`{}`)
		if compacted, err := MarshalCompactJSON(json.RawMessage(args)); err == nil && len(compacted) > 0 && compacted[0] == '{' {
			raw = json.RawMessage(compacted)
		}
		resp := &Response{
			MethodResponses: []Invocation{{Name: name, Args: raw, CallID: callID}},
			SessionState:    session,
		}
		if createdIDKey != "" && utf8.ValidString(createdIDKey) && Id(createdIDKey).Valid() {
			resp.CreatedIds = map[Id]Id{Id(createdIDKey): Id(createdIDVal)}
		}

		want, err := json.Marshal(resp)
		if err != nil {
			t.Skipf("reference json.Marshal itself failed: %v", err)
		}
		got, err := resp.AppendJSON(nil)
		if err != nil {
			t.Fatalf("AppendJSON error: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("mismatch\n got:  %s\nwant: %s", got, want)
		}

		// Must also round-trip through the real decoder, not just byte-match.
		var back Response
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatalf("AppendJSON output does not parse: %v\noutput: %s", err, got)
		}

		// WriteJSON must match AppendJSON plus exactly one trailing newline
		// (json.Encoder.Encode's convention - the contract server.go's
		// caller relies on for wire compatibility).
		var buf strings.Builder
		if err := resp.WriteJSON(&buf); err != nil {
			t.Fatalf("WriteJSON error: %v", err)
		}
		if buf.String() != string(got)+"\n" {
			t.Fatalf("WriteJSON does not match AppendJSON+newline\n got:  %q\nwant: %q", buf.String(), string(got)+"\n")
		}
	})
}

// TestMalformedArgsNeverPanics is deliberate defense in depth: every
// construction site in this codebase is audited to only ever produce
// CompactJSON for Args (TestResponseConstructionSitesAreAudited), so this
// input is never supposed to reach AppendJSON in practice. The point of this
// test is what happens if that invariant is ever violated by a future bug -
// AppendJSON must not panic, hang, or otherwise misbehave in a way worse than
// "the output may not be valid JSON", regardless of what bytes end up in
// Args.
func TestMalformedArgsNeverPanics(t *testing.T) {
	adversarial := [][]byte{
		nil,
		{},
		[]byte("{"),
		[]byte("}"),
		[]byte(`{"unterminated`),
		[]byte(`{{{{{{{{{{`),
		[]byte(strings.Repeat("[", 100000)),
		[]byte(strings.Repeat("}", 100000)),
		{0x00, 0x01, 0x02, 0xff, 0xfe},
		[]byte(strings.Repeat("a", 5_000_000)), // large but not size-limit-worthy on its own
		[]byte("\xed\xa0\x80"),                 // raw CESU-8 surrogate half: invalid UTF-8
	}
	for i, args := range adversarial {
		t.Run(t.Name()+"_"+string(rune('a'+i)), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("AppendJSON panicked on adversarial Args (case %d, %q): %v", i, truncate(args), r)
				}
			}()
			resp := &Response{
				MethodResponses: []Invocation{{Name: "m", Args: args, CallID: "c"}},
				SessionState:    "s",
			}
			out, err := resp.AppendJSON(nil)
			if err != nil {
				return // an error is an acceptable outcome; a panic or hang is not
			}
			// Output length must be linear in input length - no quadratic
			// blowup from adversarial content (a cheap DoS-shape check).
			if len(out) > 10*len(args)+1024 {
				t.Errorf("case %d: output %d bytes for %d bytes of input - possible blowup", i, len(out), len(args))
			}
		})
	}
}

func truncate(b []byte) []byte {
	if len(b) > 64 {
		return b[:64]
	}
	return b
}

// TestWriteJSONConcurrentNoContamination is specifically for responseBufPool:
// a pooled-buffer bug would not necessarily be a data race -race can catch
// (the bug can be a pure logic error - handing out a buffer another
// goroutine is still writing into, purely through the pool's Get/Put timing)
// - it would surface as one request's response bytes containing another
// request's content. Every goroutine's Response carries a unique marker in
// both Args and CallID (the two fields WriteJSON writes through the pooled
// buffer), so contamination corrupts a marker in a way string comparison
// catches; each iteration is checked against AppendJSON's un-pooled output
// as ground truth, not just checked for internal consistency.
func TestWriteJSONConcurrentNoContamination(t *testing.T) {
	const goroutines = 64
	const itersPerGoroutine = 500

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			marker := fmt.Sprintf("goroutine-%d-marker-%x", g, g*7919+104729)
			resp := &Response{
				MethodResponses: []Invocation{
					{Name: "m", Args: json.RawMessage(fmt.Sprintf(`{"marker":%q}`, marker)), CallID: marker},
				},
				SessionState: marker,
			}
			want, err := resp.AppendJSON(nil)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: ground truth AppendJSON failed: %w", g, err)
				return
			}
			wantStr := string(want) + "\n"
			for i := 0; i < itersPerGoroutine; i++ {
				var buf strings.Builder
				if err := resp.WriteJSON(&buf); err != nil {
					errs <- fmt.Errorf("goroutine %d iter %d: %w", g, i, err)
					return
				}
				if buf.String() != wantStr {
					errs <- fmt.Errorf("goroutine %d iter %d: pooled-buffer contamination\n got:  %s\nwant: %s", g, i, buf.String(), wantStr)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
