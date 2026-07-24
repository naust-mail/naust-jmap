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

// TestValidateOrderBy exercises every registration-time rule for
// OrderBy: it may appear only on an Indexed property and may name only
// declared, Immutable, scalar siblings - the rules that make a stale or
// unsortable index key unrepresentable.
func TestValidateOrderBy(t *testing.T) {
	// typeWith builds a two-property type: "p" carries the OrderBy under
	// test, "d" is the sibling it (usually) names.
	typeWith := func(p, d Property) *Type {
		return &Type{
			Name:       "T",
			Capability: "urn:example:t",
			Properties: map[string]Property{"p": p, "d": d},
		}
	}
	date := Property{Kind: KindDate, Immutable: true}
	cases := []struct {
		name string
		t    *Type
		ok   bool
	}{
		{"indexed ordered by immutable scalar", typeWith(Property{Kind: KindId, Indexed: true, OrderBy: []string{"d"}}, date), true},
		{"not indexed", typeWith(Property{Kind: KindId, OrderBy: []string{"d"}}, date), false},
		{"orders by id", typeWith(Property{Kind: KindId, Indexed: true, OrderBy: []string{"id"}}, date), false},
		{"orders by itself", typeWith(Property{Kind: KindId, Indexed: true, OrderBy: []string{"p"}}, date), false},
		{"orders by undeclared", typeWith(Property{Kind: KindId, Indexed: true, OrderBy: []string{"x"}}, date), false},
		{"orders by object", typeWith(Property{Kind: KindId, Indexed: true, OrderBy: []string{"d"}}, Property{Kind: KindObject, Immutable: true}), false},
		{"orders by array", typeWith(Property{Kind: KindId, Indexed: true, OrderBy: []string{"d"}}, Property{Kind: KindArray, Immutable: true}), false},
		{"orders by mutable", typeWith(Property{Kind: KindId, Indexed: true, OrderBy: []string{"d"}}, Property{Kind: KindDate}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.t.Validate()
			if (err == nil) != c.ok {
				t.Fatalf("Validate() err=%v, want ok=%v", err, c.ok)
			}
		})
	}
}
