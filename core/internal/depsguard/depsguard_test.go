// Package depsguard holds the zero-dependency CI guard: the core module
// must depend on the Go standard library only, forever. Storage engines
// and anything else needing third-party code live in separate modules
// (see drivers/ at the repo root).
package depsguard

import (
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/naust-mail/naust-jmap/core"

// TestCoreIsStdlibOnly lists every dependency of every package in the
// core module, including test dependencies, and fails on anything that
// is neither stdlib nor the module itself. Stdlib import paths have no
// dot in their first segment; anything with a dotted host is external.
func TestCoreIsStdlibOnly(t *testing.T) {
	// The module path pattern (not "./...") because the test's working
	// directory is this package, not the module root.
	out, err := exec.Command("go", "list", "-deps", "-test", module+"/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// go list -test emits forms like "pkg [pkg.test]"; the leading
		// field is the import path.
		pkg := strings.Fields(line)[0]
		if pkg == module || strings.HasPrefix(pkg, module+"/") {
			continue
		}
		if first, _, _ := strings.Cut(pkg, "/"); strings.Contains(first, ".") {
			t.Errorf("core module depends on non-stdlib package %s", pkg)
		}
	}
}
