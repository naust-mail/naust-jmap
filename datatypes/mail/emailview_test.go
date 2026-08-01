package mail

// EmailView tests (RFC 8621 section 4): field projection against a
// delivered fixture, the Headers on/off distinction, the header accessor
// methods (case-insensitive matching, RFC 5322 section 2.2), the address
// and msg-id list helpers (section 4.1.2.2/4.1.2.3), OpenEmailMessage's
// blob round-trip, and not-found for a missing record.
//
// RFC 8621 defines no worked JSON example for a stored Email or its
// header forms (unlike, e.g., its filter examples), so these fixtures are
// built from the section 4.1.2/4.1.3 grammar directly: multiple header
// field instances, multi-address instances, and multi-id References.

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

const viewMessage = "From: Joe Bloggs <joe@example.com>\r\n" +
	"To: Jane Doe <jane@example.com>, Sam Roe <sam@example.com>\r\n" +
	"Subject: Dinner on Thursday?\r\n" +
	"Message-ID: <msg1@example.com>\r\n" +
	"References: <a@example.com> <b@example.com>\r\n" +
	"X-Custom: first\r\n" +
	"X-Custom: second\r\n" +
	"Date: Wed, 03 Mar 2021 10:00:00 +0000\r\n" +
	"\r\n" +
	"Hi Jane, are you free on Thursday evening?\r\n"

// TestReadEmail_FieldProjection checks every EmailView field against a
// known fixture, with Headers left off (the default a mailboxIds/keywords
// consumer would use).
func TestReadEmail_FieldProjection(t *testing.T) {
	_, db, store := emailServer(t)
	id := putEmail(t, db, store, viewMessage,
		map[string]bool{"MBinbox": true}, map[string]bool{"$seen": true})

	v, err := ReadEmail(context.Background(), db, store, testAccount, jmap.Id(id), ReadEmailOptions{})
	if err != nil {
		t.Fatalf("ReadEmail: %v", err)
	}
	if string(v.Id) != id {
		t.Errorf("Id = %v, want %s", v.Id, id)
	}
	if v.BlobId == "" {
		t.Error("BlobId is empty")
	}
	if v.ThreadId == "" {
		t.Error("ThreadId is empty")
	}
	if v.Size != uint64(len(viewMessage)) {
		t.Errorf("Size = %d, want %d", v.Size, len(viewMessage))
	}
	if !v.ReceivedAt.Equal(testReceivedAt) {
		t.Errorf("ReceivedAt = %v, want %v", v.ReceivedAt, testReceivedAt)
	}
	if !v.MailboxIds[jmap.Id("MBinbox")] || len(v.MailboxIds) != 1 {
		t.Errorf("MailboxIds = %v", v.MailboxIds)
	}
	if !v.Keywords["$seen"] || len(v.Keywords) != 1 {
		t.Errorf("Keywords = %v", v.Keywords)
	}
	if v.Headers != nil {
		t.Errorf("Headers should be nil when not requested, got %v", v.Headers)
	}
}

// TestReadEmail_HeadersOnOff checks the Headers-populated case, and that
// it stays nil (rather than empty) when not requested. The parse path for
// Headers: true is the same empty-Capture parse Email/get uses for a
// headers-only request (emailget.go's getCapture with no properties
// selecting identity/values/preview), so no digest is hashed and no text
// is decoded for it - there is nothing content-derived on EmailView to
// observe that with, so this is asserted by code reference rather than by
// a runtime probe.
func TestReadEmail_HeadersOnOff(t *testing.T) {
	_, db, store := emailServer(t)
	id := putEmail(t, db, store, viewMessage, map[string]bool{"MBinbox": true}, nil)

	off, err := ReadEmail(context.Background(), db, store, testAccount, jmap.Id(id), ReadEmailOptions{})
	if err != nil {
		t.Fatalf("ReadEmail (headers off): %v", err)
	}
	if off.Headers != nil {
		t.Errorf("Headers off: got %v, want nil", off.Headers)
	}

	on, err := ReadEmail(context.Background(), db, store, testAccount, jmap.Id(id), ReadEmailOptions{Headers: true})
	if err != nil {
		t.Fatalf("ReadEmail (headers on): %v", err)
	}
	if len(on.Headers) == 0 {
		t.Fatal("Headers on: got none")
	}
	// Header returns the Raw form (RFC 8621 section 4.1.2.1): everything
	// after the colon, including the one leading space "Name: value"
	// conventionally carries.
	if got := on.Header("Subject"); got != " Dinner on Thursday?" {
		t.Errorf("Subject = %q", got)
	}
}

// TestEmailView_HasKeyword: exact match and case-insensitive lookup (a
// stored keyword is already lowercase - Email/set normalizes it - but the
// method itself lowercases the query, so a caller can pass any case).
func TestEmailView_HasKeyword(t *testing.T) {
	_, db, store := emailServer(t)
	id := putEmail(t, db, store, viewMessage,
		map[string]bool{"MBinbox": true}, map[string]bool{"$seen": true})
	v, err := ReadEmail(context.Background(), db, store, testAccount, jmap.Id(id), ReadEmailOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !v.HasKeyword("$seen") {
		t.Error("HasKeyword($seen) = false")
	}
	if !v.HasKeyword("$SEEN") {
		t.Error("HasKeyword($SEEN) = false, want case-insensitive match")
	}
	if v.HasKeyword("$flagged") {
		t.Error("HasKeyword($flagged) = true, not stored")
	}
}

// TestEmailView_HeaderAndHeaderAll: Header returns the first instance
// (RFC 5322 field-name matching is case-insensitive), HeaderAll every
// instance in message order; a missing field yields "" / nil.
func TestEmailView_HeaderAndHeaderAll(t *testing.T) {
	_, db, store := emailServer(t)
	id := putEmail(t, db, store, viewMessage, map[string]bool{"MBinbox": true}, nil)
	v, err := ReadEmail(context.Background(), db, store, testAccount, jmap.Id(id), ReadEmailOptions{Headers: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Header("x-custom"); got != " first" {
		t.Errorf("Header(x-custom) = %q, want first instance %q", got, " first")
	}
	all := v.HeaderAll("X-CUSTOM")
	if len(all) != 2 || all[0] != " first" || all[1] != " second" {
		t.Errorf("HeaderAll(X-CUSTOM) = %v", all)
	}
	if got := v.Header("X-Missing"); got != "" {
		t.Errorf("Header(X-Missing) = %q, want empty", got)
	}
	if all := v.HeaderAll("X-Missing"); all != nil {
		t.Errorf("HeaderAll(X-Missing) = %v, want nil", all)
	}
}

// TestEmailView_HeaderAddresses: a two-address To instance parses to two
// EmailAddress entries (RFC 8621 section 4.1.2.3), a missing header to
// none.
func TestEmailView_HeaderAddresses(t *testing.T) {
	_, db, store := emailServer(t)
	id := putEmail(t, db, store, viewMessage, map[string]bool{"MBinbox": true}, nil)
	v, err := ReadEmail(context.Background(), db, store, testAccount, jmap.Id(id), ReadEmailOptions{Headers: true})
	if err != nil {
		t.Fatal(err)
	}
	to := v.HeaderAddresses("To")
	if len(to) != 2 {
		t.Fatalf("HeaderAddresses(To) = %v", to)
	}
	if to[0].Email != "jane@example.com" || to[0].Name == nil || *to[0].Name != "Jane Doe" {
		t.Errorf("to[0] = %+v", to[0])
	}
	if to[1].Email != "sam@example.com" || to[1].Name == nil || *to[1].Name != "Sam Roe" {
		t.Errorf("to[1] = %+v", to[1])
	}
	if bcc := v.HeaderAddresses("Bcc"); bcc != nil {
		t.Errorf("HeaderAddresses(Bcc) = %v, want none", bcc)
	}
}

// TestEmailView_HeaderMessageIDs: a References instance with two msg-ids
// parses to both (RFC 8621 section 4.1.2.2), Message-ID to its one.
func TestEmailView_HeaderMessageIDs(t *testing.T) {
	_, db, store := emailServer(t)
	id := putEmail(t, db, store, viewMessage, map[string]bool{"MBinbox": true}, nil)
	v, err := ReadEmail(context.Background(), db, store, testAccount, jmap.Id(id), ReadEmailOptions{Headers: true})
	if err != nil {
		t.Fatal(err)
	}
	if refs := v.HeaderMessageIDs("References"); len(refs) != 2 || refs[0] != "a@example.com" || refs[1] != "b@example.com" {
		t.Errorf("HeaderMessageIDs(References) = %v", refs)
	}
	if mid := v.HeaderMessageIDs("Message-ID"); len(mid) != 1 || mid[0] != "msg1@example.com" {
		t.Errorf("HeaderMessageIDs(Message-ID) = %v", mid)
	}
}

// TestReadEmail_NotFound: an id with no stored record reports
// objectdb.ErrNotFound - the house not-found convention (see search.go,
// emailcopy.go), unlike ReadVacationResponse's implicit-disabled default.
func TestReadEmail_NotFound(t *testing.T) {
	_, db, store := emailServer(t)
	_, err := ReadEmail(context.Background(), db, store, testAccount, "Mnosuch", ReadEmailOptions{})
	if !errors.Is(err, objectdb.ErrNotFound) {
		t.Errorf("err = %v, want objectdb.ErrNotFound", err)
	}
}

// TestOpenEmailMessage_RoundTrip: the stream opened for a stored Email
// yields exactly the delivered bytes, and the reported size matches.
func TestOpenEmailMessage_RoundTrip(t *testing.T) {
	_, db, store := emailServer(t)
	id := putEmail(t, db, store, viewMessage, map[string]bool{"MBinbox": true}, nil)

	rc, size, err := OpenEmailMessage(context.Background(), db, store, testAccount, jmap.Id(id))
	if err != nil {
		t.Fatalf("OpenEmailMessage: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading blob: %v", err)
	}
	if string(got) != viewMessage {
		t.Errorf("blob content = %q, want %q", got, viewMessage)
	}
	if size != int64(len(viewMessage)) {
		t.Errorf("size = %d, want %d", size, len(viewMessage))
	}
}

// TestOpenEmailMessage_NotFound: a missing Email record reports
// objectdb.ErrNotFound, the same as ReadEmail.
func TestOpenEmailMessage_NotFound(t *testing.T) {
	_, db, store := emailServer(t)
	_, _, err := OpenEmailMessage(context.Background(), db, store, testAccount, "Mnosuch")
	if !errors.Is(err, objectdb.ErrNotFound) {
		t.Errorf("err = %v, want objectdb.ErrNotFound", err)
	}
}
