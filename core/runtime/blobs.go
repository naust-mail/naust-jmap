package runtime

// Binary data (RFC 8620 section 6): the upload and download endpoints
// and the Blob/copy method. Enabled by Server.EnableBlobs; without it
// the endpoints answer 501 and Blob/copy does not exist.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
)

type blobSupport struct {
	db    *objectdb.DB
	store blob.Store
	// slots enforces maxConcurrentUpload (section 2).
	slots chan struct{}
}

// EnableBlobs turns on the binary data endpoints (section 6) over the
// given object database and blob store, and registers Blob/copy. The
// store may share a backend with the database (blob/kvstore) or not
// (object storage); the runtime does not care.
//
// Garbage collection is not scheduled here: the embedder owns the loop
// and calls db.SweepBlobs per account at its own cadence.
func (s *Server) EnableBlobs(db *objectdb.DB, store blob.Store) {
	s.blobs = &blobSupport{db: db, store: store, slots: make(chan struct{}, s.core.MaxConcurrentUpload)}
	s.proc.Register("Blob/copy", jmap.CoreCapability, s.blobs.copy)
}

// handleUpload serves POST {uploadUrl} (section 6.1).
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.blobs == nil {
		http.Error(w, "blobs not enabled", http.StatusNotImplemented)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ident := s.authenticate(w, r)
	if ident == nil {
		return
	}
	acct := jmap.Id(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/upload/"), "/"))
	access, ok := ident.Accounts[acct]
	if !ok || strings.Contains(string(acct), "/") {
		writeProblem(w, jmap.RequestError{
			Type: "about:blank", Status: http.StatusNotFound,
			Detail: "unknown accountId",
		})
		return
	}
	// Uploading into a read-only account is refused: the caller could
	// never reference the blob, and unreferenced blobs consume storage.
	if access.ReadOnly {
		writeProblem(w, jmap.RequestError{
			Type: "about:blank", Status: http.StatusForbidden,
			Detail: "account is read-only",
		})
		return
	}
	select {
	case s.blobs.slots <- struct{}{}:
		defer func() { <-s.blobs.slots }()
	default:
		writeProblem(w, jmap.RequestError{
			Type: jmap.ProblemLimit, Status: http.StatusTooManyRequests,
			Limit:  "maxConcurrentUpload",
			Detail: "too many concurrent uploads",
		})
		return
	}
	// Stream the body straight into the blob store rather than buffering it
	// whole: the store computes the content-addressed id from the bytes at
	// Commit. The size cap is still enforced here (MaxBytesReader) so an
	// over-limit body is refused with the maxSizeUpload error.
	bw, err := s.blobs.store.Create(r.Context(), acct)
	if err != nil {
		writeProblem(w, jmap.RequestError{
			Type: "about:blank", Status: http.StatusInternalServerError,
			Detail: "storing blob failed",
		})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = bw.Abort()
		}
	}()
	n, err := io.Copy(bw, http.MaxBytesReader(w, r.Body, s.core.MaxSizeUpload))
	if err != nil {
		// The streaming copy surfaces either a body that overran the cap (a
		// client error, 413) or a backend fault while the bytes were being
		// stored (a server error); only the former is "too large".
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			writeProblem(w, jmap.RequestError{
				Type: jmap.ProblemLimit, Status: http.StatusRequestEntityTooLarge,
				Limit:  "maxSizeUpload",
				Detail: "upload exceeds maxSizeUpload",
			})
		case errors.Is(err, backend.ErrNoSpace):
			writeProblem(w, jmap.RequestError{
				Type: "about:blank", Status: http.StatusInsufficientStorage,
				Detail: "storage is full",
			})
		default:
			writeProblem(w, jmap.RequestError{
				Type: "about:blank", Status: http.StatusInternalServerError,
				Detail: "storing blob failed",
			})
		}
		return
	}
	// Record the upload and publish the content as one lease-held step: the
	// record is written before the content so a crash cannot strand
	// published bytes that no record covers, and the account lease is held
	// across both so a concurrent blob sweep cannot delete the content out
	// from under the finalize. A reupload of identical content reuses the
	// blobId (content addressing) and resets its expiry.
	blobID, err := s.blobs.db.FinalizeBlobUpload(r.Context(), acct, bw, ident.Username, time.Now())
	if err != nil {
		if errors.Is(err, backend.ErrNoSpace) {
			writeProblem(w, jmap.RequestError{
				Type: "about:blank", Status: http.StatusInsufficientStorage,
				Detail: "storage is full",
			})
			return
		}
		writeProblem(w, jmap.RequestError{
			Type: "about:blank", Status: http.StatusInternalServerError,
			Detail: "storing blob failed",
		})
		return
	}
	committed = true
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(jmap.UploadResponse{
		AccountId: acct,
		BlobId:    blobID,
		Type:      sanitizeMediaType(r.Header.Get("Content-Type")),
		Size:      n,
	})
}

// handleDownload serves GET {downloadUrl} (section 6.2). The URL
// template is /download/{accountId}/{blobId}/{name}?accept={type}.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if s.blobs == nil {
		http.Error(w, "blobs not enabled", http.StatusNotImplemented)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ident := s.authenticate(w, r)
	if ident == nil {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/download/"), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		writeProblem(w, jmap.RequestError{
			Type: "about:blank", Status: http.StatusNotFound,
			Detail: "download path must be /download/{accountId}/{blobId}/{name}",
		})
		return
	}
	acct, blobID, name := jmap.Id(parts[0]), jmap.Id(parts[1]), parts[2]
	if _, ok := ident.Accounts[acct]; !ok {
		writeProblem(w, jmap.RequestError{
			Type: "about:blank", Status: http.StatusNotFound,
			Detail: "unknown accountId",
		})
		return
	}
	rc, size, err := OpenBlob(r.Context(), s.blobs.db, s.blobs.store, acct, blobID, ident)
	if errors.Is(err, blob.ErrNotFound) {
		writeProblem(w, jmap.RequestError{
			Type: "about:blank", Status: http.StatusNotFound,
			Detail: "blob not found",
		})
		return
	}
	if err != nil {
		writeProblem(w, jmap.RequestError{
			Type: "about:blank", Status: http.StatusInternalServerError,
			Detail: "reading blob failed",
		})
		return
	}
	defer rc.Close()
	// The bytes behind a blobId are immutable, so the response is too;
	// section 6.2 recommends exactly this cache policy.
	w.Header().Set("Cache-Control", "private, immutable, max-age=31536000")
	w.Header().Set("Content-Type", sanitizeMediaType(r.URL.Query().Get("accept")))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	_, _ = io.Copy(w, rc)
}

// OpenBlob fetches a blob's content for a client-supplied blobId, applying
// the checks every client-facing blob read shares: the id must be a
// syntactically valid Id (RFC 8620 section 1.2) before it reaches any store,
// the account must hold an upload record for it (record presence is the
// existence test for a blob in an account), and an unreferenced blob is only
// accessible to its uploaders, even in shared accounts (section 6.1;
// referenced blobs follow the account's own access, already checked). Every
// denial reads as blob.ErrNotFound so probing reveals nothing: absent,
// invalid, and not-yours are indistinguishable.
//
// The download endpoint and Blob/copy go through it. A custom method that
// accepts a blobId from the client (Email/parse, Email/import) must open the
// blob through this function, never through the store directly - ids taken
// from stored records are the only ones a raw store read is safe for.
func OpenBlob(ctx context.Context, db *objectdb.DB, store blob.Store, acct, blobID jmap.Id, ident *auth.Identity) (io.ReadCloser, int64, error) {
	if !blobID.Valid() {
		return nil, 0, blob.ErrNotFound
	}
	rec, err := db.BlobUpload(ctx, acct, blobID)
	if errors.Is(err, objectdb.ErrNotFound) {
		return nil, 0, blob.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	referenced, err := db.BlobReferenced(ctx, acct, blobID)
	if err != nil {
		return nil, 0, err
	}
	if !referenced && (ident == nil || !slices.Contains(rec.Uploaders, ident.Username)) {
		return nil, 0, blob.ErrNotFound
	}
	return store.Open(ctx, acct, blobID)
}

// sanitizeMediaType parses a client-supplied media type and returns a
// clean serialization, or application/octet-stream if unusable. Both
// the upload Content-Type echo and the download "type" variable pass
// through here, so header injection via either is impossible.
func sanitizeMediaType(v string) string {
	media, params, err := mime.ParseMediaType(v)
	if err != nil {
		return "application/octet-stream"
	}
	return mime.FormatMediaType(media, params)
}

// ---- Blob/copy (section 6.3) ----

type blobCopyArgs struct {
	FromAccountId jmap.Id   `json:"fromAccountId"`
	AccountId     jmap.Id   `json:"accountId"`
	BlobIds       []jmap.Id `json:"blobIds"`
}

type blobCopyResponse struct {
	FromAccountId jmap.Id                    `json:"fromAccountId"`
	AccountId     jmap.Id                    `json:"accountId"`
	Copied        map[jmap.Id]jmap.Id        `json:"copied"`
	NotCopied     map[jmap.Id]*jmap.SetError `json:"notCopied"`
}

func (bs *blobSupport) copy(ctx context.Context, call *Call) []jmap.Invocation {
	var a blobCopyArgs
	if err := decodeArgs(call.Args, &a); err != nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	if call.Identity == nil {
		return fail(call.CallID, jmap.ErrAccountNotFound, "")
	}
	if _, ok := call.Identity.Accounts[a.FromAccountId]; a.FromAccountId == "" || !ok {
		return fail(call.CallID, jmap.ErrFromAccountNotFound, "")
	}
	if errType, desc := checkAccount(call, a.AccountId, true); errType != "" {
		return fail(call.CallID, errType, desc)
	}
	resp := blobCopyResponse{FromAccountId: a.FromAccountId, AccountId: a.AccountId}
	for _, blobID := range a.BlobIds {
		newID, serr, err := bs.copyOne(ctx, a.FromAccountId, a.AccountId, blobID, call.Identity)
		if err != nil {
			return fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		if serr != nil {
			if resp.NotCopied == nil {
				resp.NotCopied = make(map[jmap.Id]*jmap.SetError)
			}
			resp.NotCopied[blobID] = serr
			continue
		}
		if resp.Copied == nil {
			resp.Copied = make(map[jmap.Id]jmap.Id)
		}
		// The target id is recomputed from the copied bytes; content
		// addressing keeps it equal to the source id (the spec lets the
		// server pick the id in the target account).
		resp.Copied[blobID] = newID
	}
	return reply("Blob/copy", call.CallID, resp)
}

func (bs *blobSupport) copyOne(ctx context.Context, from, to, blobID jmap.Id, ident *auth.Identity) (jmap.Id, *jmap.SetError, error) {
	// A source blob the caller may not read is notFound (section 6.3
	// defines notFound for missing ids).
	newID, err := CopyBlob(ctx, bs.db, bs.store, from, to, blobID, ident)
	if errors.Is(err, blob.ErrNotFound) {
		return "", &jmap.SetError{Type: jmap.SetErrNotFound}, nil
	}
	if err != nil {
		return "", nil, err
	}
	return newID, nil, nil
}

// CopyBlob copies a blob the caller can read in the from account into the
// to account, returning its id there (the content address is recomputed as
// the bytes flow, so the copy keeps the source blobId, RFC 8620 section
// 6.3). The source open applies the same access rule as download (see
// OpenBlob; a denial reads as blob.ErrNotFound), and the bytes stream one
// piece at a time - a large blob is never buffered whole. The copy is an
// upload into the target account: recorded before the content is published
// and under the account lease, like the upload endpoint, so it exists
// there unreferenced, uploader-only, on the GC clock without a
// finalize-versus-sweep race. Blob/copy and RFC 8621 Email/copy (which
// must bring the message bytes into the target account before creating
// the Email there) go through it.
func CopyBlob(ctx context.Context, db *objectdb.DB, store blob.Store, from, to, blobID jmap.Id, ident *auth.Identity) (jmap.Id, error) {
	rc, _, err := OpenBlob(ctx, db, store, from, blobID, ident)
	if err != nil {
		return "", err
	}
	defer rc.Close()

	w, err := store.Create(ctx, to)
	if err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = w.Abort()
		}
	}()
	if _, err := io.Copy(w, rc); err != nil {
		return "", err
	}
	uploader := ""
	if ident != nil {
		uploader = ident.Username
	}
	newID, err := db.FinalizeBlobUpload(ctx, to, w, uploader, time.Now())
	if err != nil {
		return "", err
	}
	committed = true
	return newID, nil
}
