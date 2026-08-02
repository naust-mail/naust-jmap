package report

// Cost guards for ParseFieldGroups. The parser is fed content a remote
// party controls (the machine-readable part of an inbound report, RFC
// 3464 section 2 / RFC 8098 section 3), so its cost must stay linear in
// the input under hostile shapes, not just realistic ones. The worst
// case the capture bound admits is one field folded across the maximum
// number of minimal continuation lines (RFC 5322 section 2.2.3 folding);
// a naive per-fold re-copy of the accumulated value is quadratic there,
// and TestParseFieldGroupsHostileAllocs pins the linear behavior with an
// allocation ceiling - the quadratic shape allocates three orders of
// magnitude more, so a regression trips it regardless of machine speed.

import (
	"bytes"
	"strings"
	"testing"
)

// benchRealisticMDN is a typical disposition-notification field group
// (RFC 8098 section 3.1) with one folded field.
var benchRealisticMDN = []byte("Reporting-UA: joes-pc.cs.example.com; Foomail 97.1\r\n" +
	"Original-Recipient: rfc822; joe-alias@example.com\r\n" +
	"Final-Recipient: rfc822;\r\n joe@example.com\r\n" +
	"Original-Message-ID: <199509192301.23456@example.org>\r\n" +
	"Disposition: manual-action/mdn-sent-manually; displayed\r\n")

// benchRealisticDSN is a typical delivery-status content: a per-message
// group and one per-recipient group (RFC 3464 section 2).
var benchRealisticDSN = []byte("Original-Envelope-Id: env-1\r\nReporting-MTA: dns; mx.example.net\r\n\r\n" +
	"Final-Recipient: rfc822; jane@remote.example\r\n" +
	"Action: failed\r\nStatus: 5.1.1\r\n" +
	"Diagnostic-Code: smtp; 550 5.1.1 user unknown,\r\n mailbox unavailable\r\n")

// hostileFolds is the adversarial worst case within the 64K report
// capture bound: one field, then nothing but minimal fold lines.
func hostileFolds() []byte {
	var b bytes.Buffer
	b.WriteString("X-A: start\n")
	for b.Len() < 64<<10 {
		b.WriteString(" x\n")
	}
	return b.Bytes()
}

func benchParseFieldGroups(b *testing.B, raw []byte) {
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if g := ParseFieldGroups(raw); len(g) == 0 {
			b.Fatal("no groups")
		}
	}
}

func BenchmarkParseFieldGroupsMDN(b *testing.B)     { benchParseFieldGroups(b, benchRealisticMDN) }
func BenchmarkParseFieldGroupsDSN(b *testing.B)     { benchParseFieldGroups(b, benchRealisticDSN) }
func BenchmarkParseFieldGroupsHostile(b *testing.B) { benchParseFieldGroups(b, hostileFolds()) }

// TestParseFieldGroupsHostileAllocs pins linear-cost parsing of the
// hostile fold shape. The linear implementation performs ~24 allocations
// on this input; a quadratic fold accumulation performs one per fold
// line (~20000). The ceiling sits far above the former and far below
// the latter, so it is stable across Go versions but trips on any
// re-copying regression.
func TestParseFieldGroupsHostileAllocs(t *testing.T) {
	raw := hostileFolds()
	allocs := testing.AllocsPerRun(5, func() {
		if g := ParseFieldGroups(raw); len(g) == 0 {
			t.Fatal("no groups")
		}
	})
	if allocs > 500 {
		t.Fatalf("ParseFieldGroups allocated %.0f times on the hostile fold input; the linear implementation needs ~24 - fold accumulation has regressed to per-fold copying", allocs)
	}
}

// TestParseFieldGroupsFolding pins the unfolding semantics on explicit
// values: continuation lines join with a single space (RFC 5322 section
// 2.2.3), a fold before any field is dropped, colon-less lines are
// skipped without ending the current field, and blank lines delimit
// groups (RFC 3464 section 2.1).
func TestParseFieldGroupsFolding(t *testing.T) {
	raw := []byte(" orphan fold\n" +
		"X-A: a\n f1\n f2\n" +
		"not a field line\n" +
		"X-B: b\n\n" +
		"X-C: c\n c1\n")
	groups := ParseFieldGroups(raw)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	want := [][]headerKV{
		{{name: "X-A", value: "a f1 f2"}, {name: "X-B", value: "b"}},
		{{name: "X-C", value: "c c1"}},
	}
	for gi, g := range want {
		for fi, kv := range g {
			if groups[gi][fi] != kv {
				t.Errorf("group %d field %d = %+v, want %+v", gi, fi, groups[gi][fi], kv)
			}
		}
	}
	// The hostile shape parses to exactly one field whose value carries
	// every fold, so the guard above measures real work, not a bail-out.
	hostile := ParseFieldGroups(hostileFolds())
	if len(hostile) != 1 || len(hostile[0]) != 1 {
		t.Fatalf("hostile shape = %d groups, want 1 group / 1 field", len(hostile))
	}
	if v := hostile[0][0].value; !strings.HasPrefix(v, "start x x") || len(v) < 40<<10 {
		t.Errorf("hostile value = %d bytes starting %q, want the folds joined", len(v), v[:16])
	}
}
