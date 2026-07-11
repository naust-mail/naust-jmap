package objectdb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
)

// TestCommitPublishes proves the producer side of push: a successful
// commit publishes the touched types with the same state strings
// Update returns, and a commit that touches nothing publishes nothing.
func TestCommitPublishes(t *testing.T) {
	be := memory.New()
	db := New(be, lease.NewInProcess(be))
	if err := db.RegisterType(docType()); err != nil {
		t.Fatal(err)
	}
	n := notify.NewInProcess()
	db.SetNotifier(n)
	ctx := context.Background()
	sub, err := n.Subscribe(ctx, []jmap.Id{acct})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	states, err := db.Update(ctx, acct, func(u *Update) error {
		_, err := u.Create("TestDoc", Object{"title": []byte(`"hello"`)})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	got, err := sub.Wait(waitCtx)
	if err != nil {
		t.Fatal(err)
	}
	if got[acct]["TestDoc"] != states["TestDoc"] || states["TestDoc"] == "" {
		t.Fatalf("published %v, Update returned %v", got, states)
	}

	// An Update that stages nothing commits nothing and publishes
	// nothing.
	if _, err := db.Update(ctx, acct, func(u *Update) error { return nil }); err != nil {
		t.Fatal(err)
	}
	quietCtx, cancelQuiet := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelQuiet()
	if extra, err := sub.Wait(quietCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no-op Update published %v, %v", extra, err)
	}
}
