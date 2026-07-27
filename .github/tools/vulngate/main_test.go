package main

import (
	"strings"
	"testing"
)

// A trimmed sample of real govulncheck -format json output: a stream of
// concatenated objects, mixing the config and progress messages the tool emits
// with findings at both reachability levels.
const sample = `
{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck"}}
{"progress":{"message":"Scanning your code and 42 packages"}}
{"osv":{"id":"GO-2026-5970","summary":"Infinite loop on invalid input"}}
{"finding":{"osv":"GO-2026-5970","fixed_version":"v0.39.0","trace":[{"module":"golang.org/x/text"}]}}
{"finding":{"osv":"GO-2026-5970","fixed_version":"v0.39.0","trace":[
  {"module":"golang.org/x/text","package":"golang.org/x/text/unicode/norm","receiver":"Form","function":"String"},
  {"module":"example.com/app","package":"example.com/app","function":"main"}]}}
{"finding":{"osv":"GO-2026-5970","fixed_version":"v0.39.0","trace":[
  {"module":"golang.org/x/text","package":"golang.org/x/text/unicode/norm","receiver":"Form","function":"IsNormalString"},
  {"module":"example.com/app","package":"example.com/app","function":"check"}]}}
`

func TestParseKeepsOnlyCalledFindings(t *testing.T) {
	got, err := parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	syms := got["GO-2026-5970"]
	if len(syms) != 2 {
		t.Fatalf("expected 2 reachable symbols, got %d: %v", len(syms), syms)
	}
	for _, want := range []string{
		"golang.org/x/text/unicode/norm.Form.String",
		"golang.org/x/text/unicode/norm.Form.IsNormalString",
	} {
		if !syms[want] {
			t.Errorf("missing reachable symbol %s (got %v)", want, syms)
		}
	}
	// The module-only trace carries no function and must not count: it is an
	// imported-but-not-called report, not a reachable path.
	for sym := range syms {
		if !strings.Contains(sym, ".Form.") {
			t.Errorf("unexpected symbol from a non-called finding: %s", sym)
		}
	}
}

func TestParseIgnoresOutputWithNoFindings(t *testing.T) {
	got, err := parse(strings.NewReader(`{"config":{"scanner_name":"govulncheck"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected nothing reachable, got %v", got)
	}
}

func set(syms ...string) map[string]bool {
	out := map[string]bool{}
	for _, s := range syms {
		out[s] = true
	}
	return out
}

func TestJudge(t *testing.T) {
	tests := []struct {
		name      string
		reachable map[string]map[string]bool
		accepted  map[string][]string
		wantOK    bool
		wantLine  string
	}{
		{
			name:      "nothing reachable passes",
			reachable: map[string]map[string]bool{},
			accepted:  map[string][]string{},
			wantOK:    true,
		},
		{
			name:      "accepted with the reviewed surface passes",
			reachable: map[string]map[string]bool{"GO-1": set("pkg.A", "pkg.B")},
			accepted:  map[string][]string{"GO-1": {"pkg.A", "pkg.B"}},
			wantOK:    true,
			wantLine:  "reachable surface unchanged",
		},
		{
			name:      "unaccepted finding fails",
			reachable: map[string]map[string]bool{"GO-2": set("pkg.A")},
			accepted:  map[string][]string{},
			wantOK:    false,
			wantLine:  "is reachable and is not accepted",
		},
		{
			// The case a bare govulncheck cannot catch, and the reason this
			// program exists: the advisory was accepted, but the code now
			// reaches it somewhere nobody analysed.
			name:      "accepted but reached via an unreviewed symbol fails",
			reachable: map[string]map[string]bool{"GO-1": set("pkg.A", "pkg.Iter")},
			accepted:  map[string][]string{"GO-1": {"pkg.A"}},
			wantOK:    false,
			wantLine:  "not reviewed when it was accepted",
		},
		{
			// A narrower surface than reviewed is not a failure: fewer call
			// sites cannot invalidate an unreachability argument.
			name:      "reaching fewer symbols than reviewed passes",
			reachable: map[string]map[string]bool{"GO-1": set("pkg.A")},
			accepted:  map[string][]string{"GO-1": {"pkg.A", "pkg.B"}},
			wantOK:    true,
		},
		{
			name:      "accepted but absent here is only a note",
			reachable: map[string]map[string]bool{},
			accepted:  map[string][]string{"GO-1": {"pkg.A"}},
			wantOK:    true,
			wantLine:  "not reachable in this module",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, ok := judge(tt.reachable, tt.accepted)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v; report:\n%s", ok, tt.wantOK, strings.Join(report, "\n"))
			}
			if tt.wantLine != "" && !strings.Contains(strings.Join(report, "\n"), tt.wantLine) {
				t.Errorf("report missing %q; got:\n%s", tt.wantLine, strings.Join(report, "\n"))
			}
		})
	}
}
