package mail

// LMTP adapter (RFC 2033): a thin transport that slots behind an MTA (like
// Postfix) exactly as a local delivery agent does, and hands each message to
// the delivery socket. LMTP is ESMTP with two changes (RFC 2033 section 4):
// LHLO replaces HELO/EHLO, and after the final "." of DATA the server sends
// one reply per successful RCPT rather than a single reply for the whole
// transaction (section 4.2) - which is why Deliver returns one DeliveryEvent
// per recipient. LMTP MUST NOT be used on TCP port 25 and is intended for a
// trusted local channel by prior arrangement (sections 3 and 5).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Connection and transaction limits (see each use for the governing RFC 5321
// section). These bound the unauthenticated LMTP surface without a data-model
// change; the total in-memory/connection-count ceiling is a separate concern
// that lands with the streaming blob writer.
const (
	// maxCommandLine caps an LMTP command line. RFC 5321 4.5.3.1.4 sets the
	// command-line maximum at 512 octets including CRLF; the margin above
	// absorbs long addresses and ESMTP parameters. A longer line is rejected
	// 500 (4.5.3.1.9) and the connection closed (it can no longer resync).
	maxCommandLine = 1024
	// maxRecipients caps accepted recipients per transaction. RFC 5321
	// 4.5.3.1.8 requires buffering at least 100, so this sits above that floor;
	// the over-limit reply is 452 (4.5.3.1.10).
	maxRecipients = 128
	// maxRcptAttempts caps total RCPT commands per transaction (accepted or
	// refused), bounding resolver/DB amplification from a RCPT flood. It is far
	// above the accepted cap and the 100 floor, so a legitimate transaction
	// never reaches it.
	maxRcptAttempts = 1024
	// maxDrain bounds the post-Deliver body drain (see data). It is above the
	// 1000-octet text-line maximum (4.5.3.1.6) so a conformant tail resyncs,
	// but far below maxMessageSize so an oversize body is not re-read.
	maxDrain = 64 << 10
	// lmtpCommandTimeout is the idle wait for the next command. RFC 5321
	// 4.5.3.2.7: a server SHOULD wait at least 5 minutes.
	lmtpCommandTimeout = 5 * time.Minute
	// lmtpDataTimeout bounds the DATA body phase. RFC 5321 4.5.3.2.6 suggests
	// ~10 minutes awaiting the terminating ".".
	lmtpDataTimeout = 10 * time.Minute
)

// defaultMaxLMTPConns bounds how many LMTP connections are served at once. A
// delivery streams, so a connection in flight costs a buffer and not a message -
// which is why what is bounded here is CONNECTIONS. It is the honest unit: it is
// what a sender actually consumes, and unlike a cap on parses it cannot be held
// hostage by a slow one. An MTA opens a handful of channels to its local
// delivery agent (Postfix's default destination concurrency is around 20), so
// this is generous for the intended caller and still a ceiling against a local
// process that is not one.
const defaultMaxLMTPConns = 64

// LMTPOption configures an LMTP server.
type LMTPOption func(*lmtpConfig)

type lmtpConfig struct{ maxConns int }

// WithMaxLMTPConnections bounds how many LMTP connections are served at once.
// Beyond it a connection is answered with 421 and closed (RFC 5321 section 3.8:
// a server that cannot take the transaction says so and closes the channel,
// rather than dropping the TCP connection or accepting work it cannot do).
func WithMaxLMTPConnections(n int) LMTPOption {
	return func(c *lmtpConfig) { c.maxConns = n }
}

// ServeLMTP accepts connections on ln and serves LMTP on each until ln is
// closed, returning the Accept error. hostname names this server in the
// greeting and LHLO response. Each connection is handled in its own goroutine,
// and no more than the connection limit are served at once.
func ServeLMTP(ln net.Listener, d *Deliverer, hostname string, opts ...LMTPOption) error {
	cfg := lmtpConfig{maxConns: defaultMaxLMTPConns}
	for _, o := range opts {
		o(&cfg)
	}
	slots := make(chan struct{}, cfg.maxConns)

	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			// A closed listener is the intended stop signal; return it.
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Any other Accept error is transient - classically EMFILE/ENFILE
			// (file-descriptor exhaustion) or ECONNABORTED. Returning here would
			// permanently kill delivery over a temporary condition, so instead
			// log, back off (capped), and keep serving.
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else if backoff *= 2; backoff > time.Second {
				backoff = time.Second
			}
			log.Printf("naust-jmap lmtp: accept error (retry in %v): %v", backoff, err)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				serveLMTPConn(conn, d, hostname)
			}()
		default:
			// At the ceiling: tell the sender to come back rather than serving a
			// connection this server has no room for. The MTA queues and retries,
			// which is what a 4yz reply is for.
			go func() {
				_, _ = fmt.Fprintf(conn, "421 4.3.2 %s too many connections, try again later\r\n", hostname)
				_ = conn.Close()
			}()
		}
	}
}

// serveLMTPConn runs the command loop for one connection.
func serveLMTPConn(conn net.Conn, d *Deliverer, hostname string) {
	tp := textproto.NewConn(conn)
	defer tp.Close()
	// Connection-level panic backstop. Deliver has its own panic boundary at
	// the shared delivery seam; this one catches panics OUTSIDE it (command
	// parsing, the session loop) so nothing on this connection can crash the
	// co-hosted process. RFC 5321 3.8: fail with a reply (421, "closing
	// transmission channel") rather than a bare TCP drop, then close.
	defer func() {
		if p := recover(); p != nil {
			log.Printf("naust-jmap lmtp: recovered panic: %v", p)
			_ = tp.PrintfLine("421 4.3.0 %s service error, closing channel", hostname)
		}
	}()
	s := &lmtpSession{d: d, hostname: hostname, conn: conn, tp: tp, ctx: context.Background()}

	_ = tp.PrintfLine("220 %s LMTP server ready", hostname)
	for {
		// Per-command idle deadline (RFC 5321 4.5.3.2.7: a server SHOULD wait at
		// least 5 minutes for the next command), reset before each read so a
		// connection that goes silent is reclaimed instead of pinned.
		_ = conn.SetReadDeadline(time.Now().Add(lmtpCommandTimeout))
		line, tooLong, err := readLimitedLine(tp.R, maxCommandLine)
		if err != nil {
			return // connection closed, read error, or idle timeout
		}
		if tooLong {
			// RFC 5321 4.5.3.1.9: 500 for an over-length command line. The byte
			// stream can no longer be resynced, so close after the reply.
			_ = tp.PrintfLine("500 5.5.2 Command line too long")
			return
		}
		cmd, arg := splitCommand(line)
		switch strings.ToUpper(cmd) {
		case "LHLO":
			s.lhlo(arg)
		case "HELO", "EHLO":
			// LMTP has no HELO/EHLO; a server MUST NOT answer them with a
			// positive completion, so a protocol mismatch is caught (4.1).
			_ = tp.PrintfLine("500 5.5.1 Use LHLO, this is LMTP")
		case "MAIL":
			s.mail(arg)
		case "RCPT":
			s.rcpt(arg)
		case "DATA":
			if !s.data() {
				return // drain cap hit or body read error: close the connection
			}
		case "RSET":
			s.resetTxn()
			_ = tp.PrintfLine("250 2.0.0 OK")
		case "NOOP":
			_ = tp.PrintfLine("250 2.0.0 OK")
		case "VRFY":
			_ = tp.PrintfLine("502 5.5.1 VRFY not supported")
		case "QUIT":
			_ = tp.PrintfLine("221 2.0.0 %s closing connection", hostname)
			return
		default:
			_ = tp.PrintfLine("500 5.5.1 Unrecognized command")
		}
	}
}

// readLimitedLine reads one newline-terminated line from r, returning it
// without the trailing CRLF. tooLong is true (and line empty) if max octets
// pass with no newline: RFC 5321 4.5.3.1.4 caps a command line at 512 octets
// and stdlib has no length-bounded line reader, so this makes the bound
// explicit - a line with no newline cannot grow an unbounded buffer.
func readLimitedLine(r *bufio.Reader, max int) (line string, tooLong bool, err error) {
	var sb strings.Builder
	for {
		b, rerr := r.ReadByte()
		if rerr != nil {
			return "", false, rerr
		}
		if b == '\n' {
			return strings.TrimSuffix(sb.String(), "\r"), false, nil
		}
		if sb.Len() >= max {
			return "", true, nil
		}
		sb.WriteByte(b)
	}
}

// lmtpSession is one connection's state: the greeting flag and the current
// mail transaction (sender + accepted recipients).
type lmtpSession struct {
	d        *Deliverer
	hostname string
	conn     net.Conn // for read deadlines; tp wraps it
	tp       *textproto.Conn
	ctx      context.Context

	greeted      bool
	haveFrom     bool
	from         string
	rcpts        []string // recipients that resolved (got a 2xx RCPT reply)
	rcptAttempts int      // total RCPT commands this transaction (accepted or not)
}

// resetTxn clears the current transaction (RSET, LHLO, and after DATA).
func (s *lmtpSession) resetTxn() {
	s.haveFrom = false
	s.from = ""
	s.rcpts = nil
	s.rcptAttempts = 0
}

// lhlo answers LHLO with the capability list. LHLO has EHLO semantics (4.1);
// a server MUST implement PIPELINING and ENHANCEDSTATUSCODES and SHOULD
// implement 8BITMIME (section 5). SIZE advertises the message-size limit.
func (s *lmtpSession) lhlo(arg string) {
	s.resetTxn()
	s.greeted = true
	_ = s.tp.PrintfLine("250-%s", s.hostname)
	_ = s.tp.PrintfLine("250-PIPELINING")
	_ = s.tp.PrintfLine("250-ENHANCEDSTATUSCODES")
	_ = s.tp.PrintfLine("250-8BITMIME")
	_ = s.tp.PrintfLine("250 SIZE %d", s.d.MaxMessageSize())
}

// mail begins a transaction: MAIL FROM:<reverse-path> [SIZE=n]. A SIZE
// parameter over the limit is rejected here, before the body is sent.
func (s *lmtpSession) mail(arg string) {
	if !s.greeted {
		_ = s.tp.PrintfLine("503 5.5.1 Send LHLO first")
		return
	}
	if s.haveFrom {
		_ = s.tp.PrintfLine("503 5.5.1 Sender already given")
		return
	}
	from, params, ok := parsePath(arg, "FROM:")
	if !ok {
		_ = s.tp.PrintfLine("501 5.5.4 Syntax: MAIL FROM:<address>")
		return
	}
	if sz, ok := params["SIZE"]; ok {
		if n, err := strconv.ParseInt(sz, 10, 64); err == nil && n > s.d.MaxMessageSize() {
			_ = s.tp.PrintfLine("552 5.3.4 Message size exceeds %d", s.d.MaxMessageSize())
			return
		}
	}
	s.from = from
	s.haveFrom = true
	_ = s.tp.PrintfLine("250 2.1.0 OK")
}

// rcpt adds a recipient: RCPT TO:<forward-path>. The recipient is resolved
// now so an unknown address is refused here (550), not after the body; only
// resolved recipients get a reply to the final "." of DATA (4.2).
func (s *lmtpSession) rcpt(arg string) {
	if !s.haveFrom {
		_ = s.tp.PrintfLine("503 5.5.1 Need MAIL before RCPT")
		return
	}
	// Bound total RCPT commands before parsing/resolving, so a RCPT flood (even
	// of addresses that will be refused) cannot amplify into unbounded resolver
	// lookups. This attempt cap sits far above the accepted cap and the RFC 5321
	// 4.5.3.1.8 floor of 100, so it never blocks a legitimate transaction.
	if s.rcptAttempts >= maxRcptAttempts {
		_ = s.tp.PrintfLine("452 4.5.3 Too many recipients")
		return
	}
	s.rcptAttempts++
	to, _, ok := parsePath(arg, "TO:")
	if !ok {
		_ = s.tp.PrintfLine("501 5.5.4 Syntax: RCPT TO:<address>")
		return
	}
	// Cap accepted recipients. RFC 5321 4.5.3.1.8 requires buffering at least
	// 100 (rejecting a message for excessive recipients below 100 is a
	// violation), so this cap is >= that floor. 4.5.3.1.10: the over-limit reply
	// MUST be 452, and the server MUST behave in an orderly fashion - reject the
	// extra recipient, never silently discard an already-accepted one.
	if len(s.rcpts) >= maxRecipients {
		_ = s.tp.PrintfLine("452 4.5.3 Too many recipients")
		return
	}
	// Resolve now so an unknown recipient is refused at RCPT time; only
	// resolved recipients get a reply to the final "." of DATA (4.2).
	if _, ok := s.d.resolver.Resolve(s.ctx, to); !ok {
		_ = s.tp.PrintfLine("550 5.1.1 No such user here")
		return
	}
	s.rcpts = append(s.rcpts, to)
	_ = s.tp.PrintfLine("250 2.1.5 OK")
}

// data reads the message and delivers it. With no successful RCPT it MUST
// fail 503 (4.2). After the final ".", it sends one reply per recipient in
// RCPT order. It returns false when the connection should be closed (the drain
// cap was hit or the body read failed).
func (s *lmtpSession) data() bool {
	if !s.haveFrom {
		_ = s.tp.PrintfLine("503 5.5.1 Need MAIL before DATA")
		return true
	}
	if len(s.rcpts) == 0 {
		_ = s.tp.PrintfLine("503 5.5.1 No valid recipients")
		return true
	}
	_ = s.tp.PrintfLine("354 Start mail input; end with <CRLF>.<CRLF>")

	// Bound the whole DATA phase with a read deadline (RFC 5321 4.5.3.2.6
	// suggests ~10 minutes awaiting the terminating "."), so a stalled body is
	// reclaimed instead of pinning the connection.
	_ = s.conn.SetReadDeadline(time.Now().Add(lmtpDataTimeout))

	dr := s.tp.DotReader()
	events := s.d.Deliver(s.ctx, Envelope{MailFrom: s.from, Recipients: s.rcpts}, dr)
	// Deliver may stop reading early (message too large); drain the rest of the
	// dot-encoded body so the terminating "." is consumed and the command
	// stream resyncs - but bound the drain. A body that keeps streaming past
	// maxDrain (or never sends the terminating ".") is abandoned and the
	// connection closed, rather than read forever (the deadline above is the
	// backstop if bytes stop arriving mid-drain).
	n, err := io.CopyN(io.Discard, dr, maxDrain)
	if err != nil && !errors.Is(err, io.EOF) {
		return false // read error or deadline mid-drain: close
	}
	if err == nil && n == maxDrain {
		return false // more than maxDrain still pending: abandon resync, close
	}

	for _, ev := range events {
		_ = s.tp.PrintfLine("%s", lmtpReply(ev))
	}
	s.resetTxn()
	return true
}

// lmtpReply maps a DeliveryEvent to its LMTP reply line, with an enhanced
// status code (ENHANCEDSTATUSCODES, required by section 5).
func lmtpReply(ev DeliveryEvent) string {
	switch ev.Outcome {
	case Accepted:
		return "250 2.1.5 OK"
	case TempFailed:
		// Transient local failure: the client should retry (4yz).
		return "451 4.3.0 " + oneLine(ev.Reason)
	default: // Rejected: permanent (5yz), the client should bounce.
		if ev.Reason == "message too large" {
			return "552 5.3.4 message too large"
		}
		if ev.Reason == "no such recipient" {
			return "550 5.1.1 " + oneLine(ev.Reason)
		}
		return "550 5.7.1 " + oneLine(ev.Reason)
	}
}

// splitCommand splits a command line into the verb and the remainder.
func splitCommand(line string) (cmd, arg string) {
	line = strings.TrimRight(line, "\r\n")
	if i := strings.IndexByte(line, ' '); i >= 0 {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	return line, ""
}

// parsePath parses "FROM:<addr> [PARAM=value ...]" (or "TO:..."), returning
// the bracketed address (empty for the null sender <>) and any ESMTP
// parameters. It tolerates a space after the colon.
func parsePath(arg, prefix string) (addr string, params map[string]string, ok bool) {
	if len(arg) < len(prefix) || !strings.EqualFold(arg[:len(prefix)], prefix) {
		return "", nil, false
	}
	rest := strings.TrimSpace(arg[len(prefix):])
	lt := strings.IndexByte(rest, '<')
	gt := strings.IndexByte(rest, '>')
	if lt != 0 || gt < lt {
		return "", nil, false
	}
	addr = rest[1:gt]
	// Reject an envelope address carrying control characters at the door (see
	// validEnvelopeAddr): every RFC 5321 4.1.2 path terminal is an ASCII
	// graphic or space, so a control character is never legal, and rejecting it
	// here keeps every downstream sink (logs, storage labels, delivery events)
	// clean by construction rather than escaping at each one.
	if !validEnvelopeAddr(addr) {
		return "", nil, false
	}
	params = map[string]string{}
	for _, tok := range strings.Fields(rest[gt+1:]) {
		if eq := strings.IndexByte(tok, '='); eq >= 0 {
			params[strings.ToUpper(tok[:eq])] = tok[eq+1:]
		} else {
			params[strings.ToUpper(tok)] = ""
		}
	}
	return addr, params, true
}

// validEnvelopeAddr reports whether addr is free of the control characters RFC
// 5321 4.1.2 forbids in a path. Every envelope terminal there - atext,
// qtextSMTP (%d32-33 / %d35-91 / %d93-126), quoted-pairSMTP (%d92 %d32-126),
// Domain - is an ASCII graphic or space, so a C0 control (including NUL and a
// bare CR or LF), DEL, or a C1 control is never legal. The 8-bit UTF-8 of an
// internationalized address (EAI, RFC 6531/6532) is allowed: those are decoded
// runes above U+007F, not raw control octets. The empty string (null sender
// <>) is valid.
func validEnvelopeAddr(addr string) bool {
	for i := 0; i < len(addr); {
		b := addr[i]
		if b < 0x20 || b == 0x7f { // C0 controls (incl. NUL, CR, LF) and DEL
			return false
		}
		if b < 0x80 { // ASCII graphic or space
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(addr[i:])
		if r == utf8.RuneError && size == 1 {
			i++ // not valid UTF-8: opaque 8-bit, not a decodable control
			continue
		}
		if r >= 0x80 && r <= 0x9f { // C1 controls (as UTF-8 code points)
			return false
		}
		i += size
	}
	return true
}

// oneLine collapses a reason to a single reply line (no CR/LF can leak into
// the protocol stream).
func oneLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}
