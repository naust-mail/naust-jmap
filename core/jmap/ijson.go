package jmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// CheckIJSON validates that data is I-JSON (RFC 7493), which RFC 8620
// section 1.5 requires of every request body. encoding/json silently
// accepts duplicate object member names (last wins), so this walks the
// token stream and rejects them, along with invalid UTF-8 and trailing
// content. A failure maps to the notJSON problem type.
func CheckIJSON(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("body is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := checkValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing content after JSON value")
	}
	return nil
}

// maxNestingDepth bounds how deeply checkValue recurses. The streaming
// json.Decoder.Token API enforces no nesting limit of its own, so without this
// guard a deeply nested body would recurse until the goroutine stack is
// exhausted and the process crashes - a fatal, unrecoverable error that the
// request-size limit does not prevent (the crash depth is well under
// maxSizeRequest). The limit is deliberately far below the stdlib decoder's
// own 10000: a JMAP request is shallow (the request envelope, a method-call
// tuple, the args, and a filter tree of AND/OR/NOT are the only nesting), so
// this is generous headroom over any legitimate request while keeping the
// recursion - and the memory it allocates walking the body - tightly bounded.
const maxNestingDepth = 1024

func checkValue(dec *json.Decoder, depth int) error {
	if depth > maxNestingDepth {
		return fmt.Errorf("JSON nesting exceeds %d levels", maxNestingDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key := keyTok.(string)
			if _, dup := seen[key]; dup {
				return fmt.Errorf("duplicate object member %q", key)
			}
			seen[key] = struct{}{}
			if err := checkValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token() // consume '}'
		return err
	case '[':
		for dec.More() {
			if err := checkValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token() // consume ']'
		return err
	}
	return nil
}
