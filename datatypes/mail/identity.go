package mail

// Identity (RFC 8621 section 6): an email address or domain the user may
// send from, as a plain settable descriptor type with the derived
// get/changes/set methods (sections 6.1-6.3; there is no query). Creation
// and update are gated by the SendPolicy socket, feeding the section 6.3
// forbiddenFrom SetError. There is deliberately no seeding helper:
// provisioning goes through the JMAP front door (Identity/set) with a
// host-minted service credential that has access to the user's account
// (the auth AddAccess pattern) - one mechanism, working in-process and
// cross-host alike. The spec is silent on provisioning, so the library
// is too.

import (
	"encoding/json"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// TypeIdentity is the Identity datatype name.
const TypeIdentity = "Identity"

// IdentityType returns the section 6 descriptor. email is immutable, and
// the whole-domain wildcard form "*@example.com" is legal (the client may
// then use any address in the domain). mayDelete is server-set and always
// true here: which identities are protected from deletion is host policy
// the spec leaves open ("servers may wish to..."), and destroying an
// identity loses only its settings - the grant lives in the SendPolicy,
// so a recreate restores it.
func IdentityType() *descriptor.Type {
	null := json.RawMessage("null")
	emptyString := json.RawMessage(`""`)
	return &descriptor.Type{
		Name:       TypeIdentity,
		Capability: SubmissionCapabilityURI,
		Properties: map[string]descriptor.Property{
			"name":          {Kind: descriptor.KindString, Default: emptyString},
			"email":         {Kind: descriptor.KindString, Immutable: true},
			"replyTo":       {Kind: descriptor.KindArray, Nullable: true, Default: null},
			"bcc":           {Kind: descriptor.KindArray, Nullable: true, Default: null},
			"textSignature": {Kind: descriptor.KindString, Default: emptyString},
			"htmlSignature": {Kind: descriptor.KindString, Default: emptyString},
			"mayDelete":     {Kind: descriptor.KindBool, ServerSet: true, Default: json.RawMessage("true")},
		},
	}
}

// RegisterIdentity registers the Identity type with its derived methods
// (RFC 8621 sections 6.1-6.3). policy gates which addresses the account
// may hold identities for; nil installs an empty StaticSendPolicy, which
// denies everything - the safe default, so a server that serves
// identities must wire a policy with grants.
func RegisterIdentity(p *runtime.Processor, db *objectdb.DB, policy SendPolicy, core jmap.CoreCapabilities) error {
	if policy == nil {
		policy = NewStaticSendPolicy()
	}
	ext := &runtime.Extensions{
		Methods: []string{"get", "changes", "set"},
		Set:     &runtime.SetHooks{Validate: identityValidate(policy)},
	}
	return runtime.RegisterStandardTypeExt(p, db, IdentityType(), core, ext)
}

// identityValidate enforces the section 6 semantics the descriptor cannot
// express: email is required and address-shaped at create, replyTo and bcc
// are EmailAddress lists when present, and the SendPolicy gates the
// address - a create whose email the account may not send as is rejected
// with forbiddenFrom (section 6.3). Updates re-check the stored email
// (it is immutable), so an identity whose grant was revoked can no longer
// be edited, only destroyed; a pre-existing one is otherwise inert,
// because submission re-checks the policy at send.
func identityValidate(policy SendPolicy) func(*objectdb.Update, objectdb.Object, objectdb.Object, map[string]json.RawMessage) (*jmap.SetError, error) {
	return func(u *objectdb.Update, old, new objectdb.Object, _ map[string]json.RawMessage) (*jmap.SetError, error) {
		if old == nil {
			var email string
			if raw, has := new["email"]; !has || json.Unmarshal(raw, &email) != nil || email == "" {
				return invalidProp("email", "email is required"), nil
			}
			if _, _, ok := splitAddr(email); !ok {
				return invalidProp("email", "not an email address"), nil
			}
		}
		for _, prop := range []string{"replyTo", "bcc"} {
			if serr := checkEmailAddressList(prop, new[prop]); serr != nil {
				return serr, nil
			}
		}
		var email string
		json.Unmarshal(new["email"], &email)
		if !policy.CanSendAs(u.Context(), u.Account(), email) {
			return &jmap.SetError{Type: "forbiddenFrom", Description: "not allowed to send from " + email}, nil
		}
		return nil, nil
	}
}

// checkEmailAddressList validates an EmailAddress[]|null value (section
// 4.1.2.3: each element carries an email and an optional display name).
// nil (property absent or JSON null) is fine.
func checkEmailAddressList(prop string, raw json.RawMessage) *jmap.SetError {
	if raw == nil {
		return nil
	}
	var list []struct {
		Name  *string `json:"name"`
		Email *string `json:"email"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return invalidProp(prop, "not an EmailAddress list")
	}
	for _, a := range list {
		if a.Email == nil || *a.Email == "" {
			return invalidProp(prop, "EmailAddress requires an email")
		}
	}
	return nil
}
