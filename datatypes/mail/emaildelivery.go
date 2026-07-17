package mail

// EmailDelivery (RFC 8621 section 1.5) is a push-only pseudo-type. It has
// no methods and holds no records; it exists solely as part of the push
// mechanism (RFC 8620 section 7). Its state string MUST change whenever a
// new Email is added to the store and SHOULD NOT change on any other Email
// change (marking read, deleting). A client in a battery-constrained
// environment subscribes to EmailDelivery rather than Email so it is woken
// only for new mail (section 1.5.1).
//
// insertEmail - the one path every new Email takes - advances this state
// with Update.BumpState; the Email/set metadata and destroy hooks never do,
// which is exactly the 1.5 "on add, not on other change" contract.

import "github.com/naust-mail/naust-jmap/core/descriptor"

// TypeEmailDelivery is the push-only EmailDelivery type name (section 1.5).
const TypeEmailDelivery = "EmailDelivery"

// EmailDeliveryType is the method-less descriptor for EmailDelivery.
// Registered in the object database (so TypeNames/TypeState know it, and an
// EventSource or PushSubscription can request it) but never given a method
// extension: "There are no methods to act on this type" (section 1.5).
func EmailDeliveryType() *descriptor.Type {
	return &descriptor.Type{Name: TypeEmailDelivery, Capability: CapabilityURI}
}
