package websocket

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// keyGUID is the fixed string every accept key mixes in (RFC 6455
// section 4.2.2 step 5.4).
const keyGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// subprotocol is the value RFC 8887 section 4.2 requires: the client
// MUST include "jmap" in Sec-WebSocket-Protocol and the server MUST
// echo it in the reply.
const subprotocol = "jmap"

// acceptKey derives Sec-WebSocket-Accept from the client's key: the
// base64 of the SHA-1 of the (still base64-encoded) key concatenated
// with the fixed GUID (RFC 6455 section 4.2.2). SHA-1's weaknesses do
// not matter here - the construction only proves the peer speaks
// WebSocket, not anything cryptographic (section 10.8).
func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + keyGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// headerHasToken reports whether any instance of the named header
// contains the token, ASCII case-insensitively, in its comma-separated
// list (the |Connection| and |Upgrade| grammar of RFC 6455 section
// 4.2.1).
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// accept validates the client's opening handshake (RFC 6455 section
// 4.2.1, plus RFC 8887 section 4.2's mandatory "jmap" subprotocol) and
// completes the upgrade: on success the caller owns the hijacked
// connection with the 101 reply already flushed. On failure the HTTP
// error has been written and conn is nil (a malformed handshake gets a
// plain HTTP error, per 4.2.1).
func accept(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.ReadWriter) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "websocket handshake must be a GET", http.StatusMethodNotAllowed)
		return nil, nil
	}
	if !headerHasToken(r.Header, "Upgrade", "websocket") || !headerHasToken(r.Header, "Connection", "Upgrade") {
		http.Error(w, "not a websocket upgrade", http.StatusBadRequest)
		return nil, nil
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		// An unknown version aborts with 426 naming the version the
		// server speaks (RFC 6455 section 4.2.2 step 4 /version/).
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, nil
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if decoded, err := base64.StdEncoding.DecodeString(key); err != nil || len(decoded) != 16 {
		// The key must be base64 of a 16-byte value (section 4.2.1 item 5).
		http.Error(w, "malformed Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, nil
	}
	if !headerHasToken(r.Header, "Sec-WebSocket-Protocol", subprotocol) {
		// RFC 8887 section 4.2: the client MUST include "jmap".
		http.Error(w, `the "jmap" subprotocol is required`, http.StatusBadRequest)
		return nil, nil
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection cannot be hijacked", http.StatusInternalServerError)
		return nil, nil
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		http.Error(w, "hijack failed", http.StatusInternalServerError)
		return nil, nil
	}

	// The server's opening handshake (RFC 6455 section 4.2.2 step 5),
	// with the echoed subprotocol RFC 8887 section 4.2 requires.
	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\n"+
		"Sec-WebSocket-Protocol: %s\r\n\r\n", acceptKey(key), subprotocol)
	if err := brw.Flush(); err != nil {
		conn.Close()
		return nil, nil
	}
	return conn, brw
}
