// Package depsguard holds the dependency CI guard for the mail datatype
// module: it may depend on the Go standard library, the core module, and
// golang.org/x/text (charset tables and unicode normalization) - nothing
// else, ever.
package depsguard

import (
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/naust-mail/naust-jmap/datatypes/mail"

var allowedPrefixes = [...]string{
	"github.com/naust-mail/naust-jmap/core",
	"golang.org/x/text",
}

// TestMailDepsAreCoreAndXTextOnly lists every dependency of every package
// in the mail datatype module, including test dependencies, and fails on
// anything that is not stdlib, the module itself, the core module, or
// golang.org/x/text. Stdlib import paths have no dot in their first
// segment; anything with a dotted host is external.
func TestMailDepsAreCoreAndXTextOnly(t *testing.T) {
	// The module path pattern (not "./...") because the test's working
	// directory is this package, not the module root.
	out, err := exec.Command("go", "list", "-deps", "-test", module+"/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
next:
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// go list -test emits forms like "pkg [pkg.test]"; the leading
		// field is the import path. It also synthesizes a "pkg.test"
		// package per tested package, hence the module+"." prefix.
		pkg := strings.Fields(line)[0]
		if pkg == module || strings.HasPrefix(pkg, module+"/") || strings.HasPrefix(pkg, module+".") {
			continue
		}
		if first, _, _ := strings.Cut(pkg, "/"); !strings.Contains(first, ".") {
			continue // stdlib
		}
		for _, allowed := range allowedPrefixes {
			if pkg == allowed || strings.HasPrefix(pkg, allowed+"/") {
				continue next
			}
		}
		t.Errorf("mail datatype module depends on disallowed package %s", pkg)
	}
}

// oraclePkg is the frozen buffered parser, kept as the differential reference
// for the streaming rewrite. It is a whole second parser: if anything outside a
// test ever imports it, the module ships two parsers and the one nobody
// maintains is reachable from a running server.
const oraclePkg = module + "/internal/oracle"

// TestOracleIsTestOnly fails if any package in the module imports the oracle in
// its non-test build. Deps is the transitive import set of the package proper,
// so only a real import shows up here - a _test.go import does not.
func TestOracleIsTestOnly(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}} {{join .Deps \" \"}}", module+"/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		for _, dep := range fields[1:] {
			if dep == oraclePkg {
				t.Errorf("%s imports %s in its non-test build; the oracle exists only for tests", fields[0], oraclePkg)
			}
		}
	}
}
