package mdn

// MDN/send (RFC 9007 section 2.1): create MDN messages for received
// Emails and submit them for delivery. Each creation runs its own
// pipeline - validate the MDN object and its onSuccessUpdateEmail
// patch, load the referenced Email, refuse duplicates and unrequested
// or unsafe notifications, assemble the multipart/report, and queue it
// through the submission queue - so one bad entry costs a SetError,
// never the call. The mandated implicit Email/set (section 2.1) runs
// once for every entry that was sent, its response following this
// method's under the same call id.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/datatypes/mail/report"
	"github.com/naust-mail/naust-jmap/datatypes/mail/submit"
)

type mdnSend struct {
	db     *objectdb.DB
	store  blob.Store
	core   jmap.CoreCapabilities
	queue  *submit.Queue
	proc   *runtime.Processor
	policy mail.SendPolicy
}

// internalError logs a backend failure and returns the SetError shown
// to the client: a fixed description, because backend error text can
// name paths, drivers, and internal state no client should see.
func internalError(op string, err error) *jmap.SetError {
	slog.Error("naust-jmap: mdn: "+op, "err", err)
	return &jmap.SetError{Type: jmap.ErrServerFail, Description: "internal error"}
}

// internalFail is internalError's call-level form.
func internalFail(callID, op string, err error) []jmap.Invocation {
	slog.Error("naust-jmap: mdn: "+op, "err", err)
	return runtime.Fail(callID, jmap.ErrServerFail, "internal error")
}

// readTracker records the first error its reader produced, so a blob
// read failure during streaming assembly can be told apart from the
// writer's own validation errors.
type readTracker struct {
	r   io.Reader
	err error
}

func (t *readTracker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF && t.err == nil {
		t.err = err
	}
	return n, err
}

// sendArgNames is the full set of accepted MDN/send arguments (section
// 2.1); any other argument is invalidArguments.
var sendArgNames = map[string]bool{
	"accountId": true, "identityId": true, "send": true,
	"onSuccessUpdateEmail": true,
}

// sendProperties are the MDN properties a client may set in an MDN/send
// creation (section 2): the full property set minus the server-set ones
// (mdnGateway, originalRecipient, originalMessageId, error), which only
// the server may produce.
var sendProperties = map[string]bool{
	"forEmailId": true, "subject": true, "textBody": true,
	"includeOriginalMessage": true, "reportingUA": true,
	"disposition": true, "finalRecipient": true, "extensionFields": true,
}

// sendResponse is the MDN/send response (section 2.1). sent is
// Id[MDN]|null keyed by creation id, holding the properties the client
// did not set; notSent is Id[SetError]|null. A nil map marshals to
// null.
type sendResponse struct {
	AccountID jmap.Id                     `json:"accountId"`
	Sent      map[jmap.Id]json.RawMessage `json:"sent"`
	NotSent   map[jmap.Id]*jmap.SetError  `json:"notSent"`
}

func (h mdnSend) Handle(ctx context.Context, call *runtime.Call) []jmap.Invocation {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(call.Args, &all); err != nil {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	for name := range all {
		if !sendArgNames[name] {
			return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, fmt.Sprintf("unknown argument %q", name))
		}
	}
	var accountID jmap.Id
	json.Unmarshal(all["accountId"], &accountID)
	if errType, desc := runtime.CheckAccount(call, accountID, true); errType != "" {
		return runtime.Fail(call.CallID, errType, desc)
	}

	// Section 2.1: an identityId that cannot be found rejects the call
	// with invalidArguments, like the accountId.
	var identityID jmap.Id
	json.Unmarshal(all["identityId"], &identityID)
	identityID, _ = runtime.ResolveIdArg(identityID, call.CreatedIds)
	if identityID == "" {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, "identityId is required")
	}
	identity, err := mail.ReadIdentity(ctx, h.db, accountID, identityID)
	if errors.Is(err, objectdb.ErrNotFound) || errors.Is(err, objectdb.ErrUnknownType) {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, "identityId not found")
	}
	if err != nil {
		return internalFail(call.CallID, "reading identity", err)
	}

	var send map[jmap.Id]json.RawMessage
	if raw, ok := all["send"]; ok {
		if err := json.Unmarshal(raw, &send); err != nil {
			return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, "send must be a map of creation ids to MDN objects")
		}
	}
	if int64(len(send)) > h.core.MaxObjectsInSet {
		return runtime.Fail(call.CallID, jmap.ErrRequestTooLarge, "")
	}
	resp := sendResponse{AccountID: accountID}
	if len(send) == 0 {
		return runtime.Reply("MDN/send", call.CallID, resp)
	}

	// Section 2.1: the server MUST reject an MDN/send that does not
	// result in setting $mdnsent, so a missing or null
	// onSuccessUpdateEmail cannot accompany a non-empty send map.
	rawOnSuccess, ok := all["onSuccessUpdateEmail"]
	if !ok || string(rawOnSuccess) == "null" {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, "onSuccessUpdateEmail must set the $mdnsent keyword for every sent MDN")
	}
	var onSuccess map[jmap.Id]json.RawMessage
	if err := json.Unmarshal(rawOnSuccess, &onSuccess); err != nil {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, "onSuccessUpdateEmail must be an Id[PatchObject] map or null")
	}

	// Entries run in creation-id order so responses and the implicit
	// set are deterministic for a given request.
	cids := make([]jmap.Id, 0, len(send))
	for cid := range send {
		cids = append(cids, cid)
	}
	sort.Slice(cids, func(i, j int) bool { return cids[i] < cids[j] })

	updates := make(map[jmap.Id]json.RawMessage)
	for _, cid := range cids {
		patch := onSuccess["#"+cid]
		echo, emailID, setErr := h.sendOne(ctx, call, accountID, identityID, identity, send[cid], patch)
		if setErr != nil {
			if resp.NotSent == nil {
				resp.NotSent = make(map[jmap.Id]*jmap.SetError)
			}
			resp.NotSent[cid] = setErr
			continue
		}
		if resp.Sent == nil {
			resp.Sent = make(map[jmap.Id]json.RawMessage)
		}
		resp.Sent[cid] = echo
		updates[emailID] = patch
	}

	out := runtime.Reply("MDN/send", call.CallID, resp)
	if len(updates) > 0 {
		// The implicit Email/set applying the onSuccessUpdateEmail
		// patches (section 2.1); its response follows this one under the
		// same call id.
		args := map[string]any{"accountId": accountID, "update": updates}
		out = append(out, h.proc.ImplicitSet(ctx, mail.TypeEmail, args, call)...)
	}
	return out
}

// sendMDN is the client-settable slice of the MDN object (section 2),
// decoded from one send map entry after the property allowlist check.
type sendMDN struct {
	ForEmailID             jmap.Id           `json:"forEmailId"`
	Subject                string            `json:"subject"`
	TextBody               string            `json:"textBody"`
	IncludeOriginalMessage bool              `json:"includeOriginalMessage"`
	ReportingUA            string            `json:"reportingUA"`
	Disposition            *Disposition      `json:"disposition"`
	FinalRecipient         string            `json:"finalRecipient"`
	ExtensionFields        map[string]string `json:"extensionFields"`
}

// alreadySentDescription is the mdnAlreadySent description used when
// the refusal comes from the server's own issue record rather than the
// visible keyword: $mdnsent is an ordinary keyword a client can clear,
// but RFC 8098 section 2.1 forbids a second MDN for the same message
// regardless, so a retry after clearing the keyword is still refused.
const alreadySentDescription = "the server's record shows an MDN was already sent for this message"

// sendOne runs the full pipeline for one creation. It returns the sent
// echo (the properties the server set or defaulted, section 2.1) and
// the Email id the onSuccessUpdateEmail patch applies to, or a SetError.
func (h mdnSend) sendOne(ctx context.Context, call *runtime.Call, accountID, identityID jmap.Id, identity *mail.IdentityView, raw, patch json.RawMessage) (json.RawMessage, jmap.Id, *jmap.SetError) {
	var props map[string]json.RawMessage
	if err := json.Unmarshal(raw, &props); err != nil {
		return nil, "", &jmap.SetError{Type: jmap.SetErrInvalidProperties, Description: "an MDN creation must be an object"}
	}
	var bad []string
	for name := range props {
		if !sendProperties[name] {
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, "", &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: bad, Description: "unknown or server-set properties"}
	}
	var m sendMDN
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, "", &jmap.SetError{Type: jmap.SetErrInvalidProperties, Description: err.Error()}
	}
	if m.ForEmailID == "" {
		return nil, "", &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: []string{"forEmailId"}, Description: "forEmailId must not be null"}
	}
	if m.Disposition == nil {
		return nil, "", &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: []string{"disposition"}, Description: "disposition must not be null"}
	}
	// Section 2.1: the entry's patch must set $mdnsent, or the MDN must
	// not be sent at all.
	if !patchSetsMdnSent(patch) {
		return nil, "", &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: []string{"onSuccessUpdateEmail"}, Description: "the onSuccessUpdateEmail patch for this entry must set keywords/$mdnsent true"}
	}

	emailID, _ := runtime.ResolveIdArg(m.ForEmailID, call.CreatedIds)
	view, err := mail.ReadEmail(ctx, h.db, h.store, accountID, emailID, mail.ReadEmailOptions{Headers: true})
	if errors.Is(err, objectdb.ErrNotFound) {
		return nil, "", &jmap.SetError{Type: jmap.SetErrNotFound, Description: "forEmailId not found"}
	}
	if err != nil {
		return nil, "", internalError("reading email", err)
	}

	// Fast-path duplicate checks; the authoritative re-check runs under
	// the account lease in the send commit's Before hook.
	if view.HasKeyword("$mdnsent") {
		return nil, "", &jmap.SetError{Type: "mdnAlreadySent", Description: "$mdnsent keyword is already present"}
	}
	if rec, err := h.db.Get(ctx, accountID, mail.TypeEmail, emailID); err == nil && recHasIssuedMDN(rec) {
		return nil, "", &jmap.SetError{Type: "mdnAlreadySent", Description: alreadySentDescription}
	}

	// The disposition request must be present and well-formed: exactly
	// one Disposition-Notification-To field (RFC 8098 section 2.1 allows
	// at most one) with at least one parsable mailbox. Its absence, or a
	// message that is itself an MDN (section 2.1: an MDN MUST NOT be
	// generated in response to an MDN), is notFound - there is no valid
	// notification request to answer. Every Content-Type instance is
	// checked: a hostile message may repeat the field.
	for _, ct := range view.HeaderAll("Content-Type") {
		if strings.Contains(strings.ToLower(ct), "disposition-notification") {
			return nil, "", &jmap.SetError{Type: jmap.SetErrNotFound, Description: "the message is itself a disposition notification"}
		}
	}
	if len(view.HeaderAll("Disposition-Notification-To")) != 1 {
		return nil, "", &jmap.SetError{Type: jmap.SetErrNotFound, Description: "the message has no valid Disposition-Notification-To header field"}
	}
	dnt := view.HeaderAddresses("Disposition-Notification-To")
	if len(dnt) == 0 {
		return nil, "", &jmap.SetError{Type: jmap.SetErrNotFound, Description: "the message has no valid Disposition-Notification-To header field"}
	}

	// RFC 8098 section 2.2: an MDN is not generated when a
	// Disposition-Notification-Options parameter of required importance
	// is not understood. No parameters are understood here, and sending
	// despite optional-only parameters is a MAY, so the field's mere
	// presence suppresses the MDN in both sending modes, without
	// interpreting its contents.
	if len(view.HeaderAll("Disposition-Notification-Options")) != 0 {
		return nil, "", &jmap.SetError{Type: jmap.SetErrForbidden, Description: "the message carries Disposition-Notification-Options; no notification options are supported"}
	}

	// RFC 8098 section 2.1: an MDN MUST NOT be sent automatically unless
	// the notification address equals the Return-Path address, with a
	// single address on each side. A manual send (sendingMode
	// mdn-sent-manually) carries the user's consent instead, which the
	// same section accepts in place of the comparison.
	if m.Disposition.SendingMode == "mdn-sent-automatically" {
		if len(dnt) != 1 {
			return nil, "", &jmap.SetError{Type: jmap.SetErrForbidden, Description: "automatic-mode MDN refused: more than one Disposition-Notification-To address"}
		}
		rp := view.HeaderAddresses("Return-Path")
		if len(view.HeaderAll("Return-Path")) != 1 || len(rp) != 1 || !addrSpecEqual(rp[0].Email, dnt[0].Email) {
			return nil, "", &jmap.SetError{Type: jmap.SetErrForbidden, Description: "automatic-mode MDN refused: the Disposition-Notification-To address does not match the Return-Path address"}
		}
		// RFC 3834 section 2: an automatic responder must not answer a
		// message that is itself automatic. A manual send carries the
		// user's own judgment instead.
		if v := strings.ToLower(strings.TrimSpace(view.Header("Auto-Submitted"))); v != "" && v != "no" && !strings.HasPrefix(v, "no ") && !strings.HasPrefix(v, "no;") {
			return nil, "", &jmap.SetError{Type: jmap.SetErrForbidden, Description: "automatic-mode MDN refused: the message is itself automatic (Auto-Submitted)"}
		}
	}

	// finalRecipient defaults from the Identity, but only when the
	// Identity names one concrete address: a whole-domain wildcard
	// Identity is a permission, not an address, so it yields no default
	// and the client must name the address the MDN is issued for. A
	// client-set value is honored only if the identity is allowed to
	// send as that address (section 5: forbiddenFrom otherwise), and it
	// becomes the MDN's From as well when the identity has no concrete
	// address of its own.
	wildcard := strings.HasPrefix(identity.Email, "*@")
	finalRecipient := report.GenericAddress{Addr: identity.Email}
	fromEmail := identity.Email
	finalDefaulted := true
	if m.FinalRecipient != "" {
		ga, ok := genericAddress(m.FinalRecipient)
		if !ok {
			return nil, "", &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: []string{"finalRecipient"}, Description: "finalRecipient must be an address, optionally with an rfc822 or utf-8 type prefix"}
		}
		// The stated type is carried through to the generated report;
		// what the writer cannot generate (a non-ASCII address needs
		// the RFC 6533 format) is refused here, at the property.
		if err := ga.Validate(); err != nil {
			return nil, "", &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: []string{"finalRecipient"}, Description: err.Error()}
		}
		if !identity.AllowsSend(ga.Addr) {
			return nil, "", &jmap.SetError{Type: "forbiddenFrom", Description: fmt.Sprintf("the identity may not issue an MDN for %s", ga.Addr)}
		}
		finalRecipient, finalDefaulted = ga, false
		if wildcard {
			fromEmail = ga.Addr
		}
	} else if wildcard {
		return nil, "", &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: []string{"finalRecipient"}, Description: "the identity covers a whole domain; finalRecipient must name the address the MDN is issued for"}
	}
	// The grant behind the Identity is re-checked at use: an Identity
	// record outlives a revoked SendPolicy grant, and a submission would
	// re-ask the policy here too.
	if !h.policy.CanSendAs(ctx, accountID, fromEmail) {
		return nil, "", &jmap.SetError{Type: jmap.SetErrForbidden, Description: "the sending policy does not allow this identity to send"}
	}

	// Server-generated correlation fields (section 2.1): the original
	// message's Message-ID, and its Original-Recipient header only when
	// the sender supplied one (RFC 8098 section 3.2.3 MUST NOT invent
	// one otherwise).
	var originalMessageID string
	if ids := view.HeaderMessageIDs("Message-ID"); len(ids) > 0 {
		originalMessageID = ids[0]
	}
	// The sender's value is copied with its stated address-type; one
	// that does not validate is unreliable original-recipient
	// information, and section 3.2.3 omits the field for that case.
	var originalRecipient report.GenericAddress
	if v := view.Header("Original-Recipient"); v != "" {
		if ga, ok := genericAddress(v); ok && ga.Validate() == nil {
			originalRecipient = ga
		}
	}

	var dntName string
	if dnt[0].Name != nil {
		dntName = *dnt[0].Name
	}
	msg := report.Message{
		From:              report.Address{Name: identity.Name, Email: fromEmail},
		To:                report.Address{Name: dntName, Email: dnt[0].Email},
		Subject:           m.Subject,
		TextBody:          m.TextBody,
		ReportingUA:       m.ReportingUA,
		FinalRecipient:    finalRecipient,
		OriginalMessageID: originalMessageID,
		OriginalRecipient: originalRecipient,
		Disposition: report.Disposition{
			ActionMode:  m.Disposition.ActionMode,
			SendingMode: m.Disposition.SendingMode,
			Type:        m.Disposition.Type,
		},
	}
	if len(m.ExtensionFields) > 0 {
		names := make([]string, 0, len(m.ExtensionFields))
		for name := range m.ExtensionFields {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			msg.ExtensionFields = append(msg.ExtensionFields, report.ExtensionField{Name: name, Value: m.ExtensionFields[name]})
		}
	}
	var origRead *readTracker
	if m.IncludeOriginalMessage {
		rc, _, err := mail.OpenEmailMessage(ctx, h.db, h.store, accountID, emailID)
		if err != nil {
			return nil, "", internalError("opening original message", err)
		}
		defer rc.Close()
		// Streamed, not buffered: Write reads the original once while
		// it emits the third report component, and the capped buffer
		// below bounds what can accumulate. An original past the
		// whole-message bound is returned as its header block only
		// (text/rfc822-headers, RFC 6522 section 4): an MDN is a
		// notification, not a relay, and the client has no reduced form
		// to ask for itself (RFC 9007 section 2.1 offers only
		// includeOriginalMessage), so the report carries the most the
		// bound allows rather than refusing.
		origRead = &readTracker{r: rc}
		msg.Original = origRead
		msg.HeadersOnly = view.Size > mdnOriginalWholeBytes
	}

	// The report is assembled up front (the blob is written before the
	// commit either way), so a Message the report package cannot
	// represent - a header injection attempt, a bad enum - fails here as
	// invalidProperties, and the size cap fails as tooLarge. A failure
	// reading the original mid-stream is neither: the tracker tells it
	// apart.
	buf := cappedBuffer{max: mdnAssemblyBytes}
	if err := report.Write(ctx, &buf, msg); err != nil {
		switch {
		case errors.Is(err, errMessageTooLarge):
			return nil, "", &jmap.SetError{Type: jmap.SetErrTooLarge, MaxSize: mdnAssemblyBytes, Description: "the assembled MDN exceeds the maximum message size"}
		case origRead != nil && origRead.err != nil:
			return nil, "", internalError("reading original message", origRead.err)
		default:
			return nil, "", &jmap.SetError{Type: jmap.SetErrInvalidProperties, Description: err.Error()}
		}
	}

	out := submit.Outgoing{
		IdentityId: identityID,
		// Null reverse-path (RFC 8098 section 3: MDNs MUST be sent with
		// an empty envelope sender to stop notification loops);
		// NOTIFY=NEVER (RFC 3461 section 4.1) cuts DSNs about the MDN
		// itself. The stored copy files into the sent mailbox already
		// read. The recipient address is safe in an envelope position
		// because the same string is msg.To.Email, which report.Write
		// validated before a byte was emitted - the Write above must
		// stay ahead of this construction.
		Envelope:    submit.NullPathEnvelope(dnt[0].Email, map[string]*string{"NOTIFY": ptrString("NEVER")}),
		MailboxRole: "sent",
		Keywords:    json.RawMessage(`{"$seen":true}`),
	}
	opts := submit.SendOptions{
		// The Before hook re-runs the duplicate checks under the account
		// lease: two concurrent MDN/send calls for one message serialize
		// here, so exactly one submission is created (RFC 8098 section
		// 2.1's one-MDN-per-message rule holds across requests, not just
		// within one).
		Before: func(u *objectdb.Update, now time.Time) error {
			rec, err := u.Get(mail.TypeEmail, emailID)
			if err != nil {
				return err
			}
			if recHasIssuedMDN(rec) || recHasKeyword(rec, "$mdnsent") {
				return submit.ErrSkipSend
			}
			return nil
		},
		// The After hook stamps the issue record on the original Email in
		// the same commit as the send: the marker exists exactly when the
		// submission does. The record Get returns is the commit's own
		// pre-image view, so it is cloned before the property is added
		// (objectdb's modify-a-copy contract).
		After: func(u *objectdb.Update, res submit.SendResult, now time.Time) error {
			rec, err := u.Get(mail.TypeEmail, emailID)
			if err != nil {
				return err
			}
			stamp, err := json.Marshal(now.UTC().Format(time.RFC3339))
			if err != nil {
				return err
			}
			marked := make(objectdb.Object, len(rec)+1)
			for name, v := range rec {
				marked[name] = v
			}
			marked[issuedAtProperty] = stamp
			return u.PutInternal(mail.TypeEmail, emailID, marked)
		},
	}
	_, err = h.queue.Sender().Send(ctx, accountID, bytes.NewReader(buf.buf.Bytes()), out, opts)
	switch {
	case errors.Is(err, submit.ErrSkipSend):
		return nil, "", &jmap.SetError{Type: "mdnAlreadySent", Description: alreadySentDescription}
	case errors.Is(err, objectdb.ErrNotFound):
		return nil, "", &jmap.SetError{Type: jmap.SetErrNotFound, Description: "forEmailId not found"}
	case errors.Is(err, submit.ErrNoRoleMailbox):
		return nil, "", &jmap.SetError{Type: jmap.ErrServerFail, Description: "the account has no mailbox with the sent role"}
	case err != nil:
		return nil, "", internalError("queueing MDN", err)
	}

	// The sent echo carries what the server set or defaulted (section
	// 2.1): the sample response's forms are kept - finalRecipient in the
	// typed "type; addr" form, originalMessageId as the full Message-ID
	// header value with its angle brackets.
	echo := make(map[string]any)
	if finalDefaulted {
		echo["finalRecipient"] = finalRecipient.String()
	}
	if originalMessageID != "" {
		echo["originalMessageId"] = "<" + originalMessageID + ">"
	}
	if originalRecipient.Addr != "" {
		echo["originalRecipient"] = originalRecipient.String()
	}
	rawEcho, err := json.Marshal(echo)
	if err != nil {
		return nil, "", internalError("encoding sent echo", err)
	}
	return rawEcho, emailID, nil
}

// patchSetsMdnSent reports whether an onSuccessUpdateEmail patch sets
// the $mdnsent keyword true, in either PatchObject form (RFC 8620
// section 5.3): a "keywords/$mdnsent" pointer patch, or a whole
// "keywords" replacement containing the keyword. Anything else fails
// the section 2.1 server check.
func patchSetsMdnSent(patch json.RawMessage) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(patch, &m) != nil {
		return false
	}
	var b bool
	if raw, ok := m["keywords/$mdnsent"]; ok && json.Unmarshal(raw, &b) == nil && b {
		return true
	}
	if raw, ok := m["keywords"]; ok {
		var kw map[string]json.RawMessage
		if json.Unmarshal(raw, &kw) == nil {
			if v, ok := kw["$mdnsent"]; ok && json.Unmarshal(v, &b) == nil && b {
				return true
			}
		}
	}
	return false
}

// recHasIssuedMDN reports whether the stored Email record carries the
// internal issue marker.
func recHasIssuedMDN(rec objectdb.Object) bool {
	v, ok := rec[issuedAtProperty]
	return ok && string(v) != "null"
}

// recHasKeyword reports whether the stored Email record's keywords set
// the named keyword (stored lowercase).
func recHasKeyword(rec objectdb.Object, k string) bool {
	var kw map[string]bool
	if raw, ok := rec["keywords"]; ok && json.Unmarshal(raw, &kw) == nil {
		return kw[k]
	}
	return false
}

// genericAddress splits a generic-address that may carry an
// address-type prefix (RFC 8098 section 3.2.4, e.g.
// "rfc822; john@example.com") into its stated type and address. Only
// the rfc822 and utf-8 types name an email address (RFC 6533 section
// 3); a bare address states no type.
func genericAddress(v string) (report.GenericAddress, bool) {
	typ, addr, ok := strings.Cut(v, ";")
	if !ok {
		addr = strings.TrimSpace(v)
		return report.GenericAddress{Addr: addr}, addr != ""
	}
	t := strings.TrimSpace(typ)
	if !strings.EqualFold(t, "rfc822") && !strings.EqualFold(t, "utf-8") {
		return report.GenericAddress{}, false
	}
	addr = strings.TrimSpace(addr)
	return report.GenericAddress{Type: t, Addr: addr}, addr != ""
}

// addrSpecEqual compares two addr-specs the way RFC 8098 section 2.1
// prescribes for the automatic-send comparison: the local-part case
// sensitively, the domain case insensitively. Quoting canonicalization
// is not attempted; a quoted local-part simply fails the comparison,
// which refuses the automatic send - the safe direction.
func addrSpecEqual(a, b string) bool {
	ai, bi := strings.LastIndexByte(a, '@'), strings.LastIndexByte(b, '@')
	if ai <= 0 || bi <= 0 {
		return false
	}
	return a[:ai] == b[:bi] && strings.EqualFold(a[ai+1:], b[bi+1:])
}

// mdnOriginalWholeBytes bounds the returned original carried whole. An
// original at most this large is returned complete as message/rfc822; a
// larger one is returned as its header block only (RFC 6522 section 4).
const mdnOriginalWholeBytes = 1 << 20

// mdnAssemblyBytes bounds the in-memory assembly of one MDN. Every
// component is separately bounded - the notification content and text
// body by the report package's own capture bound, the returned original
// by mdnOriginalWholeBytes or its header block - summing to well under
// half of this, so the cap is a backstop (a pathological header block on
// the returned original is the one unbounded remainder), not the
// operative limit.
const mdnAssemblyBytes = 2 << 20

// errMessageTooLarge aborts an assembly that has outgrown the assembly
// bound.
var errMessageTooLarge = errors.New("mdn: assembled MDN exceeds the maximum message size")

// cappedBuffer accumulates the assembled MDN, refusing growth past max
// so no single MDN can balloon memory past its bound.
type cappedBuffer struct {
	buf bytes.Buffer
	max uint64
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if uint64(b.buf.Len())+uint64(len(p)) > b.max {
		return 0, errMessageTooLarge
	}
	return b.buf.Write(p)
}

func ptrString(s string) *string { return &s }
