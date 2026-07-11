package jmap

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Fixture from RFC 8620 section 3.3.1.
const exampleRequest = `{
  "using": [ "urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail" ],
  "methodCalls": [
    [ "method1", {
      "arg1": "arg1data",
      "arg2": "arg2data"
    }, "c1" ],
    [ "method2", {
      "arg1": "arg1data"
    }, "c2" ],
    [ "method3", {}, "c3" ]
  ]
}`

func TestParseRequestSpecExample(t *testing.T) {
	req, err := ParseRequest([]byte(exampleRequest))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if len(req.Using) != 2 || req.Using[0] != CoreCapability {
		t.Errorf("using = %v", req.Using)
	}
	if len(req.MethodCalls) != 3 {
		t.Fatalf("got %d method calls, want 3", len(req.MethodCalls))
	}
	if req.MethodCalls[0].Name != "method1" || req.MethodCalls[0].CallID != "c1" {
		t.Errorf("call 0 = %+v", req.MethodCalls[0])
	}
	var args map[string]string
	if err := json.Unmarshal(req.MethodCalls[1].Args, &args); err != nil || args["arg1"] != "arg1data" {
		t.Errorf("call 1 args = %s (err %v)", req.MethodCalls[1].Args, err)
	}
	if req.CreatedIds != nil {
		t.Error("createdIds should be nil when absent")
	}
}

func TestParseRequestNotRequest(t *testing.T) {
	cases := []string{
		`[]`,
		`{"methodCalls": []}`,
		`{"using": []}`,
		`{"using": "urn:ietf:params:jmap:core", "methodCalls": []}`,
		`{"using": [], "methodCalls": [["a", {}, "c1", "extra"]]}`,
		`{"using": [], "methodCalls": [["a", [], "c1"]]}`,
		`{"using": [], "methodCalls": [[1, {}, "c1"]]}`,
		`{"using": [], "methodCalls": [], "createdIds": {"bad id!": "x"}}`,
	}
	for _, body := range cases {
		if _, err := ParseRequest([]byte(body)); !errors.Is(err, ErrNotRequest) {
			t.Errorf("ParseRequest(%s) err = %v, want ErrNotRequest", body, err)
		}
	}
	// Unknown top-level properties must be ignored, not rejected (3.3).
	if _, err := ParseRequest([]byte(`{"using": [], "methodCalls": [], "futureProp": 1}`)); err != nil {
		t.Errorf("unknown property rejected: %v", err)
	}
}

func TestParseRequestCreatedIdsPresence(t *testing.T) {
	req, err := ParseRequest([]byte(`{"using": [], "methodCalls": [], "createdIds": {}}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.CreatedIds == nil {
		t.Error("createdIds {} should parse as non-nil empty map")
	}
}

// Fixture from RFC 8620 section 3.4.1: a method may emit several
// responses sharing one call id, and errors are ["error", ...] entries.
func TestResponseSpecExample(t *testing.T) {
	resp := Response{
		MethodResponses: []Invocation{
			{Name: "method1", Args: json.RawMessage(`{"arg1":3,"arg2":"foo"}`), CallID: "c1"},
			{Name: "method2", Args: json.RawMessage(`{"isBlah":true}`), CallID: "c2"},
			{Name: "anotherResponseFromMethod2", Args: json.RawMessage(`{"data":10,"yetmoredata":"Hello"}`), CallID: "c2"},
			ErrorInvocation("c3", MethodError{Type: ErrUnknownMethod}),
		},
		SessionState: "75128aab4b1b",
	}
	got, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"methodResponses":[` +
		`["method1",{"arg1":3,"arg2":"foo"},"c1"],` +
		`["method2",{"isBlah":true},"c2"],` +
		`["anotherResponseFromMethod2",{"data":10,"yetmoredata":"Hello"},"c2"],` +
		`["error",{"type":"unknownMethod"},"c3"]],` +
		`"sessionState":"75128aab4b1b"}`
	if string(got) != want {
		t.Errorf("marshal mismatch\n got %s\nwant %s", got, want)
	}
}

func TestResponseCreatedIdsOmission(t *testing.T) {
	got, _ := json.Marshal(Response{MethodResponses: []Invocation{}, SessionState: "s"})
	if strings.Contains(string(got), "createdIds") {
		t.Errorf("nil createdIds must be omitted: %s", got)
	}
	got, _ = json.Marshal(Response{MethodResponses: []Invocation{}, CreatedIds: map[Id]Id{}, SessionState: "s"})
	if !strings.Contains(string(got), `"createdIds":{}`) {
		t.Errorf("empty createdIds map must round-trip as {}: %s", got)
	}
}

// Fixture from RFC 8620 section 4.1: Core/echo returns its arguments.
func TestInvocationEchoRoundTrip(t *testing.T) {
	wire := `[ "Core/echo", {
      "hello": true,
      "high": 5
    }, "b3ff" ]`
	var inv Invocation
	if err := json.Unmarshal([]byte(wire), &inv); err != nil {
		t.Fatal(err)
	}
	if inv.Name != "Core/echo" || inv.CallID != "b3ff" {
		t.Errorf("decoded %+v", inv)
	}
	out, err := json.Marshal(Invocation{Name: inv.Name, Args: inv.Args, CallID: inv.CallID})
	if err != nil {
		t.Fatal(err)
	}
	// json.Marshal compacts custom-marshaler output; the args are
	// semantically identical to the spec example.
	want := `["Core/echo",{"hello":true,"high":5},"b3ff"]`
	if string(out) != want {
		t.Errorf("echo mismatch\n got %s\nwant %s", out, want)
	}
}

func TestInvocationNilArgsMarshal(t *testing.T) {
	out, err := json.Marshal(Invocation{Name: "a", CallID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `["a",{},"c"]` {
		t.Errorf("got %s", out)
	}
}

// Fixture from RFC 8620 section 3.6.1.1.
func TestRequestErrorProblemShape(t *testing.T) {
	got, _ := json.Marshal(RequestError{
		Type:   ProblemLimit,
		Limit:  "maxSizeRequest",
		Status: 400,
		Detail: "too big",
	})
	want := `{"type":"urn:ietf:params:jmap:error:limit","status":400,"detail":"too big","limit":"maxSizeRequest"}`
	if string(got) != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	got, _ = json.Marshal(RequestError{Type: ProblemNotJSON, Status: 400})
	if strings.Contains(string(got), "limit") || strings.Contains(string(got), "detail") {
		t.Errorf("empty optional fields must be omitted: %s", got)
	}
}
