package deliver

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
	"log/slog"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/addr"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
	mailrecord "github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
	"github.com/naust-mail/naust-jmap/datatypes/mail/submit"
)

// typeVacationNotified is the internal suppression ledger: one record per
// sender an auto-reply was queued for, written in the same commit as the
// reply's submission. No JMAP methods.
const typeVacationNotified = mailrecord.TypeVacationNotified

// vacationSuppressPeriod is how long one reply suppresses further replies
// to the same sender. RFC 3834 section 2 RECOMMENDS at least a week.
const vacationSuppressPeriod = 7 * 24 * time.Hour

func vacationNotifiedType() *descriptor.Type {
	return &descriptor.Type{
		Name:       typeVacationNotified,
		Capability: mail.VacationCapabilityURI,
		Properties: map[string]descriptor.Property{
			"sender": {Kind: descriptor.KindString, Internal: true, Indexed: true},
			"sentAt": {Kind: descriptor.KindDate, Internal: true},
		},
	}
}

// WithVacationResponder enables the delivery-side auto-responder, wired to
// the submission queue the replies are sent through (the value
// submit.Register returned; mail.RegisterVacationResponse must also be
// registered on the same db, it owns the configuration the responder
// reads). One option, because the responder IS the coupling of delivery
// to the send queue - without a queue there is nowhere spec-compliant to
// put a reply. New registers this responder's suppression ledger type
// when the option is present.
func WithVacationResponder(q *submit.Queue) Option {
	return func(d *Deliverer) { d.vacationQ = q }
}

// maybeVacationReply runs after one recipient's delivery committed. Every
// failure mode logs and returns: an auto-response is a courtesy (RFC 3834
// section 1: "in some circumstances it is acceptable"), and nothing about
// it may affect the delivery that triggered it.
func (d *Deliverer) maybeVacationReply(ctx context.Context, acct jmap.Id, rcpt string, env Envelope, msg *parse.Parsed, now time.Time) {
	if should, reason := vacationShouldReply(env, msg, rcpt); !should {
		slog.Debug("naust-jmap vacation: not replying", "recipient", rcpt, "reason", reason)
		return
	}
	view, err := mail.ReadVacationResponse(ctx, d.db, acct)
	if err != nil {
		slog.Error("naust-jmap vacation: load", "err", err)
		return
	}
	if !view.ActiveAt(now) {
		slog.Debug("naust-jmap vacation: not replying", "recipient", rcpt, "reason", "not enabled for now")
		return
	}
	identityId, identityEmail, ok, err := d.vacationIdentity(ctx, acct, rcpt, env.MailFrom)
	if err != nil {
		slog.Error("naust-jmap vacation: identities", "err", err)
		return
	}
	if !ok {
		// no Identity for the delivered-to address (or the sender is ourselves)
		slog.Debug("naust-jmap vacation: not replying", "recipient", rcpt, "reason", "no matching identity")
		return
	}
	// Best-effort suppression pre-check outside the lease, so the common
	// repeated-sender case never builds a blob it will discard. The
	// authoritative check re-runs under the lease in the Send Before hook.
	if ids, err := d.db.IdsWhereEqual(ctx, acct, typeVacationNotified, "sender", mailrecord.MustJSON(env.MailFrom), 0); err == nil {
		for _, id := range ids {
			rec, err := d.db.Get(ctx, acct, typeVacationNotified, id)
			if err != nil {
				continue
			}
			if at, err := parseUTCDateValue(rec["sentAt"]); err == nil && now.Sub(at) < vacationSuppressPeriod {
				slog.Debug("naust-jmap vacation: not replying", "recipient", rcpt, "reason", "sender still within suppression period")
				return
			}
		}
	}

	raw, _ := buildVacationReply(identityEmail, env.MailFrom, msg, view, now)
	sender := env.MailFrom
	out := submit.Outgoing{
		IdentityId: identityId,
		// null reverse-path (RFC 3834 section 3.1.6); NOTIFY=NEVER so no
		// DSNs about the auto-reply itself (RFC 3461 section 4.1).
		Envelope: submit.NullPathEnvelope(sender, map[string]*string{
			"NOTIFY": ptrString("NEVER"),
		}),
		MailboxRole: "sent",
		Keywords:    json.RawMessage(`{"$seen":true}`),
	}
	opts := submit.SendOptions{
		Now: func() time.Time { return now },
		// Re-check suppression under the account lease (two concurrent
		// deliveries from one sender serialize here, so exactly one wins),
		// clearing any expired entry for this sender so the fresh one below
		// replaces it.
		Before: func(u *objectdb.Update, now time.Time) error {
			rows, err := u.IdsWhereEqual(typeVacationNotified, "sender", mailrecord.MustJSON(sender))
			if err != nil {
				return err
			}
			for _, id := range rows {
				rec, err := u.Get(typeVacationNotified, id)
				if err != nil {
					return err
				}
				at, err := parseUTCDateValue(rec["sentAt"])
				if err == nil && now.Sub(at) < vacationSuppressPeriod {
					return submit.ErrSkipSend
				}
				if err := u.Destroy(typeVacationNotified, id); err != nil {
					return err
				}
			}
			return nil
		},
		After: func(u *objectdb.Update, res submit.SendResult, now time.Time) error {
			_, err := u.Create(typeVacationNotified, objectdb.Object{
				"sender": mailrecord.MustJSON(sender),
				"sentAt": mailrecord.MustJSON(now.UTC().Format(time.RFC3339)),
			})
			return err
		},
	}
	_, err = d.vacationQ.Sender().Send(ctx, acct, strings.NewReader(raw), out, opts)
	switch {
	case errors.Is(err, submit.ErrSkipSend):
		slog.Debug("naust-jmap vacation: not replying", "recipient", rcpt,
			"reason", "sender still within suppression period (authoritative check under lease)")
		return
	case errors.Is(err, submit.ErrNoRoleMailbox):
		slog.Debug("naust-jmap vacation: not replying", "recipient", rcpt, "reason", "no sent mailbox")
		return
	case err != nil:
		slog.Error("naust-jmap vacation: commit", "err", err)
		return
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
//
// vacationShouldReply reports whether to reply, and, when it refuses, a
// short reason a debug log can show without the caller re-deriving it.
func vacationShouldReply(env Envelope, msg *parse.Parsed, rcpt string) (bool, string) {
	sender := env.MailFrom
	if sender == "" {
		return false, "null reverse-path"
	}
	local, _, ok := addr.Split(sender)
	if !ok {
		return false, "sender address unparsable"
	}
	lower := strings.ToLower(local)
	if lower == "mailer-daemon" || strings.HasPrefix(lower, "owner-") || strings.HasSuffix(lower, "-request") {
		return false, "sender is a responder/owner address"
	}
	addressed := false
	for _, h := range msg.Msg.Headers {
		name := strings.ToLower(h.Name)
		if strings.HasPrefix(name, "list-") {
			return false, "list mail"
		}
		switch name {
		case "auto-submitted":
			if v := strings.ToLower(strings.TrimSpace(h.Value)); v != "" && v != "no" && !strings.HasPrefix(v, "no ") && !strings.HasPrefix(v, "no;") {
				return false, "sender is automatic (Auto-Submitted)"
			}
		case "to", "cc":
			for _, a := range message.AddressesForm(h.Value) {
				if strings.EqualFold(a.Email, rcpt) {
					addressed = true
				}
			}
		}
	}
	if !addressed {
		return false, "recipient not in To/Cc"
	}
	return true, ""
}

// vacationIdentity finds the Identity the reply is sent as: one whose
// email covers the delivered-to address. No such Identity, no reply -
// the account cannot legitimately send as that address, and an
// auto-responder must not do what a submission could not. It also
// refuses when an Identity covers the SENDER: mail from one of the
// account's own addresses must never trigger a reply to it (RFC 3834
// section 2's loop concern, the self-inflicted case).
func (d *Deliverer) vacationIdentity(ctx context.Context, acct jmap.Id, rcpt, sender string) (jmap.Id, string, bool, error) {
	ids, err := d.db.AllIds(ctx, acct, mail.TypeIdentity, 0)
	if errors.Is(err, objectdb.ErrUnknownType) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	var matchId jmap.Id
	var matchEmail string
	for _, id := range ids {
		view, err := mail.ReadIdentity(ctx, d.db, acct, id)
		if errors.Is(err, objectdb.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", "", false, err
		}
		email := view.Email
		if addr.IdentityAllows(email, sender) {
			return "", "", false, nil // our own sender: never auto-reply to ourselves
		}
		if matchId == "" && addr.IdentityAllows(email, rcpt) {
			matchId, matchEmail = id, email
			if addr.IsWildcard(email) {
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
func buildVacationReply(from, to string, orig *parse.Parsed, view *mail.VacationView, now time.Time) (raw, msgID string) {
	subject := view.Subject
	if subject == "" {
		if s := parse.HeaderInstances(orig.Msg.Headers, "Subject"); len(s) > 0 {
			subject = strings.TrimSpace(s[0])
		}
	}
	if subject == "" {
		subject = "Automatic reply"
	}
	if !strings.HasPrefix(subject, "Auto: ") {
		subject = "Auto: " + subject
	}

	body, bodyType := view.TextBody, "text/plain"
	if body == "" && view.HtmlBody != "" {
		body, bodyType = view.HtmlBody, "text/html"
	}
	if body == "" {
		body = "The recipient is currently away and will read your message later."
	}

	var nb [9]byte
	rand.Read(nb[:])
	domain := "invalid"
	if _, dom, ok := addr.Split(from); ok {
		domain = dom
	}
	msgID = hex.EncodeToString(nb[:]) + "@" + domain

	var b strings.Builder
	b.WriteString("From: <" + stripCtl(from) + ">\r\n")
	b.WriteString("To: <" + stripCtl(to) + ">\r\n")
	b.WriteString("Subject: " + encodeHeaderText(subject) + "\r\n")
	b.WriteString("Date: " + now.Format(receivedDate) + "\r\n")
	b.WriteString("Message-ID: <" + msgID + ">\r\n")
	if ids := message.MessageIDsForm(strings.Join(parse.HeaderInstances(orig.Msg.Headers, "Message-ID"), " ")); len(ids) > 0 {
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
