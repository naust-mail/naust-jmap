package runtime

// What these measure is the decoding, not the sorting. Comparing
// records pairwise decodes the same stored value about 2n log n times;
// decoding once per record and comparing decoded values makes it n.
// The gap widens with the record count, so the 10k cases are the ones
// that matter - and ReportAllocs is the number to watch, since the cost
// being removed is allocation, not comparison.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

func benchSortRecords(n, distinct int) []QueryRecord {
	out := make([]QueryRecord, n)
	for i := range out {
		id := fmt.Sprintf("R%06d", i)
		// A stride coprime with n scatters the values, so the input is
		// neither sorted nor reverse sorted.
		v := (i * 7919) % distinct
		out[i] = QueryRecord{
			Id: jmap.Id(id),
			Obj: objectdb.Object{
				"id":       json.RawMessage(strconv.Quote(id)),
				"subject":  json.RawMessage(strconv.Quote(fmt.Sprintf("Some Message Subject %d", v))),
				"received": json.RawMessage(strconv.Quote(fmt.Sprintf("2026-08-%02dT12:%02d:00Z", 1+v%28, v%60))),
			},
		}
	}
	return out
}

func benchSort(b *testing.B, recs []QueryRecord, cmps ...comparator) {
	b.Helper()
	ord := &ordering{cmps: cmps}
	work := make([]QueryRecord, len(recs))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(work, recs)
		ord.sortRecords(work)
	}
}

var (
	benchSubject  = comparator{prop: descriptor.Property{Kind: descriptor.KindString}, name: "subject"}
	benchReceived = comparator{prop: descriptor.Property{Kind: descriptor.KindDate}, name: "received"}
)

// A string comparator: the worst case, since its key is a scan plus a
// case fold.
func BenchmarkQuerySortString10k(b *testing.B) {
	benchSort(b, benchSortRecords(10000, 10000), benchSubject)
}

// One value in ten is distinct, so the id tiebreak decides most
// comparisons.
func BenchmarkQuerySortStringTies10k(b *testing.B) {
	benchSort(b, benchSortRecords(10000, 1000), benchSubject)
}

// A date comparator: its key is a scan plus an RFC 3339 parse.
func BenchmarkQuerySortDate10k(b *testing.B) {
	benchSort(b, benchSortRecords(10000, 10000), benchReceived)
}

// Two comparators, the shape a secondary sort has.
func BenchmarkQuerySortTwoComparators10k(b *testing.B) {
	benchSort(b, benchSortRecords(10000, 1000), benchReceived, benchSubject)
}

// A page-sized result, which is what most queries actually sort.
func BenchmarkQuerySortString500(b *testing.B) {
	benchSort(b, benchSortRecords(500, 500), benchSubject)
}

// Already ordered input, the case a client paging through a stable list
// produces over and over.
func BenchmarkQuerySortPresorted10k(b *testing.B) {
	recs := benchSortRecords(10000, 10000)
	ord := &ordering{cmps: []comparator{benchSubject}}
	ord.sortRecords(recs)
	work := make([]QueryRecord, len(recs))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(work, recs)
		ord.sortRecords(work)
	}
}

// The comparison rule itself, decoded values against a pair of records:
// the difference between these two is the whole point of decoding once.
func BenchmarkQuerySortCompareDecoded(b *testing.B) {
	recs := benchSortRecords(2, 2)
	a, c := benchSubject.sortValue(recs[0].Obj), benchSubject.sortValue(recs[1].Obj)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = benchSubject.compareValues(a, c)
	}
}

func BenchmarkQuerySortComparePairwise(b *testing.B) {
	recs := benchSortRecords(2, 2)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = benchSubject.compare(recs[0].Obj, recs[1].Obj)
	}
}

var benchSinkIds []jmap.Id

// Guard against the clone in sortRecords being optimized away in a
// future edit: the sorted order has to be observable.
func BenchmarkQuerySortIdsOut(b *testing.B) {
	recs := benchSortRecords(10000, 1000)
	ord := &ordering{cmps: []comparator{benchSubject}}
	work := make([]QueryRecord, len(recs))
	out := make([]jmap.Id, len(recs))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(work, recs)
		ord.sortRecords(work)
		for j, m := range work {
			out[j] = m.Id
		}
		benchSinkIds = slices.Clip(out)
	}
}

// A page-sized window out of a large result set: what a client asking
// for the first 500 of ten thousand actually needs ordered.
func BenchmarkQuerySortWindow500of10k(b *testing.B) {
	recs := benchSortRecords(10000, 10000)
	ord := &ordering{cmps: []comparator{benchSubject}, window: 500}
	work := make([]QueryRecord, len(recs))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(work, recs)
		ord.sortRecords(work)
	}
}
