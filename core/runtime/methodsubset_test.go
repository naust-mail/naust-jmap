package runtime

// A type may derive only a subset of the six standard methods (Extensions.
// Methods). RFC 8621's Thread supports only get+changes; the methods it does
// not support must not exist (a call to them is unknownMethod).

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

func TestMethodSubset(t *testing.T) {
	a := auth.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := NewProcessor()
	core := DefaultCoreCapabilities()
	ext := &Extensions{Methods: []string{"get", "changes"}}
	if err := RegisterStandardTypeExt(p, db, testNoteType(), core, ext); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability("urn:example:testnote", struct{}{}, struct{}{}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	errType := func(resp *jmap.Response, i int) string {
		if resp.MethodResponses[i].Name != "error" {
			return ""
		}
		var e struct {
			Type string `json:"type"`
		}
		json.Unmarshal(resp.MethodResponses[i].Args, &e)
		return e.Type
	}

	// The derived methods are present.
	resp := callAPI(t, ts, inv("TestNote/get", `{"accountId":"Atest1","ids":[]}`, "0"))
	if got := resp.MethodResponses[0].Name; got != "TestNote/get" {
		t.Errorf("get response = %s (%s)", got, resp.MethodResponses[0].Args)
	}

	// The methods the type does not derive are absent: unknownMethod.
	for _, m := range []string{"TestNote/set", "TestNote/copy", "TestNote/query", "TestNote/queryChanges"} {
		resp := callAPI(t, ts, inv(m, `{"accountId":"Atest1"}`, "0"))
		if got := errType(resp, 0); got != jmap.ErrUnknownMethod {
			t.Errorf("%s -> %q, want unknownMethod", m, got)
		}
	}
}
