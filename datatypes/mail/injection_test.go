package mail

// SMTP command-injection defenses. An envelope address must never carry a
// CR/LF (or an angle bracket) that would smuggle a second command onto the
// wire. Two layers guard this: the submission-method gate (splitAddr, used
// by decodeEnvelope and checkRecipients) and defense-in-depth in the
// reference relay's command builders, since SMTPRelay is a public Submitter
// any caller can drive with a raw envelope.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// jsonStr encodes s as a JSON string literal, so a payload full of control
// bytes is embedded with proper JSON escapes (not Go's %q escapes).
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// injectionAddrs are addresses crafted to break out of the "<addr>" SMTP
// framing: a smuggled command boundary, a bare framing bracket, a space
// that would start a bogus parameter, and a NUL.
var injectionAddrs = []string{
	"victim@x.example>\r\nMAIL FROM:<spoof@bank.example",
	"victim@x.example>\r\nRCPT TO:<mule@evil.example",
	"a@b.example\rDATA",
	"a@b.example\nQUIT",
	"a@b.example ",
	"a@b<c@d.example",
	"a@b.example>",
	"a@\x00.example",
}

// TestSubmissionAddressInjectionRejected: the EmailSubmission/set gate
// refuses an injecting address in either mailFrom or a recipient, with the
// section 7.5 error, before anything is ever queued.
func TestSubmissionAddressInjectionRejected(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")

	for _, bad := range injectionAddrs {
		// A recipient carrying the payload: invalidRecipients.
		emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
		r := submitOne(t, ts, fmt.Sprintf(
			`{"identityId":%q,"emailId":%q,"envelope":{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":%s}]}}`,
			identityId, emailId, jsonStr(bad)))
		if e := notCreatedErr(t, r, "s"); e["type"] != "invalidRecipients" {
			t.Errorf("recipient %q: error = %v, want invalidRecipients", bad, e["type"])
		}

		// The payload in mailFrom: the envelope is rejected as invalid.
		r = submitOne(t, ts, fmt.Sprintf(
			`{"identityId":%q,"emailId":%q,"envelope":{"mailFrom":{"email":%s},"rcptTo":[{"email":"jane@remote.example"}]}}`,
			identityId, emailId, jsonStr(bad)))
		if e := notCreatedErr(t, r, "s"); e["type"] != "invalidProperties" {
			t.Errorf("mailFrom %q: error = %v, want invalidProperties", bad, e["type"])
		}
	}
}

// TestRelayAddressInjectionRejected: the relay's command builders refuse an
// unsafe address directly - the defense-in-depth layer for a caller who
// bypasses the submission gate and hands SMTPRelay a raw envelope. The
// address never reaches the built command, and the %q-escaped error carries
// no literal CR/LF.
func TestRelayAddressInjectionRejected(t *testing.T) {
	for _, bad := range injectionAddrs {
		if _, err := buildMailCmd(SubmissionEnvelope{MailFrom: bad}, nil, false); err == nil {
			t.Errorf("buildMailCmd accepted %q", bad)
		}
		cmd, err := buildRcptCmd(SubmissionRecipient{Email: bad}, false)
		if err == nil {
			t.Errorf("buildRcptCmd accepted %q (built %q)", bad, cmd)
			continue
		}
		if strings.ContainsAny(err.Error(), "\r\n") {
			t.Errorf("error string leaks a raw CR/LF for %q: %q", bad, err.Error())
		}
	}

	// A normal address still builds, so the guard is not over-broad.
	if _, err := buildMailCmd(SubmissionEnvelope{MailFrom: "john@example.com"}, nil, false); err != nil {
		t.Errorf("buildMailCmd rejected a valid address: %v", err)
	}
	if _, err := buildRcptCmd(SubmissionRecipient{Email: "jane@remote.example"}, false); err != nil {
		t.Errorf("buildRcptCmd rejected a valid address: %v", err)
	}
}

// ptr returns a pointer to s, for the *string parameter values.
func ptr(s string) *string { return &s }

// injectionParams are DSN parameter values crafted to smuggle a command past
// the "keyword=value" framing: a CR/LF break and a NUL.
var injectionParams = []string{
	"FULL\r\nRCPT TO:<mule@evil.example>",
	"SUCCESS\nDATA",
	"HDRS\rQUIT",
	"a\x00b",
}

// dsnAdvertised is the ext map with SMTP DSN advertised, so the DSN parameter
// branches (RET/NOTIFY/ORCPT) are actually reached.
var dsnAdvertised = map[string]string{"DSN": ""}

// TestRelayParamInjectionRejected: the relay's command builders refuse an
// unsafe DSN parameter value that would otherwise reach the wire verbatim -
// RET on MAIL FROM, NOTIFY and the ORCPT addr-type on RCPT TO. Defense in
// depth for a raw-envelope caller; the submission layer's checkSMTPParams is
// the front-door gate. Errors must not leak a raw CR/LF.
func TestRelayParamInjectionRejected(t *testing.T) {
	for _, bad := range injectionParams {
		if _, err := buildMailCmd(
			SubmissionEnvelope{MailFrom: "john@example.com", MailParameters: map[string]*string{"RET": ptr(bad)}},
			dsnAdvertised, true); err == nil {
			t.Errorf("buildMailCmd accepted RET=%q", bad)
		}
		if _, err := buildRcptCmd(
			SubmissionRecipient{Email: "jane@remote.example", Parameters: map[string]*string{"NOTIFY": ptr(bad)}},
			true); err == nil {
			t.Errorf("buildRcptCmd accepted NOTIFY=%q", bad)
		}
		// ORCPT carries the payload in the addr-type (before the ";"); the
		// addr half is xtext-encoded, so the type is the only raw vector.
		orcpt := bad + ";rfc822;jane@remote.example"
		cmd, err := buildRcptCmd(
			SubmissionRecipient{Email: "jane@remote.example", Parameters: map[string]*string{"ORCPT": ptr(orcpt)}},
			true)
		if err == nil {
			t.Errorf("buildRcptCmd accepted ORCPT addr-type %q (built %q)", bad, cmd)
			continue
		}
		if strings.ContainsAny(err.Error(), "\r\n") {
			t.Errorf("error string leaks a raw CR/LF for %q: %q", bad, err.Error())
		}
	}

	// Legitimate DSN parameters still build, so the guard is not over-broad.
	if _, err := buildMailCmd(
		SubmissionEnvelope{MailFrom: "john@example.com", MailParameters: map[string]*string{"RET": ptr("HDRS")}},
		dsnAdvertised, true); err != nil {
		t.Errorf("buildMailCmd rejected a valid RET: %v", err)
	}
	if _, err := buildRcptCmd(
		SubmissionRecipient{Email: "jane@remote.example", Parameters: map[string]*string{"NOTIFY": ptr("SUCCESS,FAILURE")}},
		true); err != nil {
		t.Errorf("buildRcptCmd rejected a valid NOTIFY: %v", err)
	}
	if _, err := buildRcptCmd(
		SubmissionRecipient{Email: "jane@remote.example", Parameters: map[string]*string{"ORCPT": ptr("rfc822;jane@remote.example")}},
		true); err != nil {
		t.Errorf("buildRcptCmd rejected a valid ORCPT: %v", err)
	}
}
