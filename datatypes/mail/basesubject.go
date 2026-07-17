package mail

import "strings"

// baseSubject extracts the "base subject" of a subject string per RFC 5256
// section 2.1, used for the RFC 8621 section 3 threading subject test
// (condition 2: two messages thread only if their base subjects match).
// The algorithm strips the automatically added "Re:"/"Fwd:"/"[list]"
// decoration a reply or forward accretes, leaving the stable core.
// Comparison of two base subjects is exact (case-sensitive), as RFC 5256
// preserves the case of the base while matching the prefixes case-
// insensitively.
func baseSubject(subject string) string {
	// (1) Encoded-words are already decoded to text upstream; normalize
	// whitespace: tabs and folding to space, runs of space to one space.
	s := normalizeWhitespace(subject)
	for {
		// (2) Remove trailing "(fwd)"/WSP until none remain.
		s = stripTrailers(s)
		// (3)-(5) Remove leaders and blob prefixes until stable.
		for {
			changed := false
			if r, ok := stripLeader(s); ok {
				s = r
				changed = true
			}
			// (4) Remove a blob prefix only if a non-empty base remains.
			if r, ok := stripBlob(s); ok && strings.TrimSpace(r) != "" {
				s = r
				changed = true
			}
			if !changed {
				break
			}
		}
		// (6) Unwrap "[fwd:" ... "]" and repeat from (2).
		if inner, ok := stripFwd(s); ok {
			s = inner
			continue
		}
		return s
	}
}

// normalizeWhitespace converts every whitespace run to a single space
// (RFC 5256 step 1). Leading and trailing spaces are left for the leader
// and trailer steps to remove.
func normalizeWhitespace(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return b.String()
}

// stripTrailers removes every trailing subj-trailer: "(fwd)" or a space.
func stripTrailers(s string) string {
	for {
		switch {
		case strings.HasSuffix(s, " "):
			s = s[:len(s)-1]
		case strings.HasSuffix(s, "(fwd)"):
			s = s[:len(s)-len("(fwd)")]
		default:
			return s
		}
	}
}

// stripLeader removes one subj-leader from the front: either a single
// leading space, or zero or more blobs followed by a "Re:"/"Fwd:" token
// (subj-refwd). The blobs are consumed only if the refwd follows; a
// dangling blob is left for step (4).
func stripLeader(s string) (string, bool) {
	if strings.HasPrefix(s, " ") {
		return s[1:], true
	}
	rest := s
	for {
		if r, ok := stripBlob(rest); ok {
			rest = r
			continue
		}
		break
	}
	if r, ok := stripRefwd(rest); ok {
		return r, true
	}
	return s, false
}

// stripRefwd removes one subj-refwd: ("re" / "fw" / "fwd"), optional
// space, an optional blob, then a colon (RFC 5256). The tokens match
// case-insensitively; without the trailing colon it is not a refwd (so
// "Reply:" is preserved).
func stripRefwd(s string) (string, bool) {
	rest := s
	switch {
	case hasPrefixFold(rest, "re"):
		rest = rest[2:]
	case hasPrefixFold(rest, "fwd"):
		rest = rest[3:]
	case hasPrefixFold(rest, "fw"):
		rest = rest[2:]
	default:
		return s, false
	}
	for strings.HasPrefix(rest, " ") {
		rest = rest[1:]
	}
	if r, ok := stripBlob(rest); ok {
		rest = r
	}
	if !strings.HasPrefix(rest, ":") {
		return s, false
	}
	return rest[1:], true
}

// stripBlob removes one subj-blob: "[" then any run of non-bracket bytes
// then "]", then any trailing spaces. A "[" with no matching "]" (or a
// nested "[") is not a blob.
func stripBlob(s string) (string, bool) {
	if !strings.HasPrefix(s, "[") {
		return s, false
	}
	i := 1
	for i < len(s) && s[i] != ']' && s[i] != '[' {
		i++
	}
	if i >= len(s) || s[i] != ']' {
		return s, false
	}
	i++ // consume ']'
	for i < len(s) && s[i] == ' ' {
		i++
	}
	return s[i:], true
}

// stripFwd unwraps a whole "[fwd:" ... "]" envelope (subj-fwd-hdr /
// subj-fwd-trl), matching the header case-insensitively.
func stripFwd(s string) (string, bool) {
	if hasPrefixFold(s, "[fwd:") && strings.HasSuffix(s, "]") {
		return s[len("[fwd:") : len(s)-1], true
	}
	return s, false
}

// hasPrefixFold reports whether s begins with prefix, case-insensitively.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}
