package emailstore

import (
	"encoding/json"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// EmailMeta is the server-owned metadata an Email record needs beyond
// what the message itself yields (RFC 8621 section 4.1.1). It is supplied
// by whatever creates the record - delivery, Email/import, Email/copy -
// never derived from the message body.
type EmailMeta struct {
	BlobID     jmap.Id         // the stored raw-message blob
	ThreadID   jmap.Id         // assigned by threading
	MailboxIds json.RawMessage // Id[Boolean] object, at least one entry
	Keywords   json.RawMessage // String[Boolean] object; nil means {}
	Size       uint64          // octets of the raw message
	ReceivedAt time.Time       // internal date
}

// BuildEmailRecord assembles the stored Email record from a parsed message
// and its metadata (the RFC 8621 section 4.2 "fast" list is stored;
// everything else is recomputed from the blob on demand). The
// convenience header properties are computed with the same form functions
// Email/get uses, so the stored value and the on-demand header:{name}
// value can never disagree. The two content-derived fast fields come from the
// parse: hasAttachment from the flattened attachments view, preview from the
// text its preview sinks captured, so the message's parse must have been run
// with a capture that asked for the preview.
func BuildEmailRecord(p *parse.Parsed, meta EmailMeta) objectdb.Object {
	hp := func(field string, form HeaderForm) json.RawMessage {
		return HeaderProp{Field: field, Form: form}.Resolve(p.Msg.Headers)
	}
	keywords := meta.Keywords
	if keywords == nil {
		keywords = json.RawMessage(`{}`)
	}
	return objectdb.Object{
		"blobId":        record.MustJSON(meta.BlobID),
		"threadId":      record.MustJSON(meta.ThreadID),
		"mailboxIds":    meta.MailboxIds,
		"keywords":      keywords,
		"size":          record.MustJSON(meta.Size),
		"receivedAt":    record.MustJSON(meta.ReceivedAt.UTC().Format(time.RFC3339)),
		"messageId":     hp("Message-ID", FormMessageIds),
		"inReplyTo":     hp("In-Reply-To", FormMessageIds),
		"references":    hp("References", FormMessageIds),
		"sender":        hp("Sender", FormAddresses),
		"from":          hp("From", FormAddresses),
		"to":            hp("To", FormAddresses),
		"cc":            hp("Cc", FormAddresses),
		"bcc":           hp("Bcc", FormAddresses),
		"replyTo":       hp("Reply-To", FormAddresses),
		"subject":       hp("Subject", FormText),
		"sentAt":        hp("Date", FormDate),
		"hasAttachment": record.MustJSON(p.HasAttachment()),
		"preview":       record.MustJSON(p.Preview()),
		"threadKeys":    record.MustJSON(threadKeyMembers(p.Msg.Headers)),
	}
}
