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
	if err := checkValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing content after JSON value")
	}
	return nil
}

func checkValue(dec *json.Decoder) error {
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
			if err := checkValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token() // consume '}'
		return err
	case '[':
		for dec.More() {
			if err := checkValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token() // consume ']'
		return err
	}
	return nil
}
