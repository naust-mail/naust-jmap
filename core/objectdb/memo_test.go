package objectdb

import (
	"context"
	"errors"
	"testing"
)

// TestMemoComputesOncePerUpdate covers the Memo contract: one compute per
// key per Update, errors never cached, keys independent, and no value
// carried across Updates.
func TestMemoComputesOncePerUpdate(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	calls := 0
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		for i := 0; i < 3; i++ {
			v, err := Memo(u, "test/answer", func() (int, error) {
				calls++
				return 42, nil
			})
			if err != nil {
				return err
			}
			if v != 42 {
				t.Fatalf("call %d: got %d, want 42", i, v)
			}
		}
		// A different key computes independently.
		v, err := Memo(u, "test/other", func() (string, error) { return "x", nil })
		if err != nil || v != "x" {
			t.Fatalf("other key: got %q, %v", v, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("compute ran %d times in one Update, want 1", calls)
	}

	// A fresh Update starts with an empty cache.
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		_, err := Memo(u, "test/answer", func() (int, error) {
			calls++
			return 42, nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("compute ran %d times across two Updates, want 2", calls)
	}
}

func TestMemoDoesNotCacheErrors(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	fail := errors.New("transient")
	calls := 0
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		_, err := Memo(u, "test/flaky", func() (int, error) {
			calls++
			return 0, fail
		})
		if !errors.Is(err, fail) {
			t.Fatalf("first call: got %v, want the compute error", err)
		}
		v, err := Memo(u, "test/flaky", func() (int, error) {
			calls++
			return 7, nil
		})
		if err != nil || v != 7 {
			t.Fatalf("second call: got %d, %v", v, err)
		}
		// The successful value is now cached.
		v, err = Memo(u, "test/flaky", func() (int, error) {
			calls++
			return 0, fail
		})
		if err != nil || v != 7 {
			t.Fatalf("third call: got %d, %v, want cached 7", v, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("compute ran %d times, want 2 (error retried, success cached)", calls)
	}
}
