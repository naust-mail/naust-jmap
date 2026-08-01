package mail

// VacationView is the RFC 8621 section 8 VacationResponse configuration in
// the form the delivery-side responder consumes it. It is a read-only
// projection: vacation.go owns the stored record and the get/set wire
// surface, this is the seam a caller outside the mail package (the
// deliver package's responder) reads it through.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// VacationView holds the section 8 fields the responder reads: whether
// the auto-reply is on, its active window, and the reply content.
type VacationView struct {
	IsEnabled bool
	// FromDate/ToDate are nil for an open (unset) bound. A stored date is
	// always a valid RFC 3339 string - applyVacationUpdate rejects anything
	// else at write time - so a parse failure cannot occur for a record
	// this package wrote; it is treated the same as unset (open bound).
	FromDate *time.Time
	ToDate   *time.Time
	Subject  string
	TextBody string
	HtmlBody string
}

// ReadVacationResponse loads the account's VacationResponse configuration
// (RFC 8621 section 8). A missing record is not an error: it reads as the
// disabled view (loadVacation's defaults - isEnabled false, no reply
// configured), the spec's implicit "not configured" state.
func ReadVacationResponse(ctx context.Context, db *objectdb.DB, acct jmap.Id) (*VacationView, error) {
	full, _, err := loadVacation(ctx, db, acct)
	if err != nil {
		return nil, err
	}
	v := &VacationView{}
	json.Unmarshal(full["isEnabled"], &v.IsEnabled)
	if s, ok := decodeString(full["fromDate"]); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			v.FromDate = &t
		}
	}
	if s, ok := decodeString(full["toDate"]); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			v.ToDate = &t
		}
	}
	v.Subject, _ = decodeString(full["subject"])
	v.TextBody, _ = decodeString(full["textBody"])
	v.HtmlBody, _ = decodeString(full["htmlBody"])
	return v, nil
}

// ActiveAt reports whether the vacation response is active at t: enabled,
// and t within [FromDate, ToDate) - a nil bound is open (RFC 8621 section
// 8).
func (v *VacationView) ActiveAt(t time.Time) bool {
	if !v.IsEnabled {
		return false
	}
	if v.FromDate != nil && t.Before(*v.FromDate) {
		return false
	}
	if v.ToDate != nil && !t.Before(*v.ToDate) {
		return false
	}
	return true
}
