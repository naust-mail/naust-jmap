package deliver

// Delivery-path benchmarks: one accepted recipient per Deliver over the
// in-memory server. The vacation variant measures the responder's cost
// on the delivery path in its steady state - enabled, with the sender
// already inside the suppression period, so every delivery pays the
// pre-checks but sends nothing.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/datatypes/mail"
)

func benchMsg(i int) string {
	return bodyMsg("visitor@remote.example", "john@example.com", "hello",
		"a short line of body text", map[string]string{
			"Message-ID": fmt.Sprintf("<bench-%d@remote.example>", i),
		})
}

func benchDeliver(b *testing.B, d *Deliverer) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		evs := d.Deliver(context.Background(),
			deliveryEnv("visitor@remote.example", "john@example.com"),
			strings.NewReader(benchMsg(i)))
		if evs[0].Outcome != mail.Accepted {
			b.Fatalf("delivery: %+v", evs[0])
		}
	}
}

func BenchmarkDeliver(b *testing.B) {
	ts, db, store := emailServer(b)
	createMailbox(b, ts, `{"name":"Inbox","role":"inbox"}`)
	d := mustDeliverer(b, db, store, mapResolver{"john@example.com": testAccount})
	benchDeliver(b, d)
}

func BenchmarkDeliverVacationSuppressed(b *testing.B) {
	ts, db, store, q, _, _ := newEmailServer(b, mail.DefaultAccountCapability())
	createMailbox(b, ts, `{"name":"Inbox","role":"inbox"}`)
	createMailbox(b, ts, `{"name":"Sent","role":"sent"}`)
	createIdentity(b, ts, "john@example.com")
	enableVacation(b, ts, `,"textBody":"away"`)
	d := mustDeliverer(b, db, store, mapResolver{"john@example.com": testAccount},
		Config{MaxMessageSize: defaultMaxMessageSize, VacationQueue: q})
	// The first delivery sends the one reply; every timed iteration is
	// then inside the sender's suppression period.
	evs := d.Deliver(context.Background(),
		deliveryEnv("visitor@remote.example", "john@example.com"),
		strings.NewReader(benchMsg(-1)))
	if evs[0].Outcome != mail.Accepted {
		b.Fatalf("prime delivery: %+v", evs[0])
	}
	benchDeliver(b, d)
}
