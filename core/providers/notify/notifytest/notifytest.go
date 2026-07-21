// Package notifytest is the shared contract suite for notify.Notifier
// implementations: the single-node in-process notifier and any store-backed
// cluster variant (e.g. one riding a database publish/subscribe channel) must
// pass the identical tests, so a Notifier that passes is behaviorally
// interchangeable with every other.
//
// Every assertion is phrased as eventually-consistent. Delivery is best-effort
// and MAY be asynchronous: an in-process notifier fans out synchronously and
// satisfies each assertion on the first Wait, while a store-backed notifier
// delivers across a network round trip and needs a short window. Positive
// assertions poll Wait, accumulating what arrives (newest state per type wins,
// exactly the coalescing the push model calls for) until it matches the
// expectation or a generous deadline elapses. This mirrors the model's own
// contract (RFC 8620 section 7): dropped or duplicated push events are allowed
// because clients reconcile on state strings, so the suite asserts what MUST
// eventually be observed, never precise timing or exactly-once delivery.
package notifytest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
)

const (
	// observeBudget bounds how long a positive assertion waits for a change
	// to propagate, generous enough for a store-backed round trip.
	observeBudget = 5 * time.Second
	// observePoll is one Wait interval while accumulating changes.
	observePoll = 10 * time.Millisecond
	// silenceWindow is how long a "must stay silent" check watches before
	// concluding nothing was delivered. A negative over an asynchronous
	// transport cannot be proven absolutely; this is the pragmatic window.
	silenceWindow = 250 * time.Millisecond
)

// mergeInto folds one delivered batch into an accumulator, newest state per
// type winning - the same coalescing a subscriber sees within a single Wait.
func mergeInto(dst, src notify.Changes) {
	for acct, ts := range src {
		d := dst[acct]
		if d == nil {
			d = jmap.TypeState{}
			dst[acct] = d
		}
		for name, state := range ts {
			d[name] = state
		}
	}
}

// eventuallyChanges polls sub until the changes observed so far equal want, or
// the budget elapses. It tolerates a notifier that delivers want across several
// Waits (asynchronous transport) as well as one that delivers it all at once.
func eventuallyChanges(t *testing.T, sub notify.Subscription, want notify.Changes) {
	t.Helper()
	got := notify.Changes{}
	deadline := time.Now().Add(observeBudget)
	for {
		if reflect.DeepEqual(got, want) {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("did not observe expected changes within %v: got %v, want %v", observeBudget, got, want)
		}
		poll := observePoll
		if remaining < poll {
			poll = remaining
		}
		ctx, cancel := context.WithTimeout(context.Background(), poll)
		ch, err := sub.Wait(ctx)
		cancel()
		switch {
		case err == nil:
			mergeInto(got, ch)
		case errors.Is(err, context.DeadlineExceeded):
			// Nothing this interval; keep polling until the budget runs out.
		default:
			t.Fatalf("Wait: %v", err)
		}
	}
}

// mustStaySilent asserts nothing is delivered to sub within the silence window.
func mustStaySilent(t *testing.T, sub notify.Subscription) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), silenceWindow)
	defer cancel()
	if ch, err := sub.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected silence, observed %v (err %v)", ch, err)
	}
}

func subscribe(t *testing.T, n notify.Notifier, accounts []jmap.Id) notify.Subscription {
	t.Helper()
	sub, err := n.Subscribe(context.Background(), accounts)
	if err != nil {
		t.Fatalf("Subscribe(%v): %v", accounts, err)
	}
	return sub
}

// Run executes the full contract suite against a single notifier.
func Run(t *testing.T, newNotifier func(t *testing.T) notify.Notifier) {
	t.Run("SubscribedDelivery", func(t *testing.T) { testSubscribedDelivery(t, newNotifier) })
	t.Run("AccountIsolationAndFanout", func(t *testing.T) { testAccountIsolationAndFanout(t, newNotifier) })
	t.Run("EmptyAndNilSubscribeMatchNothing", func(t *testing.T) { testEmptyAndNilSubscribe(t, newNotifier) })
	t.Run("FirehoseStateChangeExample", func(t *testing.T) { testFirehoseStateChangeExample(t, newNotifier) })
	t.Run("Coalesce", func(t *testing.T) { testCoalesce(t, newNotifier) })
	t.Run("EmptyPublishIgnored", func(t *testing.T) { testEmptyPublishIgnored(t, newNotifier) })
	t.Run("RetainedForNextWait", func(t *testing.T) { testRetainedForNextWait(t, newNotifier) })
	t.Run("PublishReleasesBlockedWait", func(t *testing.T) { testPublishReleasesBlockedWait(t, newNotifier) })
	t.Run("CloseUnblocksWait", func(t *testing.T) { testCloseUnblocksWait(t, newNotifier) })
	t.Run("NoDeliveryAfterClose", func(t *testing.T) { testNoDeliveryAfterClose(t, newNotifier) })
}

// testSubscribedDelivery: a change to a subscribed account is delivered.
func testSubscribedDelivery(t *testing.T, newNotifier func(t *testing.T) notify.Notifier) {
	n := newNotifier(t)
	sub := subscribe(t, n, []jmap.Id{"a123"})
	defer sub.Close()

	n.Publish(context.Background(), "a123", jmap.TypeState{"Email": "s1"})
	eventuallyChanges(t, sub, notify.Changes{"a123": {"Email": "s1"}})
}

// testAccountIsolationAndFanout: a change reaches every subscriber interested
// in that account and no other. The delivery to the interested subscriber also
// confirms the publish propagated, which is what gives the silence check on the
// uninterested subscriber its meaning.
func testAccountIsolationAndFanout(t *testing.T, newNotifier func(t *testing.T) notify.Notifier) {
	n := newNotifier(t)
	ctx := context.Background()
	one := subscribe(t, n, []jmap.Id{"a1"})
	defer one.Close()
	both := subscribe(t, n, []jmap.Id{"a1", "a2"})
	defer both.Close()

	n.Publish(ctx, "a2", jmap.TypeState{"Email": "5"})
	eventuallyChanges(t, both, notify.Changes{"a2": {"Email": "5"}})
	mustStaySilent(t, one)

	n.Publish(ctx, "a1", jmap.TypeState{"Email": "6"})
	eventuallyChanges(t, one, notify.Changes{"a1": {"Email": "6"}})
	eventuallyChanges(t, both, notify.Changes{"a1": {"Email": "6"}})
}

// testEmptyAndNilSubscribe: the firehose is always explicit. Subscribe with an
// empty OR nil account set matches nothing, so an accidentally-empty list fails
// toward silence, never toward receiving every account's changes.
func testEmptyAndNilSubscribe(t *testing.T, newNotifier func(t *testing.T) notify.Notifier) {
	n := newNotifier(t)
	ctx := context.Background()
	fire, err := n.SubscribeAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fire.Close()
	none := subscribe(t, n, []jmap.Id{})
	defer none.Close()
	nilNone := subscribe(t, n, nil)
	defer nilNone.Close()

	n.Publish(ctx, "a1", jmap.TypeState{"Email": "7"})
	eventuallyChanges(t, fire, notify.Changes{"a1": {"Email": "7"}})
	mustStaySilent(t, none)
	mustStaySilent(t, nilNone)
}

// testFirehoseStateChangeExample delivers the worked StateChange "changed"-map
// example from the push specification through the firehose: each account maps
// to the new state strings of the types that changed there. Reproducing the
// specification's own account ids and state strings pins the shape a client
// reconciles against.
func testFirehoseStateChangeExample(t *testing.T, newNotifier func(t *testing.T) notify.Notifier) {
	n := newNotifier(t)
	ctx := context.Background()
	fire, err := n.SubscribeAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fire.Close()

	n.Publish(ctx, "a456", jmap.TypeState{"Mailbox": "d35ecb040aab"})
	n.Publish(ctx, "a123", jmap.TypeState{"Email": "0af7a512ce70"})
	eventuallyChanges(t, fire, notify.Changes{
		"a456": {"Mailbox": "d35ecb040aab"},
		"a123": {"Email": "0af7a512ce70"},
	})
}

// testCoalesce: several changes to one account between Waits merge into one set
// with the newest state per type winning.
func testCoalesce(t *testing.T, newNotifier func(t *testing.T) notify.Notifier) {
	n := newNotifier(t)
	ctx := context.Background()
	sub := subscribe(t, n, []jmap.Id{"a1"})
	defer sub.Close()

	n.Publish(ctx, "a1", jmap.TypeState{"Email": "1"})
	n.Publish(ctx, "a1", jmap.TypeState{"Mailbox": "2"})
	n.Publish(ctx, "a1", jmap.TypeState{"Email": "3"})
	eventuallyChanges(t, sub, notify.Changes{"a1": {"Email": "3", "Mailbox": "2"}})
}

// testEmptyPublishIgnored: a publish carrying no changed types is a no-op and
// must never wake a subscriber.
func testEmptyPublishIgnored(t *testing.T, newNotifier func(t *testing.T) notify.Notifier) {
	n := newNotifier(t)
	ctx := context.Background()
	sub := subscribe(t, n, []jmap.Id{"a1"})
	defer sub.Close()

	n.Publish(ctx, "a1", jmap.TypeState{})
	mustStaySilent(t, sub)
}

// testRetainedForNextWait: a change published while no Wait is outstanding is
// retained, not lost - the next Wait returns it.
func testRetainedForNextWait(t *testing.T, newNotifier func(t *testing.T) notify.Notifier) {
	n := newNotifier(t)
	sub := subscribe(t, n, []jmap.Id{"a1"})
	defer sub.Close()

	// Nothing is blocked in Wait at publish time.
	n.Publish(context.Background(), "a1", jmap.TypeState{"Email": "9"})
	eventuallyChanges(t, sub, notify.Changes{"a1": {"Email": "9"}})
}

// testPublishReleasesBlockedWait: a change published while a Wait is already
// blocked releases that Wait with the change.
func testPublishReleasesBlockedWait(t *testing.T, newNotifier func(t *testing.T) notify.Notifier) {
	n := newNotifier(t)
	sub := subscribe(t, n, []jmap.Id{"a1"})
	defer sub.Close()

	type result struct {
		ch  notify.Changes
		err error
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), observeBudget)
		defer cancel()
		ch, err := sub.Wait(ctx)
		done <- result{ch, err}
	}()
	// Give the Wait time to block before publishing.
	time.Sleep(20 * time.Millisecond)
	n.Publish(context.Background(), "a1", jmap.TypeState{"Email": "1"})
	select {
	case r := <-done:
		if r.err != nil || !reflect.DeepEqual(r.ch, notify.Changes{"a1": {"Email": "1"}}) {
			t.Fatalf("blocked Wait released with %v (err %v)", r.ch, r.err)
		}
	case <-time.After(observeBudget):
		t.Fatal("publish did not release the blocked Wait")
	}
}

// testCloseUnblocksWait: Close releases a blocked Wait with ErrClosed, every
// later Wait keeps returning ErrClosed, and Close is idempotent.
func testCloseUnblocksWait(t *testing.T, newNotifier func(t *testing.T) notify.Notifier) {
	n := newNotifier(t)
	sub := subscribe(t, n, []jmap.Id{"a1"})

	errc := make(chan error, 1)
	go func() {
		_, err := sub.Wait(context.Background())
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	sub.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, notify.ErrClosed) {
			t.Fatalf("blocked Wait after Close = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release the blocked Wait")
	}
	if _, err := sub.Wait(context.Background()); !errors.Is(err, notify.ErrClosed) {
		t.Fatalf("Wait after Close = %v, want ErrClosed", err)
	}
	sub.Close() // idempotent
}

// testNoDeliveryAfterClose: a closed subscription never yields a later change,
// while a live peer subscribed to the same account still does.
func testNoDeliveryAfterClose(t *testing.T, newNotifier func(t *testing.T) notify.Notifier) {
	n := newNotifier(t)
	ctx := context.Background()
	sub := subscribe(t, n, []jmap.Id{"a1"})
	other := subscribe(t, n, []jmap.Id{"a1"})
	defer other.Close()

	sub.Close()
	n.Publish(ctx, "a1", jmap.TypeState{"Email": "1"})
	eventuallyChanges(t, other, notify.Changes{"a1": {"Email": "1"}})
	if _, err := sub.Wait(context.Background()); !errors.Is(err, notify.ErrClosed) {
		t.Fatalf("closed subscription Wait = %v, want ErrClosed", err)
	}
}

// RunLinked executes the cross-instance contract: a change published on one
// notifier is observed by a subscriber on the other, in both directions. For a
// single-node notifier the pair may be the same instance, which still exercises
// the delivery path.
func RunLinked(t *testing.T, newPair func(t *testing.T) (a, b notify.Notifier)) {
	t.Run("CrossInstanceBothDirections", func(t *testing.T) {
		a, b := newPair(t)
		ctx := context.Background()

		subB := subscribe(t, b, []jmap.Id{"a1"})
		defer subB.Close()
		a.Publish(ctx, "a1", jmap.TypeState{"Email": "1"})
		eventuallyChanges(t, subB, notify.Changes{"a1": {"Email": "1"}})

		subA := subscribe(t, a, []jmap.Id{"a2"})
		defer subA.Close()
		b.Publish(ctx, "a2", jmap.TypeState{"Mailbox": "9"})
		eventuallyChanges(t, subA, notify.Changes{"a2": {"Mailbox": "9"}})
	})
}
