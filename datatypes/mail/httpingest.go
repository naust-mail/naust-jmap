package mail

// HTTP ingest adapter: a minimal, non-JMAP endpoint that hands a posted
// message to the delivery socket. It exists for MTA-less and cloud
// deployments and for testing, where an LMTP listener is inconvenient. The
// envelope travels in headers and the raw RFC 5322 message is the request
// body; the response is a JSON array with one result per recipient (the same
// per-recipient verdicts LMTP returns on the wire).

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// Ingest header names. Recipients may be given as several headers or as one
// comma-separated header; both are accepted.
const (
	headerMailFrom = "X-Naust-Mail-From"
	headerRcptTo   = "X-Naust-Rcpt-To"
)

// httpResult is the JSON shape of one recipient's delivery outcome. It is a
// wire projection of DeliveryEvent with a readable string Outcome.
type httpResult struct {
	Recipient string  `json:"recipient"`
	Outcome   string  `json:"outcome"` // accepted | rejected | tempfailed
	Reason    string  `json:"reason,omitempty"`
	EmailId   jmap.Id `json:"emailId,omitempty"`
	BlobId    jmap.Id `json:"blobId,omitempty"`
	MessageId string  `json:"messageId,omitempty"`
	Size      int64   `json:"size,omitempty"`
}

// defaultMaxIngestInFlight bounds how many ingest requests are served at once.
// As with LMTP, what is bounded is the requests in flight and not the parses
// they make: a delivery streams, so a request costs a buffer, and the honest
// ceiling is on the thing the sender actually holds.
const defaultMaxIngestInFlight = 64

// HTTPIngest is an http.Handler that ingests one message per POST.
type HTTPIngest struct {
	d *Deliverer
	// slots bounds the requests being served at once; a request beyond the
	// ceiling is told to come back rather than served without room for it.
	slots chan struct{}
}

// HTTPIngestOption configures an HTTP ingest handler.
type HTTPIngestOption func(*HTTPIngest)

// WithMaxIngestInFlight bounds how many ingest requests are served at once.
// Beyond it a request is answered 503 with a Retry-After.
func WithMaxIngestInFlight(n int) HTTPIngestOption {
	return func(h *HTTPIngest) { h.slots = make(chan struct{}, n) }
}

// NewHTTPIngest wraps a Deliverer as an HTTP ingest handler.
//
// The handler carries no authentication of its own: like an LMTP listener,
// it is the trust seam an MTA (or equivalent trusted infrastructure) is
// pointed at, not a client API. Mount it only on a trusted surface - a
// loopback or private address, or behind the host's own authentication -
// never on a mux the public can reach, where it would accept mail for any
// recipient from anyone.
func NewHTTPIngest(d *Deliverer, opts ...HTTPIngestOption) *HTTPIngest {
	h := &HTTPIngest{d: d, slots: make(chan struct{}, defaultMaxIngestInFlight)}
	for _, o := range opts {
		o(h)
	}
	return h
}

func (h *HTTPIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "only POST is supported", http.StatusMethodNotAllowed)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many deliveries in flight, try again later", http.StatusServiceUnavailable)
		return
	}
	rcpts := parseRecipients(r.Header.Values(headerRcptTo))
	if len(rcpts) == 0 {
		http.Error(w, "at least one "+headerRcptTo+" recipient is required", http.StatusBadRequest)
		return
	}
	// The same recipient ceiling the LMTP adapter enforces (maxRecipients): one
	// message is one message, but one recipient is one delivery, so the count is
	// the amplification factor and it is bounded on every ingest path.
	if len(rcpts) > maxRecipients {
		http.Error(w, "too many recipients", http.StatusBadRequest)
		return
	}
	// Reject an over-large message up front when the client declares its
	// length; a body that streams past the cap without a declared length is
	// still caught by the Deliverer and reported as a per-recipient reject.
	if r.ContentLength > h.d.MaxMessageSize() {
		http.Error(w, "message too large", http.StatusRequestEntityTooLarge)
		return
	}

	env := Envelope{MailFrom: r.Header.Get(headerMailFrom), Recipients: rcpts}
	events := h.d.Deliver(r.Context(), env, r.Body)

	results := make([]httpResult, len(events))
	for i, ev := range events {
		results[i] = httpResult{
			Recipient: ev.Recipient,
			Outcome:   ev.Outcome.String(),
			Reason:    ev.Reason,
			EmailId:   ev.EmailId,
			BlobId:    ev.BlobId,
			MessageId: ev.MessageId,
			Size:      ev.Size,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

// parseRecipients flattens one or more X-Naust-Rcpt-To header values, each of
// which may be a comma-separated list, into a clean recipient slice.
//
// It stops one past the recipient cap, which is enough for the caller to see
// that the cap was passed and refuse the request: one message addressed to a
// hundred thousand recipients is a hundred thousand deliveries, and an ingest
// must not do that work - nor even build the list for it - on the say-so of one
// header.
func parseRecipients(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
				if len(out) > maxRecipients {
					return out
				}
			}
		}
	}
	return out
}
