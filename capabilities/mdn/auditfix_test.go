package mdn

// Regression tests for the reviewed send and parse behaviors: a
// wildcard Identity yields no default finalRecipient, the SendPolicy is
// re-checked at send time, an automatic MDN never answers an automatic
// message (RFC 3834 section 2), repeated parse ids cost one verdict,
// and extension field names outside the RFC 5322 field-name grammar
// never become response object keys.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// createWildcardIdentity makes the whole-domain Identity the test
// policy grants.
func createWildcardIdentity(t *testing.T, ts *httptest.Server) jmap.Id {
	t.Helper()
	r := call(t, ts, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:submission"],"methodCalls":[
		["Identity/set",{"accountId":"Atest1","create":{"iw":{"email":"*@example.com"}}},"0"]]}`)
	var out struct {
		Created map[string]struct {
			Id jmap.Id `json:"id"`
		} `json:"created"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Created["iw"].Id == "" {
		t.Fatalf("wildcard Identity/set: %s %s", r.MethodResponses[0].Name, r.MethodResponses[0].Args)
	}
	return out.Created["iw"].Id
}

// TestSendWildcardIdentity sends via a whole-domain Identity: with no
// finalRecipient there is no address to issue the MDN for, so the entry
// fails; naming one makes it both the finalRecipient and the MDN's
// From.
func TestSendWildcardIdentity(t *testing.T) {
	e := setupSend(t, nil)
	wildcard := createWildcardIdentity(t, e.ts)
	emailId := e.importMsg(t, receivedMsg(nil))
	sendVia := func(mdnJSON string) jmap.Response {
		return call(t, e.ts, fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:mdn"],"methodCalls":[
			["MDN/send",{"accountId":"Atest1","identityId":%q,"send":{"k1546":%s},"onSuccessUpdateEmail":%s},"0"]]}`,
			wildcard, mdnJSON, sampleOnSuccess))
	}

	bare := fmt.Sprintf(`{"forEmailId":%q,"disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}`, emailId)
	serr := notSentError(t, sendVia(bare))
	if serr.Type != "invalidProperties" || !strings.Contains(serr.Description, "finalRecipient") {
		t.Fatalf("wildcard identity without finalRecipient = %+v, want invalidProperties naming finalRecipient", serr)
	}

	named := fmt.Sprintf(`{"forEmailId":%q,"finalRecipient":"rfc822; john2@example.com","disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}`, emailId)
	r := sendVia(named)
	var out struct {
		Sent map[jmap.Id]map[string]string `json:"sent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Sent["k1546"] == nil {
		t.Fatalf("wildcard identity with finalRecipient failed: %s", r.MethodResponses[0].Args)
	}
	raw := e.sentMDNRaw(t)
	top, _, _ := strings.Cut(raw, "\r\n\r\n")
	if !strings.Contains(top, "john2@example.com") {
		t.Errorf("MDN From does not carry the named address:\n%s", top)
	}
	if strings.Contains(raw, "*@") {
		t.Error("a wildcard reached the wire")
	}
}

// flagPolicy is a SendPolicy whose answer can be flipped mid-test,
// modeling a grant revoked after the Identity was created.
type flagPolicy struct{ allow atomic.Bool }

func (f *flagPolicy) CanSend(context.Context, jmap.Id) (bool, string) {
	if f.allow.Load() {
		return true, ""
	}
	return false, "revoked"
}
func (f *flagPolicy) CanSendAs(context.Context, jmap.Id, string) bool { return f.allow.Load() }

// TestSendPolicyRevoked revokes the sending grant after the Identity
// exists: the Identity record alone no longer authorizes an MDN.
func TestSendPolicyRevoked(t *testing.T) {
	policy := &flagPolicy{}
	policy.allow.Store(true)
	ts := newTestServerFull(t, nil, policy)
	e := &sendEnv{ts: ts, identity: createIdentity(t, ts)}
	r := call(t, ts, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[
		["Mailbox/set",{"accountId":"Atest1","create":{
			"mi":{"name":"Inbox","role":"inbox"},
			"ms":{"name":"Sent","role":"sent"}}},"0"]]}`)
	var out struct {
		Created map[string]struct {
			Id jmap.Id `json:"id"`
		} `json:"created"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	e.inbox, e.sent = out.Created["mi"].Id, out.Created["ms"].Id
	emailId := e.importMsg(t, receivedMsg(nil))

	policy.allow.Store(false)
	serr := notSentError(t, e.mdnSendCall(t, sampleMDN(emailId), sampleOnSuccess))
	if serr.Type != "forbidden" {
		t.Fatalf("revoked grant = %+v, want forbidden", serr)
	}

	policy.allow.Store(true)
	r = e.mdnSendCall(t, sampleMDN(emailId), sampleOnSuccess)
	var ok struct {
		Sent map[jmap.Id]json.RawMessage `json:"sent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &ok)
	if ok.Sent["k1546"] == nil {
		t.Fatalf("restored grant still refused: %s", r.MethodResponses[0].Args)
	}
}

// TestSendAutoSubmittedOriginal answers a machine-generated message:
// an automatic MDN is refused (RFC 3834 section 2), a manual one - the
// user's own judgment - goes through.
func TestSendAutoSubmittedOriginal(t *testing.T) {
	e := setupSend(t, nil)
	autoMsg := e.importMsg(t, receivedMsg(map[string]string{
		"Auto-Submitted": "auto-generated",
		"Message-ID":     "<auto-orig-1@example.org>",
	}))
	auto := fmt.Sprintf(`{"forEmailId":%q,"disposition":{"actionMode":"automatic-action","sendingMode":"mdn-sent-automatically","type":"processed"}}`, autoMsg)
	if serr := notSentError(t, e.mdnSendCall(t, auto, sampleOnSuccess)); serr.Type != "forbidden" {
		t.Errorf("automatic MDN for an automatic message = %+v, want forbidden", serr)
	}
	manual := fmt.Sprintf(`{"forEmailId":%q,"disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}`, autoMsg)
	r := e.mdnSendCall(t, manual, sampleOnSuccess)
	var out struct {
		Sent map[jmap.Id]json.RawMessage `json:"sent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Sent["k1546"] == nil {
		t.Errorf("manual MDN for an automatic message failed: %s", r.MethodResponses[0].Args)
	}
}

// TestParseDuplicateBlobIds repeats one unknown id: the verdict lists
// it once, not once per occurrence.
func TestParseDuplicateBlobIds(t *testing.T) {
	e := setupSend(t, nil)
	r := parseCall(t, e, `["Gnope","Gnope","Gnope"]`)
	var out struct {
		NotFound []jmap.Id `json:"notFound"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if len(out.NotFound) != 1 {
		t.Errorf("notFound = %v, want the repeated id once", out.NotFound)
	}
}

// TestParseExtensionNameHygiene feeds extension fields with a repeated
// name and a name outside the field-name grammar: the first value wins
// and the malformed name never becomes an object key.
func TestParseExtensionNameHygiene(t *testing.T) {
	e := setupSend(t, nil)
	raw := "From: gw@example.net\r\nTo: john@example.com\r\nSubject: receipt\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/report; report-type=disposition-notification; boundary=EE\r\n" +
		"\r\n--EE\r\nContent-Type: text/plain\r\n\r\nDisplayed.\r\n" +
		"\r\n--EE\r\nContent-Type: message/disposition-notification\r\n\r\n" +
		"Final-Recipient: rfc822; john@example.net\r\n" +
		"Disposition: manual-action/mdn-sent-manually; displayed\r\n" +
		"X-Dup: first\r\n" +
		"X-Dup: second\r\n" +
		"X-B\xc3\xa4d: dropped\r\n" +
		"\r\n--EE--\r\n"
	blobId := e.uploadBlob(t, "message/rfc822", raw)
	r := parseCall(t, e, fmt.Sprintf(`[%q]`, blobId))
	var out struct {
		Parsed map[jmap.Id]struct {
			ExtensionFields map[string]string `json:"extensionFields"`
		} `json:"parsed"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	got := out.Parsed[blobId].ExtensionFields
	if got["X-Dup"] != "first" {
		t.Errorf("X-Dup = %q, want the first occurrence", got["X-Dup"])
	}
	if len(got) != 1 {
		t.Errorf("extensionFields = %v, want only X-Dup", got)
	}
}
