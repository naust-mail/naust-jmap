// Command vulngate runs govulncheck for one module and judges the result
// against the findings this repository has accepted.
//
// It fails when a vulnerability is reachable and unaccepted, and - the part a
// bare `govulncheck ./...` cannot do - when an accepted vulnerability becomes
// reachable through a symbol nobody reviewed.
//
// That second check is the reason this exists. Accepting a finding is a claim
// about a specific reachable surface: these functions, analysed, shown not to
// reach the defect. If a dependency upgrade or a new call site makes some
// other symbol reachable, the recorded analysis no longer describes what is
// built. Accepting by advisory id alone would silently cover the new symbol
// too, which is how a reviewed exception rots into a permanent mute.
//
// Run from a module directory with GOWORK=off, so the versions examined are
// the ones that module's own go.mod selects rather than whatever the workspace
// lifts them to. See .github/SECURITY.md.
//
// This is a separate stdlib-only module, outside go.work, so it never enters
// the six-module gate and carries no dependencies of its own.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// accepted maps an advisory id to the vulnerable symbols that were reviewed
// when it was accepted. Reachability through anything not listed here fails
// the gate, because the recorded evidence does not cover it.
//
// Every entry must be justified in .github/SECURITY.md, and
// must be paired with a test that fails if the code drifts into the vulnerable
// path. For GO-2026-5970 those are TestNormSurfaceIsStringAndIsNormalOnly and
// TestNormLoopStaysBehindIter, both in
// datatypes/mail/internal/depsguard/normguard_test.go. Keep the two in step:
// widening the norm surface there means redoing the analysis here.
var accepted = map[string][]string{
	// Infinite loop in norm.Iter on invalid UTF-8. The looping functions are
	// reachable only through norm.Iter, which nothing in x/text constructs and
	// no code in this repository calls. datatypes/mail pins an affected
	// version deliberately: it is the last x/text supporting Go 1.24, and the
	// fix exists only in versions requiring Go 1.25.
	"GO-2026-5970": {
		"golang.org/x/text/unicode/norm.Form.String",
		"golang.org/x/text/unicode/norm.Form.IsNormalString",
	},
}

// govulncheck exits 0 when it finds nothing and 3 when it reports findings.
// Any other status means it did not run, which must never read as clean.
const (
	exitOK    = 0
	exitFound = 3
)

func main() {
	raw, err := runGovulncheck()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vulngate:", err)
		os.Exit(2)
	}
	reachable, err := parse(strings.NewReader(raw))
	if err != nil {
		fmt.Fprintln(os.Stderr, "vulngate: parsing govulncheck output:", err)
		os.Exit(2)
	}
	report, ok := judge(reachable, accepted)
	for _, line := range report {
		fmt.Println(line)
	}
	if !ok {
		os.Exit(1)
	}
	fmt.Println("vulngate: clean")
}

func runGovulncheck() (string, error) {
	cmd := exec.Command("govulncheck", "-format", "json", "./...")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := exitOK
	if err != nil {
		exit, isExit := err.(*exec.ExitError)
		if !isExit {
			return "", fmt.Errorf("running govulncheck: %w", err)
		}
		code = exit.ExitCode()
	}
	if code != exitOK && code != exitFound {
		return "", fmt.Errorf("govulncheck exited %d: %s", code, stderr.String())
	}
	return stdout.String(), nil
}

// parse maps advisory id to the set of vulnerable symbols reachable from the
// scanned module.
//
// govulncheck emits a stream of concatenated JSON objects rather than one
// document, which json.Decoder reads natively. A finding counts as reachable
// only when its trace names a function: govulncheck also reports
// vulnerabilities that are merely imported or required, which are not what
// this gate judges.
func parse(r io.Reader) (map[string]map[string]bool, error) {
	type frame struct {
		Package  string `json:"package"`
		Receiver string `json:"receiver"`
		Function string `json:"function"`
	}
	type message struct {
		Finding *struct {
			OSV   string  `json:"osv"`
			Trace []frame `json:"trace"`
		} `json:"finding"`
	}
	reachable := map[string]map[string]bool{}
	dec := json.NewDecoder(r)
	for {
		var msg message
		if err := dec.Decode(&msg); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if msg.Finding == nil || len(msg.Finding.Trace) == 0 {
			continue
		}
		top := msg.Finding.Trace[0]
		if top.Function == "" {
			continue
		}
		parts := []string{top.Package}
		if top.Receiver != "" {
			parts = append(parts, top.Receiver)
		}
		parts = append(parts, top.Function)
		var kept []string
		for _, p := range parts {
			if p != "" {
				kept = append(kept, p)
			}
		}
		if reachable[msg.Finding.OSV] == nil {
			reachable[msg.Finding.OSV] = map[string]bool{}
		}
		reachable[msg.Finding.OSV][strings.Join(kept, ".")] = true
	}
	return reachable, nil
}

// judge compares what is reachable against what was accepted, returning the
// lines to print and whether the gate passes.
func judge(reachable map[string]map[string]bool, accepted map[string][]string) ([]string, bool) {
	var report []string
	ok := true
	for _, osv := range sortedKeys(reachable) {
		symbols := reachable[osv]
		reviewed, isAccepted := accepted[osv]
		if !isAccepted {
			report = append(report, fmt.Sprintf("FAIL %s is reachable and is not accepted.", osv))
			for _, sym := range sortedSet(symbols) {
				report = append(report, "       via "+sym)
			}
			report = append(report,
				"     Upgrade the dependency, or record evidence that the vulnerable path is",
				"     unreachable and add it to accepted in .github/tools/vulngate/main.go.")
			ok = false
			continue
		}
		reviewedSet := map[string]bool{}
		for _, sym := range reviewed {
			reviewedSet[sym] = true
		}
		var unreviewed []string
		for _, sym := range sortedSet(symbols) {
			if !reviewedSet[sym] {
				unreviewed = append(unreviewed, sym)
			}
		}
		if len(unreviewed) > 0 {
			report = append(report,
				fmt.Sprintf("FAIL %s is accepted, but is now reachable through symbols that were", osv),
				"     not reviewed when it was accepted:")
			for _, sym := range unreviewed {
				report = append(report, "       "+sym)
			}
			report = append(report,
				"     The recorded evidence does not cover these. See",
				"     .github/SECURITY.md.")
			ok = false
			continue
		}
		report = append(report, fmt.Sprintf(
			"ok   %s accepted; reachable surface unchanged (%d symbol(s))", osv, len(symbols)))
	}
	for _, osv := range sortedKeys2(accepted) {
		if reachable[osv] == nil {
			report = append(report, fmt.Sprintf("note %s is accepted but not reachable in this module.", osv))
		}
	}
	return report, ok
}

func sortedKeys(m map[string]map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys2(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
