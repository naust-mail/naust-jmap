package mail

// SMTPRelay is the reference Submitter: a minimal SMTP client that
// relays one message to a smarthost. Intended for standards-compliant
// SMTP smarthosts reachable with TLS and SASL PLAIN authentication (or
// an explicitly plaintext localhost MTA); anything beyond that - OAuth
// submission services, exotic auth - should be supplied as its own
// Submitter implementation.
//
// It is hand-rolled over net/textproto rather than net/smtp: net/smtp
// is frozen and its Mail/Rcpt calls cannot carry SMTP parameters, which
// the RFC 3461 DSN parameters (NOTIFY/ORCPT/RET/ENVID) require.
//
// Security posture: the TLS mode is explicit and never downgrades. The
// default mode requires a successful STARTTLS before anything sensitive
// is sent and fails closed when the server does not offer it;
// opportunistic STARTTLS (proceed in plaintext when the offer is
// missing - the classic downgrade shape) deliberately does not exist
// here. TLS details come from a caller-supplied *tls.Config verbatim;
// this package invents no TLS vocabulary of its own.

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TLSMode selects how the connection to the smarthost is secured.
type TLSMode int

const (
	// RequireSTARTTLS (the zero value, and the default) connects in
	// plaintext and upgrades via STARTTLS (RFC 3207) before AUTH or
	// MAIL. If the server does not offer STARTTLS or the upgrade
	// fails, the attempt fails - there is no plaintext fallback.
	RequireSTARTTLS TLSMode = iota
	// ImplicitTLS speaks TLS from the first byte (the port-465
	// convention, RFC 8314).
	ImplicitTLS
	// Plaintext never uses TLS. It exists for smarthosts on localhost
	// (a local MTA owning onward delivery) and tests, and must be
	// chosen explicitly.
	Plaintext
)

// PlainAuth is a SASL PLAIN credential (RFC 4616).
type PlainAuth struct {
	Username string
	Password string
}

// SMTPRelayConfig configures the reference relay. Addr is required;
// every other field has a safe default.
type SMTPRelayConfig struct {
	// Addr is the smarthost, "host:port".
	Addr string
	// Mode is the TLS mode; the zero value is RequireSTARTTLS.
	Mode TLSMode
	// TLS, when non-nil, is used verbatim for the TLS client (server
	// name, root pool, pinning, client certificates - whatever the
	// caller expresses in the standard type). Nil means stdlib
	// defaults with the server name taken from Addr.
	TLS *tls.Config
	// Auth, when non-nil, authenticates with SASL PLAIN after the
	// connection is secured. Sending credentials over a Plaintext-mode
	// connection is refused unless AllowPlaintextAuth is also set.
	Auth               *PlainAuth
	AllowPlaintextAuth bool
	// Hello is the EHLO name; empty means "localhost".
	Hello string
	// Timeout bounds one Submit end to end - dial, TLS, AUTH, DATA
	// upload, and the final reply share one absolute deadline. The
	// deadline also caps at ctx's, whichever is sooner. Default 2m.
	Timeout time.Duration
}

// SMTPRelay implements Submitter over one SMTP session per Submit call.
type SMTPRelay struct {
	cfg SMTPRelayConfig
}

// NewSMTPRelay validates the configuration and returns the relay.
func NewSMTPRelay(cfg SMTPRelayConfig) (*SMTPRelay, error) {
	if cfg.Addr == "" {
		return nil, errors.New("mail: SMTPRelay needs an Addr")
	}
	if _, _, err := net.SplitHostPort(cfg.Addr); err != nil {
		return nil, fmt.Errorf("mail: SMTPRelay Addr must be host:port: %v", err)
	}
	if cfg.Auth != nil && cfg.Mode == Plaintext && !cfg.AllowPlaintextAuth {
		return nil, errors.New("mail: refusing SASL PLAIN over a Plaintext connection; set AllowPlaintextAuth to permit it")
	}
	if cfg.Hello == "" {
		cfg.Hello = "localhost"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	return &SMTPRelay{cfg: cfg}, nil
}

// Submit implements Submitter. A returned error means the attempt could
// not be made (dial, TLS, EHLO, AUTH, or a connection failure mid
// transaction). As a single-transaction client, every error path here
// precedes the end-of-data 250, so an abnormal end relays to nobody and
// error returns correctly carry no results; per-recipient outcomes are
// only reported when the session progressed far enough to produce them.
func (s *SMTPRelay) Submit(ctx context.Context, env SubmissionEnvelope, msg io.Reader) ([]RecipientResult, error) {
	deadline := time.Now().Add(s.cfg.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", s.cfg.Addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(deadline)

	tlsActive := false
	if s.cfg.Mode == ImplicitTLS {
		tc := tls.Client(conn, s.tlsConfig())
		if err := tc.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		conn = tc
		conn.SetDeadline(deadline)
		tlsActive = true
	}
	tp := textproto.NewConn(conn)
	if _, _, err := tp.ReadResponse(220); err != nil {
		return nil, fmt.Errorf("smarthost greeting: %w", err)
	}
	ext, err := relayEhlo(tp, s.cfg.Hello)
	if err != nil {
		return nil, err
	}

	if s.cfg.Mode == RequireSTARTTLS {
		if _, offered := ext["STARTTLS"]; !offered {
			return nil, errors.New("smarthost does not offer STARTTLS (required by this configuration)")
		}
		if err := relayCmd(tp, 220, "STARTTLS"); err != nil {
			return nil, err
		}
		tc := tls.Client(conn, s.tlsConfig())
		if err := tc.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		tc.SetDeadline(deadline)
		tp = textproto.NewConn(tc)
		tlsActive = true
		if ext, err = relayEhlo(tp, s.cfg.Hello); err != nil {
			return nil, err
		}
	}

	if s.cfg.Auth != nil {
		if !tlsActive && !s.cfg.AllowPlaintextAuth {
			return nil, errors.New("refusing to authenticate on an unencrypted connection")
		}
		ir := base64.StdEncoding.EncodeToString([]byte("\x00" + s.cfg.Auth.Username + "\x00" + s.cfg.Auth.Password))
		if err := relayCmd(tp, 235, "AUTH PLAIN %s", ir); err != nil {
			return nil, fmt.Errorf("authentication failed: %w", err)
		}
	}

	// SIZE fast-fail (RFC 1870): when the server declares a limit our
	// message exceeds, fail permanently without uploading anything.
	_, dsnOK := ext["DSN"]
	if v, ok := ext["SIZE"]; ok && env.Size > 0 {
		if limit, err := strconv.ParseInt(v, 10, 64); err == nil && limit > 0 && env.Size > limit {
			reply := fmt.Sprintf("552 5.3.4 message size %d exceeds smarthost limit %d", env.Size, limit)
			return rejectAll(env.Recipients, reply), nil
		}
	}

	mailCmd, err := buildMailCmd(env, ext, dsnOK)
	if err != nil {
		// Untransmittable parameter values are a permanent property of
		// this submission, not a transient of this attempt.
		return rejectAll(env.Recipients, "501 5.5.4 "+err.Error()), nil
	}
	code, reply, err := replyTo(tp, mailCmd)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		// The whole transaction is refused; every recipient shares it.
		out := make([]RecipientResult, len(env.Recipients))
		for i, r := range env.Recipients {
			out[i] = RecipientResult{Recipient: r.Email, Outcome: outcomeFor(code), Reply: reply}
		}
		return out, nil
	}

	results := make([]RecipientResult, len(env.Recipients))
	accepted := 0
	for i, r := range env.Recipients {
		rcptCmd, err := buildRcptCmd(r, dsnOK)
		if err != nil {
			results[i] = RecipientResult{Recipient: r.Email, Outcome: Rejected, Reply: "501 5.5.4 " + err.Error()}
			continue
		}
		code, reply, err := replyTo(tp, rcptCmd)
		if err != nil {
			return nil, err
		}
		results[i] = RecipientResult{Recipient: r.Email, Outcome: outcomeFor(code), Reply: reply}
		if code/100 == 2 {
			accepted++
		}
	}
	if accepted == 0 {
		relayQuit(tp)
		return results, nil
	}

	if err := relayCmd(tp, 354, "DATA"); err != nil {
		return nil, err
	}
	dw := tp.DotWriter()
	if _, err := io.Copy(dw, msg); err != nil {
		dw.Close()
		return nil, err
	}
	if err := dw.Close(); err != nil {
		return nil, err
	}
	code, reply, err = relayReadReply(tp)
	if err != nil {
		return nil, err
	}
	// RFC 8621 section 7: smtpReply SHOULD be the RCPT TO reply, unless
	// the message as a whole was refused at the DATA stage.
	for i := range results {
		if results[i].Outcome != Accepted {
			continue
		}
		if code/100 != 2 {
			results[i].Outcome = outcomeFor(code)
			results[i].Reply = reply
		}
	}
	relayQuit(tp)
	return results, nil
}

func (s *SMTPRelay) tlsConfig() *tls.Config {
	if s.cfg.TLS != nil {
		return s.cfg.TLS
	}
	host, _, _ := net.SplitHostPort(s.cfg.Addr)
	return &tls.Config{ServerName: host}
}

// ehlo sends EHLO and parses the extension keywords (RFC 5321 section
// 4.1.1.1): keyword to parameter text, keywords uppercased.
func relayEhlo(tp *textproto.Conn, hello string) (map[string]string, error) {
	id, err := tp.Cmd("EHLO %s", hello)
	if err != nil {
		return nil, err
	}
	tp.StartResponse(id)
	defer tp.EndResponse(id)
	_, msg, err := tp.ReadResponse(250)
	if err != nil {
		return nil, fmt.Errorf("EHLO refused: %w", err)
	}
	ext := make(map[string]string)
	lines := strings.Split(msg, "\n")
	for _, line := range lines[1:] { // first line is the server's name
		kw, rest, _ := strings.Cut(line, " ")
		ext[strings.ToUpper(kw)] = rest
	}
	return ext, nil
}

// cmd sends a command and requires the expected reply code.
func relayCmd(tp *textproto.Conn, expect int, format string, args ...any) error {
	id, err := tp.Cmd(format, args...)
	if err != nil {
		return err
	}
	tp.StartResponse(id)
	defer tp.EndResponse(id)
	_, _, err = tp.ReadResponse(expect)
	return err
}

// replyTo sends a command and returns whatever reply came back,
// flattened. Only transport errors are errors here - SMTP rejections
// are results.
func replyTo(tp *textproto.Conn, command string) (int, string, error) {
	id, err := tp.Cmd("%s", command)
	if err != nil {
		return 0, "", err
	}
	tp.StartResponse(id)
	defer tp.EndResponse(id)
	return relayReadReply(tp)
}

// readReply reads any reply and flattens it to the RFC 8621 section 7
// one-line form: textproto strips each continuation line's code prefix
// (the "prefix in common with the first line"), and joining the
// remaining lines with single spaces replaces the hyphens and CRLFs.
func relayReadReply(tp *textproto.Conn) (int, string, error) {
	code, msg, err := tp.ReadResponse(0)
	var pe *textproto.Error
	if errors.As(err, &pe) {
		// ReadResponse wraps non-2xx codes in a *textproto.Error even
		// with expectCode 0 disabled checking; unwrap uniformly.
		code, msg, err = pe.Code, pe.Msg, nil
	}
	if err != nil {
		return 0, "", err
	}
	return code, fmt.Sprintf("%d %s", code, strings.ReplaceAll(msg, "\n", " ")), nil
}

func relayQuit(tp *textproto.Conn) {
	if id, err := tp.Cmd("QUIT"); err == nil {
		tp.StartResponse(id)
		tp.ReadResponse(0)
		tp.EndResponse(id)
	}
}

func outcomeFor(code int) Outcome {
	switch code / 100 {
	case 2:
		return Accepted
	case 4:
		return TempFailed
	default:
		return Rejected
	}
}

func rejectAll(rcpts []SubmissionRecipient, reply string) []RecipientResult {
	out := make([]RecipientResult, len(rcpts))
	for i, r := range rcpts {
		out[i] = RecipientResult{Recipient: r.Email, Outcome: Rejected, Reply: reply}
	}
	return out
}

// buildMailCmd assembles MAIL FROM with its parameters: SIZE when the
// server supports it (RFC 1870), the DSN parameters RET and ENVID only
// when the server advertises DSN (RFC 3461 sections 4.3-4.4; ENVID is
// xtext-encoded on the wire), and any other stored parameter verbatim -
// using parameters beyond what the smarthost supports is the
// submitter's configuration risk, reported by the server.
func buildMailCmd(env SubmissionEnvelope, ext map[string]string, dsnOK bool) (string, error) {
	// Defense in depth: the submission layer already rejects unsafe
	// addresses, but SMTPRelay is a public Submitter any caller can drive
	// with a raw envelope, so an address with a smuggled CR/LF (or a
	// framing-breaking angle bracket) never reaches tp.Cmd.
	if !addrWireSafe(env.MailFrom) {
		return "", fmt.Errorf("mail: unsafe MAIL FROM address %q", env.MailFrom)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "MAIL FROM:<%s>", env.MailFrom)
	if _, ok := ext["SIZE"]; ok && env.Size > 0 {
		fmt.Fprintf(&b, " SIZE=%d", env.Size)
	}
	for _, k := range sortedParamKeys(env.MailParameters) {
		v := env.MailParameters[k]
		switch strings.ToUpper(k) {
		case "SIZE":
			continue // ours to declare
		case "ENVID":
			if dsnOK && v != nil {
				fmt.Fprintf(&b, " ENVID=%s", xtextEncode(*v))
			}
		case "RET":
			if dsnOK && v != nil {
				if !paramValueWireSafe(*v) {
					return "", fmt.Errorf("mail: unsafe RET value %q", *v)
				}
				fmt.Fprintf(&b, " RET=%s", *v)
			}
		default:
			if err := appendParam(&b, k, v); err != nil {
				return "", err
			}
		}
	}
	return b.String(), nil
}

// buildRcptCmd assembles RCPT TO with its parameters: NOTIFY and ORCPT
// only when DSN is advertised (RFC 3461 sections 4.1-4.2; ORCPT's
// address part is xtext-encoded after its addr-type), others verbatim.
func buildRcptCmd(r SubmissionRecipient, dsnOK bool) (string, error) {
	if !addrWireSafe(r.Email) {
		return "", fmt.Errorf("mail: unsafe RCPT TO address %q", r.Email)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "RCPT TO:<%s>", r.Email)
	for _, k := range sortedParamKeys(r.Parameters) {
		v := r.Parameters[k]
		switch strings.ToUpper(k) {
		case "NOTIFY":
			if dsnOK && v != nil {
				if !paramValueWireSafe(*v) {
					return "", fmt.Errorf("mail: unsafe NOTIFY value %q", *v)
				}
				fmt.Fprintf(&b, " NOTIFY=%s", *v)
			}
		case "ORCPT":
			if dsnOK && v != nil {
				typ, addr, found := strings.Cut(*v, ";")
				if !found {
					return "", fmt.Errorf("ORCPT value %q lacks an addr-type", *v)
				}
				// addr is xtext-encoded below, but the addr-type before the
				// ";" reaches the command line verbatim - gate it too.
				if !paramValueWireSafe(typ) {
					return "", fmt.Errorf("mail: unsafe ORCPT addr-type %q", typ)
				}
				fmt.Fprintf(&b, " ORCPT=%s;%s", typ, xtextEncode(addr))
			}
		default:
			if err := appendParam(&b, k, v); err != nil {
				return "", err
			}
		}
	}
	return b.String(), nil
}

// appendParam writes a generic esmtp parameter, verbatim. The stored
// value must already be a legal esmtp-value (RFC 5321 section 4.1.2:
// printable US-ASCII except "=", no spaces); anything else cannot be
// transmitted and is refused rather than silently altered.
func appendParam(b *strings.Builder, key string, val *string) error {
	if val == nil {
		fmt.Fprintf(b, " %s", key)
		return nil
	}
	if !paramValueWireSafe(*val) {
		return fmt.Errorf("parameter %s has a value not expressible in an SMTP command", key)
	}
	fmt.Fprintf(b, " %s=%s", key, *val)
	return nil
}

// paramValueWireSafe reports whether s is safe to place verbatim as an
// esmtp-value on the SMTP command line: printable US-ASCII with no space, no
// control character, and no "=". CR and LF are the bytes that would smuggle a
// second command; "=" would break "keyword=value" framing. It is the parameter
// counterpart of addrWireSafe: the DSN values reaching the wire verbatim (RET,
// NOTIFY, and ORCPT's addr-type) pass this gate even though the submission
// layer already validates them, because SMTPRelay is a public Submitter any
// caller can drive with a raw envelope.
func paramValueWireSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c > '~' || c == '=' {
			return false
		}
	}
	return true
}

func sortedParamKeys(m map[string]*string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// xtextEncode encodes a decoded parameter value as RFC 3461 section 4
// xtext: printable US-ASCII except "+" and "=" stays literal, anything
// else becomes +HH with uppercase hex.
func xtextEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '!' || c > '~' || c == '+' || c == '=' {
			fmt.Fprintf(&b, "+%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}
