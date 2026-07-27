package depsguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// normSurface is every golang.org/x/text/unicode/norm symbol this module is
// allowed to reach, written as it appears in source.
//
// The list is short on purpose. GO-2026-5970 is an infinite loop reachable
// only through norm.Iter: nextComposed and nextDecomposed, the functions that
// loop, are assigned to Iter.next inside iter.go and nowhere else, and neither
// norm's transform.go nor its readwriter.go constructs an Iter. The two
// entries below were driven through 2,271,452 inputs each - every string up to
// five bytes over a twelve-byte alphabet of UTF-8 boundary values, plus two
// million randomized inputs up to 4 KiB - without stalling, on a version of
// x/text that carries the defect. The same harness stalls norm.Iter in under a
// second, which is what makes the clean result meaningful rather than an
// absence of evidence.
//
// That reasoning covers these two calls and nothing else. Reaching any further
// into norm invalidates it, so widening this list means redoing the analysis
// and updating the summary in .github/SECURITY.md.
var normSurface = map[string]bool{
	"NFC.String":         true,
	"NFC.IsNormalString": true,
}

const normImport = `"golang.org/x/text/unicode/norm"`

// TestNormSurfaceIsStringAndIsNormalOnly walks every non-test source file in
// the module and fails on any use of the norm package outside normSurface.
// Test files are excluded: they do not ship, and the differential oracle is
// free to exercise more of the package than the server ever will.
func TestNormSurfaceIsStringAndIsNormalOnly(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	fset := token.NewFileSet()
	var checked int
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		// The local name of the norm import in this file, if it imports it.
		local := ""
		for _, imp := range file.Imports {
			if imp.Path.Value != normImport {
				continue
			}
			local = "norm"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
		if local == "" {
			return nil
		}
		checked++

		// A qualified use is norm.X; a call on a form is norm.X.Y. Visiting
		// the outer selector first lets the inner one be recorded as consumed,
		// so norm.NFC.String reports as "NFC.String" rather than as a bare
		// "NFC" plus a method.
		consumed := map[ast.Node]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			outer, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if inner, ok := outer.X.(*ast.SelectorExpr); ok {
				if id, ok := inner.X.(*ast.Ident); ok && id.Name == local {
					consumed[inner] = true
					use := inner.Sel.Name + "." + outer.Sel.Name
					if !normSurface[use] {
						t.Errorf("%s:%d: uses %s.%s, which is outside the reviewed norm surface",
							rel(root, path), fset.Position(outer.Pos()).Line, local, use)
					}
					return true
				}
			}
			if id, ok := outer.X.(*ast.Ident); ok && id.Name == local && !consumed[outer] {
				t.Errorf("%s:%d: uses %s.%s bare; only %v may be reached",
					rel(root, path), fset.Position(outer.Pos()).Line, local, outer.Sel.Name, keys(normSurface))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// A guard that silently stops finding the thing it guards is worse than no
	// guard, so assert it actually looked at the files that use norm.
	if checked == 0 {
		t.Fatal("no non-test file imports norm; this guard is no longer wired to anything")
	}
}

// TestNormLoopStaysBehindIter checks the upstream half of the GO-2026-5970
// argument, against whatever version of x/text is actually resolved.
//
// The argument has two halves. This module never calls norm.Iter - that is
// TestNormSurfaceIsStringAndIsNormalOnly, above. The other half is that
// nothing else calls it either: the looping functions nextComposed and
// nextDecomposed are assigned to Iter.next inside unicode/norm/iter.go, and no
// other file in the package, and no other package in x/text, constructs an
// Iter. Together they mean the loop sits behind a door only explicit caller
// code can open, and this module never opens it.
//
// Checking the property directly, rather than listing versions believed to be
// acceptable, is deliberate. A version list has to be maintained by hand,
// grants blanket approval to everything on it, and cannot notice a release
// that starts driving an Iter internally - which is the change that would
// actually invalidate the argument. This fails on that change, on any version,
// with no list to keep current.
func TestNormLoopStaysBehindIter(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "golang.org/x/text").Output()
	if err != nil {
		t.Fatalf("locate x/text: %v", err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		t.Fatal("x/text resolved to no directory; this guard is no longer wired to anything")
	}
	normDir := filepath.Join(root, "unicode", "norm")
	iterFile := filepath.Join(normDir, "iter.go")
	if _, err := os.Stat(iterFile); err != nil {
		t.Fatalf("unicode/norm/iter.go not found in %s: %v\n"+
			"x/text has been restructured; the GO-2026-5970 analysis in "+
			".github/SECURITY.md must be redone.", root, err)
	}

	violations, err := scanForIterUses(root, normDir, iterFile)
	if err != nil {
		t.Fatalf("walk x/text: %v", err)
	}
	for _, v := range violations {
		t.Error(v)
	}
}

// scanForIterUses reports every reference to the norm Iter type outside
// iterFile: unqualified inside the norm package directory, qualified as
// norm.Iter everywhere else. It is split out from the test so the guard can be
// pointed at a synthetic tree and shown to fail, below.
func scanForIterUses(root, normDir, iterFile string) ([]string, error) {
	fset := token.NewFileSet()
	var violations []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if path == iterFile {
			return nil // the one file allowed to drive the loop
		}
		inNorm := filepath.Dir(path) == normDir
		// Parsing every file in x/text means parsing megabytes of generated
		// tables, so skip anything that cannot mention the identifier at all.
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		needle := "norm.Iter"
		if inNorm {
			needle = "Iter"
		}
		if !strings.Contains(string(src), needle) {
			return nil
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.Ident:
				// Inside package norm, Iter is referred to unqualified.
				if inNorm && node.Name == "Iter" {
					violations = append(violations, fmt.Sprintf(
						"%s:%d: references Iter outside iter.go; the loop is no longer "+
							"reachable only through an explicitly constructed Iter, so the "+
							"GO-2026-5970 analysis no longer holds",
						rel(root, path), fset.Position(node.Pos()).Line))
				}
			case *ast.SelectorExpr:
				if id, ok := node.X.(*ast.Ident); ok && id.Name == "norm" && node.Sel.Name == "Iter" {
					violations = append(violations, fmt.Sprintf(
						"%s:%d: constructs a norm.Iter inside x/text; the loop is no longer "+
							"reachable only from caller code, so the GO-2026-5970 analysis "+
							"no longer holds",
						rel(root, path), fset.Position(node.Pos()).Line))
				}
			}
			return true
		})
		return nil
	})
	return violations, err
}

// TestNormLoopGuardDetectsViolations points the scan at a synthetic tree
// shaped like x/text, carrying the two changes upstream could make that would
// invalidate the unreachability argument. A guard that cannot be shown to fail
// is not evidence of anything.
func TestNormLoopGuardDetectsViolations(t *testing.T) {
	root := t.TempDir()
	normDir := filepath.Join(root, "unicode", "norm")
	if err := os.MkdirAll(normDir, 0o755); err != nil {
		t.Fatal(err)
	}
	iterFile := filepath.Join(normDir, "iter.go")
	write := func(path, src string) {
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Allowed: iter.go itself owns the type.
	write(iterFile, "package norm\n\ntype Iter struct{ next iterFunc }\n\ntype iterFunc func(*Iter) []byte\n")
	// Violation 1: another file in package norm starts driving an Iter.
	write(filepath.Join(normDir, "transform.go"), "package norm\n\nfunc drive() { var i Iter; _ = i }\n")
	// Violation 2: another x/text package constructs one.
	precis := filepath.Join(root, "secure", "precis")
	if err := os.MkdirAll(precis, 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(precis, "profiles.go"),
		"package precis\n\nimport \"golang.org/x/text/unicode/norm\"\n\nfunc p() { var i norm.Iter; _ = i }\n")
	// Not a violation: a test file, and an unrelated Iter in another package.
	write(filepath.Join(normDir, "iter_test.go"), "package norm\n\nfunc t() { var i Iter; _ = i }\n")
	write(filepath.Join(precis, "other.go"), "package precis\n\ntype colltabIter struct{}\n")

	violations, err := scanForIterUses(root, normDir, iterFile)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(violations) != 2 {
		t.Fatalf("expected 2 violations, got %d: %v", len(violations), violations)
	}
	joined := strings.Join(violations, "\n")
	for _, want := range []string{"transform.go", "profiles.go"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected a violation naming %s, got:\n%s", want, joined)
		}
	}
	for _, unwanted := range []string{"iter_test.go", "other.go"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("%s should not be reported, got:\n%s", unwanted, joined)
		}
	}
}

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return r
	}
	return path
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
