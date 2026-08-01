package mail

// IdentityView tests (RFC 8621 section 6): field projection against a
// created Identity, missing-record semantics, and AllowsSend - the exact
// predicate submissioncreate.go applies to each From address of an
// outgoing message (section 7.5), so this table doubles as the wildcard-
// matching contract the two packages share.
//
// RFC 8621 defines no worked JSON example for a stored Identity to
// replicate verbatim; TestIdentityLifecycle already exercises the
// section 6 default-filling wire shape, so this file's projection test
// only needs to check ReadIdentity decodes the same stored record
// correctly.

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/testsupport"
)

// identityViewServer is identityServer plus the *objectdb.DB, so a test
// can call ReadIdentity directly against the same store the Identity/set
// calls wrote to (identityServer itself returns only the HTTP server, and
// is shared by tests that have no need of the db).
func identityViewServer(t *testing.T) (*httptest.Server, *objectdb.DB) {
	t.Helper()
	a := testsupport.NewStaticAuth()
	a.AddUser("john@example.com", "secret", testAccount)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithVerifyPreImages())
	p := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	policy := NewStaticSendPolicy()
	policy.Allow(testAccount, "joe@example.com", "*@corp.example")
	if err := RegisterIdentity(p, db, policy, core); err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Capability(submissionCapabilityURI).Advertise(struct{}{}, struct{}{}).Err(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, db
}

// TestReadIdentity_Projection: every IdentityView field against an
// Identity created through the normal Identity/set surface.
func TestReadIdentity_Projection(t *testing.T) {
	ts, db := identityViewServer(t)
	r := callSubmission(t, ts, inv("Identity/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"i1":{`+
			`"name":"Joe","email":"joe@example.com",`+
			`"replyTo":[{"name":"Joe R","email":"joe+reply@example.com"}],`+
			`"bcc":[{"name":null,"email":"joe+archive@example.com"}],`+
			`"textSignature":"-- \nJoe","htmlSignature":"<p>Joe</p>"}}}`,
		testAccount), "0"))
	created := methodArgs(t, r, 0, "Identity/set")["created"].(map[string]any)
	id := created["i1"].(map[string]any)["id"].(string)

	v, err := ReadIdentity(context.Background(), db, testAccount, jmap.Id(id))
	if err != nil {
		t.Fatalf("ReadIdentity: %v", err)
	}
	if v.Id != jmap.Id(id) {
		t.Errorf("Id = %v, want %s", v.Id, id)
	}
	if v.Name != "Joe" {
		t.Errorf("Name = %q", v.Name)
	}
	if v.Email != "joe@example.com" {
		t.Errorf("Email = %q", v.Email)
	}
	if len(v.ReplyTo) != 1 || v.ReplyTo[0].Email != "joe+reply@example.com" || v.ReplyTo[0].Name == nil || *v.ReplyTo[0].Name != "Joe R" {
		t.Errorf("ReplyTo = %+v", v.ReplyTo)
	}
	if len(v.Bcc) != 1 || v.Bcc[0].Email != "joe+archive@example.com" || v.Bcc[0].Name != nil {
		t.Errorf("Bcc = %+v", v.Bcc)
	}
	if v.TextSignature != "-- \nJoe" {
		t.Errorf("TextSignature = %q", v.TextSignature)
	}
	if v.HtmlSignature != "<p>Joe</p>" {
		t.Errorf("HtmlSignature = %q", v.HtmlSignature)
	}
	if !v.MayDelete {
		t.Error("MayDelete = false, want true (server default)")
	}
}

// TestReadIdentity_Defaults: an Identity created with nothing but the
// required email carries the section 6 defaults (null replyTo/bcc,
// empty-string signatures).
func TestReadIdentity_Defaults(t *testing.T) {
	ts, db := identityViewServer(t)
	r := callSubmission(t, ts, inv("Identity/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"i1":{"email":"joe@example.com"}}}`, testAccount), "0"))
	created := methodArgs(t, r, 0, "Identity/set")["created"].(map[string]any)
	id := created["i1"].(map[string]any)["id"].(string)

	v, err := ReadIdentity(context.Background(), db, testAccount, jmap.Id(id))
	if err != nil {
		t.Fatalf("ReadIdentity: %v", err)
	}
	if v.ReplyTo != nil {
		t.Errorf("ReplyTo = %v, want nil", v.ReplyTo)
	}
	if v.Bcc != nil {
		t.Errorf("Bcc = %v, want nil", v.Bcc)
	}
	if v.TextSignature != "" || v.HtmlSignature != "" {
		t.Errorf("signatures = %q / %q, want empty", v.TextSignature, v.HtmlSignature)
	}
}

// TestReadIdentity_NotFound: a missing record reports objectdb.ErrNotFound.
func TestReadIdentity_NotFound(t *testing.T) {
	_, db := identityViewServer(t)
	_, err := ReadIdentity(context.Background(), db, testAccount, "Inosuch")
	if !errors.Is(err, objectdb.ErrNotFound) {
		t.Errorf("err = %v, want objectdb.ErrNotFound", err)
	}
}

// TestIdentityView_AllowsSend: exact match, domain case-insensitivity,
// local-part exactness, and the whole-domain wildcard rule - the same
// table TestStaticSendPolicyMatching exercises for CanSendAs, since
// AllowsSend wraps the identical addr.IdentityAllows predicate
// submissioncreate.go applies to each From address (section 7.5).
func TestIdentityView_AllowsSend(t *testing.T) {
	cases := []struct {
		identityEmail string
		from          string
		want          bool
	}{
		{"joe@example.com", "joe@example.com", true},
		{"joe@example.com", "joe@EXAMPLE.COM", true},  // domain case-insensitive
		{"joe@example.com", "JOE@example.com", false}, // local part exact
		{"joe@example.com", "jane@example.com", false},
		{"*@corp.example", "anyone@corp.example", true},
		{"*@corp.example", "anyone@other.example", false},
		{"*@corp.example", "*@corp.example", true}, // wildcard form matches itself
		{"joe@example.com", "not-an-address", false},
		{"joe@example.com", "", false},
	}
	for _, c := range cases {
		v := &IdentityView{Email: c.identityEmail}
		if got := v.AllowsSend(c.from); got != c.want {
			t.Errorf("AllowsSend(%q) with identity %q = %v, want %v", c.from, c.identityEmail, got, c.want)
		}
	}
}
