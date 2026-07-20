package mail

// stripBcc tests: the RFC 8621 section 7.5 server duty. The body must
// pass through byte-identical - only header-section Bcc fields (with
// their folded continuations) disappear.

import (
	"io"
	"strings"
	"testing"
)

func stripped(t *testing.T, in string) string {
	t.Helper()
	out, err := io.ReadAll(stripBcc(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestStripBcc(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "simple",
			in:   "From: a@x\r\nBcc: hidden@x\r\nTo: b@x\r\n\r\nbody\r\n",
			want: "From: a@x\r\nTo: b@x\r\n\r\nbody\r\n",
		},
		{
			name: "case insensitive",
			in:   "From: a@x\r\nBCC: hidden@x\r\nbCc: two@x\r\n\r\nbody\r\n",
			want: "From: a@x\r\n\r\nbody\r\n",
		},
		{
			name: "folded continuation dropped",
			in:   "From: a@x\r\nBcc: hidden@x,\r\n\tmore@x,\r\n even-more@x\r\nTo: b@x\r\n\r\nbody\r\n",
			want: "From: a@x\r\nTo: b@x\r\n\r\nbody\r\n",
		},
		{
			name: "fold after a kept field survives",
			in:   "To: b@x,\r\n\tc@x\r\nBcc: hidden@x\r\n\r\nbody\r\n",
			want: "To: b@x,\r\n\tc@x\r\n\r\nbody\r\n",
		},
		{
			name: "body is untouched even if it looks like a header",
			in:   "From: a@x\r\n\r\nBcc: not-a-header@x\r\nplain text\r\n",
			want: "From: a@x\r\n\r\nBcc: not-a-header@x\r\nplain text\r\n",
		},
		{
			name: "bare LF endings",
			in:   "From: a@x\nBcc: hidden@x\nTo: b@x\n\nbody\n",
			want: "From: a@x\nTo: b@x\n\nbody\n",
		},
		{
			name: "no bcc is byte identical",
			in:   "From: a@x\r\nTo: b@x\r\n\r\nbody line one\r\nbody line two\r\n",
			want: "From: a@x\r\nTo: b@x\r\n\r\nbody line one\r\nbody line two\r\n",
		},
		{
			name: "Bcc-prefixed field name is not Bcc",
			in:   "Bcc-Like: keep@x\r\nBcc: drop@x\r\n\r\nbody\r\n",
			want: "Bcc-Like: keep@x\r\n\r\nbody\r\n",
		},
		{
			name: "obsolete WSP before colon",
			in:   "From: a@x\r\nBcc : hidden@x\r\n\r\nbody\r\n",
			want: "From: a@x\r\n\r\nbody\r\n",
		},
		{
			name: "headers only no body",
			in:   "From: a@x\r\nBcc: hidden@x\r\n",
			want: "From: a@x\r\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripped(t, c.in); got != c.want {
				t.Errorf("got:\n%q\nwant:\n%q", got, c.want)
			}
		})
	}
}

// TestStripBccSmallReads exercises the reader with a 1-byte destination
// buffer, the pathological io.Reader consumer.
func TestStripBccSmallReads(t *testing.T) {
	in := "From: a@x\r\nBcc: hidden@x,\r\n\tfold@x\r\nTo: b@x\r\n\r\nbody\r\n"
	want := "From: a@x\r\nTo: b@x\r\n\r\nbody\r\n"
	r := stripBcc(strings.NewReader(in))
	var out []byte
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}
}
