package message

import (
	"reflect"
	"testing"
)

func ptr(s string) *string { return &s }

// The address-list example of RFC 8621 4.1.2.3 / 4.1.2.4.
const specAddressList = "  \"  James Smythe\" <james@example.com>, Friends:\r\n" +
	"  jane@example.com, =?UTF-8?Q?John_Sm=C3=AEth?=\r\n" +
	"  <john@example.com>;"

func TestAddressesFormSpecExample(t *testing.T) {
	got := AddressesForm(specAddressList)
	want := []Address{
		{Name: ptr("James Smythe"), Email: "james@example.com"},
		{Name: nil, Email: "jane@example.com"},
		{Name: ptr("John Smîth"), Email: "john@example.com"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AddressesForm = %s, want %s", fmtAddrs(got), fmtAddrs(want))
	}
}

func TestGroupedAddressesFormSpecExample(t *testing.T) {
	got := GroupedAddressesForm(specAddressList)
	want := []AddressGroup{
		{Name: nil, Addresses: []Address{
			{Name: ptr("James Smythe"), Email: "james@example.com"},
		}},
		{Name: ptr("Friends"), Addresses: []Address{
			{Name: nil, Email: "jane@example.com"},
			{Name: ptr("John Smîth"), Email: "john@example.com"},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GroupedAddressesForm mismatch:\n got %#v\nwant %#v", got, want)
	}
}

func TestAddressesFormEdgeCases(t *testing.T) {
	cases := []struct {
		name, raw string
		want      []Address
	}{
		{"comment as name", "jane@example.com (Jane Doe)",
			[]Address{{Name: ptr("Jane Doe"), Email: "jane@example.com"}}},
		{"display name wins over comment", "Jane <jane@example.com> (ignored)",
			[]Address{{Name: ptr("Jane"), Email: "jane@example.com"}}},
		{"obsolete route", "<@relay1,@relay2:user@example.com>",
			[]Address{{Name: nil, Email: "user@example.com"}}},
		{"quoted local part kept", `"john smith"@example.com`,
			[]Address{{Name: nil, Email: `"john smith"@example.com`}}},
		{"quoted pair in name", `"John \"J\" D" <j@example.com>`,
			[]Address{{Name: ptr(`John "J" D`), Email: "j@example.com"}}},
		{"empty segments skipped", ", jane@example.com ,,",
			[]Address{{Name: nil, Email: "jane@example.com"}}},
		{"no at sign best effort", "just-a-token",
			[]Address{{Name: nil, Email: "just-a-token"}}},
		{"comma in quoted name", `"Doe, Jane" <jane@example.com>`,
			[]Address{{Name: ptr("Doe, Jane"), Email: "jane@example.com"}}},
		{"comma in comment", "jane@example.com (a, b)",
			[]Address{{Name: ptr("a, b"), Email: "jane@example.com"}}},
		{"angle only", "<x@y.com>",
			[]Address{{Name: nil, Email: "x@y.com"}}},
		{"unterminated angle", "Jane <jane@example.com",
			[]Address{{Name: ptr("Jane"), Email: "jane@example.com"}}},
		{"folded addr spec", "jane\r\n @example.com",
			[]Address{{Name: nil, Email: "jane@example.com"}}},
		{"empty group", "undisclosed-recipients:;",
			nil},
		{"whitespace only", "   ", nil},
	}
	for _, c := range cases {
		got := AddressesForm(c.raw)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: AddressesForm(%q) = %s, want %s", c.name, c.raw, fmtAddrs(got), fmtAddrs(c.want))
		}
	}
}

func TestGroupedAddressesEmptyGroupKept(t *testing.T) {
	got := GroupedAddressesForm("undisclosed-recipients:;")
	want := []AddressGroup{{Name: ptr("undisclosed-recipients"), Addresses: nil}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func fmtAddrs(addrs []Address) string {
	out := "["
	for _, a := range addrs {
		name := "<nil>"
		if a.Name != nil {
			name = *a.Name
		}
		out += "{" + name + " | " + a.Email + "}"
	}
	return out + "]"
}
