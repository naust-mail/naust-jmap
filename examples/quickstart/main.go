// Command quickstart is the smallest complete naust-jmap server: one
// datatype described as data, stored in the in-memory backend, served
// over HTTP. The Todo datatype is RFC 8620's own worked example
// (section 5.7), so the requests below can be read side by side with
// the spec's.
//
// The runtime derives Todo/get, Todo/changes, Todo/set, Todo/copy,
// Todo/query, and Todo/queryChanges (RFC 8620 sections 5.1-5.6) from
// the descriptor alone; no method code is written here. Two more calls
// below turn on binary data (section 6) and push (section 7).
//
// Run it:
//
//	go run ./examples/quickstart
//
// Then talk JMAP to it (user demo@example.com, password demo):
//
//	curl -su demo@example.com:demo http://localhost:8080/.well-known/jmap
//
//	curl -su demo@example.com:demo http://localhost:8080/api \
//	  -H 'Content-Type: application/json' -d '{
//	    "using": ["urn:ietf:params:jmap:core", "urn:example:todo"],
//	    "methodCalls": [
//	      ["Todo/set", {"accountId": "Ademo", "create":
//	        {"t1": {"title": "try JMAP"}}}, "0"],
//	      ["Todo/query", {"accountId": "Ademo", "filter":
//	        {"done": false}, "sort": [{"property": "title"}]}, "1"],
//	      ["Todo/get", {"accountId": "Ademo",
//	        "#ids": {"resultOf": "1", "name": "Todo/query", "path": "/ids"}}, "2"]
//	    ]
//	  }'
//
// Watch live StateChange events while you do (RFC 8620 section 7.3):
//
//	curl -su demo@example.com:demo \
//	  'http://localhost:8080/eventsource?types=*&closeafter=no&ping=30'
package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

func main() {
	// Persistence: the in-memory backend and its in-process lease
	// manager. Swap these two lines for a real driver module (e.g.
	// drivers/sqlite) and the rest is unchanged.
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))

	// Authentication: a static user list. Real embedders implement
	// auth.Authenticator against their own accounts.
	users := auth.NewStatic()
	users.AddUser("demo@example.com", "demo", "Ademo")

	// The datatype, described as data. Property attributes drive the
	// derived method semantics: Indexed feeds /query planning, Default
	// fills creates and null patches, Immutable and ServerSet are
	// enforced on every write (RFC 8620 section 5.3).
	todo := &descriptor.Type{
		Name:       "Todo",
		Capability: "urn:example:todo",
		Properties: map[string]descriptor.Property{
			"title": {Kind: descriptor.KindString, Indexed: true},
			"done":  {Kind: descriptor.KindBool, Indexed: true, Default: json.RawMessage(`false`)},
			"notes": {Kind: descriptor.KindString},
		},
	}

	proc := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	if err := runtime.RegisterStandardType(proc, db, todo, core); err != nil {
		log.Fatal(err)
	}

	srv, err := runtime.NewServer(users, proc, "http://localhost:8080", core)
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.RegisterCapability("urn:example:todo", struct{}{}, struct{}{}); err != nil {
		log.Fatal(err)
	}

	// Binary data (RFC 8620 section 6): upload/download endpoints and
	// Blob/copy, stored in the same backend as the records.
	srv.EnableBlobs(db, kvstore.New(be))

	// Push (RFC 8620 section 7): live StateChange events on the
	// event-source endpoint. A nil subscription store and sender skips
	// PushSubscription webhooks, which only make sense with a real push
	// service to POST to.
	if err := srv.EnablePush(db, notify.NewInProcess(), nil, nil); err != nil {
		log.Fatal(err)
	}

	log.Println("JMAP session at http://localhost:8080/.well-known/jmap (demo@example.com / demo)")
	log.Fatal(http.ListenAndServe("localhost:8080", srv))
}
