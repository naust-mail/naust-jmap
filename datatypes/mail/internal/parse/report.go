package parse

import "github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"

// maxReportCapture bounds what one report part's sink retains. The
// machine-parsable content is a short group of header-format fields per
// message and per recipient (RFC 3464 section 2, RFC 8098 section 3.1), and
// correlation needs only the leading header block of the returned content -
// so a conformant report uses a fraction of this. A part that overruns the
// bound is left uninterpreted (the sink marks it) and the message falls back
// to ordinary delivery.
const maxReportCapture = 64 << 10

// ReportSink captures one report part's decoded content up to
// maxReportCapture, remembering when there was more.
type ReportSink struct {
	Raw  []byte
	Over bool
}

func (s *ReportSink) Write(b []byte) (int, error) {
	if room := maxReportCapture - len(s.Raw); room > 0 {
		if len(b) > room {
			b = b[:room]
			s.Over = true
		}
		s.Raw = append(s.Raw, b...)
	} else {
		s.Over = true
	}
	return len(b), nil // octets past the bound are discarded, not an error
}

func (s *ReportSink) Close() error { return nil }

// Report returns the capture's report sink for part, or nil if the part was
// not one the capture asked to collect (Capture.Reports was unset, or the
// part's media type is not a report-carrying one).
func (p *Parsed) Report(part *message.Part) *ReportSink {
	return p.cap.reportSinks[part]
}
