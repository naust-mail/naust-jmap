package runtime

// Hostile-input fuzz over the Foo/queryChanges argument surface (RFC
// 8620 section 5.6): whatever the client sends as sinceQueryState,
// upToId, and maxChanges, the server must answer a well-formed method
// response or a well-formed method-level error - never a transport
// failure, never a serverFail, and never an answer that violates the
// response contract (echoed oldQueryState, index-sorted added).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"

	"github.com/naust-mail/naust-jmap/core/internal/authtest"
)

var (
	fuzzOnce sync.Once
	fuzzTS   *httptest.Server
)

// fuzzServer builds one process-lifetime note server with a few
// records, shared across fuzz iterations.
func fuzzServer() *httptest.Server {
	fuzzOnce.Do(func() {
		a := authtest.NewStatic()
		a.AddUser("john@example.com", "secret", "Atest1")
		be := memory.New()
		db := objectdb.New(be, lease.NewInProcess(be))
		p := NewProcessor()
		if err := RegisterStandardType(p, db, testNoteType(), DefaultCoreCapabilities()); err != nil {
			panic(err)
		}
		for i := 0; i < 4; i++ {
			subject, _ := json.Marshal(fmt.Sprintf("n%d", i))
			if _, err := db.Update(context.Background(), "Atest1", func(u *objectdb.Update) error {
				_, err := u.Create("TestNote", objectdb.Object{"subject": subject})
				return err
			}); err != nil {
				panic(err)
			}
		}
		srv, err := NewServer(a, p, "https://jmap.example.com", DefaultCoreCapabilities())
		if err != nil {
			panic(err)
		}
		if err := srv.RegisterCapability("urn:example:testnote", struct{}{}, struct{}{}); err != nil {
			panic(err)
		}
		fuzzTS = httptest.NewServer(srv)
	})
	return fuzzTS
}

func FuzzQueryChangesArgs(f *testing.F) {
	ts := fuzzServer()
	f.Add("3", "N01", int64(10))
	f.Add("0", "", int64(0))
	f.Add("abc", "zzz", int64(-1))
	f.Add("-1", "N01", int64(1))
	f.Add("999999999999999999999999", "\x00", int64(9007199254740991))
	f.Add("1e5", "'; DROP TABLE", int64(2))

	f.Fuzz(func(t *testing.T, state, upToId string, maxChanges int64) {
		args := map[string]any{
			"accountId":       "Atest1",
			"sinceQueryState": state,
			"maxChanges":      maxChanges,
		}
		if upToId != "" {
			args["upToId"] = upToId
		}
		rawArgs, err := json.Marshal(args)
		if err != nil {
			t.Skip() // unencodable fuzz input, not a server concern
		}
		body, err := json.Marshal(map[string]any{
			"using": []string{jmap.CoreCapability, "urn:example:testnote"},
			"methodCalls": []jmap.Invocation{
				{Name: "TestNote/queryChanges", Args: rawArgs, CallID: "0"},
			},
		})
		if err != nil {
			t.Skip()
		}
		resp := post(t, ts, string(body), "application/json")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d for state %q upToId %q maxChanges %d", resp.StatusCode, state, upToId, maxChanges)
		}
		if resp.StatusCode == http.StatusBadRequest {
			return // the request envelope rejected the input (I-JSON), fine
		}
		var out jmap.Response
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("malformed response: %v", err)
		}
		if len(out.MethodResponses) != 1 {
			t.Fatalf("want one method response, got %d", len(out.MethodResponses))
		}
		got := out.MethodResponses[0]
		if got.Name == "error" {
			var e map[string]any
			json.Unmarshal(got.Args, &e)
			if e["type"] == "serverFail" {
				t.Fatalf("serverFail for state %q upToId %q maxChanges %d: %v", state, upToId, maxChanges, e)
			}
			return
		}
		var m map[string]any
		if err := json.Unmarshal(got.Args, &m); err != nil {
			t.Fatalf("malformed method response: %v", err)
		}
		if m["oldQueryState"] != state {
			t.Fatalf("oldQueryState %v does not echo %q", m["oldQueryState"], state)
		}
		last := int64(-1)
		for _, a := range anySlice(m, "added") {
			idx := int64(a.(map[string]any)["index"].(float64))
			if idx < 0 || idx <= last && last >= 0 {
				t.Fatalf("added not index-sorted or negative: %v", m["added"])
			}
			last = idx
		}
	})
}
