// Package tuning is the core module's single home for its tunable defaults:
// the knobs that trade resources against safety (piece sizes, retention
// windows, request caps) and the object-id scheme. They live here, not
// scattered across the packages that read them, so an operator can see and
// adjust them in one place and so Validate can check them against the floors
// the spec fixes.
//
// These are compiled-in defaults, not runtime configuration: the genuinely
// per-request, embedder-set limits are the JMAP session capabilities
// (maxSizeUpload, maxObjectsInSet, ...). A value here is a package variable
// only where a test needs to drive a boundary without an impractical fixture;
// changing one in production means recompiling.
//
// Spec-fixed vs tunable: everything in this file may be moved; the floors the
// spec pins some of these values to live apart in spec.go, because a spec
// floor is a fact, not a knob. Validate warns (it never blocks) when a tunable
// is set below its spec floor.
package tuning

import (
	"fmt"
	"time"
)

// IdScheme selects how objectdb assigns record ids (RFC 8620 section 1.2:
// ids are server-assigned, immutable, and drawn from the URL-safe base64
// alphabet; the spec does not require them to be random or unordered). The
// three schemes trade ordering against what an id reveals; all three emit
// ids that satisfy section 1.2's defensive-allocation guidance. The scheme
// is chosen per store with objectdb.WithIdScheme; DefaultIdScheme applies
// when the embedder does not choose.
type IdScheme int

const (
	// SchemeULID embeds the creation time (millisecond resolution) followed
	// by random bits, so ids sort by creation order and cluster by time for
	// index locality. The cost: anyone who sees an id learns when the record
	// was created.
	SchemeULID IdScheme = iota
	// SchemeSequence is a per-account counter plus a random tail: ids sort by
	// in-account creation order like a database sequence, the random tail
	// keeps them from being enumerable, and no wall-clock time is embedded.
	// The choice when id timestamps would leak more than ordering is worth.
	SchemeSequence
	// SchemeRandom is fully random with no ordering and nothing derivable
	// from the id at all.
	SchemeRandom
)

// DefaultIdScheme is the scheme a store uses when the embedder does not pass
// objectdb.WithIdScheme.
const DefaultIdScheme = SchemeULID

// String renders a scheme for logs and errors.
func (s IdScheme) String() string {
	switch s {
	case SchemeULID:
		return "ulid"
	case SchemeSequence:
		return "sequence"
	case SchemeRandom:
		return "random"
	default:
		return "unknown"
	}
}

// BlobMinUnreferencedAge is how long an unreferenced blob is kept before a
// sweep may delete it (see objectdb.SweepBlobs). RFC 8620 section 6 floors it
// at one hour (spec.go); Validate warns below that.
var BlobMinUnreferencedAge = time.Hour

// BlobPieceSize is the size of each stored piece in the chunkstore blob
// store. Larger pieces mean fewer backend writes per blob but more memory
// held per in-flight upload or download (one piece at a time). It is not part
// of the stored format - each blob records its own piece count, so a blob
// written at one size still reads back if this default later changes.
var BlobPieceSize = 4 << 20 // 4 MiB

// UploadReclaimWindow bounds how long a chunkstore run marker may go
// un-refreshed before Sweep treats the run as abandoned and reclaims its
// pieces. It MUST exceed the embedder's upload idle timeout, so a
// paused-but-live upload's connection is dropped (and its writer aborts)
// before the marker can go stale; erring long only costs dead bytes lingering
// a little before cleanup. It MUST also exceed UploadRefreshInterval, or an
// actively writing upload could be reclaimed between refreshes (Validate
// checks this).
var UploadReclaimWindow = 15 * time.Minute

// UploadRefreshInterval is how often an actively writing chunkstore upload
// re-stamps its run marker to prove the run is still live. It is well under
// UploadReclaimWindow so even a slow trickle never looks stale.
var UploadRefreshInterval = time.Minute

// DefaultMaxChanges bounds a Foo/changes response when the client omits
// maxChanges. RFC 8620 section 5.2 permits the server to choose this ("If not
// given by the client, the server may choose how many to return") and
// requires the cap be honored. Without it an omitted maxChanges would be
// unbounded: a sinceState far in the past would force the entire change log
// into one response. Keep it above the session's maxObjectsInSet (the
// single-commit ceiling) so a page of change ids never exceeds what a
// following Foo/get could request at once.
var DefaultMaxChanges = 2048

// MaxFilterNodes bounds a filter tree's total node count (operators plus
// condition leaves) in a Foo/query. CheckIJSON caps the request's JSON
// nesting depth but not a FilterOperator's breadth: a client can pack tens of
// thousands of conditions into a few megabytes, and the planner does real
// per-candidate work for each. No legitimate filter approaches this; a larger
// one is rejected as unsupportedFilter (RFC 8620 section 5.5).
var MaxFilterNodes = 1024

// MaxRequestedProperties bounds how many properties a client may name in one
// Foo/get. A type's real property set is small; the open-ended part is
// computed forms (Email's header:{name}), each resolved for every returned
// object. The cap is generous enough that no real client meets it.
var MaxRequestedProperties = 512

// EventSourceMaxPingInterval is the ceiling, in seconds, that a client's
// requested EventSource ping interval is clamped to. RFC 8620 section 7.3
// floors it at 300 (spec.go); Validate warns below that. There is no upper
// floor because section 7.3 only forbids a minimum above 30 and this server
// imposes no minimum at all.
var EventSourceMaxPingInterval uint64 = 3600

// PushSubscriptionMaxLifetime caps a push subscription's expiry: the server
// sets it when the client gives none and clamps larger client values to it.
// RFC 8620 section 7.2 floors it at 48 hours (spec.go; Validate warns below
// that) and recommends at least 7 days for non-time-bounded credentials
// (Basic auth, API tokens); this runtime has no view into credential
// lifetimes, so it always applies the non-time-bounded policy.
var PushSubscriptionMaxLifetime = 7 * 24 * time.Hour

// MaxPushSubscriptionsPerCredential is the cap on live push subscriptions per
// credential, applied by pushsub.Store when its own per-store limit is zero.
var MaxPushSubscriptionsPerCredential = 16

// PushSubscriptionCreateRateMax and PushSubscriptionCreateRateWindow bound how
// many push subscriptions one credential may create per window. RFC 8620
// section 8.6 requires a creation rate limit: every create sends an
// unsolicited POST to a client-chosen URL.
var (
	PushSubscriptionCreateRateMax    = 10
	PushSubscriptionCreateRateWindow = time.Hour
)

// PushDeliveryTTL is the RFC 8030 section 5.2 TTL header value, in seconds,
// sent on every push POST. A StateChange stays current until the next one
// replaces it, so a long retention is harmless.
var PushDeliveryTTL = 43200

// PushDelivery429Backoff is how long push delivery pauses after a 429
// response. RFC 8620 section 7.2 requires reducing push frequency; pending
// changes coalesce into one minimal StateChange while paused.
var PushDelivery429Backoff = time.Minute

// Validate reports every tunable currently set below a floor the spec fixes or
// below an internal invariant the module relies on, as human-readable warnings.
// It never returns an error and a caller never blocks on it: a deployment that
// deliberately runs below a floor still starts. The runtime logs the warnings
// at startup so an operator sees the consequence of a value they moved.
func Validate() []string {
	var warnings []string
	if BlobMinUnreferencedAge < specBlobMinUnreferencedAge {
		warnings = append(warnings, fmt.Sprintf(
			"BlobMinUnreferencedAge (%s) is below the RFC 8620 section 6 floor of %s: "+
				"an unreferenced blob may be swept before a client can reference it",
			BlobMinUnreferencedAge, specBlobMinUnreferencedAge))
	}
	if EventSourceMaxPingInterval < specMinEventSourcePingInterval {
		warnings = append(warnings, fmt.Sprintf(
			"EventSourceMaxPingInterval (%d) is below the RFC 8620 section 7.3 floor of %d: "+
				"the server would advertise a maximum ping interval the spec forbids",
			EventSourceMaxPingInterval, specMinEventSourcePingInterval))
	}
	if PushSubscriptionMaxLifetime < specMinPushSubscriptionMaxLifetime {
		warnings = append(warnings, fmt.Sprintf(
			"PushSubscriptionMaxLifetime (%s) is below the RFC 8620 section 7.2 floor of %s: "+
				"push subscriptions would expire sooner than the spec permits",
			PushSubscriptionMaxLifetime, specMinPushSubscriptionMaxLifetime))
	}
	if UploadReclaimWindow <= UploadRefreshInterval {
		warnings = append(warnings, fmt.Sprintf(
			"UploadReclaimWindow (%s) is not greater than UploadRefreshInterval (%s): "+
				"an actively writing upload could be reclaimed between marker refreshes",
			UploadReclaimWindow, UploadRefreshInterval))
	}
	return warnings
}
