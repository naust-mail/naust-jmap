package mail

// IdentityView is the RFC 8621 section 6 Identity in the form a
// server-side consumer outside this package (the submit package's
// EmailSubmission/set create, deliver's vacation responder) reads it
// through. It is a read-only projection: identity.go owns the stored
// record and the get/set wire surface, this is the seam a caller outside
// the mail package reads it through.

import (
	"context"
	"encoding/json"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/addr"
)

// IdentityView holds the section 6 Identity properties: the address it
// may send as, its reply and signature settings, and whether the account
// may delete it.
type IdentityView struct {
	Id            jmap.Id
	Name          string
	Email         string
	ReplyTo       []EmailAddress
	Bcc           []EmailAddress
	TextSignature string
	HtmlSignature string
	MayDelete     bool
}

// ReadIdentity loads the account's Identity record (RFC 8621 section 6)
// as a read-only view. A missing record reports objectdb.ErrNotFound.
func ReadIdentity(ctx context.Context, db *objectdb.DB, acct, id jmap.Id) (*IdentityView, error) {
	obj, err := db.Get(ctx, acct, TypeIdentity, id)
	if err != nil {
		return nil, err
	}
	v := &IdentityView{Id: id}
	v.Name, _ = decodeString(obj["name"])
	v.Email, _ = decodeString(obj["email"])
	json.Unmarshal(obj["replyTo"], &v.ReplyTo)
	json.Unmarshal(obj["bcc"], &v.Bcc)
	v.TextSignature, _ = decodeString(obj["textSignature"])
	v.HtmlSignature, _ = decodeString(obj["htmlSignature"])
	json.Unmarshal(obj["mayDelete"], &v.MayDelete)
	return v, nil
}

// AllowsSend reports whether the Identity allows sending a message with
// fromAddr as a From address (RFC 8621 section 6): an exact match to the
// Identity's email (domain ASCII case-insensitive, local part exact), or,
// for a whole-domain wildcard Identity ("*@example.com"), any address in
// that domain. This is exactly the check submissioncreate.go applies to
// each From address of an outgoing message, so the two can never diverge.
func (v *IdentityView) AllowsSend(fromAddr string) bool {
	return addr.IdentityAllows(v.Email, fromAddr)
}
