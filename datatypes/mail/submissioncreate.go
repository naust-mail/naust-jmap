package mail

// EmailSubmission/set create (RFC 8621 sections 7 and 7.5): resolving the
// Email and Identity, the submission-time strict RFC 5322 validity check
// (a draft may be deliberately invalid at Email creation; sending is
// where strictness lives), the envelope - the client's, or derived by the
// section 7 algorithm - the recipient and size limits, the SendPolicy
// gates, and the RFC 4865 FUTURERELEASE hold. Like every other producer
// it splits around the account lease via the create override: everything
// above runs in prepare, outside the lease; commit only re-checks the
// Email still exists and stages the record.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// submissionCreate is the EmailSubmission/set create override.
type submissionCreate struct {
	db     *objectdb.DB
	store  blob.Store
	policy SendPolicy
	limits SubmissionLimits
}

// preparedSubmission is one submission ready to commit.
type preparedSubmission struct {
	record  objectdb.Object
	emailId jmap.Id
	// envelopeChanged marks a stored envelope that differs from what the
	// client sent (derived, hold params consumed, or mailFrom substituted
	// from the Identity), so the created echo must carry it.
	envelopeChanged bool
}

// subEnvelope and subAddress are the section 7 Envelope and Address
// objects. Parameter values are the section 7 decoded form: xtext
// removed, string or null.
type subEnvelope struct {
	MailFrom *subAddress  `json:"mailFrom"`
	RcptTo   []subAddress `json:"rcptTo"`
}

type subAddress struct {
	Email      string             `json:"email"`
	Parameters map[string]*string `json:"parameters"`
}

// prepare implements SetHooks.PrepareCreate.
func (h submissionCreate) prepare(ctx context.Context, call *runtime.Call, acct, cid jmap.Id, raw json.RawMessage) (any, *jmap.SetError, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, &jmap.SetError{Type: jmap.SetErrInvalidProperties, Description: "create value is not an object"}, nil
	}
	// Only identityId, emailId, and envelope are client-settable at create
	// (section 7: everything else is server-set, and undoStatus is "server
	// set on create").
	var bad []string
	for name := range obj {
		switch name {
		case "identityId", "emailId", "envelope":
		default:
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: bad}, nil
	}

	// Resolve both references; an id that cannot be found rejects the
	// creation with invalidProperties (section 7.5).
	emailId, serr := h.resolveRef(obj, "emailId", call.CreatedIds)
	if serr != nil {
		return nil, serr, nil
	}
	identityId, serr := h.resolveRef(obj, "identityId", call.CreatedIds)
	if serr != nil {
		return nil, serr, nil
	}
	email, err := h.db.Get(ctx, acct, TypeEmail, emailId)
	if errors.Is(err, objectdb.ErrNotFound) {
		return nil, invalidProp("emailId", "no such Email"), nil
	}
	if err != nil {
		return nil, nil, err
	}
	identity, err := h.db.Get(ctx, acct, TypeIdentity, identityId)
	if errors.Is(err, objectdb.ErrNotFound) {
		return nil, invalidProp("identityId", "no such Identity"), nil
	}
	if err != nil {
		return nil, nil, err
	}
	identityEmail, _ := decodeString(identity["email"])

	// The submission-time strict check parses the message's blob: the
	// stored Email record cannot answer field multiplicity (two From
	// fields collapse to one stored value), and section 7.5 makes create
	// the moment the server "must check that the message is valid".
	var msgBlobId jmap.Id
	json.Unmarshal(email["blobId"], &msgBlobId)
	rc, _, err := runtime.OpenBlob(ctx, h.db, h.store, acct, msgBlobId, call.Identity)
	if err != nil {
		return nil, nil, err // the Email record's BlobRef guarantees the blob
	}
	msg, err := parseMessage(rc, newCapture())
	rc.Close()
	if err != nil {
		return nil, &jmap.SetError{Type: "invalidEmail", Description: "message does not parse: " + err.Error()}, nil
	}
	fromAddrs, senderAddrs, badProps := check5322(msg.msg.Headers)
	if len(badProps) > 0 {
		return nil, &jmap.SetError{Type: "invalidEmail", Properties: badProps, Description: "the message is not a valid RFC 5322 email"}, nil
	}

	// Envelope: the client's, or derived from the message (section 7).
	env, derived, serr := h.buildEnvelope(obj["envelope"], msg.msg.Headers, fromAddrs, senderAddrs, identityEmail)
	if serr != nil {
		return nil, serr, nil
	}
	if serr := h.checkRecipients(env); serr != nil {
		return nil, serr, nil
	}
	var size uint64
	json.Unmarshal(email["size"], &size)
	if h.limits.MaxMessageBytes > 0 && size > h.limits.MaxMessageBytes {
		return nil, &jmap.SetError{Type: jmap.SetErrTooLarge, MaxSize: h.limits.MaxMessageBytes, Description: "message exceeds the maximum sendable size"}, nil
	}

	// Policy gates (section 7.5): the envelope sender, the message's From
	// against the Identity (an internal comparison, wildcards included),
	// and the account's standing permission to send at all.
	if !h.policy.CanSendAs(ctx, acct, env.MailFrom.Email) {
		return nil, &jmap.SetError{Type: "forbiddenMailFrom", Description: "not allowed to send with envelope from " + env.MailFrom.Email}, nil
	}
	for _, a := range fromAddrs {
		if !identityAllows(identityEmail, a.Email) {
			return nil, &jmap.SetError{Type: "forbiddenFrom", Description: "the Identity does not allow From address " + a.Email}, nil
		}
	}
	if ok, reason := h.policy.CanSend(ctx, acct); !ok {
		return nil, &jmap.SetError{Type: "forbiddenToSend", Description: reason}, nil
	}

	now := time.Now().UTC()
	sendAt, holdStripped, serr := h.applyHold(env, now)
	if serr != nil {
		return nil, serr, nil
	}

	sendAtRaw := mustJSON(sendAt.UTC().Format(time.RFC3339))
	ds := make(map[string]deliveryStatusObj, len(env.RcptTo))
	for _, r := range env.RcptTo {
		// No SMTP exchange has happened yet; the synthetic reply (section
		// 7's three-part form) records our own acceptance into the queue.
		ds[r.Email] = deliveryStatusObj{
			SmtpReply: "250 2.0.0 message queued for delivery",
			Delivered: "queued",
			Displayed: "unknown",
		}
	}
	record := objectdb.Object{
		"identityId":     mustJSON(identityId),
		"emailId":        mustJSON(emailId),
		"threadId":       email["threadId"],
		"envelope":       mustJSON(env),
		"sendAt":         sendAtRaw,
		"undoStatus":     mustJSON(undoPending),
		"deliveryStatus": mustJSON(ds),
		"dsnBlobIds":     json.RawMessage(`[]`),
		"mdnBlobIds":     json.RawMessage(`[]`),
		"attempts":       json.RawMessage(`0`),
		"nextAttemptAt":  sendAtRaw,
		"blobId":         email["blobId"],
	}
	return &preparedSubmission{
		record:          record,
		emailId:         emailId,
		envelopeChanged: derived || holdStripped,
	}, nil, nil
}

// commit implements SetHooks.CommitCreate. The Email is re-checked under
// the lease: destroying it after the submission exists is explicitly fine
// (section 7.5), but a create must reference a live Email at the moment
// it commits.
func (h submissionCreate) commit(u *objectdb.Update, prepared any) (jmap.Id, objectdb.Object, *jmap.SetError, error) {
	ps := prepared.(*preparedSubmission)
	if _, err := u.Get(TypeEmail, ps.emailId); errors.Is(err, objectdb.ErrNotFound) {
		return "", nil, invalidProp("emailId", "no such Email"), nil
	} else if err != nil {
		return "", nil, nil, err
	}
	id, err := u.Create(TypeEmailSubmission, ps.record)
	if err != nil {
		return "", nil, nil, err
	}
	// The account now has queued work: tag it onto the queue worklist in
	// the same commit, so a scanning worker (this process's or another's)
	// can never miss it.
	if err := u.SetAccountTag(submissionQueueTag); err != nil {
		return "", nil, nil, err
	}
	// The created echo carries every property the client did not send
	// (RFC 8620 section 5.3), envelope included when the stored one
	// differs from what was sent.
	echo := objectdb.Object{
		"id":             mustJSON(id),
		"threadId":       ps.record["threadId"],
		"sendAt":         ps.record["sendAt"],
		"undoStatus":     ps.record["undoStatus"],
		"deliveryStatus": ps.record["deliveryStatus"],
		"dsnBlobIds":     ps.record["dsnBlobIds"],
		"mdnBlobIds":     ps.record["mdnBlobIds"],
	}
	if ps.envelopeChanged {
		echo["envelope"] = ps.record["envelope"]
	}
	return id, echo, nil, nil
}

// resolveRef reads a required Id property, resolving a "#creationId"
// reference against the request-wide map (RFC 8620 section 5.3).
func (h submissionCreate) resolveRef(obj map[string]json.RawMessage, prop string, createdIds map[jmap.Id]jmap.Id) (jmap.Id, *jmap.SetError) {
	s, ok := decodeString(obj[prop])
	if !ok || s == "" {
		return "", invalidProp(prop, prop+" is required")
	}
	id, ok := runtime.ResolveIdArg(jmap.Id(s), createdIds)
	if !ok {
		return "", invalidProp(prop, "reference to a record that was not created")
	}
	return id, nil
}

// check5322 is the submission-time validity check of the message (RFC
// 5322 section 3.6: exactly one From with at least one address, a Sender
// when From has several, exactly one parseable Date), reported through
// the invalidEmail SetError's properties list in Email property names.
func check5322(headers []message.HeaderField) (fromAddrs, senderAddrs []message.Address, bad []string) {
	froms := headerInstances(headers, "From")
	senders := headerInstances(headers, "Sender")
	dates := headerInstances(headers, "Date")
	if len(froms) == 1 {
		fromAddrs = message.AddressesForm(froms[0])
	}
	if len(fromAddrs) == 0 {
		bad = append(bad, "from")
	}
	if len(senders) > 1 {
		bad = append(bad, "sender")
	} else if len(senders) == 1 {
		senderAddrs = message.AddressesForm(senders[0])
		if len(senderAddrs) != 1 {
			bad = append(bad, "sender")
		}
	}
	// A multi-address From requires a Sender (RFC 5322 section 3.6.2).
	if len(fromAddrs) > 1 && len(senderAddrs) == 0 && !contains(bad, "sender") {
		bad = append(bad, "sender")
	}
	if len(dates) != 1 || message.DateForm(dates[0]) == nil {
		bad = append(bad, "sentAt")
	}
	sort.Strings(bad)
	return fromAddrs, senderAddrs, bad
}

// buildEnvelope returns the envelope to relay with: the client's,
// validated, or one derived by the section 7 algorithm. derived reports
// whether the stored envelope differs from the client's value.
func (h submissionCreate) buildEnvelope(raw json.RawMessage, headers []message.HeaderField, fromAddrs, senderAddrs []message.Address, identityEmail string) (*subEnvelope, bool, *jmap.SetError) {
	if raw != nil && !isNullRaw(raw) {
		env, serr := decodeEnvelope(raw)
		if serr != nil {
			return nil, false, serr
		}
		return env, false, nil
	}

	// mailFrom: the Sender address if present, else From (section 7). The
	// validity check already rejected multiple Sender/From fields and a
	// multi-address From without Sender, so the source field here has
	// exactly one address... unless From has several AND a Sender exists,
	// in which case Sender is the source anyway.
	source := fromAddrs
	if len(senderAddrs) == 1 {
		source = senderAddrs
	}
	if len(source) != 1 {
		return nil, false, &jmap.SetError{Type: "invalidEmail", Properties: []string{"from"}, Description: "cannot derive a single envelope sender"}
	}
	mailFrom := source[0].Email
	// "If the address found from this is not allowed by the Identity...
	// the email property from the Identity MUST be used instead"
	// (section 7). A wildcard Identity has no single substitutable
	// address, so a sender outside its domain is refused instead.
	if !identityAllows(identityEmail, mailFrom) {
		if isWildcardAddr(identityEmail) {
			return nil, false, &jmap.SetError{Type: "forbiddenMailFrom", Description: "envelope sender is outside the Identity's domain"}
		}
		mailFrom = identityEmail
	}

	// rcptTo: the deduplicated To, Cc, and Bcc addresses, no parameters
	// (section 7).
	var rcpts []subAddress
	seen := make(map[string]bool)
	for _, field := range []string{"To", "Cc", "Bcc"} {
		for _, instance := range headerInstances(headers, field) {
			for _, a := range message.AddressesForm(instance) {
				if a.Email == "" || seen[a.Email] {
					continue
				}
				seen[a.Email] = true
				rcpts = append(rcpts, subAddress{Email: a.Email})
			}
		}
	}
	return &subEnvelope{MailFrom: &subAddress{Email: mailFrom}, RcptTo: rcpts}, true, nil
}

// decodeEnvelope strictly decodes and validates a client-supplied
// envelope: the section 7 object shape, an address-shaped mailFrom (the
// spec's empty-string MAY is not taken up), and well-formed SMTP
// parameters throughout.
func decodeEnvelope(raw json.RawMessage) (*subEnvelope, *jmap.SetError) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var env subEnvelope
	if err := dec.Decode(&env); err != nil {
		return nil, invalidProp("envelope", "not an Envelope object")
	}
	if env.MailFrom == nil {
		return nil, invalidProp("envelope", "mailFrom is required")
	}
	if _, _, ok := splitAddr(env.MailFrom.Email); !ok {
		return nil, invalidProp("envelope", "mailFrom is not an email address")
	}
	if serr := checkSMTPParams(env.MailFrom.Parameters); serr != nil {
		return nil, serr
	}
	for _, r := range env.RcptTo {
		if serr := checkSMTPParams(r.Parameters); serr != nil {
			return nil, serr
		}
	}
	return &env, nil
}

// checkRecipients enforces the section 7.5 recipient errors on the final
// envelope, supplied or derived.
func (h submissionCreate) checkRecipients(env *subEnvelope) *jmap.SetError {
	if len(env.RcptTo) == 0 {
		return &jmap.SetError{Type: "noRecipients", Description: "the envelope has no recipients"}
	}
	if h.limits.MaxRecipients > 0 && uint64(len(env.RcptTo)) > h.limits.MaxRecipients {
		return &jmap.SetError{Type: "tooManyRecipients", MaxRecipients: h.limits.MaxRecipients, Description: "too many envelope recipients"}
	}
	var invalid []string
	for _, r := range env.RcptTo {
		if _, _, ok := splitAddr(r.Email); !ok {
			invalid = append(invalid, r.Email)
		}
	}
	if invalid != nil {
		return &jmap.SetError{Type: "invalidRecipients", InvalidRecipients: invalid, Description: "some recipients are not valid email addresses"}
	}
	return nil
}

// checkSMTPParams validates decoded MAIL/RCPT parameters: esmtp-keyword
// names and printable US-ASCII values (RFC 5321 section 4.1.2; RFC 3461
// section 4 requires the pre-xtext value be printable US-ASCII).
func checkSMTPParams(params map[string]*string) *jmap.SetError {
	for name, val := range params {
		if !validESMTPKeyword(name) {
			return invalidProp("envelope", fmt.Sprintf("invalid SMTP parameter name %q", name))
		}
		if val == nil {
			continue
		}
		for _, r := range *val {
			if r < 0x20 || r > 0x7e {
				return invalidProp("envelope", fmt.Sprintf("SMTP parameter %q has a non-printable value", name))
			}
		}
	}
	return nil
}

// validESMTPKeyword reports whether s is an esmtp-keyword (RFC 5321
// section 4.1.2: alphanumeric first, then alphanumeric or hyphen).
func validESMTPKeyword(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alnum := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if !alnum && (i == 0 || r != '-') {
			return false
		}
	}
	return true
}

// applyHold consumes an RFC 4865 FUTURERELEASE hold from the envelope's
// mailFrom parameters, returning the release time. The hold is enforced
// here - the submission queue holds the message and relays normally at
// release, so the smarthost needs no FUTURERELEASE support - and the
// parameter is stripped from the stored envelope, which is exactly what
// will be relayed. Exceeding maxDelayedSend is refused, not clamped (RFC
// 4865 section 4.2 items 7-8: an over-limit hold MUST be rejected;
// silently sending earlier than asked would misrepresent the request).
func (h submissionCreate) applyHold(env *subEnvelope, now time.Time) (sendAt time.Time, stripped bool, serr *jmap.SetError) {
	var holdKeys []string
	for name := range env.MailFrom.Parameters {
		if strings.EqualFold(name, "HOLDFOR") || strings.EqualFold(name, "HOLDUNTIL") {
			holdKeys = append(holdKeys, name)
		}
	}
	if len(holdKeys) == 0 {
		return now, false, nil
	}
	// "An MSA... MUST reject a MAIL command containing more than one
	// hold-param" (RFC 4865 section 4.2 item 9).
	if len(holdKeys) > 1 {
		return time.Time{}, false, invalidProp("envelope", "more than one FUTURERELEASE hold parameter")
	}
	if h.limits.MaxDelayedSend <= 0 {
		return time.Time{}, false, invalidProp("envelope", "delayed send is not supported")
	}
	key := holdKeys[0]
	val := env.MailFrom.Parameters[key]
	if val == nil {
		return time.Time{}, false, invalidProp("envelope", key+" requires a value")
	}
	if strings.EqualFold(key, "HOLDFOR") {
		// future-release-interval: an integer 1-999999999 seconds (RFC
		// 4865 section 3).
		secs, err := strconv.ParseInt(*val, 10, 64)
		if err != nil || secs < 1 || secs > 999_999_999 {
			return time.Time{}, false, invalidProp("envelope", "HOLDFOR is not a valid interval")
		}
		if secs > h.limits.MaxDelayedSend {
			return time.Time{}, false, invalidProp("envelope", fmt.Sprintf("HOLDFOR exceeds maxDelayedSend (%d seconds)", h.limits.MaxDelayedSend))
		}
		sendAt = now.Add(time.Duration(secs) * time.Second)
	} else {
		// future-release-date-time: an RFC 3339 timestamp (RFC 4865
		// section 3). A time already past just releases immediately.
		t, err := time.Parse(time.RFC3339, *val)
		if err != nil {
			return time.Time{}, false, invalidProp("envelope", "HOLDUNTIL is not a valid date-time")
		}
		if t.Sub(now) > time.Duration(h.limits.MaxDelayedSend)*time.Second {
			return time.Time{}, false, invalidProp("envelope", fmt.Sprintf("HOLDUNTIL exceeds maxDelayedSend (%d seconds)", h.limits.MaxDelayedSend))
		}
		sendAt = t
	}
	delete(env.MailFrom.Parameters, key)
	if len(env.MailFrom.Parameters) == 0 {
		env.MailFrom.Parameters = nil
	}
	return sendAt, true, nil
}

// identityAllows reports whether an Identity with the given email allows
// sending as addr: an exact match (domain ASCII case-insensitive, local
// part exact), or any address in the domain for a wildcard Identity
// (RFC 8621 section 6).
func identityAllows(identityEmail, addr string) bool {
	idLocal, idDomain, ok := splitAddr(identityEmail)
	if !ok {
		return false
	}
	local, domain, ok := splitAddr(addr)
	if !ok || !strings.EqualFold(domain, idDomain) {
		return false
	}
	return idLocal == "*" || idLocal == local
}

// isWildcardAddr reports whether addr is the whole-domain wildcard form.
func isWildcardAddr(addr string) bool {
	local, _, ok := splitAddr(addr)
	return ok && local == "*"
}

// headerInstances collects the raw values of every instance of one header
// field, in message order.
func headerInstances(headers []message.HeaderField, name string) []string {
	var out []string
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			out = append(out, h.Value)
		}
	}
	return out
}
