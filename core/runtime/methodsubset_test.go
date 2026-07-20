package runtime

// A type may derive only a subset of the six standard methods (Extensions.
// Methods). RFC 8621's Thread supports only get+changes; the methods it does
// not support must not exist (a call to them is unknownMethod).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/naust-mail/naust-jmap/core/internal/authtest"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

func TestMethodSubset(t *testing.T) {
	a := authtest.NewStatic()
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

// TestCopyWithoutSet: a type may derive /copy without /set (an immutable
// but copyable type is legitimate). Plain copies work; only
// onSuccessDestroyOriginal's implicit Foo/set has nothing to dispatch to,
// so the destroy reports serverFail while the copy stands - the shape
// RFC 8620 section 5.4 itself allows ("it is possible for the copy to
// succeed but the original not to be destroyed for some reason").
func TestCopyWithoutSet(t *testing.T) {
	a := authtest.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	a.AddAccess("john@example.com", "Ateam", auth.Access{Name: "team"})
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := NewProcessor()
	core := DefaultCoreCapabilities()
	ext := &Extensions{Methods: []string{"get", "copy"}}
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

	// The type has no /set to create records through; stage one directly.
	var orig jmap.Id
	if _, err := db.Update(context.Background(), "Atest1", func(u *objectdb.Update) error {
		id, err := u.Create("TestNote", objectdb.Object{"subject": json.RawMessage(`"hello"`)})
		orig = id
		return err
	}); err != nil {
		t.Fatal(err)
	}

	r := callAPI(t, ts, inv("TestNote/copy", fmt.Sprintf(
		`{"fromAccountId":"Atest1","accountId":"Ateam","create":{"k1":{"id":%q}},"onSuccessDestroyOriginal":true}`,
		orig), "0"))
	if len(r.MethodResponses) != 2 {
		t.Fatalf("%d responses, want 2", len(r.MethodResponses))
	}
	args := methodArgs(t, r, 0, "TestNote/copy")
	if _, ok := args["created"].(map[string]any)["k1"]; !ok {
		t.Fatalf("copy did not succeed: %v", args)
	}
	got := r.MethodResponses[1]
	var e struct {
		Type string `json:"type"`
	}
	json.Unmarshal(got.Args, &e)
	if got.Name != "error" || e.Type != jmap.ErrServerFail || got.CallID != "0" {
		t.Fatalf("implicit destroy without /set: %s %s", got.Name, got.Args)
	}
	// The copy stands and the original survives.
	r = callAPI(t, ts, inv("TestNote/get",
		fmt.Sprintf(`{"accountId":"Atest1","ids":[%q]}`, orig), "0"))
	g := methodArgs(t, r, 0, "TestNote/get")
	if list := g["list"].([]any); len(list) != 1 {
		t.Fatalf("original after failed destroy: %v", g)
	}
}
