package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/naust-mail/naust-jmap/core/internal/authtest"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
)

func benchServer(pool, perUser int) *Server {
	return &Server{
		apiSlots:     make(chan struct{}, pool),
		userSlots:    map[string]*userSlots{},
		perUserSlots: perUser,
	}
}

// The idle case: the user holds no other slot, so every acquire creates
// the per-user semaphore and every release drops it again.
func BenchmarkSlotIdleUser(b *testing.B) {
	s := benchServer(8, 4)
	ident := &auth.Identity{Username: "alice@example.com"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		slot := s.TryAcquireSlot(ident)
		if slot == nil {
			b.Fatal("acquire failed")
		}
		slot.Release()
	}
}

// The busy case: another request of the same user is in flight for the
// whole run, so the per-user semaphore already exists and each acquire
// only bumps its refcount.
func BenchmarkSlotBusyUser(b *testing.B) {
	s := benchServer(8, 4)
	ident := &auth.Identity{Username: "alice@example.com"}
	held := s.TryAcquireSlot(ident)
	if held == nil {
		b.Fatal("acquire failed")
	}
	defer held.Release()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		slot := s.TryAcquireSlot(ident)
		if slot == nil {
			b.Fatal("acquire failed")
		}
		slot.Release()
	}
}

// The full request pipeline on a held slot: size check, I-JSON
// validation, parse, using check, and Core/echo execution. The two
// shapes bound the token density a real request can have: one call
// carrying a large argument string, and many small calls.
func BenchmarkSlotProcess(b *testing.B) {
	a := authtest.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	srv, err := NewServer(a, NewProcessor(), "https://jmap.example.com", DefaultCoreCapabilities())
	if err != nil {
		b.Fatal(err)
	}
	john := testIdentity("john@example.com")

	fill := func(size int) string {
		skeleton := fmt.Sprintf(`{"using":[%q],"methodCalls":[["Core/echo",{"filler":""},"c0"]]}`, jmap.CoreCapability)
		return fmt.Sprintf(`{"using":[%q],"methodCalls":[["Core/echo",{"filler":%q},"c0"]]}`,
			jmap.CoreCapability, strings.Repeat("a", size-len(skeleton)))
	}
	var calls strings.Builder
	fmt.Fprintf(&calls, `{"using":[%q],"methodCalls":[`, jmap.CoreCapability)
	for i := 0; i < 16; i++ {
		if i > 0 {
			calls.WriteByte(',')
		}
		fmt.Fprintf(&calls, `["Core/echo",{"a":%d,"b":"x"},"c%d"]`, i, i)
	}
	calls.WriteString(`]}`)

	for _, s := range []struct {
		name string
		body []byte
	}{
		{"echo1KB", []byte(fill(1 << 10))},
		{"echo64KB", []byte(fill(64 << 10))},
		{"calls16", []byte(calls.String())},
	} {
		b.Run(s.name, func(b *testing.B) {
			b.SetBytes(int64(len(s.body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				slot := srv.TryAcquireSlot(john)
				if slot == nil {
					b.Fatal("no slot")
				}
				resp, rerr := slot.Process(context.Background(), s.body)
				slot.Release()
				if rerr != nil || len(resp.MethodResponses) == 0 {
					b.Fatalf("process failed: %+v", rerr)
				}
			}
		})
	}
}

// Distinct users acquiring concurrently: every goroutine is in the idle
// case and all of them contend on the one userMu.
func BenchmarkSlotParallelUsers(b *testing.B) {
	s := benchServer(1024, 4)
	var n atomic.Int32
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		id := fmt.Sprintf("user%d@example.com", n.Add(1))
		ident := &auth.Identity{Username: id}
		for pb.Next() {
			slot := s.TryAcquireSlot(ident)
			if slot == nil {
				b.Fatal("acquire failed")
			}
			slot.Release()
		}
	})
}
