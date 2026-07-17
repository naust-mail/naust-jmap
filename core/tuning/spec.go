package tuning

import "time"

// This file holds the floors RFC 8620 fixes, which Validate checks the
// tunables against. They are facts of the spec, not tunables: they are
// unexported and must never change. Each warning Validate emits states the
// floor it was checked against, so an operator never needs these values
// programmatically.

// specBlobMinUnreferencedAge is the floor RFC 8620 section 6 fixes: an
// unreferenced blob MUST NOT be deleted for at least one hour from upload
// (except under quota pressure, which this runtime does not implement).
// BlobMinUnreferencedAge is the tunable checked against it.
const specBlobMinUnreferencedAge = time.Hour

// specMinEventSourcePingInterval is the floor RFC 8620 section 7.3 puts on the
// ping ceiling a server advertises: the maximum allowed ping value MUST NOT be
// under 300 seconds. EventSourceMaxPingInterval is the tunable checked against
// it.
const specMinEventSourcePingInterval uint64 = 300

// specMinPushSubscriptionMaxLifetime is the floor RFC 8620 section 7.2 fixes:
// the maximum push subscription expiry a server sets MUST be at least 48 hours
// in the future (7 days is recommended). PushSubscriptionMaxLifetime is the
// tunable checked against it.
const specMinPushSubscriptionMaxLifetime = 48 * time.Hour
