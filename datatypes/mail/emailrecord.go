package mail

import (
	"encoding/json"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// emailMeta is the server-owned metadata an Email record needs beyond
// what the message itself yields (RFC 8621 section 4.1.1). It is supplied
// by whatever creates the record - delivery, Email/import, Email/copy -
// never derived from the message body.
type emailMeta struct {
	BlobID     jmap.Id         // the stored raw-message blob
	ThreadID   jmap.Id         // assigned by threading
	MailboxIds json.RawMessage // Id[Boolean] object, at least one entry
	Keywords   json.RawMessage // String[Boolean] object; nil means {}
	Size       uint64          // octets of the raw message
	ReceivedAt time.Time       // internal date
}

// buildEmailRecord assembles the stored Email record from a parsed message
// and its metadata (the RFC 8621 section 4.2 "fast" list is stored;
// everything else is recomputed from the blob on demand). The
// convenience header properties are computed with the same form functions
// Email/get uses, so the stored value and the on-demand header:{name}
// value can never disagree. The two content-derived fast fields come from the
// parse: hasAttachment from the flattened attachments view, preview from the
// text its preview sinks captured, so the message's parse must have been run
// with a capture that asked for the preview.
func buildEmailRecord(p *parsed, meta emailMeta) objectdb.Object {
	hp := func(field string, form headerForm) json.RawMessage {
		return headerProp{field: field, form: form}.resolve(p.msg.Headers)
	}
	keywords := meta.Keywords
	if keywords == nil {
		keywords = json.RawMessage(`{}`)
	}
	return objectdb.Object{
		"blobId":        mustJSON(meta.BlobID),
		"threadId":      mustJSON(meta.ThreadID),
		"mailboxIds":    meta.MailboxIds,
		"keywords":      keywords,
		"size":          mustJSON(meta.Size),
		"receivedAt":    mustJSON(meta.ReceivedAt.UTC().Format(time.RFC3339)),
		"messageId":     hp("Message-ID", formMessageIds),
		"inReplyTo":     hp("In-Reply-To", formMessageIds),
		"references":    hp("References", formMessageIds),
		"sender":        hp("Sender", formAddresses),
		"from":          hp("From", formAddresses),
		"to":            hp("To", formAddresses),
		"cc":            hp("Cc", formAddresses),
		"bcc":           hp("Bcc", formAddresses),
		"replyTo":       hp("Reply-To", formAddresses),
		"subject":       hp("Subject", formText),
		"sentAt":        hp("Date", formDate),
		"hasAttachment": mustJSON(p.hasAttachment()),
		"preview":       mustJSON(p.preview()),
		"threadKeys":    mustJSON(threadKeyMembers(p.msg.Headers)),
	}
}
