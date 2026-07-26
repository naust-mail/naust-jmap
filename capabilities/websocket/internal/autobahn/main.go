// Command autobahn is a bare RFC 6455 echo server over the frame
// codec, for exercising it with the Autobahn testsuite's
// fuzzingclient. The suite speaks raw WebSocket echo, so this harness
// deliberately skips everything JMAP: no authentication, no "jmap"
// subprotocol, binary messages echoed instead of refused. It tests
// the codec layer only, never the production handler.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/naust-mail/naust-jmap/capabilities/websocket/internal/frame"
)

// The suite's mass echo cases send messages up to 16 MiB, far above
// the production maxSizeRequest; the caps here bound the harness, not
// the codec under test.
const (
	maxMessage   = 64 << 20
	maxFragments = 1 << 20
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9001", "listen address")
	flag.Parse()
	log.Printf("echo server on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(serve)))
}

// serve performs a minimal section 4.2.2 handshake: any subprotocol
// request is ignored (none is agreed to), only the key is required.
func serve(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if r.Method != http.MethodGet || key == "" {
		http.Error(w, "not a websocket handshake", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return
	}
	nc, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer nc.Close()

	// Accept key per section 4.2.2 step 5.4: SHA-1 of the client key
	// concatenated with the fixed GUID, base64-encoded.
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	fmt.Fprintf(nc, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\n\r\n",
		base64.StdEncoding.EncodeToString(sum[:]))

	echo(nc, frame.NewReader(brw.Reader, maxMessage, maxFragments))
}

func echo(nc net.Conn, rd *frame.Reader) {
	for {
		msg, err := rd.Next()
		if err != nil {
			// A protocol error fails the connection with its close code
			// (section 7.1.7); a stream error just drops the socket.
			var pe *frame.ProtocolError
			if errors.As(err, &pe) {
				frame.WriteClose(nc, pe.Code, pe.Reason)
			}
			return
		}
		switch msg.Opcode {
		case frame.OpText, frame.OpBinary:
			if err := frame.WriteMessage(nc, msg.Opcode, msg.Payload); err != nil {
				return
			}
		case frame.OpPing:
			// A Ping is answered with a Pong carrying the same payload
			// (section 5.5.3).
			if err := frame.WriteMessage(nc, frame.OpPong, msg.Payload); err != nil {
				return
			}
		case frame.OpPong:
			// Unsolicited Pongs are permitted and ignored (section 5.5.3).
		case frame.OpClose:
			// Echo the close code, or 1000 when none was given, then
			// close the TCP connection (sections 5.5.1, 7.1.1).
			code := uint16(frame.CloseNormal)
			if len(msg.Payload) >= 2 {
				code = binary.BigEndian.Uint16(msg.Payload[:2])
			}
			frame.WriteClose(nc, code, "")
			return
		}
	}
}
