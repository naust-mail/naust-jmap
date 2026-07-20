package objectdb

import (
	"context"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
)

// pushOnlyType is a method-less registered type: it holds no records but
// carries a state string other code (a push subscription) can watch - the
// shape RFC 8621 1.5 EmailDelivery needs.
func pushOnlyType() *descriptor.Type {
	return &descriptor.Type{Name: "PushOnly", Capability: "https://naust.email/test/notes"}
}

// captureNotifier records the last Publish so a test can assert what the
// commit fanned out (RFC 8620 section 7.1).
type captureNotifier struct {
	last jmap.TypeState
}

func (c *captureNotifier) Publish(_ context.Context, _ jmap.Id, types jmap.TypeState) {
	c.last = types
}
func (c *captureNotifier) Subscribe(context.Context, []jmap.Id) (notify.Subscription, error) {
	return nil, nil
}
func (c *captureNotifier) SubscribeAll(context.Context) (notify.Subscription, error) {
	return nil, nil
}

// TestBumpStateAdvancesOnlyThatType: a commit that both writes a record and
// bumps a push-only type moves both states, and only those two, to the
// commit sequence; the bump appears in the returned states and the notify
// Publish.
func TestBumpStateAdvancesOnlyThatType(t *testing.T) {
	db := newDB(t)
	if err := db.RegisterType(pushOnlyType()); err != nil {
		t.Fatal(err)
	}
	cn := &captureNotifier{}
	db.SetNotifier(cn)

	states, err := db.Update(context.Background(), acct, func(u *Update) error {
		if _, err := u.Create("TestNote", note("hi", "there")); err != nil {
			return err
		}
		return u.BumpState("PushOnly")
	})
	if err != nil {
		t.Fatal(err)
	}

	if states["PushOnly"] == "" || states["PushOnly"] != states["TestNote"] {
		t.Fatalf("bumped state = %q, note state = %q; want equal non-empty", states["PushOnly"], states["TestNote"])
	}
	// The persisted state key agrees with what was returned.
	got, err := db.TypeState(context.Background(), acct, "PushOnly")
	if err != nil {
		t.Fatal(err)
	}
	if got != states["PushOnly"] {
		t.Fatalf("persisted PushOnly state %q != returned %q", got, states["PushOnly"])
	}
	if cn.last["PushOnly"] != states["PushOnly"] {
		t.Fatalf("notify Publish PushOnly = %q, want %q", cn.last["PushOnly"], states["PushOnly"])
	}
}

// TestBumpStateOnlyCommit: a commit that stages no record but bumps a state
// still commits - the state key advances and the type is published.
func TestBumpStateOnlyCommit(t *testing.T) {
	db := newDB(t)
	if err := db.RegisterType(pushOnlyType()); err != nil {
		t.Fatal(err)
	}
	before, _ := db.TypeState(context.Background(), acct, "PushOnly")

	states, err := db.Update(context.Background(), acct, func(u *Update) error {
		return u.BumpState("PushOnly")
	})
	if err != nil {
		t.Fatal(err)
	}
	if states["PushOnly"] == "" {
		t.Fatal("pure-bump commit returned no PushOnly state")
	}
	after, _ := db.TypeState(context.Background(), acct, "PushOnly")
	if after == before {
		t.Fatalf("PushOnly state did not advance: %q", after)
	}
	// TestNote was untouched, so it must not appear.
	if _, has := states["TestNote"]; has {
		t.Fatal("untouched type leaked into states")
	}
}

// TestBumpStateUnknownType: bumping an unregistered type is a hard error, so
// a typo cannot silently produce a state nobody can read.
func TestBumpStateUnknownType(t *testing.T) {
	db := newDB(t)
	_, err := db.Update(context.Background(), acct, func(u *Update) error {
		return u.BumpState("NoSuchType")
	})
	if err == nil {
		t.Fatal("bumping an unregistered type should error")
	}
}

// TestBumpStatePersistsAcrossReopen: the bumped state key is durable - a new
// DB over the same backend still sees the advanced value (no in-memory-only
// state), so a reconnecting client gets the right initial state.
func TestBumpStatePersistsAcrossReopen(t *testing.T) {
	be := memory.New()
	db := New(be, lease.NewInProcess(be))
	if err := db.RegisterType(pushOnlyType()); err != nil {
		t.Fatal(err)
	}
	states, err := db.Update(context.Background(), acct, func(u *Update) error {
		return u.BumpState("PushOnly")
	})
	if err != nil {
		t.Fatal(err)
	}

	db2 := New(be, lease.NewInProcess(be))
	if err := db2.RegisterType(pushOnlyType()); err != nil {
		t.Fatal(err)
	}
	got, err := db2.TypeState(context.Background(), acct, "PushOnly")
	if err != nil {
		t.Fatal(err)
	}
	if got != states["PushOnly"] {
		t.Fatalf("reopened PushOnly state %q != %q", got, states["PushOnly"])
	}
}
