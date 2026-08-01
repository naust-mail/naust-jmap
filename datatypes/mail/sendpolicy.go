package mail

// SendPolicy is the outbound authorization socket: who may send, and as
// which addresses. It is the outbound dual of the inbound Resolver, and
// deliberately not merged with it - the two are different relations (an
// address can be send-only, like an alias the user may write from but
// that receives nothing, or receive-only, like a list). The spec names
// only the error codes this socket feeds (RFC 8621 forbiddenFrom /
// forbiddenMailFrom / forbiddenToSend); what is allowed is host policy,
// so it lives behind an interface, checked at Identity create/update and
// again at submission - an Identity whose grant was later revoked is
// inert, because submission re-checks.

import (
	"context"
	"strings"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/addr"
)

// SendPolicy answers the two outbound authorization questions.
type SendPolicy interface {
	// CanSend reports whether the account may submit mail at all. A false
	// answer becomes the forbiddenToSend SetError, with reason as its
	// description (RFC 8621 section 7.5; empty is fine).
	CanSend(ctx context.Context, acct jmap.Id) (ok bool, reason string)
	// CanSendAs reports whether the account may use addr as a sending
	// address (Identity email, From, envelope mailFrom). addr may be a
	// whole-domain wildcard form ("*@example.com") when an Identity is
	// created with one (section 6): granting the wildcard means granting
	// every address in the domain.
	CanSendAs(ctx context.Context, acct jmap.Id, addr string) bool
}

// StaticSendPolicy is the built-in SendPolicy: a fixed in-memory allow-map,
// DENY by default - an account with no grants can neither send nor hold an
// Identity, because a permissive sending default is an open relay. It is
// configured at setup time and not safe for concurrent mutation.
type StaticSendPolicy struct {
	allow map[jmap.Id][]string
}

// NewStaticSendPolicy returns an empty policy: everything denied.
func NewStaticSendPolicy() *StaticSendPolicy {
	return &StaticSendPolicy{allow: make(map[jmap.Id][]string)}
}

// Allow grants the account the given sending addresses. An entry of the
// form "*@example.com" grants every address in that domain, including the
// wildcard form itself (so the account may hold a wildcard Identity).
func (s *StaticSendPolicy) Allow(acct jmap.Id, addrs ...string) {
	s.allow[acct] = append(s.allow[acct], addrs...)
}

// CanSend implements SendPolicy: an account may send when it has at least
// one grant.
func (s *StaticSendPolicy) CanSend(_ context.Context, acct jmap.Id) (bool, string) {
	if len(s.allow[acct]) == 0 {
		return false, "account has no sending grants"
	}
	return true, ""
}

// CanSendAs implements SendPolicy: addr must match a grant exactly, or a
// wildcard grant for its domain. Domains compare ASCII case-insensitively;
// the local part is exact. A wildcard addr ("*@example.com") matches only
// a wildcard grant - holding one address in a domain does not grant the
// domain.
func (s *StaticSendPolicy) CanSendAs(_ context.Context, acct jmap.Id, address string) bool {
	local, domain, ok := addr.Split(address)
	if !ok {
		return false
	}
	for _, grant := range s.allow[acct] {
		gLocal, gDomain, ok := addr.Split(grant)
		if !ok || !strings.EqualFold(domain, gDomain) {
			continue
		}
		if gLocal == "*" && local != "*" {
			return true
		}
		if gLocal == local {
			return true
		}
	}
	return false
}
