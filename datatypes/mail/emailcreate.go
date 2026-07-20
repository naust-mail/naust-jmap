package mail

// Email/set create (RFC 8621 section 4.6): the creation object is the
// input to message generation, not the record. The runtime's create
// override splits the work around the account lease the way every other
// producer does: prepare plans the message (the strict 4.6 validation in
// emailgen.go), verifies the referenced blobs, streams the generated
// message into the blob store, and re-parses it through the materialize
// seam - all outside the lease; commit validates the JMAP metadata and
// inserts the record under it. Re-parsing its own blob keeps one deriver
// of record-from-blob: a created Email's stored properties come from the
// same reader an imported one's do.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// emailCreate is the Email/set create override.
type emailCreate struct {
	mat materializer
	cfg genConfig
	// maxAttachBytes is the enforced maxSizeAttachmentsPerEmail (RFC 8621
	// section 1.3.1): the summed size of the blobs a creation references.
	maxAttachBytes int64
}

// preparedEmailCreate is one creation ready to commit: the generated
// message parsed back through the seam, plus the record metadata.
type preparedEmailCreate struct {
	pe         *pendingEmail
	mailboxIds json.RawMessage
	keywords   json.RawMessage
	receivedAt time.Time
}

// prepare implements SetHooks.PrepareCreate.
func (h emailCreate) prepare(ctx context.Context, call *runtime.Call, acct, cid jmap.Id, raw json.RawMessage) (any, *jmap.SetError, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, &jmap.SetError{Type: jmap.SetErrInvalidProperties, Description: "create value is not an object"}, nil
	}
	if _, has := obj["id"]; has {
		return nil, invalidProp("id", "id is server-set"), nil
	}
	// receivedAt is client-settable at creation, defaulting to the server's
	// now (section 4.1.1).
	receivedAt := time.Now().UTC()
	if rawAt, has := obj["receivedAt"]; has && !isNullRaw(rawAt) {
		s, ok := decodeString(rawAt)
		if !ok {
			return nil, invalidProp("receivedAt", "must be a UTCDate string"), nil
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, invalidProp("receivedAt", "not a valid UTCDate"), nil
		}
		receivedAt = t
	}
	open := func(ctx context.Context, blobID jmap.Id) (io.ReadCloser, error) {
		rc, _, err := runtime.OpenBlob(ctx, h.mat.db, h.mat.store, acct, blobID, call.Identity)
		return rc, err
	}
	m, serr := planEmailMessage(obj, h.cfg, time.Now(), open)
	if serr != nil {
		return nil, serr, nil
	}
	// Verify every referenced blob up front: blobNotFound must list ALL
	// the missing ids (section 4.6), and the advertised attachment size
	// cap is enforced on what the message would embed.
	var missing []jmap.Id
	var attachTotal int64
	for _, id := range m.blobIds {
		rc, size, err := runtime.OpenBlob(ctx, h.mat.db, h.mat.store, acct, id, call.Identity)
		if errors.Is(err, blob.ErrNotFound) {
			missing = append(missing, id)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		rc.Close()
		attachTotal += size
	}
	if missing != nil {
		return nil, &jmap.SetError{Type: "blobNotFound", NotFound: missing, Description: "referenced blobs not found"}, nil
	}
	if h.maxAttachBytes > 0 && attachTotal > h.maxAttachBytes {
		return nil, &jmap.SetError{Type: jmap.SetErrTooLarge, Description: "attachments exceed maxSizeAttachmentsPerEmail"}, nil
	}

	// Stream the generated message into the account's blob store. A blob
	// that vanishes mid-write is a store fault, not a client error: the
	// ids were just verified.
	w, err := h.mat.store.Create(ctx, acct)
	if err != nil {
		return nil, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = w.Abort()
		}
	}()
	if err := m.write(ctx, w); err != nil {
		return nil, nil, err
	}
	uploader := ""
	if call.Identity != nil {
		uploader = call.Identity.Username
	}
	blobID, err := h.mat.db.FinalizeBlobUpload(ctx, acct, w, uploader, time.Now())
	if err != nil {
		return nil, nil, err
	}
	committed = true
	pe, err := h.mat.prepare(ctx, acct, blobID, call.Identity)
	if err != nil {
		return nil, nil, err // our own fresh blob must open
	}
	return &preparedEmailCreate{
		pe:         pe,
		mailboxIds: obj["mailboxIds"],
		keywords:   obj["keywords"],
		receivedAt: receivedAt,
	}, nil, nil
}

// commit implements SetHooks.CommitCreate: the seam validates the
// metadata and inserts the record; the echo is the section 4.6 created
// response - id, blobId, threadId, size.
func (h emailCreate) commit(u *objectdb.Update, prepared any) (jmap.Id, objectdb.Object, *jmap.SetError, error) {
	pc := prepared.(*preparedEmailCreate)
	created, serr, err := h.mat.commit(u, pc.pe, pc.mailboxIds, pc.keywords, pc.receivedAt)
	if serr != nil || err != nil {
		return "", nil, serr, err
	}
	echo := objectdb.Object{
		"id":       mustJSON(created.Id),
		"blobId":   mustJSON(created.BlobId),
		"threadId": mustJSON(created.ThreadId),
		"size":     mustJSON(created.Size),
	}
	return created.Id, echo, nil, nil
}
