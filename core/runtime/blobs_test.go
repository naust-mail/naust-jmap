package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// testDocType has the blob-referencing property.
func testDocType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "TestDoc",
		Capability: "urn:example:testdoc",
		Properties: map[string]descriptor.Property{
			"title":  {Kind: descriptor.KindString},
			"blobId": {Kind: descriptor.KindId, BlobRef: true},
		},
	}
}

// multiAuth grants john two writable accounts (for Blob/copy), a
// read-only one, and gives mallory shared access to Atest1 (for the
// uploader-only access rule). Password is always "secret".
type multiAuth struct{}

func (multiAuth) Authenticate(r *http.Request) (*auth.Identity, error) {
	user, pass, ok := r.BasicAuth()
	if !ok || pass != "secret" {
		return nil, auth.ErrUnauthenticated
	}
	switch user {
	case "john@example.com":
		return &auth.Identity{
			Username: user,
			Accounts: map[jmap.Id]auth.Access{
				"Atest1": {Name: user, Personal: true},
				"Adest":  {Name: "shared"},
				"Aro":    {Name: "archive", ReadOnly: true},
			},
			Primary: "Atest1",
		}, nil
	case "mallory@example.com":
		return &auth.Identity{
			Username: user,
			Accounts: map[jmap.Id]auth.Access{"Atest1": {Name: "shared"}},
			Primary:  "Atest1",
		}, nil
	}
	return nil, auth.ErrUnauthenticated
}

func blobServer(t *testing.T, core jmap.CoreCapabilities) (*httptest.Server, *objectdb.DB, blob.Store) {
	t.Helper()
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	store := kvstore.New(be)
	p := NewProcessor()
	if err := RegisterStandardType(p, db, testDocType(), core); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(multiAuth{}, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability("urn:example:testdoc", struct{}{}, struct{}{}); err != nil {
		t.Fatal(err)
	}
	srv.EnableBlobs(db, store)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, db, store
}

func uploadHTTP(t *testing.T, ts *httptest.Server, acct, user, body, ctype string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/upload/"+acct+"/", strings.NewReader(body))
	req.SetBasicAuth(user, "secret")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func uploadOK(t *testing.T, ts *httptest.Server, acct, user, body, ctype string) jmap.UploadResponse {
	t.Helper()
	resp := uploadHTTP(t, ts, acct, user, body, ctype)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload -> %d: %s", resp.StatusCode, raw)
	}
	var out jmap.UploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func docCall(t *testing.T, ts *httptest.Server, user string, calls ...jmap.Invocation) *jmap.Response {
	t.Helper()
	req := map[string]any{
		"using":       []string{jmap.CoreCapability, "urn:example:testdoc"},
		"methodCalls": calls,
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	hreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api", strings.NewReader(string(body)))
	hreq.SetBasicAuth(user, "secret")
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out jmap.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// RFC 8620 sections 6.1 and 6.2: upload response shape, content-address
// dedup, and the download response with its immutable cache policy.
func TestUploadDownloadRoundTrip(t *testing.T) {
	ts, _, _ := blobServer(t, DefaultCoreCapabilities())

	up := uploadOK(t, ts, "Atest1", "john@example.com", "hello blob world", "text/plain")
	if up.AccountId != "Atest1" || up.Size != 16 || up.Type != "text/plain" {
		t.Fatalf("upload response = %+v", up)
	}
	if !up.BlobId.Valid() {
		t.Fatalf("blobId %q is not a valid Id", up.BlobId)
	}
	// Identical content: the existing blobId comes back (6.1).
	again := uploadOK(t, ts, "Atest1", "john@example.com", "hello blob world", "application/octet-stream")
	if again.BlobId != up.BlobId {
		t.Errorf("reupload got %s, want %s", again.BlobId, up.BlobId)
	}

	resp := get(t, ts, "/download/Atest1/"+string(up.BlobId)+"/greeting.txt?accept=text/plain",
		"john@example.com", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download -> %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello blob world" {
		t.Errorf("body = %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename=greeting.txt`) {
		t.Errorf("Content-Disposition = %q, must carry the name (6.2)", cd)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") || !strings.Contains(cc, "private") {
		t.Errorf("Cache-Control = %q, want the 6.2 immutable policy", cc)
	}
	// A hostile type variable cannot inject headers; it collapses to
	// octet-stream.
	resp = get(t, ts, "/download/Atest1/"+string(up.BlobId)+"/x?accept=bogus%0d%0aX-Evil:1",
		"john@example.com", "secret")
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("hostile accept -> Content-Type %q", ct)
	}
	if resp.Header.Get("X-Evil") != "" {
		t.Error("header injection through the type variable")
	}
}

// TestUploadStoreFull: when the blob store is at capacity, Commit fails and
// the upload is refused 507 (the body was within maxSizeUpload, so this is not
// a 413).
func TestUploadStoreFull(t *testing.T) {
	core := DefaultCoreCapabilities()
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	// The blob store has its own tiny-capacity backend; the message is well
	// under maxSizeUpload but too big to store.
	store := kvstore.New(memory.New(memory.WithCapacity(4)))
	srv, err := NewServer(multiAuth{}, NewProcessor(), "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	srv.EnableBlobs(db, store)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := uploadHTTP(t, ts, "Atest1", "john@example.com", "way over four bytes", "text/plain")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("upload into a full store -> %d, want 507", resp.StatusCode)
	}
}

// writeFaultStore is a blob.Store whose writer fails partway through the
// streamed copy. It exercises the upload path's distinction between a body over
// the size cap (a client error, 413) and a backend fault while the bytes are
// being stored (a server error): both reach handleUpload through the one
// io.Copy error, and only the former is "too large".
type writeFaultStore struct {
	err   error
	after int64
}

func (writeFaultStore) Open(context.Context, jmap.Id, jmap.Id) (io.ReadCloser, int64, error) {
	return nil, 0, blob.ErrNotFound
}
func (writeFaultStore) Put(context.Context, jmap.Id, jmap.Id, []byte) error { return nil }
func (writeFaultStore) Delete(context.Context, jmap.Id, jmap.Id) error      { return nil }
func (s writeFaultStore) Create(context.Context, jmap.Id) (blob.BlobWriter, error) {
	return &faultWriter{store: s}, nil
}

type faultWriter struct {
	store   writeFaultStore
	written int64
}

func (w *faultWriter) Write(p []byte) (int, error) {
	w.written += int64(len(p))
	if w.written > w.store.after {
		return 0, w.store.err
	}
	return len(p), nil
}
func (w *faultWriter) ID() jmap.Id              { return "Gtest" }
func (w *faultWriter) Commit() (jmap.Id, error) { return "Gtest", nil }
func (w *faultWriter) Abort() error             { return nil }

// TestUploadBackendFaultNotTooLarge: a fault while the bytes are being stored
// is reported by its real cause, not as a size violation. The body is well
// under maxSizeUpload, so 413 here would be a lie; a generic fault is 500 and
// an out-of-space fault is 507 (RFC 8620 6.1 requires an HTTP error with an RFC
// 7807 body, and the status must describe the real failure).
func TestUploadBackendFaultNotTooLarge(t *testing.T) {
	core := DefaultCoreCapabilities()
	core.MaxSizeUpload = 1 << 20 // far above the body: a 413 could only be wrong

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"generic fault", errors.New("backend unavailable"), http.StatusInternalServerError},
		{"out of space", backend.ErrNoSpace, http.StatusInsufficientStorage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := memory.New()
			db := objectdb.New(be, lease.NewInProcess(be))
			srv, err := NewServer(multiAuth{}, NewProcessor(), "https://jmap.example.com", core)
			if err != nil {
				t.Fatal(err)
			}
			srv.EnableBlobs(db, writeFaultStore{err: tc.err, after: 4})
			ts := httptest.NewServer(srv)
			defer ts.Close()

			resp := uploadHTTP(t, ts, "Atest1", "john@example.com", "a body larger than four bytes", "text/plain")
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusRequestEntityTooLarge {
				t.Fatalf("backend fault (%v) misreported as 413 too-large", tc.err)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("backend fault (%v) -> %d, want %d", tc.err, resp.StatusCode, tc.want)
			}
		})
	}
}

func TestUploadErrors(t *testing.T) {
	small := DefaultCoreCapabilities()
	small.MaxSizeUpload = 8
	ts, _, _ := blobServer(t, small)

	// Over maxSizeUpload -> 413 problem naming the limit (6.1 + 7807).
	resp := uploadHTTP(t, ts, "Atest1", "john@example.com", "way more than eight bytes", "")
	typ, p := problemType(t, resp)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || typ != jmap.ProblemLimit || p.Limit != "maxSizeUpload" {
		t.Errorf("oversized upload -> %d %s limit=%q", resp.StatusCode, typ, p.Limit)
	}
	// Unknown account -> 404, read-only account -> 403, no auth -> 401.
	if resp := uploadHTTP(t, ts, "Anope", "john@example.com", "x", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown account -> %d", resp.StatusCode)
	}
	if resp := uploadHTTP(t, ts, "Aro", "john@example.com", "x", ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("read-only account -> %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/upload/Atest1/", strings.NewReader("x"))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated upload -> %v %d", err, resp.StatusCode)
	}
	// Download of a nonexistent blob -> 404.
	if resp := get(t, ts, "/download/Atest1/Gnope/x", "john@example.com", "secret"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing blob -> %d", resp.StatusCode)
	}
}

// Section 6.1: unreferenced blobs MUST only be accessible to the
// uploader, even in shared accounts; a reference makes them account-
// visible.
func TestUnreferencedBlobUploaderOnly(t *testing.T) {
	ts, _, _ := blobServer(t, DefaultCoreCapabilities())
	up := uploadOK(t, ts, "Atest1", "mallory@example.com", "mallory's secret draft", "")
	path := "/download/Atest1/" + string(up.BlobId) + "/d.bin"

	if resp := get(t, ts, path, "john@example.com", "secret"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("non-uploader read of unreferenced blob -> %d, want 404", resp.StatusCode)
	}
	if resp := get(t, ts, path, "mallory@example.com", "secret"); resp.StatusCode != http.StatusOK {
		t.Errorf("uploader read -> %d, want 200", resp.StatusCode)
	}

	// mallory references it; the account can see it now.
	r := docCall(t, ts, "mallory@example.com", inv("TestDoc/set",
		`{"accountId":"Atest1","create":{"d":{"title":"draft","blobId":"`+string(up.BlobId)+`"}}}`, "0"))
	if _, ok := methodArgs(t, r, 0, "TestDoc/set")["created"].(map[string]any); !ok {
		t.Fatalf("create with valid blobId failed: %v", r.MethodResponses)
	}
	if resp := get(t, ts, path, "john@example.com", "secret"); resp.StatusCode != http.StatusOK {
		t.Errorf("read of referenced blob -> %d, want 200", resp.StatusCode)
	}
}

// Section 5.3: a dangling blobId is a foreign key to nowhere ->
// invalidProperties naming the property.
func TestSetRejectsDanglingBlobRef(t *testing.T) {
	ts, _, _ := blobServer(t, DefaultCoreCapabilities())
	r := docCall(t, ts, "john@example.com", inv("TestDoc/set",
		`{"accountId":"Atest1","create":{"d":{"title":"broken","blobId":"Gdoesnotexist"}}}`, "0"))
	args := methodArgs(t, r, 0, "TestDoc/set")
	nc, ok := args["notCreated"].(map[string]any)
	if !ok {
		t.Fatalf("expected notCreated: %v", args)
	}
	serr := nc["d"].(map[string]any)
	props, _ := serr["properties"].([]any)
	if serr["type"] != "invalidProperties" || len(props) != 1 || props[0] != "blobId" {
		t.Errorf("SetError = %v", serr)
	}

	// The same rule on update: patching to a dangling blobId fails, the
	// record keeps its old value.
	up := uploadOK(t, ts, "Atest1", "john@example.com", "content", "")
	r = docCall(t, ts, "john@example.com", inv("TestDoc/set",
		`{"accountId":"Atest1","create":{"d":{"title":"ok","blobId":"`+string(up.BlobId)+`"}}}`, "0"))
	created, ok := methodArgs(t, r, 0, "TestDoc/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", r.MethodResponses)
	}
	id := created["d"].(map[string]any)["id"].(string)
	r = docCall(t, ts, "john@example.com", inv("TestDoc/set",
		`{"accountId":"Atest1","update":{"`+id+`":{"blobId":"Gdoesnotexist"}}}`, "0"))
	nu, ok := methodArgs(t, r, 0, "TestDoc/set")["notUpdated"].(map[string]any)
	if !ok || nu[id].(map[string]any)["type"] != "invalidProperties" {
		t.Errorf("dangling update -> %v", r.MethodResponses)
	}
}

// Section 6.3: Blob/copy between accounts, notFound for missing ids,
// fromAccountNotFound, and the target account's write access.
func TestBlobCopy(t *testing.T) {
	ts, _, _ := blobServer(t, DefaultCoreCapabilities())
	up := uploadOK(t, ts, "Atest1", "john@example.com", "portable bytes", "")

	r := docCall(t, ts, "john@example.com", inv("Blob/copy",
		`{"fromAccountId":"Atest1","accountId":"Adest","blobIds":["`+string(up.BlobId)+`","Gmissing"]}`, "0"))
	args := methodArgs(t, r, 0, "Blob/copy")
	if args["fromAccountId"] != "Atest1" || args["accountId"] != "Adest" {
		t.Errorf("echoed accounts: %v", args)
	}
	copied, _ := args["copied"].(map[string]any)
	if copied[string(up.BlobId)] != string(up.BlobId) {
		t.Errorf("copied = %v", args["copied"])
	}
	notCopied, _ := args["notCopied"].(map[string]any)
	if e, _ := notCopied["Gmissing"].(map[string]any); e["type"] != "notFound" {
		t.Errorf("notCopied = %v", args["notCopied"])
	}

	// The copy exists in the destination and is downloadable there.
	resp := get(t, ts, "/download/Adest/"+string(up.BlobId)+"/copy.bin", "john@example.com", "secret")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "portable bytes" {
		t.Errorf("download of copy -> %d %q", resp.StatusCode, body)
	}

	// Method-level errors: unknown source, read-only target, unknown arg.
	for name, tc := range map[string]struct{ args, wantErr string }{
		"fromAccountNotFound": {`{"fromAccountId":"Anope","accountId":"Adest","blobIds":[]}`, "fromAccountNotFound"},
		"accountReadOnly":     {`{"fromAccountId":"Atest1","accountId":"Aro","blobIds":[]}`, "accountReadOnly"},
		"invalidArguments":    {`{"fromAccountId":"Atest1","accountId":"Adest","blobIds":[],"nope":1}`, "invalidArguments"},
	} {
		r := docCall(t, ts, "john@example.com", inv("Blob/copy", tc.args, "0"))
		if got := methodArgs(t, r, 0, "error")["type"]; got != tc.wantErr {
			t.Errorf("%s: error type = %v", name, got)
		}
	}
}

// countingStore wraps a Store and counts Opens, so a test can prove a
// rejected blobId never reaches the store at all.
type countingStore struct {
	blob.Store
	opens int
}

func (c *countingStore) Open(ctx context.Context, acct, id jmap.Id) (io.ReadCloser, int64, error) {
	c.opens++
	return c.Store.Open(ctx, acct, id)
}

// OpenBlob is the checked open every client-supplied blobId goes through.
// Invalid ids, unknown ids, and section 6.1 denials (a non-uploader, or no
// identity at all, on an unreferenced blob) all read as blob.ErrNotFound
// without the store being touched; the uploader reads the content while it
// is unreferenced, and any account member reads it once it is referenced.
func TestOpenBlobChecks(t *testing.T) {
	ctx := context.Background()
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	if err := db.RegisterType(testDocType()); err != nil {
		t.Fatal(err)
	}
	store := &countingStore{Store: kvstore.New(be)}

	bw, err := store.Create(ctx, "Atest1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(bw, "checked open content"); err != nil {
		t.Fatal(err)
	}
	blobID, err := db.FinalizeBlobUpload(ctx, "Atest1", bw, "john@example.com", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	john := &auth.Identity{Username: "john@example.com"}
	mallory := &auth.Identity{Username: "mallory@example.com"}

	denied := []struct {
		name  string
		id    jmap.Id
		ident *auth.Identity
	}{
		{"empty id", "", john},
		{"path-shaped id", "G../../escape", john},
		{"valid unknown id", "Gnosuchblob", john},
		{"non-uploader on unreferenced", blobID, mallory},
		{"no identity on unreferenced", blobID, nil},
	}
	for _, d := range denied {
		if _, _, err := OpenBlob(ctx, db, store, "Atest1", d.id, d.ident); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("%s: err = %v, want blob.ErrNotFound", d.name, err)
		}
	}
	if store.opens != 0 {
		t.Fatalf("denied opens reached the store %d times, want 0", store.opens)
	}

	// The uploader reads the blob while it is unreferenced.
	rc, size, err := OpenBlob(ctx, db, store, "Atest1", blobID, john)
	if err != nil {
		t.Fatalf("uploader open: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "checked open content" || size != int64(len(body)) {
		t.Fatalf("uploader read %q (size %d)", body, size)
	}

	// Reference the blob; now any member of the account reads it.
	raw, _ := json.Marshal(blobID)
	if _, err := db.Update(ctx, "Atest1", func(u *objectdb.Update) error {
		_, err := u.Create("TestDoc", objectdb.Object{"blobId": raw})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rc, _, err = OpenBlob(ctx, db, store, "Atest1", blobID, mallory)
	if err != nil {
		t.Fatalf("non-uploader open of referenced blob: %v", err)
	}
	rc.Close()
	if store.opens != 2 {
		t.Errorf("granted opens = %d, want 2", store.opens)
	}
}
