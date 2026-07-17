// Package pushsub persists PushSubscription records (RFC 8620 section
// 7.2). Subscriptions are tied to the CREDENTIAL that created them,
// not to an account (7.2.1: /get and /set take no accountId), so
// records live in their own key namespace partitioned by a credential
// key rather than in any account's range. The runtime owns all
// protocol behaviour - verification, delivery, property rules; this
// package is storage only.
//
// Key layout, sharing a backend with objectdb and the KV blob store:
//
//	!P {credential} {id}    subscription record (JSON)
//
// The leading segment "!P" contains a character outside the jmap.Id
// alphabet, so it can never collide with an account's key range.
// Writes take a per-credential lease under the synthetic partition key
// "!pushsub/{credential}" - same consistency contract as accounts:
// atomic fenced batches, serialized per partition.
package pushsub

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/naust-mail/naust-jmap/core/internal/keyenc"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// Subscription is one stored push subscription. Wire-visible
// properties follow RFC 8620 section 7.2; the rest is what the server
// needs to verify and deliver.
type Subscription struct {
	Id             jmap.Id        `json:"id"`
	DeviceClientId string         `json:"deviceClientId"`
	URL            string         `json:"url"`
	Keys           *jmap.PushKeys `json:"keys"`
	// ExpectedCode is the server-generated verification code sent in
	// the PushVerification push (7.2.2).
	ExpectedCode string `json:"expectedCode"`
	// VerificationCode is the value the client set; empty until the
	// client verifies. The server makes no delivery requests before.
	VerificationCode string `json:"verificationCode"`
	// Expires is a UTCDate; the server never pushes past it. Always set
	// here: the store's callers apply the section 7.2 defaulting rules.
	Expires string `json:"expires"`
	// Types is the list of type names to push changes for; nil means
	// all types.
	Types []string `json:"types"`
	// Credential identifies the credentials that created the
	// subscription; /get and /set only ever see records with the
	// caller's credential, and revocation destroys them (7.2).
	Credential string `json:"credential"`
	// Accounts are the delivery targets: the accounts the creating
	// identity had access to (section 7.1 pushes changes for each
	// account the user can reach).
	Accounts []jmap.Id `json:"accounts"`
}

// Verified reports whether the client has confirmed the verification
// code, allowing delivery (7.2.2).
func (s *Subscription) Verified() bool { return s.VerificationCode != "" }

// Expired reports whether the subscription's expiry has passed.
func (s *Subscription) Expired(now time.Time) bool {
	t, err := time.Parse(time.RFC3339, s.Expires)
	return err != nil || !now.Before(t)
}

var (
	// ErrNotFound is returned for an id the credential does not own.
	ErrNotFound = errors.New("pushsub: no such subscription")
	// ErrTooMany is returned by Create at the per-credential cap
	// (RFC 8620 section 8.6 requires such a limit).
	ErrTooMany = errors.New("pushsub: too many subscriptions for this credential")
)

// Store persists subscriptions in a backend.Backend.
type Store struct {
	be     backend.Backend
	leases lease.Manager
	// MaxPerCredential caps live (unexpired) subscriptions per
	// credential; zero means tuning.MaxPushSubscriptionsPerCredential.
	MaxPerCredential int
}

// NewStore wraps a backend and its lease manager; both may be shared
// with objectdb.
func NewStore(be backend.Backend, leases lease.Manager) *Store {
	return &Store{be: be, leases: leases}
}

func subKey(credential string, id jmap.Id) []byte {
	return keyenc.Key([]byte("!P"), []byte(credential), []byte(id))
}

func partition(credential string) jmap.Id {
	return jmap.Id("!pushsub/" + credential)
}

// Create stores a new subscription for its credential. Expired records
// are purged in the same commit - they no longer count toward the cap
// and the server was never allowed to push to them again anyway.
func (st *Store) Create(ctx context.Context, sub *Subscription) error {
	l, err := st.leases.Acquire(ctx, partition(sub.Credential))
	if err != nil {
		return err
	}
	defer l.Release()

	var b backend.Batch
	live := 0
	now := time.Now().UTC()
	err = st.scan(ctx, sub.Credential, func(key []byte, existing *Subscription) bool {
		if existing.Expired(now) {
			b.Delete(append([]byte(nil), key...))
		} else {
			live++
		}
		return true
	})
	if err != nil {
		return err
	}
	limit := st.MaxPerCredential
	if limit == 0 {
		limit = tuning.MaxPushSubscriptionsPerCredential
	}
	if live >= limit {
		return ErrTooMany
	}

	value, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	b.Set(subKey(sub.Credential, sub.Id), value)
	l.Fence(&b)
	return st.be.WriteBatch(ctx, &b)
}

// Update applies fn to the stored record and persists the result. An
// error from fn aborts without writing and is returned as is.
func (st *Store) Update(ctx context.Context, credential string, id jmap.Id, fn func(*Subscription) error) error {
	l, err := st.leases.Acquire(ctx, partition(credential))
	if err != nil {
		return err
	}
	defer l.Release()

	key := subKey(credential, id)
	raw, err := st.be.Get(ctx, key)
	if errors.Is(err, backend.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var sub Subscription
	if err := json.Unmarshal(raw, &sub); err != nil {
		return err
	}
	if err := fn(&sub); err != nil {
		return err
	}
	value, err := json.Marshal(&sub)
	if err != nil {
		return err
	}
	var b backend.Batch
	b.Set(key, value)
	l.Fence(&b)
	return st.be.WriteBatch(ctx, &b)
}

// Destroy removes one subscription. The URL and keys MUST be securely
// erased when a subscription is destroyed (7.2); deleting the record
// is the whole of what the storage layer can do about that.
func (st *Store) Destroy(ctx context.Context, credential string, id jmap.Id) error {
	l, err := st.leases.Acquire(ctx, partition(credential))
	if err != nil {
		return err
	}
	defer l.Release()

	key := subKey(credential, id)
	if _, err := st.be.Get(ctx, key); errors.Is(err, backend.ErrNotFound) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var b backend.Batch
	b.Delete(key)
	l.Fence(&b)
	return st.be.WriteBatch(ctx, &b)
}

// DestroyAll removes every subscription of a credential. Embedders
// MUST call this when the credential expires or is revoked (7.2 ties
// each subscription to the credentials that created it).
func (st *Store) DestroyAll(ctx context.Context, credential string) error {
	l, err := st.leases.Acquire(ctx, partition(credential))
	if err != nil {
		return err
	}
	defer l.Release()

	var b backend.Batch
	err = st.scan(ctx, credential, func(key []byte, _ *Subscription) bool {
		b.Delete(append([]byte(nil), key...))
		return true
	})
	if err != nil || len(b.Ops) == 0 {
		return err
	}
	l.Fence(&b)
	return st.be.WriteBatch(ctx, &b)
}

// Get returns one subscription of a credential, or ErrNotFound.
func (st *Store) Get(ctx context.Context, credential string, id jmap.Id) (*Subscription, error) {
	raw, err := st.be.Get(ctx, subKey(credential, id))
	if errors.Is(err, backend.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var sub Subscription
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// List returns a credential's subscriptions, expired ones included
// (destroying those is a MAY the store leaves to Create's purge).
func (st *Store) List(ctx context.Context, credential string) ([]*Subscription, error) {
	var subs []*Subscription
	err := st.scan(ctx, credential, func(_ []byte, sub *Subscription) bool {
		subs = append(subs, sub)
		return true
	})
	return subs, err
}

// All returns every stored subscription across credentials; the
// runtime uses it to restart delivery watchers after a process start.
func (st *Store) All(ctx context.Context) ([]*Subscription, error) {
	start, end := keyenc.PrefixRange([]byte("!P"))
	var subs []*Subscription
	var bad error
	err := st.be.Scan(ctx, start, end, false, func(_, value []byte) bool {
		var sub Subscription
		if err := json.Unmarshal(value, &sub); err != nil {
			bad = err
			return false
		}
		subs = append(subs, &sub)
		return true
	})
	if err == nil {
		err = bad
	}
	return subs, err
}

func (st *Store) scan(ctx context.Context, credential string, fn func(key []byte, sub *Subscription) bool) error {
	start, end := keyenc.PrefixRange([]byte("!P"), []byte(credential))
	var bad error
	err := st.be.Scan(ctx, start, end, false, func(key, value []byte) bool {
		var sub Subscription
		if err := json.Unmarshal(value, &sub); err != nil {
			bad = err
			return false
		}
		return fn(key, &sub)
	})
	if err == nil {
		err = bad
	}
	return err
}
