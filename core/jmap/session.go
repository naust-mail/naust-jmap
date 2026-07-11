package jmap

import "encoding/json"

// CoreCapability is the capability URI every server must advertise.
const CoreCapability = "urn:ietf:params:jmap:core"

// CoreCapabilities is the value of the urn:ietf:params:jmap:core key in
// the session capabilities object (RFC 8620 section 2).
type CoreCapabilities struct {
	MaxSizeUpload         int64    `json:"maxSizeUpload"`
	MaxConcurrentUpload   int64    `json:"maxConcurrentUpload"`
	MaxSizeRequest        int64    `json:"maxSizeRequest"`
	MaxConcurrentRequests int64    `json:"maxConcurrentRequests"`
	MaxCallsInRequest     int64    `json:"maxCallsInRequest"`
	MaxObjectsInGet       int64    `json:"maxObjectsInGet"`
	MaxObjectsInSet       int64    `json:"maxObjectsInSet"`
	CollationAlgorithms   []string `json:"collationAlgorithms"`
}

// Account describes one account in the session (RFC 8620 section 2).
type Account struct {
	Name                string                     `json:"name"`
	IsPersonal          bool                       `json:"isPersonal"`
	IsReadOnly          bool                       `json:"isReadOnly"`
	AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
}

// Session is the JMAP Session resource (RFC 8620 section 2).
type Session struct {
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	Accounts        map[Id]Account             `json:"accounts"`
	PrimaryAccounts map[string]Id              `json:"primaryAccounts"`
	Username        string                     `json:"username"`
	APIURL          string                     `json:"apiUrl"`
	DownloadURL     string                     `json:"downloadUrl"`
	UploadURL       string                     `json:"uploadUrl"`
	EventSourceURL  string                     `json:"eventSourceUrl"`
	State           string                     `json:"state"`
}
