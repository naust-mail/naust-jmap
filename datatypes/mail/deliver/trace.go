package deliver

// Trace stamping (RFC 5321 section 4.4): when a server receives a message it
// MUST insert a Received time-stamp line at the top, and the server that makes
// final delivery inserts the Return-Path line carrying the envelope
// reverse-path. Both are built here as a header prefix the Deliverer streams
// ahead of the message, so the stored blob and the parsed Email carry them
// identically and the raw message is never buffered to stamp it.
//
// The FOR clause is deliberately never emitted: one stored blob serves every
// envelope recipient of the message (a blobId is a content address, RFC 8620
// section 6.1), so naming a recipient in the stamp would disclose one
// recipient's address inside another recipient's copy. RFC 5321 section 4.4
// makes FOR optional ("MAY include"), and its section 7.6 warns that trace
// fields can disclose such information.

import (
	"net"
	"strings"
	"time"
)

// receivedDate is the RFC 5322 section 3.3 date-time layout used in the
// Received stamp.
const receivedDate = "Mon, 2 Jan 2006 15:04:05 -0700"

// tracePrefix builds the Return-Path and Received header lines for one
// delivery. The Return-Path line (RFC 5321 section 4.4: inserted when the
// final delivery is made) is always present; "" as the reverse-path is the
// null sender and renders as <>. The Received line is present only when the
// envelope names the receiving host (LocalName): a caller that hands the
// Deliverer a message directly, with no transport hop to describe, gets the
// Return-Path alone.
func tracePrefix(env Envelope, now time.Time) string {
	var b strings.Builder
	b.WriteString("Return-Path: <")
	b.WriteString(stripCtl(env.MailFrom))
	b.WriteString(">\r\n")
	if env.LocalName == "" {
		return b.String()
	}

	// Received grammar (RFC 5321 section 4.4): "FROM" Extended-Domain, "BY"
	// Extended-Domain, optional WITH, ";" date-time. The FROM domain is what
	// the peer claimed in LHLO/HELO, with the observed address as the TCP-info
	// comment - the claim is untrusted, so the stamp records both.
	b.WriteString("Received: from ")
	helo := sanitizeTraceToken(env.HeloName)
	peer := sanitizeTraceToken(hostOnly(env.PeerAddr))
	switch {
	case helo != "" && peer != "":
		b.WriteString(helo)
		b.WriteString(" (")
		b.WriteString(addressLiteral(peer))
		b.WriteString(")")
	case helo != "":
		b.WriteString(helo)
	case peer != "":
		b.WriteString(addressLiteral(peer))
	default:
		b.WriteString("unknown")
	}
	b.WriteString(" by ")
	b.WriteString(sanitizeTraceToken(env.LocalName))
	if p := sanitizeTraceToken(env.Protocol); p != "" {
		// WITH values come from the IANA Mail Transmission Types registry
		// (established by RFC 3848; LMTP is registered by RFC 2033). An
		// adapter whose transport has no registered value leaves Protocol
		// empty and the optional WITH clause is omitted.
		b.WriteString(" with ")
		b.WriteString(p)
	}
	b.WriteString("; ")
	b.WriteString(now.Format(receivedDate))
	b.WriteString("\r\n")
	return b.String()
}

// addressLiteral wraps an already-sanitized peer address as an RFC 5321
// section 4.1.3 address literal: [192.0.2.1] for IPv4, [IPv6:2001:db8::1]
// for IPv6 (a colon marks the IPv6 form).
func addressLiteral(peer string) string {
	if strings.Contains(peer, ":") {
		return "[IPv6:" + peer + "]"
	}
	return "[" + peer + "]"
}

// hostOnly strips the port from a "host:port" network address, tolerating a
// bare host or an address form net.SplitHostPort does not recognize.
func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// sanitizeTraceToken reduces an untrusted trace value (an LHLO argument, a
// peer address) to characters that are inert inside a Received line: letters,
// digits, and the punctuation of domains and address literals. Everything
// else - above all CR and LF (header injection) and the parentheses that
// delimit comments - is dropped, so no peer-supplied value can terminate the
// stamp, open a new header, or unbalance the TCP-info comment.
func sanitizeTraceToken(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '.', c == '-', c == '_', c == ':':
			b.WriteByte(c)
		}
	}
	return b.String()
}

// stripCtl removes ASCII control characters (including CR and LF) and DEL
// from an envelope address before it is written into the Return-Path line.
// The LMTP ingress already rejects such addresses at parse time (RFC 5321
// section 4.1.2 permits no controls in a path); this keeps the guarantee for
// callers that reach Deliver directly.
func stripCtl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
