package emailmethods

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
	"fmt"
	"io"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// EmailCreate is the Email/set create override.
type EmailCreate struct {
	Mat Materializer
	Cfg GenConfig
	// MaxAttachBytes is the enforced maxSizeAttachmentsPerEmail (RFC 8621
	// section 1.3.1): the summed size of the blobs a creation references.
	MaxAttachBytes int64
}

// preparedEmailCreate is one creation ready to commit: the generated
// message parsed back through the seam, plus the record metadata.
type preparedEmailCreate struct {
	pe         *pendingEmail
	mailboxIds json.RawMessage
	keywords   json.RawMessage
	receivedAt time.Time
}

// Prepare implements SetHooks.PrepareCreate.
func (h EmailCreate) Prepare(ctx context.Context, call *runtime.Call, acct, cid jmap.Id, raw json.RawMessage) (any, *jmap.SetError, error) {
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
		rc, _, err := runtime.OpenBlob(ctx, h.Mat.DB, h.Mat.Store, acct, blobID, call.Identity)
		return rc, err
	}
	m, serr := planEmailMessage(obj, h.Cfg, time.Now(), open)
	if serr != nil {
		return nil, serr, nil
	}
	// Verify every referenced blob up front: blobNotFound must list ALL
	// the missing ids (section 4.6), and the advertised attachment size
	// cap is enforced on what the message would embed.
	var missing []jmap.Id
	var attachTotal int64
	for _, id := range m.blobIds {
		rc, size, err := runtime.OpenBlob(ctx, h.Mat.DB, h.Mat.Store, acct, id, call.Identity)
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
	if h.MaxAttachBytes > 0 && attachTotal > h.MaxAttachBytes {
		return nil, &jmap.SetError{Type: jmap.SetErrTooLarge, Description: "attachments exceed maxSizeAttachmentsPerEmail"}, nil
	}

	// Stream the generated message into the account's blob store. A blob
	// that vanishes mid-write is a store fault, not a client error: the
	// ids were just verified.
	w, err := h.Mat.Store.Create(ctx, acct)
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
	blobID, err := h.Mat.DB.FinalizeBlobUpload(ctx, acct, w, uploader, time.Now())
	if err != nil {
		return nil, nil, err
	}
	committed = true
	pe, err := h.Mat.prepare(ctx, acct, blobID, call.Identity)
	if err != nil {
		return nil, nil, err // our own fresh blob must open
	}
	mailboxIds, serr := resolveMailboxIdRefs(obj["mailboxIds"], call.CreatedIds)
	if serr != nil {
		return nil, serr, nil
	}
	return &preparedEmailCreate{
		pe:         pe,
		mailboxIds: mailboxIds,
		keywords:   obj["keywords"],
		receivedAt: receivedAt,
	}, nil, nil
}

// resolveMailboxIdRefs substitutes "#creationId" references in mailboxIds
// keys against the request-wide creation-id map (RFC 8620 section 5.3).
// mailboxIds is an Id-keyed map (descriptor.KindObject), which falls
// outside the runtime's own KindId substitution for the standard create
// path, so Email's create override resolves it itself against
// call.CreatedIds - the same primitive runtime.ResolveIdArg exposes to
// custom method handlers for a scalar id.
func resolveMailboxIdRefs(raw json.RawMessage, createdIds map[jmap.Id]jmap.Id) (json.RawMessage, *jmap.SetError) {
	members, ok := decodeBoolMap(raw)
	if !ok {
		return raw, nil // missing/null/malformed: validateMailboxIds reports it
	}
	resolved := make(map[string]bool, len(members))
	for id, val := range members {
		real, ok := runtime.ResolveIdArg(jmap.Id(id), createdIds)
		if !ok {
			return nil, invalidProp("mailboxIds", fmt.Sprintf("reference to unknown creation id %q", id))
		}
		resolved[string(real)] = val
	}
	out, err := json.Marshal(resolved)
	if err != nil {
		return nil, invalidProp("mailboxIds", "must be an object of Mailbox id to true")
	}
	return out, nil
}

// Commit implements SetHooks.CommitCreate: the seam validates the
// metadata and inserts the record; the echo is the section 4.6 created
// response - id, blobId, threadId, size.
func (h EmailCreate) Commit(u *objectdb.Update, prepared any) (jmap.Id, objectdb.Object, *jmap.SetError, error) {
	pc := prepared.(*preparedEmailCreate)
	created, serr, err := h.Mat.commit(u, pc.pe, pc.mailboxIds, pc.keywords, pc.receivedAt)
	if serr != nil || err != nil {
		return "", nil, serr, err
	}
	echo := objectdb.Object{
		"id":       record.MustJSON(created.Id),
		"blobId":   record.MustJSON(created.BlobId),
		"threadId": record.MustJSON(created.ThreadId),
		"size":     record.MustJSON(created.Size),
	}
	return created.Id, echo, nil, nil
}
