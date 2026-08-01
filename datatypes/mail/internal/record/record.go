// Package record holds the mail package's stored-record building blocks:
// the JMAP type-name constants, the small JSON value codecs used when
// building a computed property, and a handful of record-shaped helpers
// (thread subject, address text, role-mailbox lookup) shared by more than
// one root file.
package record

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// TypeEmail is the Email type name (RFC 8621 section 4).
const TypeEmail = "Email"

// TypeMailbox is the Mailbox type name.
const TypeMailbox = "Mailbox"

// TypeThread is the Thread type name.
const TypeThread = "Thread"

// TypeIdentity is the Identity datatype name.
const TypeIdentity = "Identity"

// TypeEmailSubmission is the EmailSubmission datatype name.
const TypeEmailSubmission = "EmailSubmission"

// TypeVacationResponse is the VacationResponse datatype name.
const TypeVacationResponse = "VacationResponse"

// TypeEmailDelivery is the push-only EmailDelivery type name (section 1.5).
const TypeEmailDelivery = "EmailDelivery"

// TypeVacationNotified is the internal suppression ledger: one record per
// sender an auto-reply was queued for, written in the same commit as the
// reply's submission. No JMAP methods.
const TypeVacationNotified = "VacationNotified"

// TypeSubmissionReport is the SubmissionReport type name.
const TypeSubmissionReport = "SubmissionReport"

var JSONNull = json.RawMessage("null")

func MustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		// Values here are plain data structures; a marshal failure is a
		// programming error, not a runtime condition.
		panic(fmt.Sprintf("mail: marshalling computed value: %v", err))
	}
	return raw
}

func NonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func StrOrNull(s *string) json.RawMessage {
	if s == nil {
		return JSONNull
	}
	return MustJSON(*s)
}

// StoredSubject decodes a stored Email's subject property (a String or
// null) to its text value.
func StoredSubject(obj objectdb.Object) string {
	var s string
	if raw := obj["subject"]; raw != nil && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// address is the decode shape of a stored EmailAddress property (RFC 8621
// section 4.1.2.3): the same {name, email} JSON as internal/message.Address,
// duplicated here so this package need not import internal/message.
type address struct {
	Name  *string `json:"name"`
	Email string  `json:"email"`
}

// AddressText concatenates the names and emails of a stored EmailAddress[]
// property (from/to/cc/bcc) for substring search.
func AddressText(obj objectdb.Object, field string) string {
	var addrs []address
	json.Unmarshal(obj[field], &addrs)
	var b strings.Builder
	for _, a := range addrs {
		if a.Name != nil {
			b.WriteString(*a.Name)
			b.WriteByte(' ')
		}
		b.WriteString(a.Email)
		b.WriteByte(' ')
	}
	return b.String()
}

func ContainsFold(hay, needle string) bool {
	return strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}
