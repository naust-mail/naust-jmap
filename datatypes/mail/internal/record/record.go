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
	"github.com/naust-mail/naust-jmap/core/private/rawjson"
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
	s, _ := rawjson.String(obj["subject"])
	return s
}

// StoredPreview decodes a stored Email's preview property (RFC 8621
// section 4.1.4, capped at 256 characters by construction - see
// message.BuildPreview) to its text value.
func StoredPreview(obj objectdb.Object) string {
	s, _ := rawjson.String(obj["preview"])
	return s
}

func ContainsFold(hay, needle string) bool {
	return strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}
