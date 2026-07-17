package runtime

import (
	"encoding/json"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
)

// Toolkit for custom method handlers - methods a datatype registers with
// Processor.Register beyond the six standard RFC 8620 methods derived from a
// descriptor. RFC 8621 defines several such methods (Email/import,
// Email/parse, SearchSnippet/get); they are not object /get|/set|/query
// shapes, so the runtime cannot derive them, but they still enforce the
// section 3.6.2 account rules and build the same request/response envelopes.
// These exported wrappers expose the primitives the derived methods use so a
// custom handler behaves identically without re-implementing them.

// DecodeArgs strictly decodes method arguments into v; an unknown or mistyped
// argument is an error the caller reports as invalidArguments (RFC 8620
// section 3.6.2).
func DecodeArgs(raw json.RawMessage, v any) error { return decodeArgs(raw, v) }

// CheckAccount validates the accountId argument against the caller's access,
// returning the method error type and description to report, or "" when the
// account is usable (RFC 8620 section 3.6.2: accountNotFound, accountReadOnly).
// needWrite requires write access.
func CheckAccount(call *Call, acct jmap.Id, needWrite bool) (errType, description string) {
	return checkAccount(call, acct, needWrite)
}

// Fail builds the single error response invocation for a method call
// (RFC 8620 section 3.6.2).
func Fail(callID, errType, description string) []jmap.Invocation {
	return fail(callID, errType, description)
}

// Reply marshals args into the response invocation named name for the call
// callID.
func Reply(name, callID string, args any) []jmap.Invocation {
	return reply(name, callID, args)
}

// ValidateFilter validates a filter argument against a type's FilterCondition
// semantics exactly as Foo/query does (RFC 8620 section 5.5), returning the
// method error type and description to report, or "" when the filter is
// acceptable. sem is the type's custom FilterSemantics (nil for the core
// equality language). A custom method that reuses a type's filter language
// (RFC 8621 SearchSnippet/get reuses Email/query's) validates it this way so
// its unsupportedFilter/invalidArguments answers match the query method's.
func ValidateFilter(t *descriptor.Type, sem FilterSemantics, raw json.RawMessage) (errType, description string) {
	_, errType, description = parseFilter(t, sem, raw)
	return errType, description
}
