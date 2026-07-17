package message

// The streaming decoder must produce exactly the text the whole-buffer decode
// produces, whatever charset the part declares and however the content is cut up
// on its way in - the seams a stream has and a buffer does not are a character
// split across two writes, a CRLF split across two writes, and a malformed run
// that begins in one write and ends in another. These tests hold the two against
// each other, so the value a client is shown does not depend on how the message
// happened to arrive.

import (
	"bytes"
	"strings"
	"testing"
)

// charsets covers what a real message declares: the implicit us-ascii of RFC
// 2045 5.2, UTF-8, a single-byte legacy charset, a multi-byte one (where a
// character really can straddle a write), and a name that means nothing.
var charsets = []string{"", "us-ascii", "utf-8", "iso-8859-1", "windows-1252", "shift_jis", "utf-16", "x-nonsense"}

// decodeStreamed decodes data through the streaming writer in n-octet writes.
func decodeStreamed(data []byte, charset *string, n int) (string, bool) {
	var out bytes.Buffer
	w := NewTextWriter(&out, charset)
	for len(data) > 0 {
		take := n
		if take > len(data) {
			take = len(data)
		}
		if _, err := w.Write(data[:take]); err != nil {
			panic(err)
		}
		data = data[take:]
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return out.String(), w.Problem()
}

// textCases are the bodies whose decoding a stream could plausibly get wrong.
var textCases = []string{
	"",
	"hello",
	"line one\r\nline two\r\n",
	"bare \r carriage return",
	"\r",
	"\r\n",
	"\r\r\n",
	"\n\r",
	"ends in cr\r",
	"caf\xc3\xa9 \xe2\x82\xac \xf0\x9f\x92\xa9", // multi-byte characters
	"caf\xc3",                          // a character cut off at the end
	"a\xff\xfe\xfdb",                   // a malformed run in the middle
	"\xff\xff",                         // nothing but malformed octets
	"\xe2\x82",                         // an unfinished character, then EOF
	"caf\xe9 na\xefve",                 // latin-1 octets
	"\x82\xa0\x82\xa2\x82\xa4",         // shift_jis kana
	"valid \xef\xbf\xbd already",       // a U+FFFD that is content, not a substitution
	"crlf inside a rune: \xc3\r\n\xa9", // a broken character around a line ending
	strings.Repeat("long ", 5000) + "\xff" + "\r", // past any internal buffer
}

// TestTextWriterMatchesDecodeBody: for every charset, every body, and every
// write size, the streamed text and problem flag are the buffered ones.
func TestTextWriterMatchesDecodeBody(t *testing.T) {
	for _, cs := range charsets {
		charset := &cs
		if cs == "" {
			charset = nil // the implicit us-ascii
		}
		for _, body := range textCases {
			want, wantProblem := DecodeBody([]byte(body), charset)
			for _, n := range []int{1, 2, 3, 7, 64, 4096, len(body) + 1} {
				if n <= 0 {
					continue
				}
				got, gotProblem := decodeStreamed([]byte(body), charset, n)
				if got != want || gotProblem != wantProblem {
					t.Errorf("charset %q, body %q, %d-octet writes:\n got %q problem=%v\nwant %q problem=%v",
						cs, body, n, got, gotProblem, want, wantProblem)
				}
			}
		}
	}
}

// FuzzTextWriter is the same claim over arbitrary content: any octets, any
// charset, any write size, one answer.
func FuzzTextWriter(f *testing.F) {
	for i, body := range textCases {
		f.Add([]byte(body), uint8(i%len(charsets)), uint8(1))
		f.Add([]byte(body), uint8(i%len(charsets)), uint8(3))
	}
	f.Fuzz(func(t *testing.T, data []byte, cs uint8, size uint8) {
		charset := &charsets[int(cs)%len(charsets)]
		if *charset == "" {
			charset = nil
		}
		n := int(size)
		if n == 0 {
			n = 1
		}
		want, wantProblem := DecodeBody(data, charset)
		got, gotProblem := decodeStreamed(data, charset, n)
		if got != want || gotProblem != wantProblem {
			t.Errorf("charset %v, %d-octet writes:\n got %q problem=%v\nwant %q problem=%v",
				charset, n, got, gotProblem, want, wantProblem)
		}
	})
}
