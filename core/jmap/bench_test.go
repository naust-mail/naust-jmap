package jmap

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// realisticRequest builds a JMAP request body shaped like a real client's
// initial sync: Email/query followed by Email/get with a back-reference
// (RFC 8620 section 3.7), and nCalls total method calls in the batch.
func realisticRequest(nCalls int) []byte {
	var b strings.Builder
	b.WriteString(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[`)
	for i := 0; i < nCalls; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `["Email/get",{"accountId":"a1","ids":["e%d","e%d","e%d"],`+
			`"properties":["id","subject","from","to","receivedAt","preview","hasAttachment","keywords"]},"c%d"]`,
			i, i+1, i+2, i)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// realisticEmailObject is one Email object as an Email/get response would
// carry it (RFC 8621 4.1): the properties a typical client asks for, already
// the shape a Resolve call produces per record.
func realisticEmailObject(i int) string {
	return fmt.Sprintf(`{"id":"e%d","subject":"Re: quarterly numbers","from":[{"name":"Alice Smith","email":"alice@example.com"}],`+
		`"to":[{"name":"Bob Jones","email":"bob@example.com"}],"receivedAt":"2026-07-21T12:00:00Z",`+
		`"preview":"Thanks for sending these over, I had a couple of questions about the totals in section three...",`+
		`"hasAttachment":false,"keywords":{"$seen":true},"mailboxIds":{"m1":true},"size":4821}`, i)
}

// realisticResponse builds a JMAP response shaped like the server's actual
// Email/get reply (server.go's json.NewEncoder(w).Encode(resp)): nCalls
// method responses, each carrying a page of Email objects as the args
// already-serialized json.RawMessage (as reply() in core/runtime produces).
func realisticResponse(nCalls, perCallEmails int) *Response {
	resps := make([]Invocation, nCalls)
	for c := 0; c < nCalls; c++ {
		var b strings.Builder
		b.WriteString(`{"accountId":"a1","state":"1","list":[`)
		for i := 0; i < perCallEmails; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(realisticEmailObject(i))
		}
		b.WriteString(`],"notFound":[]}`)
		resps[c] = Invocation{Name: "Email/get", Args: json.RawMessage(b.String()), CallID: fmt.Sprintf("c%d", c)}
	}
	return &Response{MethodResponses: resps, SessionState: "s1"}
}

func BenchmarkResponseMarshal(b *testing.B) {
	resp := realisticResponse(4, 50)
	out, err := json.Marshal(resp)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(out)))
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(resp); err != nil {
			b.Fatal(err)
		}
	}
}

// discard is an io.Writer that only counts bytes, so WriteJSON's actual
// write cost is measured without a real socket/buffer's overhead mixed in.
type discard struct{ n int }

func (d *discard) Write(p []byte) (int, error) { d.n += len(p); return len(p), nil }

// BenchmarkWriteJSON is server.go's actual per-response call
// (resp.WriteJSON(w)), pooled buffer included - what a running server
// experiences, not the isolated AppendJSON cost measured elsewhere.
func BenchmarkWriteJSON(b *testing.B) {
	resp := realisticResponse(4, 50)
	var d discard
	if err := resp.WriteJSON(&d); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(d.n))
	for i := 0; i < b.N; i++ {
		if err := resp.WriteJSON(&d); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteJSONParallel is the same call under concurrent load - what
// actually exercises whether responseBufPool helps or contends, since a
// sync.Pool's per-P local cache is the thing a single-goroutine benchmark
// cannot show.
func BenchmarkWriteJSONParallel(b *testing.B) {
	resp := realisticResponse(4, 50)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var d discard
		for pb.Next() {
			if err := resp.WriteJSON(&d); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkCheckIJSON(b *testing.B) {
	body := realisticRequest(16)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if err := CheckIJSON(body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseRequest(b *testing.B) {
	body := realisticRequest(16)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if _, err := ParseRequest(body); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRequestValidateAndParse mirrors the server's actual per-request
// work: CheckIJSON then ParseRequest over the same bytes (server.go).
func BenchmarkRequestValidateAndParse(b *testing.B) {
	body := realisticRequest(16)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if err := CheckIJSON(body); err != nil {
			b.Fatal(err)
		}
		if _, err := ParseRequest(body); err != nil {
			b.Fatal(err)
		}
	}
}
