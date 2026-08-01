// Package addr holds the mail package's address-string helpers: splitting
// and wire-safety checks used by outbound send policy and submission
// validation.
package addr

import "strings"

// Split splits an address at its last "@" into local part and domain,
// both required to be non-empty. It also rejects any address that is not
// wire-safe (see WireSafe): this is the single gate every envelope and
// policy address passes through, so a control character can never reach
// the SMTP command line as a smuggled CR/LF, and "<addr>" framing cannot
// be broken.
func Split(addr string) (local, domain string, ok bool) {
	if !WireSafe(addr) {
		return "", "", false
	}
	i := strings.LastIndex(addr, "@")
	if i <= 0 || i == len(addr)-1 {
		return "", "", false
	}
	return addr[:i], addr[i+1:], true
}

// WireSafe reports whether addr is safe to place inside an SMTP
// "<...>" command argument (RFC 5321): printable US-ASCII with no space,
// no control character, and no angle bracket. CR and LF are the bytes that
// would smuggle a second command onto the wire; angle brackets would break
// the framing. This is deliberately stricter than a full addr-spec parse -
// the goal is wire safety, and no legitimate envelope address needs those
// bytes. (Internationalized addresses would arrive with SMTPUTF8, which
// this relay does not yet speak.)
func WireSafe(addr string) bool {
	return TokenSafe(addr) && !strings.ContainsAny(addr, "<>")
}

// TokenSafe reports whether s is printable ASCII with no white space -
// safe inside an angle-bracketed or parameter context.
func TokenSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return s != ""
}

// IdentityAllows reports whether an Identity with the given email allows
// sending as addr: an exact match (domain ASCII case-insensitive, local
// part exact), or any address in the domain for a wildcard Identity
// (RFC 8621 section 6).
func IdentityAllows(identityEmail, addr string) bool {
	idLocal, idDomain, ok := Split(identityEmail)
	if !ok {
		return false
	}
	local, domain, ok := Split(addr)
	if !ok || !strings.EqualFold(domain, idDomain) {
		return false
	}
	return idLocal == "*" || idLocal == local
}

// IsWildcard reports whether addr is the whole-domain wildcard form.
func IsWildcard(addr string) bool {
	local, _, ok := Split(addr)
	return ok && local == "*"
}
