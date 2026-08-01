package emailmethods

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

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

	if err := CheckEmailGetArgs(build(maxBodyProperties)); err != nil {
		t.Fatalf("list at the cap rejected: %v", err)
	}
	if err := CheckEmailGetArgs(build(maxBodyProperties + 1)); err == nil {
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
