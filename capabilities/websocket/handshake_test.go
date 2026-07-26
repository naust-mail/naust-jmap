package websocket

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The worked example of RFC 6455 section 4.2.2: the key
// "dGhlIHNhbXBsZSBub25jZQ==" must produce the accept value
// "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=".
func TestAcceptKeyExample(t *testing.T) {
	if got := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("acceptKey = %q", got)
	}
}

// handshakeServer serves accept() and reports whether it succeeded.
func handshakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _ := accept(w, r)
		if conn != nil {
			conn.Close()
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// rawRequest sends verbatim request bytes and returns the status line
// plus headers of the reply.
func rawRequest(t *testing.T, ts *httptest.Server, request string) (status string, header http.Header) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.Status, resp.Header
}

const goodHandshake = "GET /ws HTTP/1.1\r\n" +
	"Host: server.example.com\r\n" +
	"Upgrade: websocket\r\n" +
	"Connection: Upgrade\r\n" +
	"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
	"Sec-WebSocket-Version: 13\r\n" +
	"Sec-WebSocket-Protocol: jmap\r\n\r\n"

func TestHandshakeAccepts(t *testing.T) {
	ts := handshakeServer(t)
	status, h := rawRequest(t, ts, goodHandshake)
	if !strings.HasPrefix(status, "101") {
		t.Fatalf("status %q", status)
	}
	if got := h.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Errorf("accept %q", got)
	}
	// RFC 8887 section 4.2: the reply MUST carry the echoed subprotocol.
	if got := h.Get("Sec-WebSocket-Protocol"); got != "jmap" {
		t.Errorf("subprotocol %q", got)
	}
	if !strings.EqualFold(h.Get("Upgrade"), "websocket") || !strings.EqualFold(h.Get("Connection"), "Upgrade") {
		t.Errorf("upgrade headers: %v", h)
	}
}

// Tokens are case-insensitive and may sit inside comma-separated lists
// (RFC 6455 section 4.2.1); the subprotocol header may offer several
// values as long as "jmap" is among them.
func TestHandshakeTokenForms(t *testing.T) {
	ts := handshakeServer(t)
	req := "GET /ws HTTP/1.1\r\n" +
		"Host: server.example.com\r\n" +
		"Upgrade: WebSocket\r\n" +
		"Connection: keep-alive, UPGRADE\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Protocol: chat, jmap\r\n\r\n"
	status, _ := rawRequest(t, ts, req)
	if !strings.HasPrefix(status, "101") {
		t.Fatalf("status %q", status)
	}
}

func TestHandshakeRejections(t *testing.T) {
	replace := func(old, new string) string { return strings.Replace(goodHandshake, old, new, 1) }
	for name, tc := range map[string]struct {
		request    string
		wantStatus string
	}{
		"post method":         {replace("GET", "POST"), "405"},
		"no upgrade header":   {replace("Upgrade: websocket\r\n", ""), "400"},
		"no connection token": {replace("Connection: Upgrade", "Connection: keep-alive"), "400"},
		"wrong version":       {replace("Sec-WebSocket-Version: 13", "Sec-WebSocket-Version: 8"), "426"},
		"missing key":         {replace("Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n", ""), "400"},
		"short key":           {replace("dGhlIHNhbXBsZSBub25jZQ==", "c2hvcnQ="), "400"},
		"key not base64":      {replace("dGhlIHNhbXBsZSBub25jZQ==", "!!!not-base64!!!"), "400"},
		"no subprotocol":      {replace("Sec-WebSocket-Protocol: jmap\r\n", ""), "400"},
		"wrong subprotocol":   {replace("Sec-WebSocket-Protocol: jmap", "Sec-WebSocket-Protocol: chat"), "400"},
	} {
		status, h := rawRequest(t, ts(t), tc.request)
		if !strings.HasPrefix(status, tc.wantStatus) {
			t.Errorf("%s: status %q, want %s", name, status, tc.wantStatus)
		}
		if name == "wrong version" && h.Get("Sec-WebSocket-Version") != "13" {
			t.Errorf("426 reply must name version 13 (RFC 6455 section 4.2.2)")
		}
	}
}

// ts gives each rejection case a fresh server so a failed handshake
// cannot poison a kept-alive connection for the next case.
func ts(t *testing.T) *httptest.Server { return handshakeServer(t) }

// The subprotocol may arrive across multiple header lines (RFC 6455
// section 4.2.1 item 8 is a #-list); "jmap" in any of them counts.
func TestHandshakeMultipleProtocolHeaders(t *testing.T) {
	req := "GET /ws HTTP/1.1\r\n" +
		"Host: server.example.com\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Protocol: chat\r\n" +
		"Sec-WebSocket-Protocol: jmap\r\n\r\n"
	status, _ := rawRequest(t, handshakeServer(t), req)
	if !strings.HasPrefix(status, "101") {
		t.Fatalf("status %q", status)
	}
}

// Requested extensions are never negotiated: the reply MUST NOT list
// extensions the server did not agree to (RFC 6455 section 4.2.2 step
// 5.6), and this server agrees to none - RFC 7692 compression is out
// of scope.
func TestHandshakeIgnoresExtensions(t *testing.T) {
	req := strings.Replace(goodHandshake, "Sec-WebSocket-Version: 13\r\n",
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Extensions: permessage-deflate\r\n", 1)
	status, h := rawRequest(t, handshakeServer(t), req)
	if !strings.HasPrefix(status, "101") {
		t.Fatalf("status %q", status)
	}
	if got := h.Get("Sec-WebSocket-Extensions"); got != "" {
		t.Fatalf("server offered extensions it never negotiated: %q", got)
	}
}
