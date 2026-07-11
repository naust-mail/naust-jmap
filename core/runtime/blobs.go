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
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.core.MaxSizeUpload))
	if err != nil {
		writeProblem(w, jmap.RequestError{
			Type: jmap.ProblemLimit, Status: http.StatusRequestEntityTooLarge,
			Limit:  "maxSizeUpload",
			Detail: "upload exceeds maxSizeUpload",
		})
		return
	}
	blobID := blob.IdFor(data)
	if err := s.blobs.store.Put(r.Context(), acct, blobID, data); err != nil {
		writeProblem(w, jmap.RequestError{
			Type: "about:blank", Status: http.StatusInternalServerError,
			Detail: "storing blob failed",
		})
		return
	}
	// The record makes the blob exist; a reupload of identical content
	// reuses the blobId (content addressing) and resets its expiry.
	if err := s.blobs.db.RecordBlobUpload(r.Context(), acct, blobID, ident.Username, time.Now()); err != nil {
		writeProblem(w, jmap.RequestError{
			Type: "about:blank", Status: http.StatusInternalServerError,
			Detail: "recording upload failed",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(jmap.UploadResponse{
		AccountId: acct,
		BlobId:    blobID,
		Type:      sanitizeMediaType(r.Header.Get("Content-Type")),
		Size:      int64(len(data)),
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
	rc, size, err := s.blobs.open(r.Context(), acct, blobID, ident)
	if errors.Is(err, objectdb.ErrNotFound) || errors.Is(err, blob.ErrNotFound) {
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

// open fetches a blob's content after the access rule of section 6.1:
// an unreferenced blob is only accessible to its uploaders, even in
// shared accounts (referenced blobs follow the account's own access,
// already checked). Denials read as not-found so probing reveals
// nothing.
func (bs *blobSupport) open(ctx context.Context, acct, blobID jmap.Id, ident *auth.Identity) (io.ReadCloser, int64, error) {
	rec, err := bs.db.BlobUpload(ctx, acct, blobID)
	if err != nil {
		return nil, 0, err
	}
	referenced, err := bs.db.BlobReferenced(ctx, acct, blobID)
	if err != nil {
		return nil, 0, err
	}
	if !referenced && !slices.Contains(rec.Uploaders, ident.Username) {
		return nil, 0, objectdb.ErrNotFound
	}
	return bs.store.Open(ctx, acct, blobID)
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
		serr, err := bs.copyOne(ctx, a.FromAccountId, a.AccountId, blobID, call.Identity)
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
		// Content addressing keeps the id stable across accounts, so the
		// copied blob keeps its blobId (the spec lets the server pick).
		resp.Copied[blobID] = blobID
	}
	return reply("Blob/copy", call.CallID, resp)
}

func (bs *blobSupport) copyOne(ctx context.Context, from, to, blobID jmap.Id, ident *auth.Identity) (*jmap.SetError, error) {
	// Same access rule as download; a source blob the caller may not
	// read is notFound (section 6.3 defines notFound for missing ids).
	rc, _, err := bs.open(ctx, from, blobID, ident)
	if errors.Is(err, objectdb.ErrNotFound) || errors.Is(err, blob.ErrNotFound) {
		return &jmap.SetError{Type: jmap.SetErrNotFound}, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	if err := bs.store.Put(ctx, to, blobID, data); err != nil {
		return nil, err
	}
	// The copy is an upload into the target account: it exists there
	// unreferenced, uploader-only, on the GC clock, like any upload.
	if err := bs.db.RecordBlobUpload(ctx, to, blobID, ident.Username, time.Now()); err != nil {
		return nil, err
	}
	return nil, nil
}
