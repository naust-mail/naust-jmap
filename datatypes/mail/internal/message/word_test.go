package message

import "testing"

func TestTextForm(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"plain", " hello world", "hello world"},
		{"unfold keeps wsp", " hello\r\n world", "hello world"},
		{"leading wsp removed", "   \thello", "hello"},
		{"q encoded utf8", " =?UTF-8?Q?John_Sm=C3=AEth?=", "John Smîth"},
		{"b encoded", " =?utf-8?B?SGVsbG8=?=", "Hello"},
		{"adjacent words join", "=?utf-8?B?SGVsbG8=?= =?utf-8?B?d29ybGQ=?=", "Helloworld"},
		{"word then plain keeps space", "=?utf-8?Q?Hi?= there", "Hi there"},
		{"plain then word keeps space", "re: =?utf-8?Q?Hi?=", "re: Hi"},
		{"embedded not decoded", "abc=?utf-8?Q?x?=", "abc=?utf-8?Q?x?="},
		{"trailing punct not decoded", "=?utf-8?Q?x?=.", "=?utf-8?Q?x?=."},
		{"unknown charset kept raw", "=?x-mystery?Q?x?=", "=?x-mystery?Q?x?="},
		{"bad base64 replaced", "=?utf-8?B?!!!!?=", "�"},
		{"bad q hex replaced", "=?utf-8?Q?a=ZZ?=", "�"},
		{"iso-8859-1", "=?iso-8859-1?Q?caf=E9?=", "café"},
		{"language tag stripped", "=?utf-8*en?Q?hi?=", "hi"},
		{"controls dropped from decoded", "=?utf-8?Q?a=00b=07c?=", "abc"},
		{"nfc normalized", "=?utf-8?B?ZcyB?=", "é"}, // e + combining acute
		{"q underscore is space", "=?utf-8?Q?a_b?=", "a b"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := TextForm(c.raw); got != c.want {
			t.Errorf("%s: TextForm(%q) = %q, want %q", c.name, c.raw, got, c.want)
		}
	}
}

func TestDecodeBody(t *testing.T) {
	utf8cs := "utf-8"
	latin1 := "iso-8859-1"
	unknown := "x-mystery"

	s, problem := DecodeBody([]byte("a\r\nb\r\n"), &utf8cs)
	if s != "a\nb\n" || problem {
		t.Errorf("crlf: got %q problem=%v", s, problem)
	}
	s, problem = DecodeBody([]byte("caf\xe9"), &latin1)
	if s != "café" || problem {
		t.Errorf("latin1: got %q problem=%v", s, problem)
	}
	s, problem = DecodeBody([]byte("caf\xe9"), &unknown)
	if s != "caf�" || !problem {
		t.Errorf("unknown charset: got %q problem=%v", s, problem)
	}
	s, problem = DecodeBody([]byte("hi\x80there"), nil) // implicit us-ascii
	if s != "hi�there" || !problem {
		t.Errorf("ascii high byte: got %q problem=%v", s, problem)
	}
	s, problem = DecodeBody([]byte("ok"), nil)
	if s != "ok" || problem {
		t.Errorf("ascii clean: got %q problem=%v", s, problem)
	}
}
