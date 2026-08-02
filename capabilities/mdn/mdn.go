// Package mdn implements the JMAP MDN capability (RFC 9007): creating
// and sending Message Disposition Notifications (RFC 8098) for received
// messages with MDN/send, and reading received MDNs with MDN/parse.
//
// The package is a capability plugin over the mail module's public
// surface: MDNs are assembled and recognized by datatypes/mail/report,
// sent through the submission queue (datatypes/mail/submit), and
// correlated back to Emails through the submission Message-ID index.
// Wire the capability by registering the methods on the processor and
// advertising the capability's empty object (RFC 9007 section 1.3) on
// the server:
//
//	err := mdn.Register(proc, mdn.Config{DB: db, Store: blobs, Core: core, Queue: queue})
//	...
//	err = srv.Capability(mdn.CapabilityURI).Advertise(struct{}{}, struct{}{}).Err()
package mdn

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/datatypes/mail/submit"
)

// CapabilityURI identifies the MDN capability (RFC 9007 section 1.3).
// Its value in both the session capabilities object and each account's
// accountCapabilities is an empty object.
const CapabilityURI = "urn:ietf:params:jmap:mdn"

// issuedAtProperty is the internal Email property this package uses as
// the server's record that an MDN has been issued for a message: RFC
// 8098 sections 2.1 and 4 require one MDN per message at most, and the
// $mdnsent keyword alone cannot carry that guarantee because clients
// may clear it (RFC 8621 gives keywords no protection). The property
// stores the UTCDate of the send; it is invisible on the wire and
// writable only through objectdb's internal-write path, so a client
// cannot forge or erase it. The record is per Email record: a message
// duplicated into a second Email (Email/copy, a second delivery)
// carries its own marker, so the guarantee is per stored copy, not per
// Message-ID.
const issuedAtProperty = "mdnIssuedAt"

// EmailInternalProperties declares the internal Email properties this
// package needs. The embedder passes the result through
// EmailConfig.InternalProperties when registering the mail module;
// Register verifies at startup that this happened.
func EmailInternalProperties() map[string]descriptor.Property {
	return map[string]descriptor.Property{
		issuedAtProperty: {Kind: descriptor.KindDate, Nullable: true},
	}
}

// Config configures Register.
type Config struct {
	// DB is the object database the Email and EmailSubmission records
	// live in. Required.
	DB *objectdb.DB
	// Store is where message blobs live; MDN/parse reads the blobs it is
	// asked to parse from it, and MDN/send reads the original message
	// when includeOriginalMessage asks for the third report part.
	// Required.
	Store blob.Store
	// Core is the RFC 8620 capability object whose limits bound the
	// methods (MDN/parse caps blobIds at maxObjectsInGet).
	Core jmap.CoreCapabilities
	// Queue is the submission queue MDNs are sent through; it also
	// answers the Original-Message-ID correlation MDN/parse uses to
	// resolve forEmailId (RFC 9007 section 2.2), and it carries the
	// SendPolicy re-checked before every queued MDN. The policy comes
	// from the queue's own registration, so what MDN/send enforces and
	// what EmailSubmission/set enforces cannot disagree. Required.
	Queue *submit.Queue
}

// Register registers the MDN/send and MDN/parse methods (RFC 9007
// section 2) under CapabilityURI. The mail module must already be
// registered on the same processor with EmailInternalProperties()
// passed through EmailConfig.InternalProperties: Register verifies the
// Email descriptor carries the declared internal properties and fails
// otherwise, so a missing wiring step is a startup error rather than a
// runtime surprise. Advertising the capability's empty session object
// is the embedder's step (see the package example).
func Register(p *runtime.Processor, cfg Config) error {
	var missing []string
	if cfg.DB == nil {
		missing = append(missing, "DB")
	}
	if cfg.Store == nil {
		missing = append(missing, "Store")
	}
	if cfg.Queue == nil {
		missing = append(missing, "Queue")
	}
	if len(missing) > 0 {
		return fmt.Errorf("mdn: Register: Config missing required fields: %s", strings.Join(missing, ", "))
	}
	t := cfg.DB.Type(mail.TypeEmail)
	if t == nil {
		return fmt.Errorf("mdn: Register: the Email type is not registered; call mail.RegisterEmail before mdn.Register")
	}
	for name := range EmailInternalProperties() {
		prop, ok := t.Properties[name]
		if !ok || !prop.Internal {
			return fmt.Errorf("mdn: Register: the Email descriptor lacks internal property %q; pass mdn.EmailInternalProperties() through EmailConfig.InternalProperties when calling mail.RegisterEmail", name)
		}
	}
	if cfg.Core.MaxObjectsInGet == 0 {
		slog.Warn("naust-jmap: mdn: Core.MaxObjectsInGet is 0; every non-empty MDN/parse will be rejected")
	}
	p.Register("MDN/send", CapabilityURI, mdnSend{db: cfg.DB, store: cfg.Store, core: cfg.Core, queue: cfg.Queue, proc: p, policy: cfg.Queue.Policy()}.Handle)
	p.Register("MDN/parse", CapabilityURI, mdnParse{db: cfg.DB, store: cfg.Store, core: cfg.Core, queue: cfg.Queue}.Handle)
	return nil
}
