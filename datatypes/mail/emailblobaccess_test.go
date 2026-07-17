package mail

// Email/parse and Email/import take blobIds from the client, so both read
// blobs through the runtime's checked open. RFC 8620 section 6.1:
// "unreferenced blobs MUST only be accessible to the uploader, even in
// shared accounts" - and an invalid or unknown blobId reads as not found,
// indistinguishable from an absent blob.

import (
	"fmt"
	"testing"
)

// TestParseUnreferencedBlobUploaderOnly: jane (same account, not the
// uploader) cannot parse john's unreferenced blob; john can; once the blob
// is referenced by an imported Email, jane can too.
func TestParseUnreferencedBlobUploaderOnly(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	blobID := uploadBlob(t, db, store, simpleMessage)
	args := fmt.Sprintf(`{"accountId":%q,"blobIds":[%q]}`, testAccount, blobID)

	resp := methodArgs(t, callMailAs(t, ts, "jane@example.com", inv("Email/parse", args, "0")), 0, "Email/parse")
	if resp["parsed"] != nil {
		t.Fatalf("jane parsed an unreferenced blob she did not upload: %v", resp["parsed"])
	}
	if nf, ok := resp["notFound"].([]any); !ok || len(nf) != 1 || nf[0] != blobID {
		t.Fatalf("notFound = %v, want [%q]", resp["notFound"], blobID)
	}

	resp = methodArgs(t, callMailAs(t, ts, "john@example.com", inv("Email/parse", args, "0")), 0, "Email/parse")
	if parsed, ok := resp["parsed"].(map[string]any); !ok || parsed[blobID] == nil {
		t.Fatalf("uploader could not parse own unreferenced blob: %v", resp)
	}

	// Importing the blob references it; account access now suffices.
	imp := doImport(t, ts, fmt.Sprintf(`{"accountId":%q,"emails":{"e1":{"blobId":%q,"mailboxIds":{%q:true}}}}`,
		testAccount, blobID, inbox))
	if imp["created"] == nil {
		t.Fatalf("referencing import failed: %v", imp)
	}
	resp = methodArgs(t, callMailAs(t, ts, "jane@example.com", inv("Email/parse", args, "0")), 0, "Email/parse")
	if parsed, ok := resp["parsed"].(map[string]any); !ok || parsed[blobID] == nil {
		t.Fatalf("jane could not parse a referenced blob: %v", resp)
	}
}

// TestImportUnreferencedBlobUploaderOnly: the same section 6.1 rule on the
// import path, where a denied blob is a per-Email invalidProperties
// SetError; once referenced, another member may import it too (duplicates
// are allowed, section 4.8).
func TestImportUnreferencedBlobUploaderOnly(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	blobID := uploadBlob(t, db, store, simpleMessage)
	args := fmt.Sprintf(`{"accountId":%q,"emails":{"e1":{"blobId":%q,"mailboxIds":{%q:true}}}}`,
		testAccount, blobID, inbox)

	resp := methodArgs(t, callMailAs(t, ts, "jane@example.com", inv("Email/import", args, "0")), 0, "Email/import")
	nc, ok := resp["notCreated"].(map[string]any)
	if !ok || nc["e1"] == nil {
		t.Fatalf("jane imported an unreferenced blob she did not upload: %v", resp)
	}
	if serr := nc["e1"].(map[string]any); serr["type"] != "invalidProperties" {
		t.Fatalf("SetError type = %v, want invalidProperties", serr["type"])
	}

	resp = doImport(t, ts, args)
	if created, ok := resp["created"].(map[string]any); !ok || created["e1"] == nil {
		t.Fatalf("uploader could not import own blob: %v", resp)
	}

	resp = methodArgs(t, callMailAs(t, ts, "jane@example.com", inv("Email/import", args, "0")), 0, "Email/import")
	if created, ok := resp["created"].(map[string]any); !ok || created["e1"] == nil {
		t.Fatalf("jane could not import a referenced blob: %v", resp)
	}
}

// TestParseMalformedBlobIdsNotFound: syntactically invalid ids (not RFC 8620
// section 1.2 Ids) are reported notFound rather than reaching any store.
func TestParseMalformedBlobIdsNotFound(t *testing.T) {
	ts, _, _ := emailServer(t)
	args := fmt.Sprintf(`{"accountId":%q,"blobIds":["","../../escape","G*bad"]}`, testAccount)
	resp := methodArgs(t, callMail(t, ts, inv("Email/parse", args, "0")), 0, "Email/parse")
	if resp["parsed"] != nil {
		t.Fatalf("parsed = %v, want null", resp["parsed"])
	}
	nf, ok := resp["notFound"].([]any)
	if !ok || len(nf) != 3 {
		t.Fatalf("notFound = %v, want all three malformed ids", resp["notFound"])
	}
}
