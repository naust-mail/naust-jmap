package mail

// The delivery-side vacation responder. RFC 8621 section 8 requires that
// when a message is delivered to an account with an active
// VacationResponse, the response be sent "in accordance with RFC 3834" -
// whose rules this file implements: who never gets a reply (section 2),
// how the reply is formed (section 3), and the per-sender suppression
// period (section 2's "history"; at least a week is RECOMMENDED).
//
// The reply is not a side channel: it is a real Email in the account's
// sent mailbox and a real EmailSubmission through the one outbound queue,
// created server-side under the same rules a client's send follows - so
// retry, deliveryStatus, report ingestion, and observability all apply to
// it unchanged. The submission's envelope has the null reverse-path
// (RFC 3834 section 3.1.6 SHOULD, and RFC 8621 section 7 permits an empty
// mailFrom email) and NOTIFY=NEVER on the recipient (RFC 3461 section
// 4.1), so an auto-reply can never generate a bounce that generates a
// reply: the loop is cut at both ends.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// TypeVacationNotified is the internal suppression ledger: one record per
// sender an auto-reply was queued for, written in the same commit as the
// reply's submission. No JMAP methods.
const TypeVacationNotified = "VacationNotified"

// vacationSuppressPeriod is how long one reply suppresses further replies
// to the same sender. RFC 3834 section 2 RECOMMENDS at least a week.
const vacationSuppressPeriod = 7 * 24 * time.Hour

func vacationNotifiedType() *descriptor.Type {
	return &descriptor.Type{
		Name:       TypeVacationNotified,
		Capability: VacationCapabilityURI,
		Properties: map[string]descriptor.Property{
			"sender": {Kind: descriptor.KindString, Internal: true, Indexed: true},
			"sentAt": {Kind: descriptor.KindDate, Internal: true},
		},
	}
}

// WithVacationResponder enables the delivery-side auto-responder, wired to
// the submission queue the replies are sent through (the value
// RegisterEmailSubmission returned; RegisterVacationResponse must also be
// registered on the same db, it owns the configuration the responder
// reads). One option, because the responder IS the coupling of delivery
// to the send queue - without a queue there is nowhere spec-compliant to
// put a reply.
func WithVacationResponder(q *SubmissionQueue) DelivererOption {
	return func(d *Deliverer) { d.vacationQ = q }
}

// maybeVacationReply runs after one recipient's delivery committed. Every
// failure mode logs and returns: an auto-response is a courtesy (RFC 3834
// section 1: "in some circumstances it is acceptable"), and nothing about
// it may affect the delivery that triggered it.
func (d *Deliverer) maybeVacationReply(ctx context.Context, acct jmap.Id, rcpt string, env Envelope, msg *parsed, now time.Time) {
	if !vacationShouldReply(env, msg, rcpt) {
		return
	}
	full, _, err := loadVacation(ctx, d.db, acct)
	if err != nil {
		log.Printf("naust-jmap vacation: load: %v", err)
		return
	}
	if !vacationEnabledNow(full, now) {
		return
	}
	identityId, identityEmail, ok, err := d.vacationIdentity(ctx, acct, rcpt, env.MailFrom)
	if err != nil {
		log.Printf("naust-jmap vacation: identities: %v", err)
		return
	}
	if !ok {
		return // no Identity for the delivered-to address (or the sender is ourselves)
	}
	// Best-effort suppression pre-check outside the lease, so the common
	// repeated-sender case never builds a blob it will discard. The
	// authoritative check re-runs under the lease in vacationCommit.
	if ids, err := d.db.IdsWhereEqual(ctx, acct, TypeVacationNotified, "sender", mustJSON(env.MailFrom)); err == nil {
		for _, id := range ids {
			rec, err := d.db.Get(ctx, acct, TypeVacationNotified, id)
			if err != nil {
				continue
			}
			if at, err := parseUTCDateValue(rec["sentAt"]); err == nil && now.Sub(at) < vacationSuppressPeriod {
				return
			}
		}
	}

	raw, replyMsgID := buildVacationReply(identityEmail, env.MailFrom, msg, full, now)
	bw, err := d.store.Create(ctx, acct)
	if err != nil {
		log.Printf("naust-jmap vacation: blob create: %v", err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = bw.Abort()
		}
	}()
	if _, err := bw.Write([]byte(raw)); err != nil {
		log.Printf("naust-jmap vacation: blob write: %v", err)
		return
	}
	replyParsed, err := parseMessage(strings.NewReader(raw), func() *capture { c := newCapture(); c.preview = true; return c }())
	if err != nil {
		log.Printf("naust-jmap vacation: reply parse: %v", err)
		return
	}

	blobID := bw.ID()
	finalized, _, err := d.db.FinalizeBlobUploadThenUpdate(ctx, acct, bw, rcpt, now,
		d.vacationCommit(blobID, int64(len(raw)), replyParsed, replyMsgID, identityId, env.MailFrom, now))
	committed = finalized != ""
	switch {
	case errors.Is(err, errVacationSkip):
		// Suppressed or no sent mailbox: nothing was queued. The blob was
		// already published by the finalize half; unreferenced, it is
		// reclaimed by the blob sweep like any abandoned upload.
		return
	case err != nil || finalized == "":
		log.Printf("naust-jmap vacation: commit: %v", err)
		return
	}
	d.vacationQ.ring()
}

// errVacationSkip aborts the reply commit for a non-error reason: the
// sender is inside the suppression period, or the account has no sent
// mailbox to hold the reply.
var errVacationSkip = errors.New("mail: vacation reply skipped")

// vacationCommit is the lease-held half of one reply: re-check
// suppression under the lease (two concurrent deliveries from one sender
// serialize here, so exactly one wins), create the Email in the sent
// mailbox, its EmailSubmission in the queue, and the suppression entry -
// one commit, so a crash leaves either everything or nothing.
func (d *Deliverer) vacationCommit(blobID jmap.Id, size int64, replyParsed *parsed, replyMsgID string, identityId jmap.Id, sender string, now time.Time) func(u *objectdb.Update) error {
	return func(u *objectdb.Update) error {
		rows, err := u.IdsWhereEqual(TypeVacationNotified, "sender", mustJSON(sender))
		if err != nil {
			return err
		}
		for _, id := range rows {
			rec, err := u.Get(TypeVacationNotified, id)
			if err != nil {
				return err
			}
			at, err := parseUTCDateValue(rec["sentAt"])
			if err == nil && now.Sub(at) < vacationSuppressPeriod {
				return errVacationSkip
			}
			// Expired entry for this sender: replaced by the fresh one below.
			if err := u.Destroy(TypeVacationNotified, id); err != nil {
				return err
			}
		}
		sent, err := roleMailboxId(u, "sent")
		if err != nil {
			return err
		}
		if sent == "" {
			return errVacationSkip
		}
		emailId, err := insertEmail(u, replyParsed, emailMeta{
			BlobID:     blobID,
			MailboxIds: mailboxIdsJSON(map[jmap.Id]bool{sent: true}),
			Keywords:   json.RawMessage(`{"$seen":true}`),
			Size:       uint64(size),
			ReceivedAt: now,
		})
		if err != nil {
			return err
		}
		email, err := u.Get(TypeEmail, emailId)
		if err != nil {
			return err
		}
		nowRaw := mustJSON(now.UTC().Format(time.RFC3339))
		env := subEnvelope{
			MailFrom: &subAddress{Email: ""}, // null reverse-path (RFC 3834 section 3.1.6)
			RcptTo: []subAddress{{Email: sender, Parameters: map[string]*string{
				"NOTIFY": ptrString("NEVER"), // no DSNs about the auto-reply (RFC 3834 section 3.1.6, RFC 3461 section 4.1)
			}}},
		}
		record := objectdb.Object{
			"identityId": mustJSON(identityId),
			"emailId":    mustJSON(emailId),
			"threadId":   email["threadId"],
			"envelope":   mustJSON(env),
			"sendAt":     nowRaw,
			"undoStatus": mustJSON(undoPending),
			"deliveryStatus": mustJSON(map[string]deliveryStatusObj{sender: {
				SmtpReply: "250 2.0.0 message queued for delivery",
				Delivered: "queued",
				Displayed: "unknown",
			}}),
			"attempts":      json.RawMessage(`0`),
			"nextAttemptAt": nowRaw,
			"blobId":        mustJSON(blobID),
		}
		if replyMsgID != "" {
			record["messageId"] = mustJSON(replyMsgID)
		}
		if _, err := u.Create(TypeEmailSubmission, record); err != nil {
			return err
		}
		if err := u.SetAccountTag(submissionQueueTag); err != nil {
			return err
		}
		_, err = u.Create(TypeVacationNotified, objectdb.Object{
			"sender": mustJSON(sender),
			"sentAt": nowRaw,
		})
		return err
	}
}

func ptrString(s string) *string { return &s }

// vacationShouldReply applies the RFC 3834 section 2 refusal rules that
// read the triggering message alone:
//   - never to the null reverse-path (a reply to a bounce is a loop);
//   - never when Auto-Submitted is present with any value other than
//     "no" (the message is itself automatic);
//   - never to list mail (any List-* field, RFC 4021's set);
//   - never to responder/owner addresses by local-part convention;
//   - only when the delivered-to address appears in To or Cc (the
//     message was addressed to this recipient, not merely delivered).
func vacationShouldReply(env Envelope, msg *parsed, rcpt string) bool {
	sender := env.MailFrom
	if sender == "" {
		return false
	}
	local, _, ok := splitAddr(sender)
	if !ok {
		return false
	}
	lower := strings.ToLower(local)
	if lower == "mailer-daemon" || strings.HasPrefix(lower, "owner-") || strings.HasSuffix(lower, "-request") {
		return false
	}
	addressed := false
	for _, h := range msg.msg.Headers {
		name := strings.ToLower(h.Name)
		if strings.HasPrefix(name, "list-") {
			return false
		}
		switch name {
		case "auto-submitted":
			if v := strings.ToLower(strings.TrimSpace(h.Value)); v != "" && v != "no" && !strings.HasPrefix(v, "no ") && !strings.HasPrefix(v, "no;") {
				return false
			}
		case "to", "cc":
			for _, a := range message.AddressesForm(h.Value) {
				if strings.EqualFold(a.Email, rcpt) {
					addressed = true
				}
			}
		}
	}
	return addressed
}

// vacationIdentity finds the Identity the reply is sent as: one whose
// email covers the delivered-to address. No such Identity, no reply -
// the account cannot legitimately send as that address, and an
// auto-responder must not do what a submission could not. It also
// refuses when an Identity covers the SENDER: mail from one of the
// account's own addresses must never trigger a reply to it (RFC 3834
// section 2's loop concern, the self-inflicted case).
func (d *Deliverer) vacationIdentity(ctx context.Context, acct jmap.Id, rcpt, sender string) (jmap.Id, string, bool, error) {
	ids, err := d.db.AllIds(ctx, acct, TypeIdentity, 0)
	if errors.Is(err, objectdb.ErrUnknownType) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	var matchId jmap.Id
	var matchEmail string
	for _, id := range ids {
		rec, err := d.db.Get(ctx, acct, TypeIdentity, id)
		if errors.Is(err, objectdb.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", "", false, err
		}
		email, _ := decodeString(rec["email"])
		if identityAllows(email, sender) {
			return "", "", false, nil // our own sender: never auto-reply to ourselves
		}
		if matchId == "" && identityAllows(email, rcpt) {
			matchId, matchEmail = id, email
			if isWildcardAddr(email) {
				// A wildcard Identity covers the address but names no
				// sendable mailbox; reply as the delivered-to address.
				matchEmail = rcpt
			}
		}
	}
	return matchId, matchEmail, matchId != "", nil
}

// buildVacationReply renders the RFC 3834 section 3 response: To is the
// Return-Path address only (section 3.1.3: the reply goes to the address
// in the envelope, nothing from the message body), Auto-Submitted:
// auto-replied (section 3.1.7 MUST), the subject prefixed "Auto: "
// (section 3.1.5), and In-Reply-To/References threading the triggering
// message (section 3.1.4). The body is the configured text (RFC 8621
// section 8: textBody, else htmlBody, else a brief default).
func buildVacationReply(from, to string, orig *parsed, full map[string]json.RawMessage, now time.Time) (raw, msgID string) {
	subject, _ := decodeString(full["subject"])
	if subject == "" {
		if s := headerInstances(orig.msg.Headers, "Subject"); len(s) > 0 {
			subject = strings.TrimSpace(s[0])
		}
	}
	if subject == "" {
		subject = "Automatic reply"
	}
	if !strings.HasPrefix(subject, "Auto: ") {
		subject = "Auto: " + subject
	}

	body, bodyType := "", "text/plain"
	if s, ok := decodeString(full["textBody"]); ok {
		body = s
	} else if s, ok := decodeString(full["htmlBody"]); ok {
		body, bodyType = s, "text/html"
	}
	if body == "" {
		body = "The recipient is currently away and will read your message later."
	}

	var nb [9]byte
	rand.Read(nb[:])
	domain := "invalid"
	if _, dom, ok := splitAddr(from); ok {
		domain = dom
	}
	msgID = hex.EncodeToString(nb[:]) + "@" + domain

	var b strings.Builder
	b.WriteString("From: <" + stripCtl(from) + ">\r\n")
	b.WriteString("To: <" + stripCtl(to) + ">\r\n")
	b.WriteString("Subject: " + encodeHeaderText(subject) + "\r\n")
	b.WriteString("Date: " + now.Format(receivedDate) + "\r\n")
	b.WriteString("Message-ID: <" + msgID + ">\r\n")
	if ids := message.MessageIDsForm(strings.Join(headerInstances(orig.msg.Headers, "Message-ID"), " ")); len(ids) > 0 {
		b.WriteString("In-Reply-To: <" + ids[0] + ">\r\n")
		b.WriteString("References: <" + ids[0] + ">\r\n")
	}
	b.WriteString("Auto-Submitted: auto-replied\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: " + bodyType + "; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	b.WriteString("\r\n")
	return b.String(), msgID
}

// encodeHeaderText renders unstructured header text: verbatim when it is
// printable ASCII, as RFC 2047 UTF-8 B encoded-words otherwise. Control
// characters can never pass either way, so a hostile subject cannot open
// a new header in the reply.
func encodeHeaderText(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	// Chunk the raw UTF-8 on rune boundaries so each encoded-word stays
	// within RFC 2047 section 2's 75-character limit.
	const chunk = 45
	var words []string
	for b := []byte(s); len(b) > 0; {
		n := chunk
		if n > len(b) {
			n = len(b)
		}
		for n > 1 && n < len(b) && b[n]&0xc0 == 0x80 {
			n--
		}
		words = append(words, "=?utf-8?B?"+base64.StdEncoding.EncodeToString(b[:n])+"?=")
		b = b[n:]
	}
	return strings.Join(words, "\r\n ")
}

// roleMailboxId is the account's Mailbox with the given role, or "" (a
// role is unique per account, RFC 8621 section 2).
func roleMailboxId(u *objectdb.Update, role string) (jmap.Id, error) {
	ids, err := u.IdsWhereEqual(TypeMailbox, "role", mustJSON(role))
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}
