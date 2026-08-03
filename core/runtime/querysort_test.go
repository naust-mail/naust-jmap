package runtime

// The sort behind Foo/query orders records by decoding each one's
// comparison values once (see ordering.sortRecords) rather than
// decoding on every comparison. Two properties hold that change to
// account: the order must be exactly the one comparing records pairwise
// gives, for every combination of kinds, collations, directions and
// missing values; and the decoding must stay proportional to the record
// count, never to the comparison count.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// sortProps covers every comparison branch: a string (i;ascii-casemap,
// the fold), a string sorted under i;ascii-numeric, an integer, a date,
// a boolean, and a nullable string whose encoding carries a tag byte.
var sortProps = map[string]descriptor.Property{
	"subject":  {Kind: descriptor.KindString},
	"seq":      {Kind: descriptor.KindString},
	"size":     {Kind: descriptor.KindUnsignedInt},
	"received": {Kind: descriptor.KindDate},
	"flagged":  {Kind: descriptor.KindBool},
	"folder":   {Kind: descriptor.KindString, Nullable: true},
}

// sortRecordsFor builds records whose values repeat heavily, so ties
// are common and the id tiebreak decides them, and where every property
// is absent from some records, which sorts them first.
func sortRecordsFor(n int, rng *rand.Rand) []QueryRecord {
	out := make([]QueryRecord, n)
	for i := range out {
		id := fmt.Sprintf("R%06d", i)
		obj := objectdb.Object{"id": json.RawMessage(strconv.Quote(id))}
		if i%7 != 0 {
			obj["subject"] = json.RawMessage(strconv.Quote(fmt.Sprintf("Subject %d", rng.Intn(n/4+1))))
		}
		if i%5 != 0 {
			obj["seq"] = json.RawMessage(strconv.Quote(fmt.Sprintf("%03d items", rng.Intn(20))))
		}
		if i%3 != 0 {
			obj["size"] = json.RawMessage(strconv.Itoa(rng.Intn(50)))
		}
		if i%4 != 0 {
			obj["received"] = json.RawMessage(strconv.Quote(fmt.Sprintf("2026-08-%02dT12:%02d:00Z", 1+rng.Intn(28), rng.Intn(60))))
		}
		if i%2 != 0 {
			obj["flagged"] = json.RawMessage(strconv.FormatBool(rng.Intn(2) == 0))
		}
		switch i % 6 {
		case 0: // absent
		case 1:
			obj["folder"] = json.RawMessage(`null`)
		default:
			obj["folder"] = json.RawMessage(strconv.Quote(fmt.Sprintf("Folder %d", rng.Intn(3))))
		}
		out[i] = QueryRecord{Id: jmap.Id(id), Obj: obj}
	}
	return out
}

// The decoded sort and the pairwise comparison are the same order. A
// divergence means a decoded value lost something the raw comparison
// sees - a missing property, an unencodable value, a collation.
func TestSortRecordsMatchesPairwiseComparison(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	records := sortRecordsFor(600, rng)

	names := make([]string, 0, len(sortProps))
	for name := range sortProps {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		for _, descending := range []bool{false, true} {
			for _, numeric := range []bool{false, true} {
				if numeric && sortProps[name].Kind != descriptor.KindString {
					continue // the collation applies to strings only (5.5)
				}
				c := comparator{prop: sortProps[name], name: name, descending: descending, numeric: numeric}
				ord := &ordering{cmps: []comparator{c}}

				got := slices.Clone(records)
				ord.sortRecords(got)

				want := slices.Clone(records)
				less := ord.compareFunc()
				slices.SortStableFunc(want, func(a, b QueryRecord) int { return less(a.Obj, b.Obj) })

				label := fmt.Sprintf("%s descending=%t numeric=%t", name, descending, numeric)
				for i := range got {
					if got[i].Id != want[i].Id {
						t.Fatalf("%s: decoded sort and pairwise comparison disagree at %d: %s vs %s",
							label, i, got[i].Id, want[i].Id)
					}
				}
			}
		}
	}
}

// Multiple comparators, where the second and third decide the ties the
// first leaves, and the id tiebreak decides what none of them do.
func TestSortRecordsMultipleComparators(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	records := sortRecordsFor(400, rng)
	ord := &ordering{cmps: []comparator{
		{prop: sortProps["flagged"], name: "flagged", descending: true},
		{prop: sortProps["size"], name: "size"},
		{prop: sortProps["subject"], name: "subject", descending: true},
	}}

	got := slices.Clone(records)
	ord.sortRecords(got)

	want := slices.Clone(records)
	less := ord.compareFunc()
	slices.SortStableFunc(want, func(a, b QueryRecord) int { return less(a.Obj, b.Obj) })
	for i := range got {
		if got[i].Id != want[i].Id {
			t.Fatalf("decoded sort and pairwise comparison disagree at %d: %s vs %s", i, got[i].Id, want[i].Id)
		}
	}
}

// Sorting the same records twice must give the same answer: paging,
// anchors and Foo/queryChanges positions all assume the order between
// two identical calls is identical (5.5).
func TestSortRecordsIsDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	records := sortRecordsFor(300, rng)
	ord := &ordering{cmps: []comparator{{prop: sortProps["size"], name: "size"}}}

	first := slices.Clone(records)
	ord.sortRecords(first)
	for round := 0; round < 8; round++ {
		shuffled := slices.Clone(records)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		ord.sortRecords(shuffled)
		for i := range first {
			if first[i].Id != shuffled[i].Id {
				t.Fatalf("round %d: order differs at %d: %s vs %s", round, i, first[i].Id, shuffled[i].Id)
			}
		}
	}
}

// The decoding budget: a sort must decode each record's value once, not
// once per comparison. Comparing records pairwise costs about 2n log n
// decodes - for 2000 records that is upwards of 40 allocations each -
// so a bound of 4 per record catches a return to that shape while
// leaving room for the slices the sort itself allocates.
func TestSortRecordsAllocationBudget(t *testing.T) {
	rng := rand.New(rand.NewSource(17))
	records := sortRecordsFor(2000, rng)
	ord := &ordering{cmps: []comparator{{prop: sortProps["subject"], name: "subject"}}}
	work := make([]QueryRecord, len(records))

	allocs := testing.AllocsPerRun(5, func() {
		copy(work, records)
		ord.sortRecords(work)
	})
	if budget := float64(4 * len(records)); allocs > budget {
		t.Fatalf("sorting %d records allocated %.0f times, budget %.0f: the sort is decoding per comparison again",
			len(records), allocs, budget)
	}
}
