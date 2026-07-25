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

func TestCheckValueScalar(t *testing.T) {
	str := Property{Kind: KindString}
	nullableStr := Property{Kind: KindString, Nullable: true}
	boolean := Property{Kind: KindBool}
	integer := Property{Kind: KindInt}
	uint := Property{Kind: KindUnsignedInt}
	date := Property{Kind: KindDate}
	id := Property{Kind: KindId}

	cases := []struct {
		name string
		p    Property
		raw  string
		ok   bool
	}{
		{"string accepts string", str, `"x"`, true},
		{"string rejects number", str, `3`, false},
		{"string rejects null when not nullable", str, `null`, false},
		{"string accepts null when nullable", nullableStr, `null`, true},

		{"bool accepts true", boolean, `true`, true},
		{"bool accepts false", boolean, `false`, true},
		{"bool rejects string", boolean, `"true"`, false},
		{"bool rejects null when not nullable", boolean, `null`, false},

		{"int accepts positive", integer, `42`, true},
		{"int accepts negative", integer, `-42`, true},
		{"int rejects string", integer, `"42"`, false},
		{"int rejects above MaxInt", integer, `9007199254740992`, false},
		{"int rejects below MinInt", integer, `-9007199254740992`, false},
		{"int accepts MaxInt", integer, `9007199254740991`, true},
		{"int accepts MinInt", integer, `-9007199254740991`, true},

		{"unsignedInt accepts zero", uint, `0`, true},
		{"unsignedInt accepts positive", uint, `42`, true},
		{"unsignedInt rejects negative", uint, `-1`, false},
		{"unsignedInt rejects above MaxInt", uint, `9007199254740992`, false},

		{"date accepts RFC 3339", date, `"2026-07-26T12:00:00Z"`, true},
		{"date rejects non-date string", date, `"not a date"`, false},
		{"date rejects number", date, `1234`, false},

		{"id accepts valid id", id, `"abc-123_XYZ"`, true},
		{"id rejects empty string", id, `""`, false},
		{"id rejects invalid characters", id, `"has a space"`, false},
		{"id rejects number", id, `5`, false},
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

// TestValidateFieldRules exercises the per-type and per-property structural
// checks in Type.Validate that TestValidateOrderBy and
// TestValidateRejectsIndexedComposite do not already cover.
func TestValidateFieldRules(t *testing.T) {
	cases := []struct {
		name string
		t    *Type
		ok   bool
	}{
		{
			"empty name",
			&Type{Name: "", Capability: "urn:example:t"},
			false,
		},
		{
			"empty capability",
			&Type{Name: "T", Capability: ""},
			false,
		},
		{
			"declares id",
			&Type{Name: "T", Capability: "urn:example:t", Properties: map[string]Property{"id": {Kind: KindId}}},
			false,
		},
		{
			"empty property name",
			&Type{Name: "T", Capability: "urn:example:t", Properties: map[string]Property{"": {Kind: KindString}}},
			false,
		},
		{
			"unknown kind",
			&Type{Name: "T", Capability: "urn:example:t", Properties: map[string]Property{"p": {Kind: Kind(99)}}},
			false,
		},
		{
			"blobRef not KindId",
			&Type{Name: "T", Capability: "urn:example:t", Properties: map[string]Property{"p": {Kind: KindString, BlobRef: true}}},
			false,
		},
		{
			"blobRef on KindId",
			&Type{Name: "T", Capability: "urn:example:t", Properties: map[string]Property{"p": {Kind: KindId, BlobRef: true}}},
			true,
		},
		{
			"setIndexed on scalar",
			&Type{Name: "T", Capability: "urn:example:t", Properties: map[string]Property{"p": {Kind: KindString, SetIndexed: true}}},
			false,
		},
		{
			"setIndexed on object",
			&Type{Name: "T", Capability: "urn:example:t", Properties: map[string]Property{"p": {Kind: KindObject, SetIndexed: true}}},
			true,
		},
		{
			"setIndexed on array",
			&Type{Name: "T", Capability: "urn:example:t", Properties: map[string]Property{"p": {Kind: KindArray, SetIndexed: true}}},
			true,
		},
		{
			"indexed and setIndexed together",
			&Type{Name: "T", Capability: "urn:example:t", Properties: map[string]Property{"p": {Kind: KindString, Indexed: true, SetIndexed: true}}},
			false,
		},
		{
			"default fails CheckValue for kind",
			&Type{Name: "T", Capability: "urn:example:t", Properties: map[string]Property{"p": {Kind: KindString, Default: json.RawMessage(`5`)}}},
			false,
		},
		{
			"default violates unsignedInt range",
			&Type{Name: "T", Capability: "urn:example:t", Properties: map[string]Property{"p": {Kind: KindUnsignedInt, Default: json.RawMessage(`-1`)}}},
			false,
		},
		{
			"default valid for kind",
			&Type{Name: "T", Capability: "urn:example:t", Properties: map[string]Property{"p": {Kind: KindString, Default: json.RawMessage(`"x"`)}}},
			true,
		},
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

// TestValidateAcceptsWellFormedType is the happy path: a type combining
// every property attribute at once - Immutable, ServerSet, Indexed with
// OrderBy, SetIndexed, BlobRef, Nullable, Internal, and Default - must pass
// Validate with no error.
func TestValidateAcceptsWellFormedType(t *testing.T) {
	ty := &Type{
		Name:       "T",
		Capability: "urn:example:t",
		Properties: map[string]Property{
			"threadId": {
				Kind:      KindId,
				Indexed:   true,
				OrderBy:   []string{"receivedAt", "sentAt"},
				Immutable: true,
			},
			"receivedAt": {Kind: KindDate, Immutable: true},
			"sentAt":     {Kind: KindDate, Immutable: true, Nullable: true},
			"mailboxIds": {Kind: KindObject, SetIndexed: true},
			"keywords":   {Kind: KindArray, SetIndexed: true},
			"blobId":     {Kind: KindId, BlobRef: true, Immutable: true},
			"size":       {Kind: KindUnsignedInt, ServerSet: true, Default: json.RawMessage(`0`)},
			"threadKeys": {Kind: KindString, Internal: true},
			"subject":    {Kind: KindString, Default: json.RawMessage(`""`)},
		},
	}
	if err := ty.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
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
