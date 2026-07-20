package notify

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// waitFor calls Wait with a deadline so a test never hangs.
func waitFor(t *testing.T, sub Subscription, d time.Duration) (Changes, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return sub.Wait(ctx)
}

// mustTimeOut asserts nothing is pending on sub.
func mustTimeOut(t *testing.T, sub Subscription) {
	t.Helper()
	if got, err := waitFor(t, sub, 20*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, %v; want deadline exceeded", got, err)
	}
}

func TestPublishSubscribeCoalesce(t *testing.T) {
	n := NewInProcess()
	ctx := context.Background()
	sub, err := n.Subscribe(ctx, []jmap.Id{"Aone"})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	mustTimeOut(t, sub)

	// Several commits between Waits merge into one Changes with the
	// newest state per type (the section 7 coalescing shape).
	n.Publish(ctx, "Aone", jmap.TypeState{"Foo": "1"})
	n.Publish(ctx, "Aone", jmap.TypeState{"Bar": "2"})
	n.Publish(ctx, "Aone", jmap.TypeState{"Foo": "3"})
	got, err := waitFor(t, sub, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := Changes{"Aone": {"Foo": "3", "Bar": "2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Wait = %v, want %v", got, want)
	}

	// Wait cleared the pending set.
	mustTimeOut(t, sub)
}

func TestAccountIsolationAndFanout(t *testing.T) {
	n := NewInProcess()
	ctx := context.Background()
	one, _ := n.Subscribe(ctx, []jmap.Id{"Aone"})
	defer one.Close()
	both, _ := n.Subscribe(ctx, []jmap.Id{"Aone", "Atwo"})
	defer both.Close()

	n.Publish(ctx, "Atwo", jmap.TypeState{"Foo": "5"})
	mustTimeOut(t, one)
	got, err := waitFor(t, both, time.Second)
	if err != nil || !reflect.DeepEqual(got, Changes{"Atwo": {"Foo": "5"}}) {
		t.Fatalf("both: %v, %v", got, err)
	}

	// One publish reaches every interested subscriber.
	n.Publish(ctx, "Aone", jmap.TypeState{"Foo": "6"})
	for _, sub := range []Subscription{one, both} {
		got, err := waitFor(t, sub, time.Second)
		if err != nil || !reflect.DeepEqual(got, Changes{"Aone": {"Foo": "6"}}) {
			t.Fatalf("fanout: %v, %v", got, err)
		}
	}
}

func TestPublishReleasesBlockedWait(t *testing.T) {
	n := NewInProcess()
	ctx := context.Background()
	sub, _ := n.Subscribe(ctx, []jmap.Id{"Aone"})
	defer sub.Close()

	type result struct {
		ch  Changes
		err error
	}
	done := make(chan result, 1)
	go func() {
		ch, err := sub.Wait(context.Background())
		done <- result{ch, err}
	}()
	time.Sleep(10 * time.Millisecond)
	n.Publish(ctx, "Aone", jmap.TypeState{"Foo": "1"})
	select {
	case r := <-done:
		if r.err != nil || !reflect.DeepEqual(r.ch, Changes{"Aone": {"Foo": "1"}}) {
			t.Fatalf("Wait = %v, %v", r.ch, r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("publish did not release the blocked Wait")
	}
}

func TestClose(t *testing.T) {
	n := NewInProcess()
	ctx := context.Background()
	sub, _ := n.Subscribe(ctx, []jmap.Id{"Aone"})
	other, _ := n.Subscribe(ctx, []jmap.Id{"Aone"})
	defer other.Close()

	// Close releases a blocked Wait with ErrClosed.
	errs := make(chan error, 1)
	go func() {
		_, err := sub.Wait(context.Background())
		errs <- err
	}()
	time.Sleep(10 * time.Millisecond)
	sub.Close()
	select {
	case err := <-errs:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("blocked Wait after Close = %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not release the blocked Wait")
	}
	if _, err := sub.Wait(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Wait after Close = %v, want ErrClosed", err)
	}
	sub.Close() // idempotent

	// Publishing after a close still reaches live subscribers.
	n.Publish(ctx, "Aone", jmap.TypeState{"Foo": "1"})
	if got, err := waitFor(t, other, time.Second); err != nil || len(got) != 1 {
		t.Fatalf("live subscriber after peer close: %v, %v", got, err)
	}
}

// TestFirehoseSubscription: SubscribeAll receives every account's
// changes (what a queue worker uses to wake on commits from any process
// sharing the store), while Subscribe with an empty OR nil set receives
// nothing - an accidentally-empty list fails toward silence, never
// toward the firehose.
func TestFirehoseSubscription(t *testing.T) {
	n := NewInProcess()
	ctx := context.Background()
	fire, err := n.SubscribeAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fire.Close()
	none, err := n.Subscribe(ctx, []jmap.Id{})
	if err != nil {
		t.Fatal(err)
	}
	defer none.Close()
	nilNone, err := n.Subscribe(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer nilNone.Close()

	n.Publish(ctx, "Aone", jmap.TypeState{"Email": "5"})
	n.Publish(ctx, "Atwo", jmap.TypeState{"EmailSubmission": "9"})

	got, err := waitFor(t, fire, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := Changes{
		"Aone": jmap.TypeState{"Email": "5"},
		"Atwo": jmap.TypeState{"EmailSubmission": "9"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("firehose Changes = %v, want %v", got, want)
	}
	mustTimeOut(t, none)
	mustTimeOut(t, nilNone)
}
