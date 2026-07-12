package message

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPreviewTruncation(t *testing.T) {
	long := strings.Repeat("wörd ", 100) // multibyte, ~500 chars
	m := Parse([]byte("Content-Type: text/plain; charset=utf-8\r\n\r\n" + long))
	if n := utf8.RuneCountInString(m.Preview); n != previewMaxChars {
		t.Errorf("preview length = %d runes, want %d", n, previewMaxChars)
	}
	if !utf8.ValidString(m.Preview) {
		t.Error("preview is not valid UTF-8")
	}
}

func TestPreviewCollapsesWhitespace(t *testing.T) {
	m := Parse([]byte("\r\n\r\n  hello\t\r\n\r\n   world  \r\n"))
	if m.Preview != "hello world" {
		t.Errorf("preview = %q", m.Preview)
	}
}

func TestPreviewHTMLFallback(t *testing.T) {
	body := `<html><head><style>p { color: red; }</style></head>` +
		`<body><script>alert("no");</script><p>Caf&eacute;? Tom &amp; Jerry &lt;3</p></body></html>`
	m := Parse([]byte("Content-Type: text/html\r\n\r\n" + body))
	// &eacute; is not in the minimal entity map: kept literally.
	want := "Caf&eacute;? Tom & Jerry <3"
	if m.Preview != want {
		t.Errorf("preview = %q, want %q", m.Preview, want)
	}
}

func TestPreviewPrefersText(t *testing.T) {
	raw := crlf(`Content-Type: multipart/alternative; boundary=bb

--bb
Content-Type: text/plain

plain wins
--bb
Content-Type: text/html

<p>html loses</p>
--bb--
`)
	m := Parse([]byte(raw))
	if m.Preview != "plain wins" {
		t.Errorf("preview = %q", m.Preview)
	}
}
