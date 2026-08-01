package search

// The section 5 highlighting building blocks: range merging, <mark>
// wrapping with HTML escaping, and the octet-budgeted body preview
// (bodyPreview).

import (
	"strings"
	"testing"
)

// TestMergeRanges: overlapping, adjacent (touching), and nested ranges all
// collapse into their span; disjoint ranges stay separate, and inputs are
// sorted by start regardless of input order.
func TestMergeRanges(t *testing.T) {
	cases := []struct {
		name string
		in   [][2]int
		want [][2]int
	}{
		{"empty", nil, nil},
		{"single", [][2]int{{2, 5}}, [][2]int{{2, 5}}},
		{"overlapping", [][2]int{{0, 5}, {3, 8}}, [][2]int{{0, 8}}},
		{"adjacent (touching at the boundary)", [][2]int{{0, 5}, {5, 10}}, [][2]int{{0, 10}}},
		{"nested", [][2]int{{0, 10}, {2, 5}}, [][2]int{{0, 10}}},
		{"disjoint stays separate", [][2]int{{0, 2}, {5, 7}}, [][2]int{{0, 2}, {5, 7}}},
		{"unsorted input is sorted", [][2]int{{5, 7}, {0, 2}}, [][2]int{{0, 2}, {5, 7}}},
		{"multi-overlap chain merges into one", [][2]int{{10, 15}, {0, 3}, {2, 11}}, [][2]int{{0, 15}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeRanges(append([][2]int(nil), c.in...))
			if len(got) != len(c.want) {
				t.Fatalf("mergeRanges(%v) = %v, want %v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("mergeRanges(%v) = %v, want %v", c.in, got, c.want)
				}
			}
		})
	}
}

// TestHighlightRanges: each range is wrapped in <mark></mark>, the text
// outside ranges is escaped, and the special characters &, <, >, and " are
// escaped both inside and outside a highlighted range (" is not one of the
// three section 5 requires escaping, so it must pass through unescaped).
func TestHighlightRanges(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		ranges [][2]int
		want   string
	}{
		{"no ranges: only escaping", `<a & "b">`, nil, `&lt;a &amp; "b"&gt;`},
		{"single range", "hello world", [][2]int{{6, 11}}, "hello <mark>world</mark>"},
		{"range at the very start", "hello world", [][2]int{{0, 5}}, "<mark>hello</mark> world"},
		{"multiple occurrences highlighted", "a cat and a cat", [][2]int{{2, 5}, {12, 15}}, "a <mark>cat</mark> and a <mark>cat</mark>"},
		{"escaping inside a highlighted range", `a <b> & c`, [][2]int{{2, 5}}, `a <mark>&lt;b&gt;</mark> &amp; c`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := highlightRanges(c.text, c.ranges); got != c.want {
				t.Errorf("highlightRanges(%q, %v) = %q, want %q", c.text, c.ranges, got, c.want)
			}
		})
	}
}

// TestBodyPreview: the excerpt is escaped, highlighted at every occurrence
// of a term, and bracketed by an ellipsis only on sides that are not a
// body edge; a window with no match for the given terms yields no preview.
func TestBodyPreview(t *testing.T) {
	scan := bodyScan{matched: true, window: "alpha <needle> & needle omega", atStart: true, atEnd: true}
	preview := bodyPreview(scan, []string{"needle"})
	if !strings.Contains(preview, "<mark>needle</mark>") {
		t.Errorf("preview = %q, want the term highlighted", preview)
	}
	if strings.Count(preview, "<mark>") != 2 {
		t.Errorf("preview = %q, want both occurrences highlighted", preview)
	}
	if strings.HasPrefix(preview, "...") || strings.HasSuffix(preview, "...") {
		t.Errorf("preview = %q: both edges are body edges, want no ellipsis", preview)
	}
	if !strings.Contains(preview, "&lt;") || !strings.Contains(preview, "&gt;") || !strings.Contains(preview, "&amp;") {
		t.Errorf("preview = %q, want the surrounding <, >, & escaped", preview)
	}

	notAtEdges := bodyScan{matched: true, window: "middle needle text", atStart: false, atEnd: false}
	preview = bodyPreview(notAtEdges, []string{"needle"})
	if !strings.HasPrefix(preview, "...") || !strings.HasSuffix(preview, "...") {
		t.Errorf("preview = %q, want an ellipsis on both sides (neither is a body edge)", preview)
	}

	noMatch := bodyPreview(bodyScan{matched: true, window: "nothing relevant here"}, []string{"absent"})
	if noMatch != "" {
		t.Errorf("preview with no term occurrence in the window = %q, want empty", noMatch)
	}

	unmatched := bodyPreview(bodyScan{}, []string{"needle"})
	if unmatched != "" {
		t.Errorf("preview of an unmatched scan = %q, want empty", unmatched)
	}
}
