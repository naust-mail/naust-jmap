package submit

// Report ingestion: correlating an inbound DSN or MDN with the
// EmailSubmission it reports on (RFC 8621 section 7: deliveryStatus is
// updated and dsnBlobIds/mdnBlobIds grown "if the server is receiving"
// these reports) and deciding, per recipient, whether the report advances
// anything - which is what bounds acceptance.
//
// The bound is a state machine, not a counter. Per envelope recipient of a
// submission, delivery reporting moves strictly forward: open -> at most
// one interim (Action: delayed) report -> one terminal (delivered/failed)
// report, after which the recipient is sealed; and at most one disposition
// notification is ever accepted (RFC 8098 section 2.1: an MDN "MUST NOT be
// sent more than once", so a second one is by definition not a real MDN;
// RFC 3464 section 3 expects one terminal DSN per recipient; RFC 8621
// fixes deliveryStatus's delivered as final once yes/no). A matched report
// that advances no recipient's machine is swallowed - logged, never
// delivered - so replaying or endlessly varying a report (RFC 3464 section
// 4.1's forgery concern) buys an attacker nothing after the first one.
//
// Forged verdicts are cut off a second way: delivered may flip to its
// final yes/no only on the strong correlation key - the ENVID this
// server itself stamped (the submission id, RFC 3461 section 5.4) - never
// on the optional Message-ID fallback, which can pin a report and consume
// a slot but cannot finalize what the envelope did not prove.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
	"github.com/naust-mail/naust-jmap/datatypes/mail/report"
)

// typeSubmissionReport is the internal record type behind an
// EmailSubmission's dsnBlobIds and mdnBlobIds: one record per state-machine
// slot a received report consumed, holding its own reference to the
// report's blob. The submission record itself never grows; the wire lists
// are computed from these records on /get, and the submission destroy hook
// removes them (releasing the blobs). The type has no JMAP methods: it is
// registered with the object database only.
const typeSubmissionReport = record.TypeSubmissionReport

// State-machine slots (see the package comment above).
const (
	slotInterim  = "interim"
	slotTerminal = "terminal"
	slotReceipt  = "receipt"
)

// submissionReportType is the internal descriptor for typeSubmissionReport.
func submissionReportType() *descriptor.Type {
	return &descriptor.Type{
		Name:       typeSubmissionReport,
		Capability: CapabilityURI,
		Properties: map[string]descriptor.Property{
			"submissionId": {Kind: descriptor.KindId, Internal: true, Indexed: true},
			"kind":         {Kind: descriptor.KindString, Internal: true}, // report.KindDSN or report.KindMDN
			"recipient":    {Kind: descriptor.KindString, Internal: true}, // the envelope recipient whose slot this consumed
			"slot":         {Kind: descriptor.KindString, Internal: true},
			"blobId":       {Kind: descriptor.KindId, BlobRef: true, Internal: true},
			"receivedAt":   {Kind: descriptor.KindDate, Internal: true},
		},
	}
}

// reportRow is one loaded SubmissionReport record.
type reportRow struct {
	id         jmap.Id
	kind       string
	recipient  string
	slot       string
	blobId     jmap.Id
	receivedAt string
}

// loadReportRows returns the report records of one submission via the
// submissionId index, in received order (receivedAt, then id for a stable
// tiebreak). The set is small by construction: the state machine admits at
// most two DSN slots and one MDN slot per envelope recipient.
func loadReportRows(get func(string, jmap.Id) (objectdb.Object, error), index func(string, string, json.RawMessage) ([]jmap.Id, error), subId jmap.Id) ([]reportRow, error) {
	ids, err := index(typeSubmissionReport, "submissionId", record.MustJSON(subId))
	if err != nil {
		return nil, err
	}
	rows := make([]reportRow, 0, len(ids))
	for _, id := range ids {
		obj, err := get(typeSubmissionReport, id)
		if errors.Is(err, objectdb.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		r := reportRow{id: id}
		json.Unmarshal(obj["kind"], &r.kind)
		json.Unmarshal(obj["recipient"], &r.recipient)
		json.Unmarshal(obj["slot"], &r.slot)
		json.Unmarshal(obj["blobId"], &r.blobId)
		json.Unmarshal(obj["receivedAt"], &r.receivedAt)
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].receivedAt != rows[j].receivedAt {
			return rows[i].receivedAt < rows[j].receivedAt
		}
		return rows[i].id < rows[j].id
	})
	return rows, nil
}

// IngestOptions carries the report-ingestion knobs a Deliverer holds, so the
// ingestion state machine below takes them as parameters instead of reading
// a *Deliverer directly (its nouns are submission-side, not delivery-side).
type IngestOptions struct {
	// MessageIDFallback additionally correlates DSNs by the returned
	// content's Message-ID (see WithMessageIDCorrelation).
	MessageIDFallback bool
}

// IngestReport runs inside the recipient account's update, after a message
// was recognized as a report. matched reports whether it correlated with a
// submission in this account; deliver whether the message should still be
// created as an inbox Email (true when the report advanced at least one
// recipient's machine, RFC 8621 section 7 wants the user to see it; false
// means it was swallowed). An uncorrelated report is ordinary mail.
func IngestReport(u *objectdb.Update, rep *report.Inbound, blobID jmap.Id, now time.Time, opts IngestOptions) (matched, deliver bool, err error) {
	subId, sub, exact, err := correlateSubmission(u, rep, opts)
	if err != nil || subId == "" {
		return false, false, err
	}
	rows, err := loadReportRows(u.Get, u.IdsWhereEqual, subId)
	if err != nil {
		return true, false, err
	}
	var ds map[string]deliveryStatusObj
	if err := json.Unmarshal(sub["deliveryStatus"], &ds); err != nil {
		return true, false, nil // unreadable record; swallow rather than guess
	}

	taken := func(rcpt, slot string) bool {
		for _, r := range rows {
			if r.slot == slot && strings.EqualFold(r.recipient, rcpt) {
				return true
			}
		}
		return false
	}
	type consumedSlot struct{ kind, rcpt, slot string }
	var consumed []consumedSlot
	dsChanged := false

	switch rep.Kind {
	case report.KindDSN:
		for _, g := range rep.Rcpts {
			rcpt := matchEnvelopeRecipient(ds, g.Addr)
			if rcpt == "" {
				continue // a recipient this submission never addressed
			}
			st := ds[rcpt]
			sealed := st.Delivered == "yes" || st.Delivered == "no" || taken(rcpt, slotTerminal)
			switch g.Action {
			case "delayed":
				// The one interim slot (RFC 3464 section 2.3.3: delayed "may
				// be issued"; section 3 expects further DSNs to follow, and
				// the terminal one is what they lead to).
				if sealed || taken(rcpt, slotInterim) {
					continue
				}
				consumed = append(consumed, consumedSlot{rep.Kind, rcpt, slotInterim})
				if exact && g.SMTPDiag != "" {
					// Not yet final, so smtpReply may still improve (RFC 8621
					// section 7: final only after delivered is yes/no).
					st.SmtpReply = g.SMTPDiag
					ds[rcpt] = st
					dsChanged = true
				}
			case "delivered", "failed":
				if sealed {
					continue
				}
				consumed = append(consumed, consumedSlot{rep.Kind, rcpt, slotTerminal})
				if exact {
					if g.Action == "delivered" {
						st.Delivered = "yes"
					} else {
						st.Delivered = "no"
					}
					if g.SMTPDiag != "" {
						st.SmtpReply = g.SMTPDiag
					}
					ds[rcpt] = st
					dsChanged = true
				}
			default:
				// relayed/expanded (RFC 3464 section 2.3.3) report a handoff,
				// not an outcome; they advance nothing here.
			}
		}
	case report.KindMDN:
		rcpt := matchEnvelopeRecipient(ds, rep.FinalRecipient)
		if rcpt == "" && len(ds) == 1 {
			// A single-recipient submission leaves no ambiguity when the
			// notification omits or rewrites Final-Recipient (RFC 8098
			// section 3.2.4 allows either address form).
			for k := range ds {
				rcpt = k
			}
		}
		if rcpt == "" || taken(rcpt, slotReceipt) {
			break // no attributable recipient, or their one MDN already came
		}
		consumed = append(consumed, consumedSlot{rep.Kind, rcpt, slotReceipt})
		if rep.Disposition == "displayed" {
			st := ds[rcpt]
			if st.Displayed != "yes" {
				st.Displayed = "yes" // RFC 8621 section 7: displayed turns yes on a displayed MDN
				ds[rcpt] = st
				dsChanged = true
			}
		}
	}

	if len(consumed) == 0 {
		slog.Info("naust-jmap delivery: report advanced nothing, swallowed", "kind", rep.Kind, "submissionId", subId)
		return true, false, nil
	}
	slog.Debug("naust-jmap delivery: report matched", "kind", rep.Kind, "submissionId", subId, "consumed", len(consumed))
	at := record.MustJSON(now.UTC().Format(time.RFC3339))
	for _, c := range consumed {
		if _, err := u.Create(typeSubmissionReport, objectdb.Object{
			"submissionId": record.MustJSON(subId),
			"kind":         record.MustJSON(c.kind),
			"recipient":    record.MustJSON(c.rcpt),
			"slot":         record.MustJSON(c.slot),
			"blobId":       record.MustJSON(blobID),
			"receivedAt":   at,
		}); err != nil {
			return true, false, err
		}
	}
	newSub := cloneObject(sub)
	if dsChanged {
		newSub["deliveryStatus"] = record.MustJSON(ds)
	}
	// Written back even when only the report records changed: the computed
	// dsnBlobIds/mdnBlobIds are part of the submission's wire object, so the
	// change must surface through EmailSubmission/changes and push.
	if err := u.Put(record.TypeEmailSubmission, subId, newSub); err != nil {
		return true, false, err
	}
	return true, true, nil
}

// correlateSubmission finds the submission a report is about, in the
// account the report was delivered to. exact reports the strong key: the
// ENVID this server stamped (equal to the submission id, RFC 3461 section
// 5.4), or an MDN's Original-Message-ID - RFC 8098 defines no envelope
// identifier, so that is the only key an MDN can ever present. The DSN
// Message-ID fallback (returned-content headers) is the weak key: enabled
// only by WithMessageIDCorrelation, and never exact. An ambiguous
// Message-ID (several submissions of the same message) matches nothing.
func correlateSubmission(u *objectdb.Update, rep *report.Inbound, opts IngestOptions) (jmap.Id, objectdb.Object, bool, error) {
	bySubmissionId := func(id jmap.Id) (objectdb.Object, error) {
		sub, err := u.Get(record.TypeEmailSubmission, id)
		if errors.Is(err, objectdb.ErrNotFound) || errors.Is(err, objectdb.ErrUnknownType) {
			return nil, nil // no such submission here (or no submission support at all)
		}
		return sub, err
	}
	byMessageId := func(mid string) (jmap.Id, objectdb.Object, error) {
		ids, err := u.IdsWhereEqual(record.TypeEmailSubmission, "messageId", record.MustJSON(mid))
		if errors.Is(err, objectdb.ErrUnknownType) || len(ids) != 1 {
			return "", nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		sub, err := bySubmissionId(ids[0])
		if sub == nil {
			return "", nil, err
		}
		return ids[0], sub, nil
	}

	switch rep.Kind {
	case report.KindDSN:
		if id := jmap.Id(rep.Envid); rep.Envid != "" && id.Valid() {
			sub, err := bySubmissionId(id)
			if err != nil {
				return "", nil, false, err
			}
			if sub != nil {
				return id, sub, true, nil
			}
		}
		if opts.MessageIDFallback && rep.ReturnedMessageID != "" {
			id, sub, err := byMessageId(rep.ReturnedMessageID)
			return id, sub, false, err
		}
	case report.KindMDN:
		id, sub, err := byMessageId(rep.OrigMessageID)
		return id, sub, true, err
	}
	return "", nil, false, nil
}

// matchEnvelopeRecipient finds the deliveryStatus key a reported address
// refers to. The domain compares case-insensitively (RFC 5321 section
// 2.3.11); the whole-address case-insensitive fallback tolerates reporting
// MTAs that rewrite local-part case, which correlation should survive - a
// wrong pick is impossible since keys differing only by case cannot
// coexist as distinct envelope recipients in practice.
func matchEnvelopeRecipient(ds map[string]deliveryStatusObj, addr string) string {
	if addr == "" {
		return ""
	}
	if _, ok := ds[addr]; ok {
		return addr
	}
	for k := range ds {
		if strings.EqualFold(k, addr) {
			return k
		}
	}
	return ""
}
