package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
)

// pushServer is noteServer plus a second type (so the types filter is
// observable) and push enabled.
func pushServer(t *testing.T) *httptest.Server {
	t.Helper()
	core := DefaultCoreCapabilities()
	a := auth.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := NewProcessor()
	if err := RegisterStandardType(p, db, testNoteType(), core); err != nil {
		t.Fatal(err)
	}
	taskType := &descriptor.Type{
		Name:       "TestTask",
		Capability: "urn:example:testnote",
		Properties: map[string]descriptor.Property{
			"title": {Kind: descriptor.KindString},
		},
	}
	if err := RegisterStandardType(p, db, taskType, core); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability("urn:example:testnote", struct{}{}, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if err := srv.EnablePush(db, notify.NewInProcess(), nil, nil); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// sseClient reads a text/event-stream response one event at a time.
type sseClient struct {
	resp *http.Response
	br   *bufio.Reader
}

func openEventSource(t *testing.T, ts *httptest.Server, query string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/eventsource?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("john@example.com", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	return &sseClient{resp: resp, br: bufio.NewReader(resp.Body)}
}

// readEvent returns the next event's name and data line, or an error
// at end of stream.
func (c *sseClient) readEvent() (name, data string, err error) {
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case strings.HasPrefix(line, "id:"):
			return "", "", fmt.Errorf("stream sent an event id: %q", line)
		case line == "" && name != "":
			return name, data, nil
		}
	}
}

// mustStateEvent asserts the next event is a well-formed StateChange
// (section 7.1 shape) and returns its changed map.
func (c *sseClient) mustStateEvent(t *testing.T) map[string]map[string]string {
	t.Helper()
	name, data, err := c.readEvent()
	if err != nil {
		t.Fatal(err)
	}
	if name != "state" {
		t.Fatalf("event %q (%s), want state", name, data)
	}
	var sc struct {
		Type    string                       `json:"@type"`
		Changed map[string]map[string]string `json:"changed"`
	}
	if err := json.Unmarshal([]byte(data), &sc); err != nil {
		t.Fatalf("state data %q: %v", data, err)
	}
	if sc.Type != "StateChange" {
		t.Fatalf("@type %q, want StateChange", sc.Type)
	}
	return sc.Changed
}

func TestEventSourceStream(t *testing.T) {
	ts := pushServer(t)
	c := openEventSource(t, ts, "types=*&closeafter=no&ping=0")

	// On connect: the full current state of every type ("0" = never
	// written), so a reconnecting client can never miss changes.
	changed := c.mustStateEvent(t)
	if changed["Atest1"]["TestNote"] != "0" || changed["Atest1"]["TestTask"] != "0" {
		t.Fatalf("initial state: %v", changed)
	}

	// A commit pushes a state event carrying only the changed type,
	// with the state a Foo/get would now return (section 7.1).
	createNote(t, ts, `{"subject":"hello"}`)
	changed = c.mustStateEvent(t)
	if changed["Atest1"]["TestNote"] != "1" {
		t.Fatalf("after create: %v", changed)
	}
	if _, has := changed["Atest1"]["TestTask"]; has {
		t.Fatalf("unchanged type in StateChange: %v", changed)
	}
}

func TestEventSourceCloseAfterState(t *testing.T) {
	ts := pushServer(t)
	// closeafter=state: the server MUST end the response after pushing
	// a state event (section 7.3).
	c := openEventSource(t, ts, "types=*&closeafter=state&ping=0")
	c.mustStateEvent(t)
	if _, _, err := c.readEvent(); err != io.EOF {
		t.Fatalf("stream still open after state event: %v", err)
	}
}

func TestEventSourcePing(t *testing.T) {
	ts := pushServer(t)
	c := openEventSource(t, ts, "types=*&closeafter=no&ping=1")
	c.mustStateEvent(t)

	// One ping interval with no changes: a ping event whose data
	// carries the interval in use (section 7.3).
	name, data, err := c.readEvent()
	if err != nil {
		t.Fatal(err)
	}
	if name != "ping" {
		t.Fatalf("event %q, want ping", name)
	}
	var ping struct {
		Interval uint64 `json:"interval"`
	}
	if err := json.Unmarshal([]byte(data), &ping); err != nil || ping.Interval != 1 {
		t.Fatalf("ping data %q (%v)", data, err)
	}
}

func TestEventSourceTypeFilter(t *testing.T) {
	ts := pushServer(t)
	c := openEventSource(t, ts, "types=TestTask&closeafter=no&ping=0")

	changed := c.mustStateEvent(t)
	if _, has := changed["Atest1"]["TestNote"]; has || changed["Atest1"]["TestTask"] != "0" {
		t.Fatalf("initial state ignores the types filter: %v", changed)
	}

	// The server MUST only push changes for the listed types (7.3):
	// the TestNote commit is silent, so the next event on the stream
	// is the TestTask commit after it.
	createNote(t, ts, `{"subject":"quiet"}`)
	r := callAPI(t, ts, inv("TestTask/set",
		`{"accountId":"Atest1","create":{"c":{"title":"loud"}}}`, "0"))
	if _, ok := methodArgs(t, r, 0, "TestTask/set")["created"].(map[string]any); !ok {
		t.Fatal("task create failed")
	}
	changed = c.mustStateEvent(t)
	if _, has := changed["Atest1"]["TestNote"]; has || changed["Atest1"]["TestTask"] != "2" {
		t.Fatalf("filtered event: %v", changed)
	}
}

func TestEventSourceErrors(t *testing.T) {
	ts := pushServer(t)

	for _, tc := range []struct {
		name, query string
		want        int
	}{
		{"missing types", "closeafter=no&ping=0", http.StatusBadRequest},
		{"unknown type", "types=Bogus&closeafter=no&ping=0", http.StatusBadRequest},
		{"bad closeafter", "types=*&closeafter=maybe&ping=0", http.StatusBadRequest},
		{"missing ping", "types=*&closeafter=no", http.StatusBadRequest},
		{"negative ping", "types=*&closeafter=no&ping=-1", http.StatusBadRequest},
		{"huge ping clamps and connects", "types=*&closeafter=state&ping=999999", http.StatusOK},
	} {
		resp := get(t, ts, "/eventsource?"+tc.query, "john@example.com", "secret")
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}

	resp := get(t, ts, "/eventsource?types=*&closeafter=no&ping=0", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/eventsource?types=*&closeafter=no&ping=0", nil)
	req.SetBasicAuth("john@example.com", "secret")
	postResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", postResp.StatusCode)
	}
}
