package mail

import "testing"

// TestBaseSubject exercises the RFC 5256 section 2.1 base-subject
// extraction that the RFC 8621 section 3 subject test relies on. The
// cases are drawn directly from the ABNF constructs: subj-refwd
// ("Re:"/"Fw:"/"Fwd:"), subj-blob ("[tag]"), subj-trailer ("(fwd)"),
// and the subj-fwd "[fwd: ... ]" envelope, plus the boundary the ABNF
// draws (a colon-less "Reply:" is not a refwd).
func TestBaseSubject(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// No decoration.
		{"Dinner on Thursday?", "Dinner on Thursday?"},
		// A single refwd leader, case-insensitive, with and without space.
		{"Re: Hello", "Hello"},
		{"RE: Hello", "Hello"},
		{"re:Hello", "Hello"},
		{"Fwd: Hello", "Hello"},
		{"Fw: Hello", "Hello"},
		// Stacked leaders.
		{"Re: RE: re: Hello", "Hello"},
		{"Fwd: Re: Hello", "Hello"},
		// A blob (list tag) leader, before and after the refwd.
		{"[list] Re: Hello", "Hello"},
		{"Re: [list] Hello", "Hello"},
		{"[list] Hello", "Hello"},
		// A trailing "(fwd)" trailer and surrounding whitespace.
		{"Hello (fwd)", "Hello"},
		{"  Re:   Hello  ", "Hello"},
		// The "[fwd: ... ]" envelope is unwrapped, then re-processed.
		{"[fwd: Re: Hello]", "Hello"},
		// A colon-less token that only looks like a refwd is preserved.
		{"Reply: Hello", "Reply: Hello"},
		{"Recent news", "Recent news"},
		// Removing a blob that would empty the subject is not done: the
		// bracketed text is the whole base.
		{"[nothing else]", "[nothing else]"},
	}
	for _, c := range cases {
		if got := baseSubject(c.in); got != c.want {
			t.Errorf("baseSubject(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBaseSubjectThreadEquality checks the equality the threading subject
// test actually asks: a reply's decorated subject reduces to the same
// base as the original, while a genuinely different subject does not.
func TestBaseSubjectThreadEquality(t *testing.T) {
	if baseSubject("Dinner on Thursday?") != baseSubject("Re: Dinner on Thursday?") {
		t.Error("a reply subject should share the original's base subject")
	}
	if baseSubject("Dinner on Thursday?") == baseSubject("Lunch on Friday?") {
		t.Error("different subjects should not share a base subject")
	}
}
