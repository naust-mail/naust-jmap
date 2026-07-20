package runtime

import (
	"context"
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

// ResolveIdArg resolves a client-supplied Id that may be a "#creationId"
// reference against the request-wide creation-id map (RFC 8620 section 5.3;
// call.CreatedIds), returning the real id, or false when the reference names
// no created record. A plain id passes through unchanged. Custom methods
// that take record ids (RFC 8621 Email/copy takes the id of the Email to
// copy) resolve them this way so references behave as they do in the
// derived methods.
func ResolveIdArg(id jmap.Id, createdIds map[jmap.Id]jmap.Id) (jmap.Id, bool) {
	return resolveIdArg(id, createdIds)
}

// ImplicitSet runs the /set method of typeName as an implicit continuation
// of the in-flight call orig and returns its response invocations, which the
// caller appends after its own response. This is the mechanism the specs
// mandate when one method's success triggers a follow-up set: /copy's
// onSuccessDestroyOriginal ("the server MUST make a single call to Foo/set"
// after emitting the /copy response, RFC 8620 section 5.4) and
// EmailSubmission/set's onSuccessUpdateEmail/onSuccessDestroyEmail (RFC 8621
// section 7.5). args is marshaled as the set arguments. The synthesized call
// inherits orig's method call id (both responses answer the one client
// call), identity, and creation-id map (a "#creationId" in the built
// arguments resolves against the request-wide map, section 5.3).
//
// The continuation dispatches to whatever handler is registered for
// typeName+"/set" without re-checking capability opt-ins: the client call
// that triggered it was already gated, and the specs mandate the implicit
// set unconditionally (a client using only the submission capability still
// gets its mandatory implicit Email/set). This is deliberately not a general
// invoke-a-method facility: the caller cannot choose the call id or the
// identity, so a method can only continue its own call, never originate one.
func (p *Processor) ImplicitSet(ctx context.Context, typeName string, args any, orig *Call) []jmap.Invocation {
	raw, err := json.Marshal(args)
	if err != nil {
		return fail(orig.CallID, jmap.ErrServerFail, err.Error())
	}
	name := typeName + "/set"
	m, ok := p.methods[name]
	if !ok {
		return fail(orig.CallID, jmap.ErrServerFail, name+" is not registered")
	}
	return m.handler(ctx, &Call{
		Name: name, Args: raw, CallID: orig.CallID,
		Identity: orig.Identity, CreatedIds: orig.CreatedIds,
	})
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
