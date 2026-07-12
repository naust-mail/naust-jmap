package message

import (
	"reflect"
	"testing"
	"time"
)

func TestMessageIDsForm(t *testing.T) {
	cases := []struct {
		name, raw string
		want      []string
	}{
		{"single", " <1234@local.machine.example>", []string{"1234@local.machine.example"}},
		{"multiple", " <a@b.example> <c@d.example>", []string{"a@b.example", "c@d.example"}},
		{"comment stripped", " (added by relay) <a@b.example>", []string{"a@b.example"}},
		{"folding wsp inside removed", " <a\r\n @b.example>", []string{"a@b.example"}},
		{"no brackets fails", " not-a-msg-id", nil},
		{"empty", "", nil},
	}
	for _, c := range cases {
		if got := MessageIDsForm(c.raw); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: MessageIDsForm(%q) = %v, want %v", c.name, c.raw, got, c.want)
		}
	}
}

func TestURLsForm(t *testing.T) {
	// Shapes from RFC 2369.
	raw := " <ftp://ftp.host.com/list.txt> (FTP),\r\n <mailto:list@host.com?subject=help>"
	want := []string{"ftp://ftp.host.com/list.txt", "mailto:list@host.com?subject=help"}
	if got := URLsForm(raw); !reflect.DeepEqual(got, want) {
		t.Errorf("URLsForm = %v, want %v", got, want)
	}
	if got := URLsForm(" NO (this list does not allow unsubscription)"); got != nil {
		t.Errorf("URLsForm without brackets = %v, want nil", got)
	}
}

func TestDateForm(t *testing.T) {
	got := DateForm(" Fri, 21 Nov 1997 09:55:06 -0600")
	if got == nil {
		t.Fatal("DateForm returned nil for a valid date")
	}
	want := time.Date(1997, 11, 21, 9, 55, 6, 0, time.FixedZone("", -6*3600))
	if !got.Equal(want) {
		t.Errorf("DateForm = %v, want %v", got, want)
	}
	if DateForm(" not a date") != nil {
		t.Error("DateForm parsed garbage")
	}
	if DateForm(" 21 Nov 97 09:55:06 GMT") == nil {
		t.Error("DateForm rejected obsolete two-digit year form")
	}
}
