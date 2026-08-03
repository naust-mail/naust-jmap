package runtime

// Foo/query reads back only the window it asked for, so ordering stops
// at the end of that window (see ordering.window). What the caller sees
// must be indistinguishable from ordering everything.

import (
	"math/rand"
	"slices"
	"testing"
)

// A windowed sort must place, at every position the caller reads,
// exactly what a full sort would have placed there.
func TestWindowedSortMatchesFullSort(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, n := range []int{1, 2, 13, 14, 100, 999, 5000} {
		records := sortRecordsFor(n, rng)
		for _, w := range []int{1, 2, 5, 13, 14, 50, 500, n - 1, n, n + 10} {
			if w <= 0 {
				continue
			}
			full := slices.Clone(records)
			(&ordering{cmps: []comparator{{prop: sortProps["size"], name: "size"}}}).sortRecords(full)

			windowed := slices.Clone(records)
			(&ordering{cmps: []comparator{{prop: sortProps["size"], name: "size"}}, window: w}).sortRecords(windowed)

			limit := min(w, n)
			for i := 0; i < limit; i++ {
				if full[i].Id != windowed[i].Id {
					t.Fatalf("n=%d window=%d: position %d is %s, full sort gives %s",
						n, w, i, windowed[i].Id, full[i].Id)
				}
			}
			if len(windowed) != n {
				t.Fatalf("n=%d window=%d: %d results, want %d", n, w, len(windowed), n)
			}
		}
	}
}

// Every comparator kind, both directions, with ties and absent values.
func TestWindowedSortAcrossKinds(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	records := sortRecordsFor(1200, rng)
	for name, prop := range sortProps {
		for _, descending := range []bool{false, true} {
			c := comparator{prop: prop, name: name, descending: descending}
			full := slices.Clone(records)
			(&ordering{cmps: []comparator{c}}).sortRecords(full)
			windowed := slices.Clone(records)
			(&ordering{cmps: []comparator{c}, window: 200}).sortRecords(windowed)
			for i := 0; i < 200; i++ {
				if full[i].Id != windowed[i].Id {
					t.Fatalf("%s descending=%t: position %d is %s, full sort gives %s",
						name, descending, i, windowed[i].Id, full[i].Id)
				}
			}
		}
	}
}
