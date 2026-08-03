package quotas

// DefaultTypeCapabilities returns the IANA "JMAP Data Types" registry
// (https://www.iana.org/assignments/jmap/) as the type-name-to-
// capability mapping the RFC 9425 section 4.1 types filtering runs
// against: a Quota's types entry is recognized by a client only when
// the client's request opted in to the listed capability. Embedders
// serving vendor types pass their own table (or this one extended)
// through Config.TypeCapabilities.
func DefaultTypeCapabilities() map[string]string {
	return map[string]string{
		"Core":                      "urn:ietf:params:jmap:core",
		"PushSubscription":          "urn:ietf:params:jmap:core",
		"Mailbox":                   "urn:ietf:params:jmap:mail",
		"Thread":                    "urn:ietf:params:jmap:mail",
		"Email":                     "urn:ietf:params:jmap:mail",
		"EmailDelivery":             "urn:ietf:params:jmap:mail",
		"SearchSnippet":             "urn:ietf:params:jmap:mail",
		"Identity":                  "urn:ietf:params:jmap:submission",
		"EmailSubmission":           "urn:ietf:params:jmap:submission",
		"VacationResponse":          "urn:ietf:params:jmap:vacationresponse",
		"MDN":                       "urn:ietf:params:jmap:mdn",
		"Quota":                     "urn:ietf:params:jmap:quota",
		"SieveScript":               "urn:ietf:params:jmap:sieve",
		"Principal":                 "urn:ietf:params:jmap:principals",
		"ShareNotification":         "urn:ietf:params:jmap:principals",
		"AddressBook":               "urn:ietf:params:jmap:contacts",
		"ContactCard":               "urn:ietf:params:jmap:contacts",
		"Calendar":                  "urn:ietf:params:jmap:calendars",
		"CalendarEvent":             "urn:ietf:params:jmap:calendars",
		"CalendarEventNotification": "urn:ietf:params:jmap:calendars",
		"ParticipantIdentity":       "urn:ietf:params:jmap:calendars",
	}
}
