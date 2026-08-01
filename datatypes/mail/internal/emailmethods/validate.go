package emailmethods

// Email/set mailboxIds/keywords validation (RFC 8621 section 4.1.1),
// shared by the Set.Validate hook (root email.go) and the materialize
// seam's commit half (materialize.go), so it lives here rather than in
// root: root cannot be imported back by this package.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// maxKeywordsPerEmail bounds an Email's keyword set. RFC 8621 defines
// tooManyKeywords but advertises no capability field for the limit: it is
// server-defined. 100 is generous for real mail.
const maxKeywordsPerEmail = 100

// keywordForbidden is the set of characters a keyword MUST NOT contain
// (RFC 8621 section 4.1.1): ( ) { ] % * " \. Note the spec lists the
// closing bracket and opening brace only.
const keywordForbidden = `(){]%*"\`

// ValidateMailboxIds enforces the "belongs to >= 1 Mailbox" invariant
// (RFC 8621 section 4.1.1), that every value is true, that every key is an
// existing Mailbox, and the tooManyMailboxes limit. It normalizes nothing.
func ValidateMailboxIds(u *objectdb.Update, new objectdb.Object, maxMailboxes *int64) (*jmap.SetError, error) {
	members, ok := decodeBoolMap(new["mailboxIds"])
	if !ok {
		return invalidProp("mailboxIds", "must be an object of Mailbox id to true"), nil
	}
	if len(members) == 0 {
		return invalidProp("mailboxIds", "an Email must be in at least one Mailbox"), nil
	}
	if maxMailboxes != nil && int64(len(members)) > *maxMailboxes {
		return &jmap.SetError{Type: "tooManyMailboxes", Description: "too many Mailboxes for one Email"}, nil
	}
	for id, val := range members {
		if !val {
			return invalidProp("mailboxIds", "each value must be true"), nil
		}
		obj, err := u.Get(record.TypeMailbox, jmap.Id(id))
		if errors.Is(err, objectdb.ErrNotFound) || (err == nil && obj == nil) {
			return invalidProp("mailboxIds", fmt.Sprintf("Mailbox %q does not exist", id)), nil
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// ValidateKeywords enforces keyword syntax (RFC 8621 section 4.1.1),
// lowercases keys in place (servers MUST return keywords in lowercase),
// requires every value be true, and enforces tooManyKeywords.
func ValidateKeywords(new objectdb.Object) (*jmap.SetError, error) {
	members, ok := decodeBoolMap(new["keywords"])
	if !ok {
		return invalidProp("keywords", "must be an object of keyword to true"), nil
	}
	if len(members) > maxKeywordsPerEmail {
		return &jmap.SetError{Type: "tooManyKeywords", Description: "too many keywords on one Email"}, nil
	}
	lowered := make(map[string]bool, len(members))
	for kw, val := range members {
		if !val {
			return invalidProp("keywords", "each value must be true"), nil
		}
		if !validKeyword(kw) {
			return invalidProp("keywords", fmt.Sprintf("%q is not a valid keyword", kw)), nil
		}
		lowered[strings.ToLower(kw)] = true
	}
	raw, err := json.Marshal(lowered)
	if err != nil {
		return nil, err
	}
	new["keywords"] = raw
	return nil, nil
}

// validKeyword reports whether s is a valid IMAP/JMAP keyword: 1-255
// characters in ASCII %x21-%x7e with none of keywordForbidden.
func validKeyword(s string) bool {
	if len(s) < 1 || len(s) > 255 {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e || strings.ContainsRune(keywordForbidden, r) {
			return false
		}
	}
	return true
}

// decodeBoolMap decodes a JSON object of string to bool. ok is false if
// the value is absent, null, or not an object of booleans.
func decodeBoolMap(raw json.RawMessage) (map[string]bool, bool) {
	if isNullRaw(raw) {
		return nil, false
	}
	var m map[string]bool
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}
