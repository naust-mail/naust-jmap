package rawjson

import "testing"

// Smoke tests only: the deep differential suite (table + fuzzer vs
// encoding/json) lives with the scanner in internal/jsonscan and keeps
// running unchanged. These pin the forwarding and the documented null
// divergence at the public surface.
func TestForwarding(t *testing.T) {
	if s, ok := String([]byte(` "a\n" `)); !ok || s != "a\n" {
		t.Errorf("String = %q, %v", s, ok)
	}
	if _, ok := String([]byte(`null`)); ok {
		t.Error("String(null) must report false")
	}
	if n, ok := Int([]byte(`-42`)); !ok || n != -42 {
		t.Errorf("Int = %d, %v", n, ok)
	}
	if _, ok := Int([]byte(`1.5`)); ok {
		t.Error("Int(1.5) must report false")
	}
	if b, ok := Bool([]byte(`true`)); !ok || !b {
		t.Errorf("Bool = %v, %v", b, ok)
	}
	var keys []string
	if err := EachKey([]byte(`{"a":1,"b":{"c":2}}`), func(k string) { keys = append(keys, k) }); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "a" || keys[1] != "b" {
		t.Errorf("EachKey = %v", keys)
	}
	if err := EachKey([]byte(`null`), func(string) { t.Error("null must make no calls") }); err != nil {
		t.Errorf("EachKey(null) err = %v", err)
	}
	if err := EachKey([]byte(`[]`), func(string) {}); err == nil {
		t.Error("EachKey(array) must error")
	}
	m, err := Members([]byte(`{"@type":"A","junk":[1,{}],"@type":"B","id":"x"}`), map[string]bool{"@type": true, "id": true})
	if err != nil || len(m) != 2 || string(m["@type"]) != `"B"` || string(m["id"]) != `"x"` {
		t.Errorf("Members = %v, %v", m, err)
	}
	if m, err := Members([]byte(`null`), map[string]bool{"a": true}); m != nil || err != nil {
		t.Errorf("Members(null) = %v, %v, want nil map, nil error", m, err)
	}
	if _, err := Members([]byte(`{"a":1} trailing`), nil); err == nil {
		t.Error("Members with trailing bytes must error")
	}
}
