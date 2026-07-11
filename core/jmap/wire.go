package jmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Invocation is the [name, arguments, methodCallId] triple of RFC 8620
// section 3.2, used for both method calls and responses.
type Invocation struct {
	// Name is the method or response name, e.g. "Email/get" or "error".
	Name string
	// Args holds the arguments object as raw JSON. Methods decode it into
	// their own argument types; nil marshals as an empty object.
	Args json.RawMessage
	// CallID is the client-chosen method call id, echoed on every response
	// that the call produces.
	CallID string
}

// MarshalJSON encodes the invocation as a three-element JSON array.
func (inv Invocation) MarshalJSON() ([]byte, error) {
	args := inv.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	name, err := json.Marshal(inv.Name)
	if err != nil {
		return nil, err
	}
	callID, err := json.Marshal(inv.CallID)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	buf.Write(name)
	buf.WriteByte(',')
	buf.Write(args)
	buf.WriteByte(',')
	buf.Write(callID)
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// UnmarshalJSON decodes a three-element array whose first and third
// elements are strings and whose second is a JSON object.
func (inv *Invocation) UnmarshalJSON(data []byte) error {
	var parts []json.RawMessage
	if err := json.Unmarshal(data, &parts); err != nil {
		return fmt.Errorf("invocation is not an array: %w", err)
	}
	if len(parts) != 3 {
		return fmt.Errorf("invocation has %d elements, want 3", len(parts))
	}
	if err := json.Unmarshal(parts[0], &inv.Name); err != nil {
		return fmt.Errorf("invocation name: %w", err)
	}
	if first := firstByte(parts[1]); first != '{' {
		return errors.New("invocation arguments are not an object")
	}
	inv.Args = parts[1]
	if err := json.Unmarshal(parts[2], &inv.CallID); err != nil {
		return fmt.Errorf("invocation call id: %w", err)
	}
	return nil
}

func firstByte(raw json.RawMessage) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return b
	}
	return 0
}

// Request is the API request object of RFC 8620 section 3.3.
type Request struct {
	// Using is the set of capability URIs the client opts in to.
	Using []string
	// MethodCalls are processed sequentially, in order.
	MethodCalls []Invocation
	// CreatedIds, if non-nil, seeds the request-wide creation-id map
	// (proxy support); nil means the property was absent.
	CreatedIds map[Id]Id
}

// ErrNotRequest is returned by ParseRequest when the body is valid JSON
// but does not match the Request type signature (the notRequest problem).
var ErrNotRequest = errors.New("jmap: body does not match the Request object")

// ParseRequest decodes and validates a Request. The body must already be
// known to be valid I-JSON (see CheckIJSON); this checks the type
// signature only. Unknown properties are ignored per section 3.3.
func ParseRequest(body []byte) (*Request, error) {
	var aux struct {
		Using       *[]string     `json:"using"`
		MethodCalls *[]Invocation `json:"methodCalls"`
		CreatedIds  map[Id]Id     `json:"createdIds"`
	}
	if err := json.Unmarshal(body, &aux); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotRequest, err)
	}
	if aux.Using == nil || aux.MethodCalls == nil {
		return nil, fmt.Errorf("%w: missing using or methodCalls", ErrNotRequest)
	}
	for id := range aux.CreatedIds {
		if !id.Valid() {
			return nil, fmt.Errorf("%w: invalid creation id %q", ErrNotRequest, id)
		}
	}
	return &Request{Using: *aux.Using, MethodCalls: *aux.MethodCalls, CreatedIds: aux.CreatedIds}, nil
}

// Response is the API response object of RFC 8620 section 3.4.
type Response struct {
	MethodResponses []Invocation `json:"methodResponses"`
	// CreatedIds is returned only if the request supplied it; a non-nil
	// empty map round-trips as {} (omitzero omits only the nil map).
	CreatedIds   map[Id]Id `json:"createdIds,omitzero"`
	SessionState string    `json:"sessionState"`
}

// ResultReference selects part of an earlier method's response as an
// argument value (RFC 8620 section 3.7).
type ResultReference struct {
	ResultOf string `json:"resultOf"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}
