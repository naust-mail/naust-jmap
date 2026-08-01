package mail

// EmailView is the RFC 8621 section 4 Email in the form a server-side
// consumer outside this package (the deliver package's report and MDN
// handling, the submit package) reads it through. It is a read-only
// projection: email.go and emailget.go own the stored record and the
// Email/get wire surface; this is the seam a caller reads it through
// without importing this package's internal parsing and storage details.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailstore"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
)

// EmailAddress is one parsed mailbox from an address header (RFC 8621
// section 4.1.2.3): a display name, when present, and an email address.
type EmailAddress struct {
	Name  *string `json:"name"`
	Email string  `json:"email"`
}

// EmailView holds the section 4 fields a read-only consumer needs: the
// stored metadata every Email carries (section 4.1.1), and, when asked
// for, the message's header fields (section 4.1.3). Headers is nil unless
// ReadEmailOptions.Headers was set on the call that produced the view -
// a consumer that only needs mailboxIds/keywords never opens the blob.
type EmailView struct {
	Id       jmap.Id
	BlobId   jmap.Id
	ThreadId jmap.Id
	// Size is the raw message's octet count (section 4.1.1).
	Size uint64
	// ReceivedAt is the internal date (section 4.1.1).
	ReceivedAt time.Time
	// MailboxIds is the set of Mailboxes the Email belongs to (section
	// 4.1.1); always non-empty for a stored Email.
	MailboxIds map[jmap.Id]bool
	// Keywords is the Email's keyword set (section 4.1.1), keys already
	// lowercase as stored (Email/set normalizes them at write time).
	Keywords map[string]bool
	// Headers is the message's header fields in message order, or nil if
	// ReadEmailOptions.Headers was false. Only the header block of the
	// blob is parsed to populate it; no body content is decoded.
	Headers []message.HeaderField
}

// HasKeyword reports whether the Email carries keyword k. Matching is
// case-insensitive: stored keywords are already lowercase, but a caller
// may pass a keyword in any case (RFC 8621 section 4.1.1 keywords are a
// case-sensitive string type on the wire, though this package's Email/set
// always lowercases what it stores).
func (v *EmailView) HasKeyword(k string) bool {
	return v.Keywords[strings.ToLower(k)]
}

// Header returns the raw value of the first instance of the named header
// field (RFC 5322 field-name matching is case-insensitive), or "" if the
// field is absent or Headers was not requested. Unlike the Email/get
// header:{name} property (which resolves to the last instance, section
// 4.1.3), this is a simple first-match lookup, matching the common
// net/http-style header accessor.
func (v *EmailView) Header(name string) string {
	for _, h := range v.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// HeaderAll returns the raw value of every instance of the named header
// field, in message order. It is nil if the field is absent or Headers
// was not requested.
func (v *EmailView) HeaderAll(name string) []string {
	var out []string
	for _, h := range v.Headers {
		if strings.EqualFold(h.Name, name) {
			out = append(out, h.Value)
		}
	}
	return out
}

// HeaderAddresses parses the named header's instances as an address list
// (RFC 8621 section 4.1.2.3, e.g. From, To, Cc, Bcc, Reply-To, Sender).
// Each instance is parsed and the results concatenated in message order;
// parsing is best effort and never fails, matching internal/message's
// address-list parser.
func (v *EmailView) HeaderAddresses(name string) []EmailAddress {
	var out []EmailAddress
	for _, raw := range v.HeaderAll(name) {
		for _, a := range message.AddressesForm(raw) {
			out = append(out, EmailAddress{Name: a.Name, Email: a.Email})
		}
	}
	return out
}

// HeaderMessageIDs parses the named header's instances as a msg-id list
// (RFC 8621 section 4.1.2.2, e.g. Message-ID, In-Reply-To, References).
// Each instance is parsed and the results concatenated in message order.
func (v *EmailView) HeaderMessageIDs(name string) []string {
	var out []string
	for _, raw := range v.HeaderAll(name) {
		out = append(out, message.MessageIDsForm(raw)...)
	}
	return out
}

// ReadEmailOptions configures ReadEmail.
type ReadEmailOptions struct {
	// Headers requests the message's header fields (populates
	// EmailView.Headers). Only the blob's header block is parsed to
	// satisfy it; no body content is decoded.
	Headers bool
}

// ReadEmail loads the account's Email record (RFC 8621 section 4) as a
// read-only view. A missing record reports objectdb.ErrNotFound, unlike
// ReadVacationResponse: there is no implicit default for an Email that
// does not exist.
func ReadEmail(ctx context.Context, db *objectdb.DB, store blob.Store, acct, id jmap.Id, opts ReadEmailOptions) (*EmailView, error) {
	obj, err := db.Get(ctx, acct, TypeEmail, id)
	if err != nil {
		return nil, err
	}
	v := &EmailView{Id: id}
	json.Unmarshal(obj["blobId"], &v.BlobId)
	json.Unmarshal(obj["threadId"], &v.ThreadId)
	json.Unmarshal(obj["size"], &v.Size)
	if s, ok := decodeString(obj["receivedAt"]); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			v.ReceivedAt = t
		}
	}
	v.MailboxIds = emailstore.MailboxIdsOf(obj)
	v.Keywords = emailstore.ObjectKeys(obj["keywords"])
	if opts.Headers {
		msg, err := parseEmailHeaders(ctx, store, acct, v.BlobId)
		if err != nil {
			return nil, err
		}
		v.Headers = msg.Msg.Headers
	}
	return v, nil
}

// parseEmailHeaders opens the message blob and parses it with an empty
// Capture (internal/parse): the header block is always produced, and with
// every capture flag false, no body content is decoded (no digest, no
// preview, no text) - the same capture Email/get builds for a headers-only
// request (emailget.go's getCapture).
func parseEmailHeaders(ctx context.Context, store blob.Store, acct, blobID jmap.Id) (*parse.Parsed, error) {
	rc, _, err := store.Open(ctx, acct, blobID)
	if err != nil {
		return nil, fmt.Errorf("mail: opening message blob %s: %w", blobID, err)
	}
	defer rc.Close()
	return parse.ParseMessage(rc, parse.NewCapture())
}

// OpenEmailMessage opens the Email's raw message blob for streaming read
// (RFC 8620 section 6.1). The caller must close the returned stream. A
// missing Email record reports objectdb.ErrNotFound.
func OpenEmailMessage(ctx context.Context, db *objectdb.DB, store blob.Store, acct, id jmap.Id) (io.ReadCloser, int64, error) {
	obj, err := db.Get(ctx, acct, TypeEmail, id)
	if err != nil {
		return nil, 0, err
	}
	var blobID jmap.Id
	if err := json.Unmarshal(obj["blobId"], &blobID); err != nil {
		return nil, 0, fmt.Errorf("mail: Email record has no blobId: %w", err)
	}
	return store.Open(ctx, acct, blobID)
}
