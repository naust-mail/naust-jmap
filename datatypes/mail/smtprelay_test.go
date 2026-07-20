package mail

// SMTPRelay tests against a scripted in-process smarthost: parameter
// forwarding (xtext, DSN gating), per-recipient outcomes, DATA-stage
// rejection, SIZE fast-fail, the STARTTLS-required posture, AUTH PLAIN,
// and reply flattening.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// smarthost is a scripted single-connection SMTP server.
type smarthost struct {
	ln   net.Listener
	ext  []string // EHLO keyword lines, e.g. "DSN", "SIZE 1000"
	rcpt func(cmd string) string
	mail string // MAIL reply; "" = 250
	data string // final DATA reply; "" = 250
	tls  *tls.Config

	mu   sync.Mutex
	cmds []string
	body string
}

func (h *smarthost) commands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.cmds...)
}

func (h *smarthost) received() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.body
}

func newSmarthost(t *testing.T, h *smarthost) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.ln = ln
	t.Cleanup(func() { ln.Close() })
	go h.serve(t)
	return ln.Addr().String()
}

func (h *smarthost) serve(t *testing.T) {
	conn, err := h.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	h.session(t, conn, true)
}

func (h *smarthost) session(t *testing.T, conn net.Conn, greet bool) {
	tp := textproto.NewConn(conn)
	if greet {
		// No greeting after a STARTTLS upgrade (RFC 3207): the client
		// speaks first with a fresh EHLO.
		tp.PrintfLine("220 fake.example ESMTP")
	}
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		h.mu.Lock()
		h.cmds = append(h.cmds, line)
		h.mu.Unlock()
		verb := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(verb, "EHLO"):
			// Real shape: the server name first, extensions after.
			lines := append([]string{"fake.example greets you"}, h.ext...)
			for _, e := range lines[:len(lines)-1] {
				tp.PrintfLine("250-%s", e)
			}
			tp.PrintfLine("250 %s", lines[len(lines)-1])
		case verb == "STARTTLS":
			if h.tls == nil {
				tp.PrintfLine("454 not today")
				continue
			}
			tp.PrintfLine("220 go ahead")
			tc := tls.Server(conn, h.tls)
			if err := tc.Handshake(); err != nil {
				return
			}
			tc.SetDeadline(time.Now().Add(10 * time.Second))
			h.session(t, tc, false)
			return
		case strings.HasPrefix(verb, "AUTH PLAIN "):
			raw, err := base64.StdEncoding.DecodeString(line[len("AUTH PLAIN "):])
			if err == nil && string(raw) == "\x00user\x00pass" {
				tp.PrintfLine("235 2.7.0 ok")
			} else {
				tp.PrintfLine("535 5.7.8 bad credentials")
			}
		case strings.HasPrefix(verb, "MAIL"):
			if h.mail != "" {
				for _, l := range strings.Split(h.mail, "\n") {
					tp.PrintfLine("%s", l)
				}
			} else {
				tp.PrintfLine("250 2.1.0 sender ok")
			}
		case strings.HasPrefix(verb, "RCPT"):
			reply := "250 2.1.5 recipient ok"
			if h.rcpt != nil {
				reply = h.rcpt(line)
			}
			for _, l := range strings.Split(reply, "\n") {
				tp.PrintfLine("%s", l)
			}
		case verb == "DATA":
			tp.PrintfLine("354 send it")
			body, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.body = string(body)
			h.mu.Unlock()
			if h.data != "" {
				tp.PrintfLine("%s", h.data)
			} else {
				tp.PrintfLine("250 2.0.0 queued")
			}
		case verb == "QUIT":
			tp.PrintfLine("221 bye")
			return
		default:
			tp.PrintfLine("500 what")
		}
	}
}

func strptr(s string) *string { return &s }

func relayFor(t *testing.T, cfg SMTPRelayConfig) *SMTPRelay {
	t.Helper()
	r, err := NewSMTPRelay(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func findCmd(t *testing.T, cmds []string, prefix string) string {
	t.Helper()
	for _, c := range cmds {
		if strings.HasPrefix(strings.ToUpper(c), strings.ToUpper(prefix)) {
			return c
		}
	}
	t.Fatalf("no %q in %v", prefix, cmds)
	return ""
}

// TestSMTPRelayFlow: the full plaintext transaction - SIZE declared,
// ENVID xtext-encoded, NOTIFY forwarded, message dot-transported, every
// recipient Accepted with its RCPT reply.
func TestSMTPRelayFlow(t *testing.T) {
	h := &smarthost{ext: []string{"DSN", "SIZE 100000"}}
	addr := newSmarthost(t, h)
	r := relayFor(t, SMTPRelayConfig{Addr: addr, Mode: Plaintext, Hello: "box.example"})

	msg := "From: a@x\r\nTo: b@x\r\n\r\nline one\r\n.leading dot\r\n"
	res, err := r.Submit(context.Background(), SubmissionEnvelope{
		MailFrom:       "john@example.com",
		MailParameters: map[string]*string{"ENVID": strptr("id with space+plus"), "RET": strptr("HDRS")},
		Recipients: []SubmissionRecipient{
			{Email: "jane@remote.example", Parameters: map[string]*string{"NOTIFY": strptr("SUCCESS,FAILURE")}},
		},
		Size: int64(len(msg)),
	}, strings.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Outcome != Accepted || res[0].Reply != "250 2.1.5 recipient ok" {
		t.Fatalf("results = %+v", res)
	}
	cmds := h.commands()
	mail := findCmd(t, cmds, "MAIL FROM")
	if !strings.Contains(mail, "MAIL FROM:<john@example.com>") ||
		!strings.Contains(mail, fmt.Sprintf("SIZE=%d", len(msg))) ||
		!strings.Contains(mail, "ENVID=id+20with+20space+2Bplus") ||
		!strings.Contains(mail, "RET=HDRS") {
		t.Errorf("MAIL = %q", mail)
	}
	rcpt := findCmd(t, cmds, "RCPT TO")
	if !strings.Contains(rcpt, "RCPT TO:<jane@remote.example>") ||
		!strings.Contains(rcpt, "NOTIFY=SUCCESS,FAILURE") {
		t.Errorf("RCPT = %q", rcpt)
	}
	// The fake's ReadDotBytes normalizes CRLF to LF; the dot-stuffed
	// leading dot must round-trip.
	if got, want := h.received(), strings.ReplaceAll(msg, "\r\n", "\n"); got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	if !strings.Contains(strings.ToUpper(findCmd(t, cmds, "EHLO")), "BOX.EXAMPLE") {
		t.Errorf("EHLO = %q", findCmd(t, cmds, "EHLO"))
	}
}

// TestSMTPRelayNoDSN: DSN parameters vanish when the server does not
// advertise the extension (RFC 3461 best-effort relaying); generic
// parameters still ride.
func TestSMTPRelayNoDSN(t *testing.T) {
	h := &smarthost{}
	addr := newSmarthost(t, h)
	r := relayFor(t, SMTPRelayConfig{Addr: addr, Mode: Plaintext})
	_, err := r.Submit(context.Background(), SubmissionEnvelope{
		MailFrom:       "john@example.com",
		MailParameters: map[string]*string{"ENVID": strptr("x"), "RET": strptr("FULL"), "FOO": nil},
		Recipients: []SubmissionRecipient{
			{Email: "jane@remote.example", Parameters: map[string]*string{"NOTIFY": strptr("NEVER"), "ORCPT": strptr("rfc822;jane@remote.example")}},
		},
	}, strings.NewReader("From: a@x\r\n\r\nb\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	cmds := h.commands()
	mail := findCmd(t, cmds, "MAIL FROM")
	if strings.Contains(mail, "ENVID") || strings.Contains(mail, "RET") {
		t.Errorf("DSN params sent without DSN: %q", mail)
	}
	if !strings.Contains(mail, " FOO") {
		t.Errorf("generic param dropped: %q", mail)
	}
	rcpt := findCmd(t, cmds, "RCPT TO")
	if strings.Contains(rcpt, "NOTIFY") || strings.Contains(rcpt, "ORCPT") {
		t.Errorf("DSN params sent without DSN: %q", rcpt)
	}
}

// TestSMTPRelayOrcptXtext: ORCPT keeps its addr-type and xtext-encodes
// the address (RFC 3461 section 4.2).
func TestSMTPRelayOrcptXtext(t *testing.T) {
	h := &smarthost{ext: []string{"DSN"}}
	addr := newSmarthost(t, h)
	r := relayFor(t, SMTPRelayConfig{Addr: addr, Mode: Plaintext})
	_, err := r.Submit(context.Background(), SubmissionEnvelope{
		MailFrom: "john@example.com",
		Recipients: []SubmissionRecipient{
			{Email: "jane@remote.example", Parameters: map[string]*string{"ORCPT": strptr("rfc822;jane+tag@remote.example")}},
		},
	}, strings.NewReader("From: a@x\r\n\r\nb\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	rcpt := findCmd(t, h.commands(), "RCPT TO")
	if !strings.Contains(rcpt, "ORCPT=rfc822;jane+2Btag@remote.example") {
		t.Errorf("RCPT = %q", rcpt)
	}
}

// TestSMTPRelayPerRecipient: mixed RCPT outcomes, DATA still sent for
// the accepted one, and a multi-line RCPT reply flattens to one line.
func TestSMTPRelayPerRecipient(t *testing.T) {
	h := &smarthost{rcpt: func(cmd string) string {
		switch {
		case strings.Contains(cmd, "ok@"):
			return "250-2.1.5 first line\n250 second line"
		case strings.Contains(cmd, "gone@"):
			return "550 5.1.1 unknown user"
		default:
			return "451 4.7.0 try later"
		}
	}}
	addr := newSmarthost(t, h)
	r := relayFor(t, SMTPRelayConfig{Addr: addr, Mode: Plaintext})
	res, err := r.Submit(context.Background(), SubmissionEnvelope{
		MailFrom: "john@example.com",
		Recipients: []SubmissionRecipient{
			{Email: "ok@x.example"}, {Email: "gone@x.example"}, {Email: "slow@x.example"},
		},
	}, strings.NewReader("From: a@x\r\n\r\nb\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []RecipientResult{
		{Recipient: "ok@x.example", Outcome: Accepted, Reply: "250 2.1.5 first line second line"},
		{Recipient: "gone@x.example", Outcome: Rejected, Reply: "550 5.1.1 unknown user"},
		{Recipient: "slow@x.example", Outcome: TempFailed, Reply: "451 4.7.0 try later"},
	}
	for i, w := range want {
		if res[i] != w {
			t.Errorf("result[%d] = %+v, want %+v", i, res[i], w)
		}
	}
	if h.received() == "" {
		t.Error("DATA skipped despite an accepted recipient")
	}
}

// TestSMTPRelayAllRejected: with no accepted recipient, DATA is never
// attempted and no error is returned - the outcomes are the answer.
func TestSMTPRelayAllRejected(t *testing.T) {
	h := &smarthost{rcpt: func(string) string { return "550 5.1.1 no" }}
	addr := newSmarthost(t, h)
	r := relayFor(t, SMTPRelayConfig{Addr: addr, Mode: Plaintext})
	res, err := r.Submit(context.Background(), SubmissionEnvelope{
		MailFrom:   "john@example.com",
		Recipients: []SubmissionRecipient{{Email: "a@x.example"}, {Email: "b@x.example"}},
	}, strings.NewReader("From: a@x\r\n\r\nb\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rr := range res {
		if rr.Outcome != Rejected {
			t.Errorf("%+v", rr)
		}
	}
	for _, c := range h.commands() {
		if strings.EqualFold(c, "DATA") {
			t.Error("DATA sent with zero accepted recipients")
		}
	}
	if h.received() != "" {
		t.Error("message transmitted with zero accepted recipients")
	}
}

// TestSMTPRelayDataRejected: RCPT-accepted recipients take the DATA
// reply when the message as a whole is refused (RFC 8621 section 7).
func TestSMTPRelayDataRejected(t *testing.T) {
	h := &smarthost{data: "554 5.6.0 content rejected"}
	addr := newSmarthost(t, h)
	r := relayFor(t, SMTPRelayConfig{Addr: addr, Mode: Plaintext})
	res, err := r.Submit(context.Background(), SubmissionEnvelope{
		MailFrom:   "john@example.com",
		Recipients: []SubmissionRecipient{{Email: "a@x.example"}},
	}, strings.NewReader("From: a@x\r\n\r\nb\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Outcome != Rejected || res[0].Reply != "554 5.6.0 content rejected" {
		t.Fatalf("result = %+v", res[0])
	}
}

// TestSMTPRelaySizeFastFail: a message over the advertised SIZE fails
// permanently without MAIL ever being sent.
func TestSMTPRelaySizeFastFail(t *testing.T) {
	h := &smarthost{ext: []string{"SIZE 10"}}
	addr := newSmarthost(t, h)
	r := relayFor(t, SMTPRelayConfig{Addr: addr, Mode: Plaintext})
	res, err := r.Submit(context.Background(), SubmissionEnvelope{
		MailFrom:   "john@example.com",
		Recipients: []SubmissionRecipient{{Email: "a@x.example"}},
		Size:       100,
	}, strings.NewReader(strings.Repeat("x", 100)))
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Outcome != Rejected || !strings.HasPrefix(res[0].Reply, "552 5.3.4") {
		t.Fatalf("result = %+v", res[0])
	}
	for _, c := range h.commands() {
		if strings.HasPrefix(strings.ToUpper(c), "MAIL") {
			t.Error("MAIL sent despite SIZE fast-fail")
		}
	}
}

// TestSMTPRelayBadParamValue: a stored parameter value that cannot
// appear in an SMTP command is refused permanently, never mangled.
func TestSMTPRelayBadParamValue(t *testing.T) {
	h := &smarthost{}
	addr := newSmarthost(t, h)
	r := relayFor(t, SMTPRelayConfig{Addr: addr, Mode: Plaintext})
	res, err := r.Submit(context.Background(), SubmissionEnvelope{
		MailFrom:       "john@example.com",
		MailParameters: map[string]*string{"FOO": strptr("has space")},
		Recipients:     []SubmissionRecipient{{Email: "a@x.example"}},
	}, strings.NewReader("From: a@x\r\n\r\nb\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Outcome != Rejected || !strings.HasPrefix(res[0].Reply, "501 5.5.4") {
		t.Fatalf("result = %+v", res[0])
	}
}

// TestSMTPRelayRequireSTARTTLS: the default mode fails closed against a
// server that does not offer STARTTLS - a could-not-attempt error, no
// plaintext fallback.
func TestSMTPRelayRequireSTARTTLS(t *testing.T) {
	h := &smarthost{} // no STARTTLS in EHLO
	addr := newSmarthost(t, h)
	r := relayFor(t, SMTPRelayConfig{Addr: addr})
	_, err := r.Submit(context.Background(), SubmissionEnvelope{
		MailFrom:   "john@example.com",
		Recipients: []SubmissionRecipient{{Email: "a@x.example"}},
	}, strings.NewReader("From: a@x\r\n\r\nb\r\n"))
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v, want a STARTTLS refusal", err)
	}
	for _, c := range h.commands() {
		if strings.HasPrefix(strings.ToUpper(c), "MAIL") || strings.HasPrefix(strings.ToUpper(c), "AUTH") {
			t.Errorf("sensitive command %q sent without TLS", c)
		}
	}
}

// selfSigned builds a throwaway TLS server config plus the pool that
// trusts it.
func selfSigned(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, pool
}

// TestSMTPRelaySTARTTLSAndAuth: the full default-posture path - EHLO,
// STARTTLS upgrade, fresh EHLO, AUTH PLAIN over the secured channel,
// then the transaction. The host supplies trust via a standard
// tls.Config, nothing invented.
func TestSMTPRelaySTARTTLSAndAuth(t *testing.T) {
	srvTLS, pool := selfSigned(t)
	h := &smarthost{ext: []string{"STARTTLS", "AUTH PLAIN"}, tls: srvTLS}
	addr := newSmarthost(t, h)
	r := relayFor(t, SMTPRelayConfig{
		Addr: addr,
		TLS:  &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
		Auth: &PlainAuth{Username: "user", Password: "pass"},
	})
	res, err := r.Submit(context.Background(), SubmissionEnvelope{
		MailFrom:   "john@example.com",
		Recipients: []SubmissionRecipient{{Email: "a@x.example"}},
	}, strings.NewReader("From: a@x\r\n\r\nb\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Outcome != Accepted {
		t.Fatalf("result = %+v", res[0])
	}
	cmds := h.commands()
	var sawStarttls bool
	var ehloCount int
	for i, c := range cmds {
		u := strings.ToUpper(c)
		if u == "STARTTLS" {
			sawStarttls = true
		}
		if strings.HasPrefix(u, "EHLO") {
			ehloCount++
		}
		if strings.HasPrefix(u, "AUTH") && !sawStarttls {
			t.Errorf("AUTH before STARTTLS (command %d: %q)", i, c)
		}
	}
	if !sawStarttls || ehloCount != 2 {
		t.Errorf("commands = %v", cmds)
	}
}

// TestSMTPRelayConfigValidation: constructor invariants.
func TestSMTPRelayConfigValidation(t *testing.T) {
	if _, err := NewSMTPRelay(SMTPRelayConfig{}); err == nil {
		t.Error("empty Addr accepted")
	}
	if _, err := NewSMTPRelay(SMTPRelayConfig{Addr: "no-port"}); err == nil {
		t.Error("Addr without port accepted")
	}
	if _, err := NewSMTPRelay(SMTPRelayConfig{
		Addr: "127.0.0.1:25", Mode: Plaintext, Auth: &PlainAuth{Username: "u", Password: "p"},
	}); err == nil {
		t.Error("PLAIN over Plaintext accepted without AllowPlaintextAuth")
	}
	if _, err := NewSMTPRelay(SMTPRelayConfig{
		Addr: "127.0.0.1:25", Mode: Plaintext, Auth: &PlainAuth{Username: "u", Password: "p"},
		AllowPlaintextAuth: true,
	}); err != nil {
		t.Errorf("explicit AllowPlaintextAuth rejected: %v", err)
	}
}

// TestXtextEncode: RFC 3461 section 4 encoding rules.
func TestXtextEncode(t *testing.T) {
	cases := map[string]string{
		"simple":        "simple",
		"with space":    "with+20space",
		"a+b":           "a+2Bb",
		"a=b":           "a+3Db",
		"caf\xc3\xa9":   "caf+C3+A9",
		"ctl\x01":       "ctl+01",
		"":              "",
		"!printable~ok": "!printable~ok",
	}
	for in, want := range cases {
		if got := xtextEncode(in); got != want {
			t.Errorf("xtextEncode(%q) = %q, want %q", in, got, want)
		}
	}
}
