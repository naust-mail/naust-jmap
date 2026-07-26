package websocket

import "strings"

// Capability is the value advertised under CapabilityURI in the
// session capabilities object (RFC 8887 section 3).
type Capability struct {
	// URL is the WebSocket URL endpoint; section 3 requires the wss
	// scheme (TLS in front of the server, as everywhere else).
	URL string `json:"url"`
	// SupportsPush reports whether this connection can deliver
	// StateChange push notifications (section 4.3.5).
	SupportsPush bool `json:"supportsPush"`
}

// SessionCapability builds the section 3 capability object from the
// server's base URL and the path the handler is mounted on: https
// becomes wss (and, for TLS-less development setups only, http
// becomes ws - production traffic MUST be wss per section 4.2).
func SessionCapability(baseURL, path string, supportsPush bool) Capability {
	u := baseURL + path
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	return Capability{URL: u, SupportsPush: supportsPush}
}
