package mail

// Tests for the registration configs: required-field validation names
// every missing field at once, and EmailConfig.InternalProperties merges
// extra properties into the Email descriptor as Internal ones, rejecting
// a name RFC 8621 section 4.1 already defines.

import (
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/search"
)

func emailTestDeps() (*runtime.Processor, *objectdb.DB) {
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	return runtime.NewProcessor(), db
}

func TestRegisterEmailMissingFields(t *testing.T) {
	p, _ := emailTestDeps()
	err := RegisterEmail(p, EmailConfig{})
	if err == nil {
		t.Fatal("RegisterEmail accepted an empty EmailConfig")
	}
	for _, field := range []string{"DB", "Store", "Searcher"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error %q does not name missing field %s", err, field)
		}
	}
}

func TestRegisterEmailInternalProperties(t *testing.T) {
	p, db := emailTestDeps()
	store := kvstore.New(memory.New())
	if err := RegisterThread(p, ThreadConfig{DB: db, Core: runtime.DefaultCoreCapabilities()}); err != nil {
		t.Fatal(err)
	}
	err := RegisterEmail(p, EmailConfig{
		DB: db, Store: store, Core: runtime.DefaultCoreCapabilities(),
		AccountCapability: DefaultAccountCapability(),
		Searcher:          search.New(store),
		// Internal deliberately unset: RegisterEmail must force it.
		InternalProperties: map[string]descriptor.Property{
			"vendorMarker": {Kind: descriptor.KindObject},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	prop, ok := db.Type(TypeEmail).Properties["vendorMarker"]
	if !ok {
		t.Fatal("declared internal property missing from the Email descriptor")
	}
	if !prop.Internal {
		t.Error("declared internal property not forced Internal")
	}
}

func TestRegisterEmailInternalPropertyCollision(t *testing.T) {
	p, db := emailTestDeps()
	store := kvstore.New(memory.New())
	err := RegisterEmail(p, EmailConfig{
		DB: db, Store: store, Core: runtime.DefaultCoreCapabilities(),
		AccountCapability: DefaultAccountCapability(),
		Searcher:          search.New(store),
		InternalProperties: map[string]descriptor.Property{
			"subject": {Kind: descriptor.KindString},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "subject") {
		t.Fatalf("collision with an Email property = %v, want an error naming it", err)
	}
}
