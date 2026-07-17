package chunkstore

import (
	"bytes"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

func TestManifestRoundTrip(t *testing.T) {
	for _, m := range []manifest{
		{run: runID{1, 2, 3}, count: 0, size: 0},
		{run: runID{0xff, 0, 0xff}, count: 1, size: 5},
		{run: runID{9: 42}, count: 1 << 20, size: 1<<40 + 7},
	} {
		got, err := decodeManifest(encodeManifest(m))
		if err != nil {
			t.Fatalf("decode(encode(%+v)): %v", m, err)
		}
		if got != m {
			t.Fatalf("round trip: got %+v, want %+v", got, m)
		}
	}
}

// TestDecodeManifestRejectsGarbage: a value that is not a manifest this
// version wrote must fail cleanly, never panic or silently misread. A
// damaged store or a value from another key must not be trusted.
func TestDecodeManifestRejectsGarbage(t *testing.T) {
	good := encodeManifest(manifest{run: runID{1}, count: 3, size: 99})
	cases := map[string][]byte{
		"nil":             nil,
		"empty":           {},
		"one byte":        {manifestVersion},
		"truncated":       good[:manifestLen-1],
		"one byte over":   append(append([]byte{}, good...), 0),
		"unknown version": func() []byte { b := append([]byte{}, good...); b[0] = 0xEE; return b }(),
	}
	for name, b := range cases {
		if _, err := decodeManifest(b); err == nil {
			t.Errorf("%s: decodeManifest accepted a bad value", name)
		}
	}
}

func TestMarkerValueRoundTrip(t *testing.T) {
	acct, run := jmap.Id("Aone"), runID{7: 3, 15: 9}
	stamp := time.Unix(0, 1_720_000_000_123_456_789)
	gotAcct, gotRun, gotStamp, ok := decodeMarker(markerValue(acct, run, stamp))
	if !ok || gotAcct != acct || gotRun != run || !gotStamp.Equal(stamp) {
		t.Fatalf("marker round trip: acct=%q run=%v stamp=%v ok=%v", gotAcct, gotRun, gotStamp, ok)
	}
	// A run with no account is still a valid marker (empty account id).
	if _, _, _, ok := decodeMarker(markerValue("", run, stamp)); !ok {
		t.Error("empty-account marker did not decode")
	}
	// A value too short to hold a run id and a stamp is not a marker.
	if _, _, _, ok := decodeMarker(make([]byte, runIDLen+7)); ok {
		t.Error("short value decoded as a marker")
	}
}

// TestKeysDistinctAndOrdered: keys for different content must differ, and
// piece keys must sort by piece index so a range scan reads a blob's
// pieces in order (including across the decimal-to-binary boundary that a
// naive string index would get wrong: piece 9 before piece 10).
func TestKeysDistinctAndOrdered(t *testing.T) {
	acct := jmap.Id("Aone")
	run := runID{5: 5}

	if !bytes.Equal(manifestKey(acct, "Gx"), manifestKey(acct, "Gx")) {
		t.Error("manifestKey is not deterministic")
	}
	if bytes.Equal(manifestKey(acct, "Gx"), manifestKey(acct, "Gy")) {
		t.Error("different blobIds share a manifest key")
	}
	if bytes.Equal(manifestKey("Aone", "Gx"), manifestKey("Atwo", "Gx")) {
		t.Error("the same blobId shares a key across accounts")
	}

	prev := pieceKey(acct, run, 0)
	for i := uint32(1); i <= 300; i++ {
		cur := pieceKey(acct, run, i)
		if bytes.Compare(prev, cur) >= 0 {
			t.Fatalf("piece key %d does not sort after %d", i, i-1)
		}
		prev = cur
	}

	// A piece key must fall inside its run's scan range and a different
	// run's pieces must fall outside it.
	start, end := pieceRange(acct, run)
	pk := pieceKey(acct, run, 42)
	if bytes.Compare(pk, start) < 0 || bytes.Compare(pk, end) >= 0 {
		t.Error("piece key outside its own run range")
	}
	other := pieceKey(acct, runID{5: 6}, 42)
	if bytes.Compare(other, start) >= 0 && bytes.Compare(other, end) < 0 {
		t.Error("another run's piece fell inside this run's range")
	}
}
