package deliver

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/naust-mail/naust-jmap/core/private/rawjson"
)

// decodeString decodes a stored JSON string value; ok is false for a
// missing value, or a non-string (exactly rawjson.String's contract).
// Duplicated from root's decodeString (same trivial wrapper; root cannot
// be imported for an unexported helper).
func decodeString(raw json.RawMessage) (string, bool) {
	return rawjson.String(raw)
}

// parseUTCDateValue decodes a stored UTCDate string value. Duplicated
// from submit's parseUTCDateValue (same trivial wrapper; unexported,
// package-private there).
func parseUTCDateValue(raw json.RawMessage) (time.Time, error) {
	s, ok := decodeString(raw)
	if !ok {
		return time.Time{}, fmt.Errorf("not a UTCDate value: %s", raw)
	}
	return time.Parse(time.RFC3339, s)
}
