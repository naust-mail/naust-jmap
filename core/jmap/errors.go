package jmap

import "encoding/json"

// Request-level problem types (RFC 8620 section 3.6.1), returned as an
// RFC 7807 problem-details body with an HTTP error status.
const (
	ProblemUnknownCapability = "urn:ietf:params:jmap:error:unknownCapability"
	ProblemNotJSON           = "urn:ietf:params:jmap:error:notJSON"
	ProblemNotRequest        = "urn:ietf:params:jmap:error:notRequest"
	ProblemLimit             = "urn:ietf:params:jmap:error:limit"
)

// RequestError is an RFC 7807 problem-details object.
type RequestError struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitzero"`
	// Limit names the exceeded limit; required when Type is ProblemLimit.
	Limit string `json:"limit,omitzero"`
}

// Method-level error types (RFC 8620 section 3.6.2 plus the additional
// errors defined by the standard methods in section 5).
const (
	ErrServerUnavailable      = "serverUnavailable"
	ErrServerFail             = "serverFail"
	ErrServerPartialFail      = "serverPartialFail"
	ErrUnknownMethod          = "unknownMethod"
	ErrInvalidArguments       = "invalidArguments"
	ErrInvalidResultReference = "invalidResultReference"
	ErrForbidden              = "forbidden"
	ErrAccountNotFound        = "accountNotFound"
	ErrAccountNotSupported    = "accountNotSupportedByMethod"
	ErrAccountReadOnly        = "accountReadOnly"

	ErrRequestTooLarge         = "requestTooLarge"
	ErrCannotCalculateChanges  = "cannotCalculateChanges"
	ErrTooManyChanges          = "tooManyChanges"
	ErrStateMismatch           = "stateMismatch"
	ErrAnchorNotFound          = "anchorNotFound"
	ErrUnsupportedSort         = "unsupportedSort"
	ErrUnsupportedFilter       = "unsupportedFilter"
	ErrFromAccountNotFound     = "fromAccountNotFound"
	ErrFromAccountNotSupported = "fromAccountNotSupportedByMethod"
)

// MethodError is the arguments object of an "error" response.
type MethodError struct {
	Type        string `json:"type"`
	Description string `json:"description,omitzero"`
}

// ErrorInvocation builds the ["error", {...}, callID] response for a
// failed method call (section 3.6.2). Built via MarshalCompactJSON, not
// json.Marshal directly: see reply()'s comment in core/runtime/standard.go
// for why response Args must come from an audited construction site.
func ErrorInvocation(callID string, e MethodError) Invocation {
	args, err := MarshalCompactJSON(e)
	if err != nil {
		args = CompactJSON(`{"type":"serverFail"}`)
	}
	return Invocation{Name: "error", Args: json.RawMessage(args), CallID: callID}
}

// SetError types (RFC 8620 section 5.3 plus /copy's alreadyExists).
const (
	SetErrForbidden         = "forbidden"
	SetErrOverQuota         = "overQuota"
	SetErrTooLarge          = "tooLarge"
	SetErrRateLimit         = "rateLimit"
	SetErrNotFound          = "notFound"
	SetErrInvalidPatch      = "invalidPatch"
	SetErrWillDestroy       = "willDestroy"
	SetErrInvalidProperties = "invalidProperties"
	SetErrSingleton         = "singleton"
	SetErrAlreadyExists     = "alreadyExists"
)

// SetError describes why one record in a /set call was rejected.
type SetError struct {
	Type        string `json:"type"`
	Description string `json:"description,omitzero"`
	// Properties lists the invalid properties; SHOULD be set when Type is
	// invalidProperties.
	Properties []string `json:"properties,omitzero"`
	// ExistingId accompanies alreadyExists on /copy.
	ExistingId Id `json:"existingId,omitzero"`
	// NotFound accompanies blobNotFound on Email/set create (RFC 8621
	// section 4.6): every referenced blobId that could not be found.
	NotFound []Id `json:"notFound,omitzero"`
	// MaxSize accompanies tooLarge where the defining spec requires the
	// limit on the error itself (RFC 8621 section 7.5: the maximum size
	// of a message that may be sent, in octets).
	MaxSize uint64 `json:"maxSize,omitzero"`
	// MaxRecipients accompanies tooManyRecipients on EmailSubmission/set
	// create (RFC 8621 section 7.5).
	MaxRecipients uint64 `json:"maxRecipients,omitzero"`
	// InvalidRecipients accompanies invalidRecipients on
	// EmailSubmission/set create (RFC 8621 section 7.5): the rcptTo
	// addresses that are not valid for sending to.
	InvalidRecipients []string `json:"invalidRecipients,omitzero"`
}
