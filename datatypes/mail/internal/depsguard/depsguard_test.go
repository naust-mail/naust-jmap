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
