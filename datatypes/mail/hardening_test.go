package mail

import (
	"fmt"
	"strings"
	"testing"
)

// TestThreadKeysNotExposed: the internal threadKeys index that threading
// uses is never visible on the Email/get wire, and cannot be requested.
func TestThreadKeysNotExposed(t *testing.T) {
	ts, db, store := emailServer(t)
	id := putEmail(t, db, store, simpleMessage, map[string]bool{"MBinbox": true}, nil)

	// It never appears in a normal (default-property) response.
	if _, has := emailGet(t, ts, id, "")["threadKeys"]; has {
		t.Fatal("threadKeys leaked into Email/get response")
	}

	// Explicitly requesting it is invalidArguments: to a client it does
	// not exist.
	r := callMail(t, ts, inv("Email/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q],"properties":["threadKeys"]}`, testAccount, id), "0"))
	if r.MethodResponses[0].Name != "error" {
		t.Fatalf("requesting threadKeys should error, got %s", r.MethodResponses[0].Name)
	}
	if got := methodArgs(t, r, 0, "error")["type"]; got != "invalidArguments" {
		t.Fatalf("want invalidArguments, got %v", got)
	}
}

// TestEmailParsePropertiesCap: Email/parse with a properties list over the
// cap is invalidArguments. 512 duplicates internal/emailmethods's
// maxParseProperties (surface test, cannot import the internal package's
// unexported constant).
func TestEmailParsePropertiesCap(t *testing.T) {
	const maxParseProperties = 512
	ts, _, _ := emailServer(t)
	var b strings.Builder
	fmt.Fprintf(&b, `{"accountId":%q,"blobIds":[],"properties":[`, testAccount)
	for i := 0; i <= maxParseProperties; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"header:X-%d"`, i)
	}
	b.WriteString(`]}`)
	r := callMail(t, ts, inv("Email/parse", b.String(), "0"))
	if r.MethodResponses[0].Name != "error" {
		t.Fatalf("oversized properties should error, got %s", r.MethodResponses[0].Name)
	}
	if got := methodArgs(t, r, 0, "error")["type"]; got != "invalidArguments" {
		t.Fatalf("want invalidArguments, got %v", got)
	}
}
