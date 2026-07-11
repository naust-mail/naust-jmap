package jmap

// Push wire types (RFC 8620 section 7).

// TypeState is a map from a type name (e.g. "Email") to the value the
// "state" property of a Foo/get call for that type would currently
// return (RFC 8620 section 7.1). Clients compare these against their
// own state strings to decide whether to fetch changes.
type TypeState map[string]string

// StateChange is the object the server pushes to a client when data
// changes (RFC 8620 section 7.1). It is deliberately minimal: just
// enough for the client to know whether it needs to resync via the
// /changes methods.
type StateChange struct {
	// Type MUST be the string "StateChange".
	Type string `json:"@type"`
	// Changed maps an account id to the states of the data types that
	// have changed for that account since the last StateChange was
	// pushed, for each account the user has access to in which
	// something changed.
	Changed map[Id]TypeState `json:"changed"`
}

// PushKeys is the client-generated encryption key material of a
// PushSubscription (RFC 8620 section 7.2). If supplied, the server MUST
// encrypt everything it sends to the subscription URL as specified in
// RFC 8291.
type PushKeys struct {
	// P256dh is the P-256 ECDH public key, URL-safe base64 encoded.
	P256dh string `json:"p256dh"`
	// Auth is the 16-octet authentication secret, URL-safe base64
	// encoded.
	Auth string `json:"auth"`
}

// PushVerification is the object POSTed to a PushSubscription's URL
// immediately after the subscription is created (RFC 8620 section
// 7.2.2). The server makes no further requests to the URL until the
// client updates the subscription with the verification code.
type PushVerification struct {
	// Type MUST be the string "PushVerification".
	Type string `json:"@type"`
	// PushSubscriptionId is the id of the push subscription created.
	PushSubscriptionId Id `json:"pushSubscriptionId"`
	// VerificationCode is the code to add to the push subscription. It
	// MUST contain sufficient entropy to defeat brute-force guessing.
	VerificationCode string `json:"verificationCode"`
}
