// Package runtime executes the JMAP protocol: request dispatch,
// back-reference resolution, capability enforcement, and (in later
// steps) the session endpoint and standard method derivation.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
)

// Call is one method invocation being processed.
type Call struct {
	Name   string
	Args   json.RawMessage
	CallID string
	// Identity is the authenticated caller; standard methods check its
	// account access (nil only in tests that bypass HTTP).
	Identity *auth.Identity
	// CreatedIds is the single request-wide creation-id map (RFC 8620
	// section 5.3): /set handlers add entries; "#id" references read it.
	CreatedIds map[jmap.Id]jmap.Id
}

// Handler processes one method call and returns the response
// invocations to append (a method may emit more than one, all sharing
// the request's call id).
type Handler func(ctx context.Context, call *Call) []jmap.Invocation

// Processor holds the method table and executes requests.
type Processor struct {
	methods      map[string]method
	capabilities map[string]bool
	// MaxCallsInRequest bounds methodCalls length; zero means no bound.
	MaxCallsInRequest int
}

type method struct {
	handler    Handler
	capability string
}

// NewProcessor returns a Processor with the core capability and
// Core/echo registered.
func NewProcessor() *Processor {
	p := &Processor{
		methods:      make(map[string]method),
		capabilities: map[string]bool{jmap.CoreCapability: true},
	}
	p.Register("Core/echo", jmap.CoreCapability, echo)
	return p
}

// Register adds a method. Its capability is advertised as supported and
// the method is callable only when the request opts in to it.
func (p *Processor) Register(name, capability string, h Handler) {
	p.methods[name] = method{handler: h, capability: capability}
	p.capabilities[capability] = true
}

// Supports reports whether a capability URI is registered.
func (p *Processor) Supports(capability string) bool { return p.capabilities[capability] }

func echo(_ context.Context, call *Call) []jmap.Invocation {
	// call.Args is the client's own request bytes: CheckIJSON already proved
	// it's syntactically valid JSON before ParseRequest ran, but not that it's
	// compact - a client is free to send pretty-printed args. Response.WriteJSON
	// assumes every response Invocation's Args is compact (see reply()'s
	// comment in standard.go), so this goes through the same MarshalCompactJSON
	// every other construction site uses - json.RawMessage implements
	// json.Marshaler, so Marshal(json.RawMessage(x)) compacts x the same way
	// Marshal(anyOtherValue) would, with no separate code path needed.
	args, err := jmap.MarshalCompactJSON(json.RawMessage(call.Args))
	if err != nil {
		return []jmap.Invocation{jmap.ErrorInvocation(call.CallID, jmap.MethodError{Type: jmap.ErrServerFail})}
	}
	return []jmap.Invocation{{Name: "Core/echo", Args: json.RawMessage(args), CallID: call.CallID}}
}

// CheckUsing validates the request's capability opt-ins, returning a
// request-level unknownCapability problem if any is unsupported
// (RFC 8620 section 3.6.1).
func (p *Processor) CheckUsing(req *jmap.Request) *jmap.RequestError {
	for _, c := range req.Using {
		if !p.capabilities[c] {
			return &jmap.RequestError{
				Type:   jmap.ProblemUnknownCapability,
				Status: 400,
				Detail: fmt.Sprintf("capability %q is not supported by this server", c),
			}
		}
	}
	return nil
}

// Process executes the request's method calls sequentially and returns
// the response. sessionState is stamped onto the response (section 3.4).
// CheckUsing and request limits must have been enforced by the caller.
func (p *Processor) Process(ctx context.Context, req *jmap.Request, ident *auth.Identity, sessionState string) *jmap.Response {
	resp := &jmap.Response{
		MethodResponses: make([]jmap.Invocation, 0, len(req.MethodCalls)),
		SessionState:    sessionState,
	}
	using := make(map[string]bool, len(req.Using))
	for _, c := range req.Using {
		using[c] = true
	}
	createdIds := req.CreatedIds
	if createdIds == nil {
		createdIds = make(map[jmap.Id]jmap.Id)
	}
	for _, inv := range req.MethodCalls {
		resp.MethodResponses = append(resp.MethodResponses, p.processCall(ctx, inv, ident, using, createdIds, resp.MethodResponses)...)
	}
	// createdIds is returned only when the request supplied it (3.4).
	if req.CreatedIds != nil {
		resp.CreatedIds = createdIds
	}
	return resp
}

func (p *Processor) processCall(ctx context.Context, inv jmap.Invocation, ident *auth.Identity, using map[string]bool, createdIds map[jmap.Id]jmap.Id, prior []jmap.Invocation) (out []jmap.Invocation) {
	// A panicking method must not take the request down; it failed alone
	// (crash-only per-request isolation). The panic value is logged
	// server-side but never returned to the client: it can carry internal
	// state or a fragment of the data being processed, so the response
	// carries only a generic serverFail (RFC 8620 section 3.6.2).
	defer func() {
		if r := recover(); r != nil {
			log.Printf("naust-jmap: recovered panic in method %q: %v", inv.Name, r)
			out = []jmap.Invocation{jmap.ErrorInvocation(inv.CallID, jmap.MethodError{
				Type:        jmap.ErrServerFail,
				Description: "internal error",
			})}
		}
	}()

	m, known := p.methods[inv.Name]
	// The server behaves as though non-opted capabilities do not exist
	// (section 3.3), so a real method outside "using" is unknownMethod.
	if !known || !using[m.capability] {
		return []jmap.Invocation{jmap.ErrorInvocation(inv.CallID, jmap.MethodError{Type: jmap.ErrUnknownMethod})}
	}
	args, rerr := resolveBackrefs(inv.Args, prior)
	if rerr != nil {
		return []jmap.Invocation{jmap.ErrorInvocation(inv.CallID, *rerr)}
	}
	call := &Call{Name: inv.Name, Args: args, CallID: inv.CallID, Identity: ident, CreatedIds: createdIds}
	return m.handler(ctx, call)
}
