package jmap

// UploadResponse is the JSON object a successful upload returns
// (RFC 8620 section 6.1).
type UploadResponse struct {
	// AccountId is the account used for the call.
	AccountId Id `json:"accountId"`
	// BlobId represents the uploaded binary data; the data is immutable
	// and the id refers only to the bytes, not any metadata.
	BlobId Id `json:"blobId"`
	// Type is the media type as set in the upload's Content-Type header.
	Type string `json:"type"`
	// Size is the file size in octets.
	Size int64 `json:"size"`
}
