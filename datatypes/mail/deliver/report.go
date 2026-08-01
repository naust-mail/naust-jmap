package deliver

import (
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
	"github.com/naust-mail/naust-jmap/datatypes/mail/report"
)

// extractReport interprets a parsed message as a report. It returns nil
// unless the message is a well-formed multipart/report (RFC 6522 section 3:
// the machine-parsable status is the second body part; its own media type,
// not the report-type parameter, decides how it is read) whose machine part
// was captured whole.
func extractReport(p *parse.Parsed) *report.Inbound {
	root := p.Msg.Root
	if root.Type != "multipart/report" || len(root.SubParts) < 2 {
		return nil
	}
	machine := root.SubParts[1]
	sink := p.Report(machine)
	if sink == nil || sink.Over {
		return nil
	}
	var rep *report.Inbound
	switch machine.Type {
	case "message/delivery-status":
		if ds := report.ParseDeliveryStatus(sink.Raw); ds != nil {
			rep = &report.Inbound{Kind: report.KindDSN, Envid: ds.Envid, Rcpts: ds.Rcpts}
		}
	case "message/disposition-notification":
		if n := report.ParseDispositionNotification(sink.Raw); n != nil {
			rep = &report.Inbound{
				Kind:           report.KindMDN,
				OrigMessageID:  n.OrigMessageID,
				FinalRecipient: n.FinalRecipient,
				Disposition:    n.Disposition,
			}
		}
	}
	if rep == nil {
		return nil
	}
	// The third part, when present, is the returned content (RFC 6522
	// section 3): the original message or its headers, whose Message-ID is
	// the fallback correlation key.
	if len(root.SubParts) >= 3 {
		if s := p.Report(root.SubParts[2]); s != nil && !s.Over {
			rep.ReturnedMessageID = report.MessageIDFromHeaderBlock(s.Raw)
		}
	}
	return rep
}
