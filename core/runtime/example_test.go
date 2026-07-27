package runtime_test

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

type exampleAuth struct{}

func (exampleAuth) Authenticate(*http.Request) (*auth.Identity, error) {
	return nil, auth.ErrUnauthenticated
}

// A complete server: storage, a datatype described as data, and the HTTP
// face. RegisterStandardType derives Todo/get, Todo/changes, Todo/set,
// Todo/copy, Todo/query and Todo/queryChanges (RFC 8620 sections 5.1-5.6)
// from the descriptor alone.
func ExampleRegisterStandardType() {
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))

	todo := &descriptor.Type{
		Name:       "Todo",
		Capability: "urn:example:todo",
		Properties: map[string]descriptor.Property{
			"title": {Kind: descriptor.KindString, Indexed: true},
			"done":  {Kind: descriptor.KindBool, Indexed: true, Default: json.RawMessage(`false`)},
		},
	}

	proc := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	if err := runtime.RegisterStandardType(proc, db, todo, core); err != nil {
		panic(err)
	}

	srv, err := runtime.NewServer(exampleAuth{}, proc, "http://localhost:8080", core)
	if err != nil {
		panic(err)
	}
	defer srv.Close()

	// Advertise the capability so clients may opt into it with "using".
	if err := srv.Capability("urn:example:todo").
		Advertise(struct{}{}, struct{}{}).Err(); err != nil {
		panic(err)
	}

	// Both optional. Blobs add the upload/download endpoints and
	// Blob/copy; push adds /eventsource. A production server passes a
	// real subscription store and sender rather than nil (RFC 8620
	// section 7.2 has no opt-out).
	srv.EnableBlobs(db, kvstore.New(be))
	if err := srv.EnablePush(db, notify.NewInProcess(), nil, nil); err != nil {
		panic(err)
	}

	// srv is an http.Handler: http.ListenAndServe("localhost:8080", srv)
	_ = srv
}

// Adding a method the derived six do not cover. The runtime dispatches it
// exactly like a derived one: same capability gating, same back-reference
// resolution, same concurrency slot.
func ExampleProcessor_Register() {
	proc := runtime.NewProcessor()

	proc.Register("Todo/archive", "urn:example:todo",
		func(ctx context.Context, call *runtime.Call) []jmap.Invocation {
			var args struct {
				AccountId jmap.Id `json:"accountId"`
			}
			if err := runtime.DecodeArgs(call.Args, &args); err != nil {
				return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
			}
			// Always check account access before acting on it.
			if errType, desc := runtime.CheckAccount(call, args.AccountId, true); errType != "" {
				return runtime.Fail(call.CallID, errType, desc)
			}
			return runtime.Reply(call.Name, call.CallID, map[string]any{"archived": 0})
		})
}
