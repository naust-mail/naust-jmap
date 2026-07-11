package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
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
