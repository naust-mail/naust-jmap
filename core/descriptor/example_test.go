package descriptor_test

import (
	"encoding/json"
	"fmt"

	"github.com/naust-mail/naust-jmap/core/descriptor"
)

// A datatype is described as data. The runtime derives /get, /changes,
// /set, /copy, /query and /queryChanges from this description alone; no
// method code is written per type.
//
// Indexed properties can be filtered and sorted on with an index scan
// rather than a full collection walk. Immutable and ServerSet carry the
// RFC 8620 semantics the runtime enforces before any plugin code runs.
func ExampleType() {
	todo := &descriptor.Type{
		Name:       "Todo",
		Capability: "urn:example:todo",
		Properties: map[string]descriptor.Property{
			"title":     {Kind: descriptor.KindString, Indexed: true},
			"done":      {Kind: descriptor.KindBool, Indexed: true, Default: json.RawMessage(`false`)},
			"notes":     {Kind: descriptor.KindString, Nullable: true},
			"createdAt": {Kind: descriptor.KindDate, ServerSet: true},
		},
	}

	fmt.Println(todo.Name, len(todo.Properties), todo.Properties["title"].Indexed)
	// Output: Todo 4 true
}
