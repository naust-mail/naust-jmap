package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

func TestProcessEcho(t *testing.T) {
	p := NewProcessor()
	req, err := jmap.ParseRequest([]byte(`{
		"using": ["urn:ietf:params:jmap:core"],
		"methodCalls": [[ "Core/echo", { "hello": true, "high": 5 }, "b3ff" ]]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	resp := p.Process(context.Background(), req, nil, "state1")
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"methodResponses":[["Core/echo",{"hello":true,"high":5},"b3ff"]],"sessionState":"state1"}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
}

// Section 3.3: the server behaves as though capabilities not opted into
// do not exist, so even Core/echo needs urn:ietf:params:jmap:core.
func TestCapabilityOptInGating(t *testing.T) {
	p := NewProcessor()
	req, err := jmap.ParseRequest([]byte(`{"using": [], "methodCalls": [[ "Core/echo", {}, "c1" ]]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp := p.Process(context.Background(), req, nil, "s")
	var e jmap.MethodError
	if resp.MethodResponses[0].Name != "error" {
		t.Fatalf("got %+v", resp.MethodResponses[0])
	}
	if err := json.Unmarshal(resp.MethodResponses[0].Args, &e); err != nil || e.Type != jmap.ErrUnknownMethod {
		t.Errorf("non-opted method -> %v %v", e, err)
	}
}

func TestCheckUsing(t *testing.T) {
	p := NewProcessor()
	if rerr := p.CheckUsing(&jmap.Request{Using: []string{jmap.CoreCapability}}); rerr != nil {
		t.Errorf("core capability rejected: %+v", rerr)
	}
	rerr := p.CheckUsing(&jmap.Request{Using: []string{"https://example.com/apis/foobar"}})
	if rerr == nil || rerr.Type != jmap.ProblemUnknownCapability || rerr.Status != 400 {
		t.Errorf("unknown capability -> %+v", rerr)
	}
}

func TestCreatedIdsEchoSemantics(t *testing.T) {
	p := NewProcessor()
	// Absent in request -> absent in response.
	req, _ := jmap.ParseRequest([]byte(`{"using": ["urn:ietf:params:jmap:core"], "methodCalls": []}`))
	resp := p.Process(context.Background(), req, nil, "s")
	if resp.CreatedIds != nil {
		t.Error("createdIds appeared without being requested")
	}
	// Present (even empty) -> echoed, including handler additions.
	p.Register("Test/make", jmap.CoreCapability, func(_ context.Context, call *Call) []jmap.Invocation {
		call.CreatedIds["clientid1"] = "Sreal1"
		return []jmap.Invocation{{Name: "Test/make", Args: json.RawMessage(`{}`), CallID: call.CallID}}
	})
	req, _ = jmap.ParseRequest([]byte(`{
		"using": ["urn:ietf:params:jmap:core"],
		"methodCalls": [["Test/make", {}, "c1"]],
		"createdIds": {"older": "Sprev"}
	}`))
	resp = p.Process(context.Background(), req, nil, "s")
	if resp.CreatedIds == nil || resp.CreatedIds["older"] != "Sprev" || resp.CreatedIds["clientid1"] != "Sreal1" {
		t.Errorf("createdIds = %v", resp.CreatedIds)
	}
}

func TestPanicIsolation(t *testing.T) {
	p := NewProcessor()
	p.Register("Boom/now", jmap.CoreCapability, func(_ context.Context, _ *Call) []jmap.Invocation {
		panic("kaboom")
	})
	req, _ := jmap.ParseRequest([]byte(`{
		"using": ["urn:ietf:params:jmap:core"],
		"methodCalls": [["Boom/now", {}, "c1"], ["Core/echo", {"still": "alive"}, "c2"]]
	}`))
	resp := p.Process(context.Background(), req, nil, "s")
	var e jmap.MethodError
	if err := json.Unmarshal(resp.MethodResponses[0].Args, &e); err != nil || e.Type != jmap.ErrServerFail {
		t.Errorf("panic -> %v %v", e, err)
	}
	if resp.MethodResponses[1].Name != "Core/echo" {
		t.Error("request died with the panicking method")
	}
}
