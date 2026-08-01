package jmap

import (
	"errors"
	"unicode/utf8"

	"github.com/naust-mail/naust-jmap/core/internal/jsonscan"
)

// CheckIJSON validates that data is I-JSON (RFC 7493), which RFC 8620
// section 1.5 requires of every request body: valid UTF-8 throughout,
// exactly one JSON value with no trailing content, and no duplicate
// object member names (encoding/json silently accepts those, last
// wins). The walk is the shared single-pass scanner, so this costs no
// allocations on a well-shaped body. A failure maps to the notJSON
// problem type.
func CheckIJSON(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("body is not valid UTF-8")
	}
	return jsonscan.CheckIJSON(data, maxNestingDepth)
}

// maxNestingDepth bounds how many containers may enclose a value. Without
// a cap, a deeply nested body would recurse the walk until the goroutine
// stack is exhausted and the process crashes - a fatal, unrecoverable
// error that the request-size limit does not prevent (the crash depth is
// well under maxSizeRequest). The limit is deliberately far below the
// stdlib decoder's own 10000: a JMAP request is shallow (the request
// envelope, a method-call tuple, the args, and a filter tree of
// AND/OR/NOT are the only nesting), so this is generous headroom over any
// legitimate request while keeping the recursion tightly bounded.
const maxNestingDepth = 1024
