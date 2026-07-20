package message

// Write-side twins of the parsed-form functions (forms.go, word.go,
// address.go): serialize the RFC 8621 section 4.1.2 forms back into raw
// header values for message generation (RFC 8621 section 4.6). Each
// Format* function returns the value as a list of atoms - units that must
// not be split across a fold - which FoldValue joins into a folded header
// value (RFC 5322 section 2.2.3). Non-ASCII text becomes RFC 2047
// encoded-words, the inverse of decodeWords.

import (
	"encoding/base64"
	"strings"
	"time"
)

// foldLimit is the line length FoldValue folds toward (RFC 5322 section
// 2.1.1: lines SHOULD be no more than 78 characters). A single atom longer
// than the limit is written unbroken; the hard 998 limit is the caller's
// concern.
const foldLimit = 78

// FoldValue joins atoms into a header value for the named field, folding
// with CRLF + space before any atom that would push the line past
// foldLimit. The returned value carries no leading space and no
// terminating CRLF (the HeaderField.Value convention).
func FoldValue(name string, atoms []string) string {
	var b strings.Builder
	line := len(name) + 2 // "Name: "
	for i, atom := range atoms {
		if i > 0 {
			if line+1+len(atom) > foldLimit {
				b.WriteString("\r\n ")
				line = 1
			} else {
				b.WriteString(" ")
				line++
			}
		}
		b.WriteString(atom)
		line += len(atom)
	}
	return b.String()
}

// EncodeText serializes an unstructured header value (the Text form,
// RFC 8621 4.1.2.2) into atoms. A value of printable ASCII is split at
// its single spaces so it can fold; one with white space folding would
// not reproduce (runs of spaces, tabs, edge spaces) stays a single atom;
// anything else is emitted as a run of RFC 2047 "B" encoded-words, whose
// interior white space survives decoding because it is inside the
// payloads (white space between adjacent encoded-words is dropped when
// decoded, RFC 2047 section 6.2 - the inverse of decodeWords).
func EncodeText(s string) []string {
	if !isPrintableASCII(s) {
		return encodeWordsB(s)
	}
	if s == strings.Join(strings.Fields(s), " ") {
		return strings.Fields(s)
	}
	return []string{s}
}

// maxEncodedWordSource is how many source octets one encoded-word carries:
// base64 of 45 octets is 60 characters, keeping "=?utf-8?B?...?=" within
// the 75-character encoded-word limit (RFC 2047 section 2).
const maxEncodedWordSource = 45

// encodeWordsB encodes s as a sequence of B encoded-words, splitting at
// rune boundaries so no word carries a partial UTF-8 sequence.
func encodeWordsB(s string) []string {
	var words []string
	for len(s) > 0 {
		n := len(s)
		if n > maxEncodedWordSource {
			n = maxEncodedWordSource
			for n > 0 && s[n]&0xc0 == 0x80 { // do not split a rune
				n--
			}
		}
		words = append(words, "=?utf-8?B?"+base64.StdEncoding.EncodeToString([]byte(s[:n]))+"?=")
		s = s[n:]
	}
	return words
}

// isPrintableASCII reports whether s is entirely printable ASCII and space
// - a value safe to write into a header verbatim.
func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// atext characters beyond alphanumerics (RFC 5322 section 3.2.3).
const atextSpecials = "!#$%&'*+-/=?^_`{|}~"

func isAtext(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return strings.IndexByte(atextSpecials, c) >= 0
	}
}

// isQuotedString reports whether s is one complete quoted-string
// (RFC 5322 section 3.2.4): quoted from end to end, every interior quote
// or backslash part of a quoted-pair.
func isQuotedString(s string) bool {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return false
	}
	for i := 1; i < len(s)-1; i++ {
		switch s[i] {
		case '\\':
			i++
			if i == len(s)-1 {
				return false // the pair consumed the closing quote
			}
		case '"':
			return false // an unescaped quote ends the string early
		}
	}
	return true
}

// isDotAtom reports whether s is a dot-atom (RFC 5322 section 3.2.3):
// atext runs joined by single dots.
func isDotAtom(s string) bool {
	if s == "" || s[0] == '.' || s[len(s)-1] == '.' || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '.' && !isAtext(s[i]) {
			return false
		}
	}
	return true
}

// encodePhrase serializes a display-name (RFC 5322 phrase): atom text as
// is, ASCII with specials as a quoted-string, anything else as
// encoded-words. The inverse of displayName.
func encodePhrase(name string) []string {
	if !isPrintableASCII(name) {
		return encodeWordsB(name)
	}
	plain := true
	for _, w := range strings.Fields(name) {
		if !isDotAtom(w) {
			plain = false
			break
		}
	}
	if plain && name == strings.Join(strings.Fields(name), " ") {
		return strings.Fields(name)
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(name); i++ {
		if name[i] == '"' || name[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(name[i])
	}
	b.WriteByte('"')
	return []string{b.String()}
}

// FormatAddrSpec serializes one addr-spec, quoting the local part when it
// is not a dot-atom. ok is false when the address cannot be represented in
// a header: it is not printable ASCII or has an empty side (EAI addresses,
// RFC 6532, are out of scope).
func FormatAddrSpec(email string) (string, bool) {
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || !isPrintableASCII(email) {
		return "", false
	}
	local, domain := email[:at], email[at+1:]
	if strings.ContainsAny(domain, " \t\"\\()<>,;:") {
		return "", false
	}
	// A local part in dot-atom form, or already a well-formed quoted-string
	// (which is how the parse side hands a quoted local part back), passes
	// through; anything else gets quoted here.
	if isDotAtom(local) || isQuotedString(local) {
		return email, true
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(local); i++ {
		if local[i] == '"' || local[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(local[i])
	}
	b.WriteString(`"@`)
	b.WriteString(domain)
	return b.String(), true
}

// FormatAddresses serializes an EmailAddress list (the Addresses form,
// RFC 8621 4.1.2.3) into atoms, one "phrase <addr-spec>," (or bare
// addr-spec) mailbox per element. ok is false when any address fails
// FormatAddrSpec.
func FormatAddresses(list []Address) ([]string, bool) {
	var atoms []string
	for i, a := range list {
		spec, ok := FormatAddrSpec(a.Email)
		if !ok {
			return nil, false
		}
		var mailbox []string
		if a.Name != nil && *a.Name != "" {
			mailbox = append(encodePhrase(*a.Name), "<"+spec+">")
		} else {
			mailbox = []string{spec}
		}
		if i < len(list)-1 {
			mailbox[len(mailbox)-1] += ","
		}
		atoms = append(atoms, mailbox...)
	}
	return atoms, true
}

// FormatGroupedAddresses serializes an EmailAddressGroup list (the
// GroupedAddresses form, RFC 8621 4.1.2.4) into atoms. A nil-named group's
// members are written bare; a named group is "phrase : members ;".
func FormatGroupedAddresses(groups []AddressGroup) ([]string, bool) {
	var atoms []string
	for gi, g := range groups {
		members, ok := FormatAddresses(g.Addresses)
		if !ok {
			return nil, false
		}
		if g.Name != nil {
			name := encodePhrase(*g.Name)
			name[len(name)-1] += ":"
			atoms = append(atoms, name...)
			if len(members) == 0 {
				atoms[len(atoms)-1] += ";"
			}
		}
		if len(members) > 0 {
			last := &members[len(members)-1]
			if g.Name != nil {
				*last += ";"
			}
			atoms = append(atoms, members...)
		}
		if gi < len(groups)-1 {
			atoms[len(atoms)-1] += ","
		}
	}
	return atoms, true
}

// FormatMessageIDs serializes a msg-id list (the MessageIds form, RFC 8621
// 4.1.2.5) into "<id>" atoms. ok is false when an id contains characters
// that cannot appear in a msg-id.
func FormatMessageIDs(ids []string) ([]string, bool) {
	atoms := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || !isPrintableASCII(id) || strings.ContainsAny(id, " <>") {
			return nil, false
		}
		atoms = append(atoms, "<"+id+">")
	}
	return atoms, true
}

// FormatURLs serializes a URL list (the URLs form, RFC 8621 4.1.2.7,
// RFC 2369) into "<url>," atoms. ok is false when a URL contains
// characters that cannot appear inside the angle brackets.
func FormatURLs(urls []string) ([]string, bool) {
	atoms := make([]string, 0, len(urls))
	for i, u := range urls {
		if u == "" || !isPrintableASCII(u) || strings.ContainsAny(u, " <>") {
			return nil, false
		}
		atom := "<" + u + ">"
		if i < len(urls)-1 {
			atom += ","
		}
		atoms = append(atoms, atom)
	}
	return atoms, true
}

// FormatDate serializes a time as an RFC 5322 date-time (section 3.3), the
// inverse of DateForm.
func FormatDate(t time.Time) string {
	return t.Format("Mon, 2 Jan 2006 15:04:05 -0700")
}
