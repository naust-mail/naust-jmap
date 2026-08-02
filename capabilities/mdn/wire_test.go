package mdn

// Wire-level assertions on what MDN/send actually produces: the raw
// message stored in the sent mailbox (RFC 8098 section 3's report
// structure, the RFC 3834 Auto-Submitted marker, the section 2.1
// prohibition on a Disposition-Notification-To of its own) and the
// queued EmailSubmission's envelope (null reverse-path, NOTIFY=NEVER).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// sentMDNRaw finds the single Email in the sent mailbox and downloads
// its raw message.
func (e *sendEnv) sentMDNRaw(t *testing.T) string {
	t.Helper()
	r := call(t, e.ts, fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/query",{"accountId":"Atest1","filter":{"inMailbox":%q}},"0"],
		["Email/get",{"accountId":"Atest1","#ids":{"resultOf":"0","name":"Email/query","path":"/ids"},"properties":["blobId","keywords"]},"1"]]}`,
		e.sent))
	var got struct {
		List []struct {
			BlobId   jmap.Id         `json:"blobId"`
			Keywords map[string]bool `json:"keywords"`
		} `json:"list"`
	}
	json.Unmarshal(r.MethodResponses[1].Args, &got)
	if len(got.List) != 1 {
		t.Fatalf("sent mailbox holds %d emails, want the one MDN copy: %s", len(got.List), r.MethodResponses[1].Args)
	}
	// The stored copy files as an already-read message.
	if !got.List[0].Keywords["$seen"] {
		t.Errorf("MDN copy keywords = %v, want $seen", got.List[0].Keywords)
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/download/Atest1/"+string(got.List[0].BlobId)+"/mdn", nil)
	req.SetBasicAuth("john@example.com", "secret")
	res, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || len(raw) == 0 {
		t.Fatalf("download = %d, %d bytes", res.StatusCode, len(raw))
	}
	return string(raw)
}

// submissionEnvelope reads the single queued EmailSubmission's
// envelope.
func (e *sendEnv) submissionEnvelope(t *testing.T) (mailFrom string, rcptTo []struct {
	Email      string             `json:"email"`
	Parameters map[string]*string `json:"parameters"`
}) {
	t.Helper()
	r := call(t, e.ts, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"],"methodCalls":[
		["EmailSubmission/query",{"accountId":"Atest1"},"0"],
		["EmailSubmission/get",{"accountId":"Atest1","#ids":{"resultOf":"0","name":"EmailSubmission/query","path":"/ids"},"properties":["envelope"]},"1"]]}`)
	var got struct {
		List []struct {
			Envelope struct {
				MailFrom struct {
					Email string `json:"email"`
				} `json:"mailFrom"`
				RcptTo []struct {
					Email      string             `json:"email"`
					Parameters map[string]*string `json:"parameters"`
				} `json:"rcptTo"`
			} `json:"envelope"`
		} `json:"list"`
	}
	json.Unmarshal(r.MethodResponses[1].Args, &got)
	if len(got.List) != 1 {
		t.Fatalf("submissions = %d, want 1: %s", len(got.List), r.MethodResponses[1].Args)
	}
	return got.List[0].Envelope.MailFrom.Email, got.List[0].Envelope.RcptTo
}

// TestSendWireFull sends an MDN with the returned original and checks
// the produced message and envelope on the wire.
func TestSendWireFull(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.importMsg(t, receivedMsg(map[string]string{
		"Original-Recipient": "rfc822; john-alias@example.com",
	}))
	mdnJSON := fmt.Sprintf(`{
		"forEmailId": %q,
		"subject": "Read receipt",
		"textBody": "Displayed.",
		"includeOriginalMessage": true,
		"disposition": {"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}
	}`, emailId)
	r := e.mdnSendCall(t, mdnJSON, sampleOnSuccess)
	var out struct {
		Sent map[jmap.Id]map[string]string `json:"sent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Sent["k1546"] == nil {
		t.Fatalf("send failed: %s", r.MethodResponses[0].Args)
	}
	// The generated Original-Recipient came from the original's header,
	// so the echo reports it as server-set.
	if got := out.Sent["k1546"]["originalRecipient"]; got != "rfc822; john-alias@example.com" {
		t.Errorf("echoed originalRecipient = %q", got)
	}

	raw := e.sentMDNRaw(t)
	for _, want := range []string{
		// RFC 3834 section 5 via RFC 8098 section 3: a generated MDN is
		// an automatic response.
		"Auto-Submitted: auto-replied",
		"Content-Type: multipart/report; report-type=disposition-notification;",
		"Content-Type: message/disposition-notification",
		"Original-Recipient: rfc822; john-alias@example.com",
		"Final-Recipient: rfc822; john@example.com",
		"Original-Message-ID: <199509192301.23456@example.org>",
		"Disposition: manual-action/mdn-sent-manually; displayed",
		// The third component returns the original in full (RFC 6522
		// section 3).
		"Content-Type: message/rfc822",
		"Subject: World domination",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("MDN wire lacks %q", want)
		}
	}
	// RFC 8098 section 2.1: an MDN MUST NOT itself request an MDN. The
	// check covers the MDN's own header block only - the returned
	// original inside the third component keeps its fields verbatim.
	top, _, _ := strings.Cut(raw, "\r\n\r\n")
	if strings.Contains(top, "Disposition-Notification-To") {
		t.Error("MDN header block carries a Disposition-Notification-To field")
	}

	// RFC 8098 section 3: the MDN's envelope sender is null so it can
	// never bounce a bounce; the recipient is the notification address
	// with DSNs suppressed (RFC 3461 section 4.1).
	mailFrom, rcptTo := e.submissionEnvelope(t)
	if mailFrom != "" {
		t.Errorf("envelope mailFrom = %q, want the null reverse-path", mailFrom)
	}
	if len(rcptTo) != 1 || rcptTo[0].Email != "joe@example.com" {
		t.Fatalf("envelope rcptTo = %+v", rcptTo)
	}
	if p := rcptTo[0].Parameters["NOTIFY"]; p == nil || *p != "NEVER" {
		t.Errorf("rcptTo NOTIFY = %v, want NEVER", p)
	}
}

// TestSendWireMinimal sends the minimal MDN for an original with no
// Message-ID and no Original-Recipient: the optional notification
// fields and the third component must be absent, not invented (RFC
// 8098 sections 3.2.3 and 3.2.5).
func TestSendWireMinimal(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.importMsg(t, receivedMsg(map[string]string{"Message-ID": ""}))
	mdnJSON := fmt.Sprintf(`{"forEmailId":%q,"disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}`, emailId)
	r := e.mdnSendCall(t, mdnJSON, sampleOnSuccess)
	var out struct {
		Sent map[jmap.Id]map[string]string `json:"sent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	echo := out.Sent["k1546"]
	if echo == nil {
		t.Fatalf("send failed: %s", r.MethodResponses[0].Args)
	}
	if _, has := echo["originalMessageId"]; has {
		t.Errorf("echo invents originalMessageId: %v", echo)
	}
	raw := e.sentMDNRaw(t)
	for _, absent := range []string{"Original-Message-ID", "Original-Recipient", "message/rfc822"} {
		if strings.Contains(raw, absent) {
			t.Errorf("minimal MDN wire carries %q", absent)
		}
	}
}

// TestSendWireAddressType: a stated utf-8 address-type survives to the
// wire on both recipient fields (RFC 6533 section 3 registers it
// alongside rfc822), a non-ASCII address is refused at the property
// naming the missing RFC 6533 format, and an Original-Recipient header
// whose value does not validate is omitted (RFC 8098 section 3.2.3: no
// reliable original-recipient information).
func TestSendWireAddressType(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.importMsg(t, receivedMsg(map[string]string{
		"Original-Recipient": "utf-8; john-alias@example.com",
	}))
	mdnJSON := fmt.Sprintf(`{"forEmailId": %q, "finalRecipient": "utf-8; john@example.com",
		"disposition": {"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}`, emailId)
	r := e.mdnSendCall(t, mdnJSON, sampleOnSuccess)
	var out struct {
		Sent map[jmap.Id]map[string]string `json:"sent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Sent["k1546"] == nil {
		t.Fatalf("send failed: %s", r.MethodResponses[0].Args)
	}
	if got := out.Sent["k1546"]["originalRecipient"]; got != "utf-8; john-alias@example.com" {
		t.Errorf("echoed originalRecipient = %q", got)
	}
	raw := e.sentMDNRaw(t)
	for _, want := range []string{
		"Original-Recipient: utf-8; john-alias@example.com",
		"Final-Recipient: utf-8; john@example.com",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("MDN wire lacks %q", want)
		}
	}

	second := e.importMsg(t, receivedMsg(map[string]string{"Message-ID": "<addr-type-2@example.org>"}))
	nonASCII := fmt.Sprintf(`{"forEmailId": %q, "finalRecipient": "utf-8; jos\u00e9@example.com",
		"disposition": {"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}`, second)
	serr := notSentError(t, e.mdnSendCall(t, nonASCII, sampleOnSuccess))
	if serr.Type != "invalidProperties" || !strings.Contains(serr.Description, "6533") {
		t.Errorf("non-ASCII finalRecipient = %+v, want invalidProperties naming RFC 6533", serr)
	}
}

// TestSendUnreliableOriginalRecipient: an Original-Recipient header the
// shared validation refuses never reaches the generated report.
func TestSendUnreliableOriginalRecipient(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.importMsg(t, receivedMsg(map[string]string{
		"Original-Recipient": "x-thing; john-alias@example.com",
	}))
	r := e.mdnSendCall(t, sampleMDN(emailId), sampleOnSuccess)
	var out struct {
		Sent map[jmap.Id]map[string]string `json:"sent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Sent["k1546"] == nil {
		t.Fatalf("send failed: %s", r.MethodResponses[0].Args)
	}
	if _, ok := out.Sent["k1546"]["originalRecipient"]; ok {
		t.Error("unreliable Original-Recipient echoed")
	}
	if raw := e.sentMDNRaw(t); strings.Contains(raw, "Original-Recipient") {
		t.Error("unreliable Original-Recipient reached the wire")
	}
}
