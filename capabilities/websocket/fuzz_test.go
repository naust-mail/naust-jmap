package websocket

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/naust-mail/naust-jmap/core/runtime"
)

// The fuzzer drives the JMAP message dispatch through a live server:
// every input is delivered as one complete text message, so the frame
// layer (fuzzed separately) is exercised only on its happy path and
// the JSON envelope handling of section 4.3 takes all the abuse. The
// oracle is liveness plus shape: after any input the server must still
// answer a well-formed probe request, must never close the connection
// (no dispatch outcome for a valid-UTF-8, size-bounded text message
// closes it), and every reply must be a JSON object carrying @type.

// fuzzEnv is one process-lifetime server plus a reusable client
// connection; fuzz workers are separate processes, so plain globals
// are single-threaded here.
var (
	fuzzOnce sync.Once
	fuzzURL  string
	fuzzConn *fuzzClient
)

// fuzzClient is a minimal masked-writer client that reports errors
// instead of failing a test, so the fuzz loop can redial.
type fuzzClient struct {
	nc net.Conn
	br *bufio.Reader
}

func startFuzzServer() {
	proc := runtime.NewProcessor()
	srv, err := runtime.NewServer(staticAuth{}, proc, "https://jmap.example.com", runtime.DefaultCoreCapabilities())
	if err != nil {
		panic(err)
	}
	h := NewHandler(srv, staticAuth{})
	if err := srv.Capability(CapabilityURI).Handle("/ws", h).Err(); err != nil {
		panic(err)
	}
	// The server lives for the whole fuzz process; the process exit is
	// its cleanup.
	fuzzURL = httptest.NewServer(srv).URL
}

func dialFuzz(url string) (*fuzzClient, error) {
	nc, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(nc, "GET /ws HTTP/1.1\r\n"+
		"Host: server.example.com\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Authorization: Basic am9obkBleGFtcGxlLmNvbTpzZWNyZXQ=\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Sec-WebSocket-Protocol: jmap\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		nc.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		nc.Close()
		return nil, fmt.Errorf("handshake status %d", resp.StatusCode)
	}
	return &fuzzClient{nc: nc, br: br}, nil
}

func (c *fuzzClient) sendText(payload []byte) error {
	key := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	b := []byte{0x81}
	switch {
	case len(payload) <= 125:
		b = append(b, 0x80|byte(len(payload)))
	case len(payload) <= 0xffff:
		b = append(b, 0x80|126, byte(len(payload)>>8), byte(len(payload)))
	default:
		b = append(b, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(len(payload)))
		b = append(b, ext[:]...)
	}
	b = append(b, key[:]...)
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ key[i%4]
	}
	_, err := c.nc.Write(append(b, masked...))
	return err
}

// next returns the next data or close frame's opcode and payload.
func (c *fuzzClient) next(deadline time.Time) (byte, []byte, error) {
	c.nc.SetReadDeadline(deadline)
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	length := int64(hdr[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0] & 0x0f, payload, nil
}

func FuzzDispatch(f *testing.F) {
	for _, seed := range []string{
		`{"@type":"Request","id":"R1","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"hi":1},"c0"]]}`,
		`{"@type":"Request","using":[],"methodCalls":[]}`,
		`The quick brown fox jumps over the lazy dog`,
		`{}`, `[]`, `null`, `42`, `"Request"`,
		`{"@type":5}`, `{"@type":"Nope","id":"x"}`,
		`{"@type":"Request","id":""}`,
		`{"@type":"WebSocketPushEnable","dataTypes":["Mailbox"],"pushState":"aaa"}`,
		`{"@type":"WebSocketPushEnable","dataTypes":"notalist"}`,
		`{"@type":"WebSocketPushDisable"}`,
		`{"@type":"Request","id":"` + strings.Repeat("a", 300) + `"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		// Invalid UTF-8 and oversize messages are the frame layer's
		// territory (closes 1007/1009 by design); skip so every input
		// here reaches dispatch.
		if !utf8.Valid(data) || len(data) > 1<<20 {
			t.Skip()
		}
		// An input carrying the probe's own id could leave a stale
		// look-alike reply that desynchronizes a later run.
		if strings.Contains(string(data), "fuzz-probe") {
			t.Skip()
		}
		fuzzOnce.Do(startFuzzServer)
		if fuzzConn == nil {
			c, err := dialFuzz(fuzzURL)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			fuzzConn = c
		}
		c := fuzzConn
		if err := c.sendText(data); err != nil {
			fuzzConn = nil
			t.Fatalf("send: %v", err)
		}
		// The probe pins the oracle: whatever the input provoked, the
		// server must still answer this labeled request.
		probe := `{"@type":"Request","id":"fuzz-probe","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"c0"]]}`
		if err := c.sendText([]byte(probe)); err != nil {
			fuzzConn = nil
			t.Fatalf("send probe: %v", err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			op, payload, err := c.next(deadline)
			if err != nil {
				fuzzConn = nil
				t.Fatalf("input %q: connection died before the probe reply: %v", data, err)
			}
			switch op {
			case 0x1: // text
				var m map[string]json.RawMessage
				if err := json.Unmarshal(payload, &m); err != nil || m["@type"] == nil {
					fuzzConn = nil
					t.Fatalf("input %q: reply is not a JSON object with @type: %q", data, payload)
				}
				var id string
				json.Unmarshal(m["requestId"], &id)
				if id == "fuzz-probe" {
					var typ string
					json.Unmarshal(m["@type"], &typ)
					if typ != "Response" {
						fuzzConn = nil
						t.Fatalf("input %q: probe answered with %q", data, payload)
					}
					return
				}
			case 0x8: // close
				fuzzConn = nil
				t.Fatalf("input %q: server closed the connection: %q", data, payload)
			}
		}
	})
}
