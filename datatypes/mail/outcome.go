package mail

// Outcome is the disposition of a delivery to one recipient. It maps
// directly to an SMTP/LMTP reply class: Accepted -> 2xx, Rejected -> 5xx
// (permanent, the sender should bounce), TempFailed -> 4xx (transient, the
// sender should retry). TempFailed is the zero value on purpose: a recipient
// whose verdict has not yet been reached reads as a transient failure - the
// safe default, since an interrupted delivery is then retried rather than
// falsely reported as delivered.
type Outcome int

const (
	TempFailed Outcome = iota
	Rejected
	Accepted
)

// String is the lowercase name of an Outcome, for logs and the HTTP ingest
// response.
func (o Outcome) String() string {
	switch o {
	case Accepted:
		return "accepted"
	case Rejected:
		return "rejected"
	case TempFailed:
		return "tempfailed"
	default:
		return "unknown"
	}
}
