package descriptor

import (
	"encoding/json"
	"testing"
)

func TestCheckValueComposite(t *testing.T) {
	obj := Property{Kind: KindObject}
	arr := Property{Kind: KindArray}
	nullableObj := Property{Kind: KindObject, Nullable: true}

	cases := []struct {
		name string
		p    Property
		raw  string
		ok   bool
	}{
		{"object accepts object", obj, `{"a":true}`, true},
		{"object accepts empty object", obj, `{}`, true},
		{"object rejects array", obj, `[1,2]`, false},
		{"object rejects string", obj, `"x"`, false},
		{"object rejects null when not nullable", obj, `null`, false},
		{"object accepts null when nullable", nullableObj, `null`, true},
		{"array accepts array", arr, `["x","y"]`, true},
		{"array accepts empty array", arr, `[]`, true},
		{"array rejects object", arr, `{"a":true}`, false},
		{"array rejects number", arr, `3`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.p.CheckValue(json.RawMessage(c.raw))
			if (err == nil) != c.ok {
				t.Fatalf("CheckValue(%s) err=%v, want ok=%v", c.raw, err, c.ok)
			}
		})
	}
}

func TestValidateRejectsIndexedComposite(t *testing.T) {
	for _, k := range []Kind{KindObject, KindArray} {
		ty := &Type{
			Name:       "T",
			Capability: "urn:example:t",
			Properties: map[string]Property{"p": {Kind: k, Indexed: true}},
		}
		if err := ty.Validate(); err == nil {
			t.Fatalf("kind %d: Indexed composite must fail Validate", k)
		}
	}
}
