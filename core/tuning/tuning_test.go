package tuning

import (
	"strings"
	"testing"
	"time"
)

// assertWarns fails unless Validate returns a warning mentioning name.
func assertWarns(t *testing.T, name string) {
	t.Helper()
	for _, msg := range Validate() {
		if strings.Contains(msg, name) {
			return
		}
	}
	t.Fatalf("expected a warning mentioning %q, got %v", name, Validate())
}

// TestValidateDefaultsSilent asserts the shipped defaults sit at or above every
// spec floor and internal invariant, so a stock deployment logs nothing.
func TestValidateDefaultsSilent(t *testing.T) {
	if w := Validate(); len(w) != 0 {
		t.Fatalf("shipped defaults produced warnings: %v", w)
	}
}

// TestValidateAtFloorSilent asserts a value set exactly to its spec floor is
// accepted: RFC 8620 states each as a minimum ("at least"), so the floor
// itself is compliant and only a value strictly below it warns.
func TestValidateAtFloorSilent(t *testing.T) {
	defer func(a time.Duration, b uint64, c time.Duration) {
		BlobMinUnreferencedAge, EventSourceMaxPingInterval, PushSubscriptionMaxLifetime = a, b, c
	}(BlobMinUnreferencedAge, EventSourceMaxPingInterval, PushSubscriptionMaxLifetime)

	BlobMinUnreferencedAge = specBlobMinUnreferencedAge
	EventSourceMaxPingInterval = specMinEventSourcePingInterval
	PushSubscriptionMaxLifetime = specMinPushSubscriptionMaxLifetime
	if w := Validate(); len(w) != 0 {
		t.Fatalf("values exactly at their floor produced warnings: %v", w)
	}
}

// TestValidateBlobAgeBelowFloor covers the RFC 8620 section 6 floor: a blob
// retention below one hour lets a sweep delete an unreferenced blob too early.
func TestValidateBlobAgeBelowFloor(t *testing.T) {
	defer func(v time.Duration) { BlobMinUnreferencedAge = v }(BlobMinUnreferencedAge)
	BlobMinUnreferencedAge = specBlobMinUnreferencedAge - time.Minute
	assertWarns(t, "BlobMinUnreferencedAge")
}

// TestValidatePingBelowFloor covers the RFC 8620 section 7.3 floor: a maximum
// ping interval below 300 seconds is a ceiling the spec forbids advertising.
func TestValidatePingBelowFloor(t *testing.T) {
	defer func(v uint64) { EventSourceMaxPingInterval = v }(EventSourceMaxPingInterval)
	EventSourceMaxPingInterval = specMinEventSourcePingInterval - 1
	assertWarns(t, "EventSourceMaxPingInterval")
}

// TestValidatePushLifetimeBelowFloor covers the RFC 8620 section 7.2 floor: a
// maximum push subscription expiry under 48 hours is shorter than permitted.
func TestValidatePushLifetimeBelowFloor(t *testing.T) {
	defer func(v time.Duration) { PushSubscriptionMaxLifetime = v }(PushSubscriptionMaxLifetime)
	PushSubscriptionMaxLifetime = specMinPushSubscriptionMaxLifetime - time.Hour
	assertWarns(t, "PushSubscriptionMaxLifetime")
}

// TestValidateReclaimNotAboveRefresh covers the internal invariant: the reclaim
// window must exceed the refresh interval, or an actively writing upload could
// be reclaimed between two marker refreshes.
func TestValidateReclaimNotAboveRefresh(t *testing.T) {
	defer func(w, r time.Duration) {
		UploadReclaimWindow, UploadRefreshInterval = w, r
	}(UploadReclaimWindow, UploadRefreshInterval)

	// Equal is already a violation (not strictly greater).
	UploadReclaimWindow = UploadRefreshInterval
	assertWarns(t, "UploadReclaimWindow")
}
