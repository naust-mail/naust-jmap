package search

// Match's header-field conditions (RFC 8621 section 4.4.1): the stored
// from/to/cc/bcc/subject text conditions, and the generic two-element
// "header" condition (field presence, or a substring of any instance's
// value) that matchHeader implements.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// headerFixtureMsg carries a from/to/cc with a display name and a
// repeated custom header, for the address and header-instance conditions.
const headerFixtureMsg = "From: John Doe <john@example.com>\r\n" +
	"To: Jane Roe <jane@example.com>\r\n" +
	"Cc: cc@example.com\r\n" +
	"Subject: Hello World\r\n" +
	"X-Custom: FooBar\r\n" +
	"X-Custom: second value\r\n" +
	"Message-ID: <hdr1@example.com>\r\n" +
	"Date: Wed, 03 Mar 2021 10:00:00 +0000\r\n" +
	"\r\nbody\r\n"

// headerFixture stores headerFixtureMsg and returns a searcher plus the
// record with from/to/cc/bcc/subject populated as Email/set would store
// them (the stored properties Match reads for those fields, independent
// of the blob).
func headerFixture(t *testing.T) (*InProcess, objectdb.Object) {
	t.Helper()
	s, obj := searcherFor(t, headerFixtureMsg)
	obj["from"] = json.RawMessage(`[{"name":"John Doe","email":"john@example.com"}]`)
	obj["to"] = json.RawMessage(`[{"name":"Jane Roe","email":"jane@example.com"}]`)
	obj["cc"] = json.RawMessage(`[{"name":null,"email":"cc@example.com"}]`)
	obj["bcc"] = json.RawMessage(`[]`)
	obj["subject"] = record.MustJSON("Hello World")
	return s, obj
}

func matchField(t *testing.T, s *InProcess, obj objectdb.Object, field, q string) bool {
	t.Helper()
	got, err := s.Match(context.Background(), testAccount, obj, field, json.RawMessage(record.MustJSON(q)))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestMatchAddressFields: from/to/cc match by local part, domain, or
// display name, case-insensitively; bcc is empty and matches nothing.
func TestMatchAddressFields(t *testing.T) {
	s, obj := headerFixture(t)
	for _, q := range []string{"john", "example.com", "John Doe", "JOHN"} {
		if !matchField(t, s, obj, "from", q) {
			t.Errorf("from did not match %q", q)
		}
	}
	if matchField(t, s, obj, "from", "jane") {
		t.Error("from matched a To-only term")
	}
	if !matchField(t, s, obj, "to", "jane") || !matchField(t, s, obj, "to", "Roe") {
		t.Error("to did not match by local part or display name")
	}
	if !matchField(t, s, obj, "cc", "cc@example.com") {
		t.Error("cc did not match its address")
	}
	if matchField(t, s, obj, "bcc", "anything") {
		t.Error("empty bcc matched a term")
	}
}

// TestMatchSubjectField: the subject condition is a case-insensitive
// substring of the stored subject.
func TestMatchSubjectField(t *testing.T) {
	s, obj := headerFixture(t)
	if !matchField(t, s, obj, "subject", "hello") {
		t.Error("subject did not match a case-differing substring")
	}
	if matchField(t, s, obj, "subject", "goodbye") {
		t.Error("subject matched an absent term")
	}
}

// matchHeaderCond calls Match with field "header" and the given condition
// list (one element: presence; two: presence plus a value substring).
func matchHeaderCond(t *testing.T, s *InProcess, obj objectdb.Object, h []string) bool {
	t.Helper()
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Match(context.Background(), testAccount, obj, "header", raw)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestMatchHeaderCondition: a one-element condition tests presence of the
// named header; a two-element condition additionally requires a
// case-insensitive substring of at least one of its instances' values.
func TestMatchHeaderCondition(t *testing.T) {
	s, obj := headerFixture(t)
	if !matchHeaderCond(t, s, obj, []string{"X-Custom"}) {
		t.Error("presence condition did not match an existing header")
	}
	if matchHeaderCond(t, s, obj, []string{"X-Missing"}) {
		t.Error("presence condition matched an absent header")
	}
	if !matchHeaderCond(t, s, obj, []string{"X-Custom", "foobar"}) {
		t.Error("value condition did not match a case-differing substring in the first instance")
	}
	if !matchHeaderCond(t, s, obj, []string{"X-Custom", "second"}) {
		t.Error("value condition did not match a substring in the second instance")
	}
	if matchHeaderCond(t, s, obj, []string{"X-Custom", "nope"}) {
		t.Error("value condition matched an absent substring")
	}
	if matchHeaderCond(t, s, obj, nil) {
		t.Error("empty condition matched")
	}
}
