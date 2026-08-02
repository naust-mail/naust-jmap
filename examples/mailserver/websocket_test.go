package main

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
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/capabilities/websocket"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/examples/internal/tokenauth"
)

// TestWebSocketEndToEnd exercises the full production wiring on a live
// socket: bearer-token login, the RFC 6455 handshake against the
// registrar-mounted /ws endpoint, WebSocketPushEnable with its
// snapshot, a real Mailbox/set commit observed as a StateChange, a
// Request/Response exchange, and finally tokenauth.RevokeUser killing
// the connection with close code 1008.
func TestWebSocketEndToEnd(t *testing.T) {
	// The same assembly main() performs, on the in-memory backend.
	store := memory.New()
	db := objectdb.New(store, lease.NewInProcess(store))
	notifier := notify.NewInProcess()
	users := tokenauth.New()
	users.AddUser("demo@example.com", "demo", "Ademo")

	proc := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	if err := mail.RegisterMailbox(proc, mail.MailboxConfig{DB: db, Core: core}); err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewServer(users, proc, "http://mail.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Capability(mail.CapabilityURI).Advertise(struct{}{}, mail.DefaultAccountCapability()).Err(); err != nil {
		t.Fatal(err)
	}
	ws := websocket.NewHandler(srv, users, websocket.Config{})
	ws.EnablePush(db, notifier)
	if err := srv.Capability(websocket.CapabilityURI).
		Advertise(websocket.SessionCapability(srv.BaseURL(), "/ws", ws.SupportsPush()), struct{}{}).
		Handle("/ws", ws).
		Err(); err != nil {
		t.Fatal(err)
	}

	root := http.NewServeMux()
	root.Handle("/login", users.LoginHandler())
	root.Handle("/", srv)
	ts := httptest.NewServer(root)
	defer ts.Close()

	// Mint a bearer token exactly as a client would.
	loginReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/login", nil)
	loginReq.SetBasicAuth("demo@example.com", "demo")
	loginResp, err := http.DefaultClient.Do(loginReq)
	if err != nil {
		t.Fatal(err)
	}
	tokenBytes, _ := io.ReadAll(loginResp.Body)
	loginResp.Body.Close()
	token := string(tokenBytes)

	// Raw RFC 6455 handshake with the token.
	nc, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	fmt.Fprintf(nc, "GET /ws HTTP/1.1\r\n"+
		"Host: mail.example.com\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Authorization: Bearer %s\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Sec-WebSocket-Protocol: jmap\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", token)
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Protocol") != "jmap" {
		t.Fatalf("handshake: %d %v", resp.StatusCode, resp.Header)
	}

	send := func(payload string) {
		t.Helper()
		key := [4]byte{1, 2, 3, 4}
		b := []byte{0x81}
		switch {
		case len(payload) <= 125:
			b = append(b, 0x80|byte(len(payload)))
		default:
			b = append(b, 0x80|126, byte(len(payload)>>8), byte(len(payload)))
		}
		b = append(b, key[:]...)
		for i := 0; i < len(payload); i++ {
			b = append(b, payload[i]^key[i%4])
		}
		if _, err := nc.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	recv := func() (op byte, payload []byte) {
		t.Helper()
		nc.SetReadDeadline(time.Now().Add(5 * time.Second))
		var hdr [2]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			t.Fatal(err)
		}
		length := int64(hdr[1] & 0x7f)
		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(br, ext[:]); err != nil {
				t.Fatal(err)
			}
			length = int64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(br, ext[:]); err != nil {
				t.Fatal(err)
			}
			length = int64(binary.BigEndian.Uint64(ext[:]))
		}
		payload = make([]byte, length)
		if _, err := io.ReadFull(br, payload); err != nil {
			t.Fatal(err)
		}
		return hdr[0] & 0x0f, payload
	}
	// Push events interleave freely with responses, so messages of the
	// wrong type are set aside, not dropped.
	var pending []map[string]any
	recvJSON := func(wantType string) map[string]any {
		t.Helper()
		for i, m := range pending {
			if m["@type"] == wantType {
				pending = append(pending[:i:i], pending[i+1:]...)
				return m
			}
		}
		for {
			op, payload := recv()
			if op != 0x1 {
				t.Fatalf("opcode %d while waiting for %s", op, wantType)
			}
			var m map[string]any
			if err := json.Unmarshal(payload, &m); err != nil {
				t.Fatal(err)
			}
			if m["@type"] == wantType {
				return m
			}
			pending = append(pending, m)
		}
	}

	// Enable push for everything; the first StateChange is the snapshot.
	send(`{"@type":"WebSocketPushEnable","dataTypes":null}`)
	recvJSON("StateChange")

	// A real commit over the socket, and its push notification.
	send(`{"@type":"Request","id":"R1","using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],` +
		`"methodCalls":[["Mailbox/set",{"accountId":"Ademo","create":{"i":{"name":"Inbox","role":"inbox"}}},"c0"]]}`)
	m := recvJSON("Response")
	if m["requestId"] != "R1" {
		t.Fatalf("response: %v", m)
	}
	sc := recvJSON("StateChange")
	if sc["changed"].(map[string]any)["Ademo"] == nil {
		t.Fatalf("push after commit: %v", sc)
	}

	// Revocation reaches the live socket: 1008, then EOF.
	users.RevokeUser("demo@example.com", time.Now())
	op, payload := recv()
	if op != 0x8 {
		t.Fatalf("opcode %d, want close", op)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != 1008 {
		t.Fatalf("close code %d, want 1008", code)
	}
}
