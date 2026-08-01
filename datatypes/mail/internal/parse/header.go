package parse

import (
	"sort"
	"strings"

	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// HeaderInstances collects the raw values of every instance of one header
// field, in message order.
func HeaderInstances(headers []message.HeaderField, name string) []string {
	var out []string
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			out = append(out, h.Value)
		}
	}
	return out
}

// Check5322 is the submission-time validity check of the message (RFC
// 5322 section 3.6: exactly one From with at least one address, a Sender
// when From has several, exactly one parseable Date), reported through
// the invalidEmail SetError's properties list in Email property names.
func Check5322(headers []message.HeaderField) (fromAddrs, senderAddrs []message.Address, bad []string) {
	froms := HeaderInstances(headers, "From")
	senders := HeaderInstances(headers, "Sender")
	dates := HeaderInstances(headers, "Date")
	if len(froms) == 1 {
		fromAddrs = message.AddressesForm(froms[0])
	}
	if len(fromAddrs) == 0 {
		bad = append(bad, "from")
	}
	if len(senders) > 1 {
		bad = append(bad, "sender")
	} else if len(senders) == 1 {
		senderAddrs = message.AddressesForm(senders[0])
		if len(senderAddrs) != 1 {
			bad = append(bad, "sender")
		}
	}
	// A multi-address From requires a Sender (RFC 5322 section 3.6.2).
	if len(fromAddrs) > 1 && len(senderAddrs) == 0 && !containsStr(bad, "sender") {
		bad = append(bad, "sender")
	}
	if len(dates) != 1 || message.DateForm(dates[0]) == nil {
		bad = append(bad, "sentAt")
	}
	sort.Strings(bad)
	return fromAddrs, senderAddrs, bad
}

// containsStr reports whether s occurs in list.
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// MessageID is the first Message-ID of the message, or "" - purely for
// correlating a DeliveryEvent with external logs.
func MessageID(headers []message.HeaderField) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, "Message-ID") {
			if ids := message.MessageIDsForm(h.Value); len(ids) > 0 {
				return ids[0]
			}
		}
	}
	return ""
}
