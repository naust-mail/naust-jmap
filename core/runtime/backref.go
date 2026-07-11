package runtime

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// resolveBackrefs implements RFC 8620 section 3.7: any argument whose
// name starts with "#" holds a ResultReference to be replaced with the
// referenced part of an earlier response before the method runs.
// It returns the rewritten arguments, or a method-level error
// (invalidResultReference on resolution failure; invalidArguments when
// both "foo" and "#foo" are present).
func resolveBackrefs(args json.RawMessage, prior []jmap.Invocation) (json.RawMessage, *jmap.MethodError) {
	if len(args) == 0 {
		return args, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, &jmap.MethodError{Type: jmap.ErrInvalidArguments, Description: "arguments are not an object"}
	}
	changed := false
	for key, raw := range m {
		if !strings.HasPrefix(key, "#") {
			continue
		}
		name := key[1:]
		if _, both := m[name]; both {
			return nil, &jmap.MethodError{
				Type:        jmap.ErrInvalidArguments,
				Description: fmt.Sprintf("argument %q supplied in both normal and referenced form", name),
			}
		}
		var ref jmap.ResultReference
		if err := json.Unmarshal(raw, &ref); err != nil {
			return nil, &jmap.MethodError{Type: jmap.ErrInvalidResultReference, Description: "malformed ResultReference"}
		}
		value, err := resolveReference(ref, prior)
		if err != nil {
			return nil, &jmap.MethodError{Type: jmap.ErrInvalidResultReference, Description: err.Error()}
		}
		delete(m, key)
		m[name] = value
		changed = true
	}
	if !changed {
		return args, nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, &jmap.MethodError{Type: jmap.ErrServerFail, Description: err.Error()}
	}
	return out, nil
}

func resolveReference(ref jmap.ResultReference, prior []jmap.Invocation) (json.RawMessage, error) {
	for _, resp := range prior {
		if resp.CallID != ref.ResultOf {
			continue
		}
		// The FIRST response with the call id decides; its name must match.
		if resp.Name != ref.Name {
			return nil, fmt.Errorf("response name %q does not match reference name %q", resp.Name, ref.Name)
		}
		return evalPointer(resp.Args, ref.Path)
	}
	return nil, fmt.Errorf("no response with call id %q", ref.ResultOf)
}

// evalPointer applies a JSON Pointer (RFC 6901) extended with the "*"
// array-mapping token of RFC 8620 section 3.7.
func evalPointer(doc json.RawMessage, path string) (json.RawMessage, error) {
	if path == "" {
		return doc, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("pointer %q does not start with /", path)
	}
	tokens := strings.Split(path[1:], "/")
	for i, t := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
	}
	return evalTokens(doc, tokens)
}

func evalTokens(doc json.RawMessage, tokens []string) (json.RawMessage, error) {
	if len(tokens) == 0 {
		return doc, nil
	}
	token, rest := tokens[0], tokens[1:]
	switch firstJSONByte(doc) {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(doc, &obj); err != nil {
			return nil, err
		}
		child, ok := obj[token]
		if !ok {
			return nil, fmt.Errorf("no member %q", token)
		}
		return evalTokens(child, rest)
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(doc, &arr); err != nil {
			return nil, err
		}
		if token == "*" {
			return mapArray(arr, rest)
		}
		idx, err := strconv.Atoi(token)
		if err != nil || idx < 0 || idx >= len(arr) {
			return nil, fmt.Errorf("bad array index %q", token)
		}
		return evalTokens(arr[idx], rest)
	default:
		return nil, fmt.Errorf("cannot descend into scalar with token %q", token)
	}
}

// mapArray applies the remaining tokens to every element and returns the
// results as one array; element results that are themselves arrays are
// flattened into the output (section 3.7).
func mapArray(arr []json.RawMessage, rest []string) (json.RawMessage, error) {
	results := make([]json.RawMessage, 0, len(arr))
	for _, item := range arr {
		r, err := evalTokens(item, rest)
		if err != nil {
			return nil, err
		}
		if firstJSONByte(r) == '[' {
			var inner []json.RawMessage
			if err := json.Unmarshal(r, &inner); err != nil {
				return nil, err
			}
			results = append(results, inner...)
		} else {
			results = append(results, r)
		}
	}
	return json.Marshal(results)
}

func firstJSONByte(raw json.RawMessage) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return b
	}
	return 0
}
