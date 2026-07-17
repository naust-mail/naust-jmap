package mail

// Email tests: the stored fast list and its /get shape, on-demand body
// parts and bodyValues, the header:{name} parsed forms, and the M2-
// restricted /set (no create, keyword/mailboxId semantics, destroy).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

var testReceivedAt = time.Date(2021, 3, 4, 12, 0, 0, 0, time.UTC)

// newEmailServer wires Mailbox + Email with a blob store, using acctCap
// for both the advertised capability and the enforced limits.
func newEmailServer(t *testing.T, acctCap AccountCapability) (*httptest.Server, *objectdb.DB, blob.Store) {
	t.Helper()
	a := auth.NewStatic()
	a.AddUser("john@example.com", "secret", testAccount)
	// jane shares the account without having uploaded anything, for the
	// RFC 8620 section 6.1 unreferenced-blob visibility tests.
	a.AddUser("jane@example.com", "secret", "Ajane")
	a.AddAccess("jane@example.com", testAccount, auth.Access{Name: "shared"})
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	store := kvstore.New(memory.New())
	p := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	if err := RegisterMailbox(p, db, core); err != nil {
		t.Fatal(err)
	}
	if err := RegisterThread(p, db, core); err != nil {
		t.Fatal(err)
	}
	if err := RegisterEmail(p, db, store, core, acctCap, nil); err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability(CapabilityURI, struct{}{}, acctCap); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, db, store
}

func emailServer(t *testing.T) (*httptest.Server, *objectdb.DB, blob.Store) {
	return newEmailServer(t, DefaultAccountCapability())
}

// storeAndRecord streams content into a fresh blob writer and finalizes it
// (record then publish) in testAccount, the way delivery establishes a
// message's blob, and returns its content-addressed id. The record must
// exist or the referential blobId check rejects later Email updates.
func storeAndRecord(t *testing.T, db *objectdb.DB, store blob.Store, content string, at time.Time) jmap.Id {
	t.Helper()
	ctx := context.Background()
	bw, err := store.Create(ctx, testAccount)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(bw, content); err != nil {
		t.Fatal(err)
	}
	id, err := db.FinalizeBlobUpload(ctx, testAccount, bw, "john@example.com", at)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// putEmail parses raw, stores its blob, and creates the Email record in
// the given mailboxes with the given keywords, returning the Email id.
func putEmail(t *testing.T, db *objectdb.DB, store blob.Store, raw string, mailboxIds map[string]bool, keywords map[string]bool) string {
	t.Helper()
	return putEmailAt(t, db, store, raw, mailboxIds, keywords, testReceivedAt)
}

// putEmailAt is putEmail with an explicit receivedAt, for ordering and
// thread tests. It runs the shared insertEmail path, so threading and
// the Mailbox counters are exercised exactly as delivery will exercise
// them.
func putEmailAt(t *testing.T, db *objectdb.DB, store blob.Store, raw string, mailboxIds map[string]bool, keywords map[string]bool, receivedAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	blobID := storeAndRecord(t, db, store, raw, receivedAt)
	mb, _ := json.Marshal(mailboxIds)
	var kw json.RawMessage
	if keywords != nil {
		kw, _ = json.Marshal(keywords)
	}
	c := newCapture()
	c.preview = true
	msg, err := parseMessage(strings.NewReader(raw), c)
	if err != nil {
		t.Fatal(err)
	}
	var id jmap.Id
	if _, err := db.Update(ctx, testAccount, func(u *objectdb.Update) error {
		created, err := insertEmail(u, msg, emailMeta{
			BlobID: blobID, MailboxIds: mb, Keywords: kw,
			Size: uint64(len(raw)), ReceivedAt: receivedAt,
		})
		id = created
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return string(id)
}

func emailGet(t *testing.T, ts *httptest.Server, id, extra string) map[string]any {
	t.Helper()
	args := fmt.Sprintf(`{"accountId":%q,"ids":[%q]%s}`, testAccount, id, extra)
	r := callMail(t, ts, inv("Email/get", args, "0"))
	res := methodArgs(t, r, 0, "Email/get")
	list, ok := res["list"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("Email/get list: %v", res)
	}
	return list[0].(map[string]any)
}

const simpleMessage = "From: Joe Bloggs <joe@example.com>\r\n" +
	"To: Jane Doe <jane@example.com>\r\n" +
	"Subject: Dinner on Thursday?\r\n" +
	"Message-ID: <msg1@example.com>\r\n" +
	"Date: Wed, 03 Mar 2021 10:00:00 +0000\r\n" +
	"List-Post: <mailto:list@example.com>\r\n" +
	"\r\n" +
	"Hi Jane, are you free on Thursday evening?\r\n"

func TestEmailGetShape(t *testing.T) {
	ts, db, store := emailServer(t)
	id := putEmail(t, db, store, simpleMessage, map[string]bool{"MBinbox": true}, nil)

	obj := emailGet(t, ts, id, "")

	// The RFC 8621 section 4.2 default property list (plus id).
	wantKeys := append([]string{}, emailDefaultGetProperties...)
	if len(obj) != len(wantKeys) {
		t.Fatalf("got %d keys %v, want %d", len(obj), keysOf(obj), len(wantKeys))
	}
	for _, k := range wantKeys {
		if _, has := obj[k]; !has {
			t.Fatalf("missing property %q in %v", k, keysOf(obj))
		}
	}

	if obj["subject"] != "Dinner on Thursday?" {
		t.Errorf("subject: %v", obj["subject"])
	}
	from := obj["from"].([]any)
	if len(from) != 1 || from[0].(map[string]any)["email"] != "joe@example.com" ||
		from[0].(map[string]any)["name"] != "Joe Bloggs" {
		t.Errorf("from: %v", obj["from"])
	}
	if mids := obj["messageId"].([]any); len(mids) != 1 || mids[0] != "msg1@example.com" {
		t.Errorf("messageId: %v", obj["messageId"])
	}
	if obj["hasAttachment"] != false {
		t.Errorf("hasAttachment: %v", obj["hasAttachment"])
	}
	if kw := obj["keywords"].(map[string]any); len(kw) != 0 {
		t.Errorf("keywords: %v", kw)
	}
	if mb := obj["mailboxIds"].(map[string]any); mb["MBinbox"] != true || len(mb) != 1 {
		t.Errorf("mailboxIds: %v", mb)
	}
	// A plain text message: one text/plain part, present in both textBody
	// and htmlBody, no attachments, and no bodyValues without a fetch flag.
	if tb := obj["textBody"].([]any); len(tb) != 1 || tb[0].(map[string]any)["type"] != "text/plain" {
		t.Errorf("textBody: %v", obj["textBody"])
	}
	if hb := obj["htmlBody"].([]any); len(hb) != 1 {
		t.Errorf("htmlBody: %v", obj["htmlBody"])
	}
	if att := obj["attachments"].([]any); len(att) != 0 {
		t.Errorf("attachments: %v", att)
	}
	if bv := obj["bodyValues"].(map[string]any); len(bv) != 0 {
		t.Errorf("bodyValues without fetch flag: %v", bv)
	}
}

const altMessage = "From: a@example.com\r\n" +
	"Subject: multipart\r\n" +
	"Content-Type: multipart/alternative; boundary=b\r\n" +
	"\r\n" +
	"--b\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"plain body text here\r\n" +
	"--b\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>html body</p>\r\n" +
	"--b--\r\n"

func TestEmailGetBodyValues(t *testing.T) {
	ts, db, store := emailServer(t)
	id := putEmail(t, db, store, altMessage, map[string]bool{"MBinbox": true}, nil)

	obj := emailGet(t, ts, id,
		`,"fetchTextBodyValues":true,"fetchHTMLBodyValues":true`)
	bv := obj["bodyValues"].(map[string]any)
	if len(bv) != 2 {
		t.Fatalf("bodyValues: %v", bv)
	}
	// The text/plain part (partId "1") value round-trips.
	one := bv["1"].(map[string]any)
	if one["value"] != "plain body text here" || one["isTruncated"] != false {
		t.Errorf("bodyValues[1]: %v", one)
	}

	// maxBodyValueBytes truncates on a codepoint boundary and flags it.
	obj = emailGet(t, ts, id,
		`,"fetchTextBodyValues":true,"maxBodyValueBytes":5`)
	one = obj["bodyValues"].(map[string]any)["1"].(map[string]any)
	if v := one["value"].(string); len(v) > 5 || one["isTruncated"] != true {
		t.Errorf("truncated value: %q trunc=%v", v, one["isTruncated"])
	}
}

func TestEmailHeaderForms(t *testing.T) {
	ts, db, store := emailServer(t)
	id := putEmail(t, db, store, simpleMessage, map[string]bool{"MBinbox": true}, nil)

	obj := emailGet(t, ts, id, `,"properties":["header:Subject:asText",`+
		`"header:To:asAddresses","header:Message-ID:asMessageIds",`+
		`"header:Date:asDate","header:List-Post:asURLs"]`)

	if obj["header:Subject:asText"] != "Dinner on Thursday?" {
		t.Errorf("asText: %v", obj["header:Subject:asText"])
	}
	to := obj["header:To:asAddresses"].([]any)
	if len(to) != 1 || to[0].(map[string]any)["email"] != "jane@example.com" {
		t.Errorf("asAddresses: %v", obj["header:To:asAddresses"])
	}
	if mids := obj["header:Message-ID:asMessageIds"].([]any); len(mids) != 1 || mids[0] != "msg1@example.com" {
		t.Errorf("asMessageIds: %v", obj["header:Message-ID:asMessageIds"])
	}
	if !strings.HasPrefix(obj["header:Date:asDate"].(string), "2021-03-03T10:00:00") {
		t.Errorf("asDate: %v", obj["header:Date:asDate"])
	}
	if urls := obj["header:List-Post:asURLs"].([]any); len(urls) != 1 || urls[0] != "mailto:list@example.com" {
		t.Errorf("asURLs: %v", obj["header:List-Post:asURLs"])
	}
}

func TestEmailHeaderFormForbidden(t *testing.T) {
	ts, db, store := emailServer(t)
	id := putEmail(t, db, store, simpleMessage, map[string]bool{"MBinbox": true}, nil)

	// asDate on an address field is inappropriate -> invalidArguments
	// (RFC 8621 section 4.2), which fails the whole method call.
	r := callMail(t, ts, inv("Email/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q],"properties":["header:From:asDate"]}`, testAccount, id), "0"))
	got := r.MethodResponses[0]
	if got.Name != "error" {
		t.Fatalf("want error, got %s %s", got.Name, got.Args)
	}
	var e map[string]any
	json.Unmarshal(got.Args, &e)
	if e["type"] != "invalidArguments" {
		t.Fatalf("want invalidArguments: %v", e)
	}
}

// TestEmailStoredMatchesComputed proves the stored fast-list value and the
// on-demand header:{name} form derive from the same parse and agree.
func TestEmailStoredMatchesComputed(t *testing.T) {
	ts, db, store := emailServer(t)
	id := putEmail(t, db, store, simpleMessage, map[string]bool{"MBinbox": true}, nil)

	obj := emailGet(t, ts, id, `,"properties":["from","header:From:asAddresses","subject","header:Subject:asText"]`)
	if fmt.Sprint(obj["from"]) != fmt.Sprint(obj["header:From:asAddresses"]) {
		t.Errorf("from stored %v vs computed %v", obj["from"], obj["header:From:asAddresses"])
	}
	if obj["subject"] != obj["header:Subject:asText"] {
		t.Errorf("subject stored %v vs computed %v", obj["subject"], obj["header:Subject:asText"])
	}
}

func TestEmailSetCreateRejected(t *testing.T) {
	ts, db, store := emailServer(t)
	_ = putEmail(t, db, store, simpleMessage, map[string]bool{"MBinbox": true}, nil)

	r := callMail(t, ts, inv("Email/set",
		fmt.Sprintf(`{"accountId":%q,"create":{"c":{"mailboxIds":{"MBinbox":true}}}}`, testAccount), "0"))
	args := methodArgs(t, r, 0, "Email/set")
	se := args["notCreated"].(map[string]any)["c"].(map[string]any)
	if se["type"] != "forbidden" {
		t.Fatalf("want forbidden, got %v", se)
	}
}

func TestEmailSetKeywords(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	id := putEmail(t, db, store, simpleMessage, map[string]bool{inbox: true}, nil)

	update := func(patch string) map[string]any {
		r := callMail(t, ts, inv("Email/set",
			fmt.Sprintf(`{"accountId":%q,"update":{%q:%s}}`, testAccount, id, patch), "0"))
		return methodArgs(t, r, 0, "Email/set")
	}

	// Member patch adds a keyword; an uppercase keyword is lowercased.
	if _, ok := update(`{"keywords/$seen":true,"keywords/Foo":true}`)["updated"].(map[string]any)[id]; !ok {
		t.Fatal("keyword add rejected")
	}
	kw := emailGet(t, ts, id, `,"properties":["keywords"]`)["keywords"].(map[string]any)
	if kw["$seen"] != true || kw["foo"] != true || len(kw) != 2 {
		t.Fatalf("keywords: %v", kw)
	}

	// Invalid keyword syntax (space) and value false are both rejected.
	se := update(`{"keywords/bad kw":true}`)["notUpdated"].(map[string]any)[id].(map[string]any)
	if se["type"] != "invalidProperties" {
		t.Fatalf("bad keyword: %v", se)
	}
	se = update(`{"keywords/x":false}`)["notUpdated"].(map[string]any)[id].(map[string]any)
	if se["type"] != "invalidProperties" {
		t.Fatalf("false value: %v", se)
	}

	// Exceeding the keyword limit is tooManyKeywords.
	big := map[string]bool{}
	for i := 0; i < maxKeywordsPerEmail+1; i++ {
		big[fmt.Sprintf("k%d", i)] = true
	}
	bigJSON, _ := json.Marshal(big)
	se = update(fmt.Sprintf(`{"keywords":%s}`, bigJSON))["notUpdated"].(map[string]any)[id].(map[string]any)
	if se["type"] != "tooManyKeywords" {
		t.Fatalf("want tooManyKeywords: %v", se)
	}
}

func TestEmailSetMailboxes(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	archive := createMailbox(t, ts, `{"name":"Archive","role":"archive"}`)
	id := putEmail(t, db, store, simpleMessage, map[string]bool{inbox: true}, nil)

	update := func(patch string) map[string]any {
		r := callMail(t, ts, inv("Email/set",
			fmt.Sprintf(`{"accountId":%q,"update":{%q:%s}}`, testAccount, id, patch), "0"))
		return methodArgs(t, r, 0, "Email/set")
	}

	// Move: add archive, remove inbox in one patch.
	if _, ok := update(fmt.Sprintf(`{"mailboxIds/%s":true,"mailboxIds/%s":null}`, archive, inbox))["updated"].(map[string]any)[id]; !ok {
		t.Fatal("move rejected")
	}
	mb := emailGet(t, ts, id, `,"properties":["mailboxIds"]`)["mailboxIds"].(map[string]any)
	if mb[archive] != true || len(mb) != 1 {
		t.Fatalf("after move: %v", mb)
	}

	// Removing the last mailbox violates the >= 1 invariant.
	se := update(fmt.Sprintf(`{"mailboxIds/%s":null}`, archive))["notUpdated"].(map[string]any)[id].(map[string]any)
	wantInvalidProp(t, se, "mailboxIds")

	// A nonexistent mailbox is rejected.
	se = update(`{"mailboxIds/MBnope":true}`)["notUpdated"].(map[string]any)[id].(map[string]any)
	wantInvalidProp(t, se, "mailboxIds")
}

func TestEmailTooManyMailboxes(t *testing.T) {
	acctCap := DefaultAccountCapability()
	one := int64(1)
	acctCap.MaxMailboxesPerEmail = &one
	ts, db, store := newEmailServer(t, acctCap)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	archive := createMailbox(t, ts, `{"name":"Archive","role":"archive"}`)
	id := putEmail(t, db, store, simpleMessage, map[string]bool{inbox: true}, nil)

	r := callMail(t, ts, inv("Email/set",
		fmt.Sprintf(`{"accountId":%q,"update":{%q:{"mailboxIds/%s":true}}}`, testAccount, id, archive), "0"))
	se := methodArgs(t, r, 0, "Email/set")["notUpdated"].(map[string]any)[id].(map[string]any)
	if se["type"] != "tooManyMailboxes" {
		t.Fatalf("want tooManyMailboxes: %v", se)
	}
}

func TestEmailDestroyAndChanges(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	id := putEmail(t, db, store, simpleMessage, map[string]bool{inbox: true}, nil)

	// State before the update, for Email/changes.
	before := methodArgs(t, callMail(t, ts, inv("Email/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q]}`, testAccount, id), "0")), 0, "Email/get")["state"].(string)

	callMail(t, ts, inv("Email/set",
		fmt.Sprintf(`{"accountId":%q,"update":{%q:{"keywords/$seen":true}}}`, testAccount, id), "0"))

	ch := methodArgs(t, callMail(t, ts, inv("Email/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testAccount, before), "0")), 0, "Email/changes")
	if upd := ch["updated"].([]any); len(upd) != 1 || upd[0] != id {
		t.Fatalf("changes updated: %v", ch)
	}

	// Destroy removes it from the store view.
	r := callMail(t, ts, inv("Email/set",
		fmt.Sprintf(`{"accountId":%q,"destroy":[%q]}`, testAccount, id), "0"))
	if d := methodArgs(t, r, 0, "Email/set")["destroyed"].([]any); len(d) != 1 || d[0] != id {
		t.Fatalf("destroyed: %v", d)
	}
	nf := methodArgs(t, callMail(t, ts, inv("Email/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q]}`, testAccount, id), "0")), 0, "Email/get")["notFound"].([]any)
	if len(nf) != 1 || nf[0] != id {
		t.Fatalf("notFound: %v", nf)
	}
}

// rfc421Message mirrors the RFC 8621 section 4.2.1 Email/get example: a
// message from Joe Bloggs, subject "Dinner on Thursday?", a List-POST
// header, and an HTML body.
const rfc421Message = "From: Joe Bloggs <joe@example.com>\r\n" +
	"To: partytime@lists.example.com\r\n" +
	"Subject: Dinner on Thursday?\r\n" +
	"List-POST: <mailto:partytime@lists.example.com>\r\n" +
	"Date: Wed, 03 Mar 2021 10:00:00 +0000\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<html><body><p>Hello, are you free for dinner on Thursday?</p></body></html>\r\n"

// TestEmailGetRFC421Example issues the exact request shape from RFC 8621
// section 4.2.1 (the same properties, bodyProperties, fetchHTMLBodyValues,
// and maxBodyValueBytes) and checks the documented response shape.
func TestEmailGetRFC421Example(t *testing.T) {
	ts, db, store := emailServer(t)
	id := putEmail(t, db, store, rfc421Message, map[string]bool{"MBf123": true}, nil)

	obj := emailGet(t, ts, id, `,"properties":["threadId","mailboxIds","from",`+
		`"subject","receivedAt","header:List-POST:asURLs","htmlBody","bodyValues"],`+
		`"bodyProperties":["partId","blobId","size","type"],`+
		`"fetchHTMLBodyValues":true,"maxBodyValueBytes":256`)

	// Exactly the requested properties (plus the always-present id).
	wantKeys := []string{"id", "threadId", "mailboxIds", "from", "subject",
		"receivedAt", "header:List-POST:asURLs", "htmlBody", "bodyValues"}
	if len(obj) != len(wantKeys) {
		t.Fatalf("keys %v, want %v", keysOf(obj), wantKeys)
	}
	for _, k := range wantKeys {
		if _, has := obj[k]; !has {
			t.Fatalf("missing %q in %v", k, keysOf(obj))
		}
	}

	from := obj["from"].([]any)
	if len(from) != 1 || from[0].(map[string]any)["name"] != "Joe Bloggs" ||
		from[0].(map[string]any)["email"] != "joe@example.com" {
		t.Errorf("from: %v", obj["from"])
	}
	if urls := obj["header:List-POST:asURLs"].([]any); len(urls) != 1 ||
		urls[0] != "mailto:partytime@lists.example.com" {
		t.Errorf("List-POST asURLs: %v", obj["header:List-POST:asURLs"])
	}
	// Each htmlBody part carries only the requested bodyProperties.
	for _, p := range obj["htmlBody"].([]any) {
		part := p.(map[string]any)
		for k := range part {
			switch k {
			case "partId", "blobId", "size", "type":
			default:
				t.Errorf("htmlBody part has unrequested property %q: %v", k, part)
			}
		}
		if part["type"] != "text/html" {
			t.Errorf("htmlBody part type: %v", part["type"])
		}
	}
	if bv := obj["bodyValues"].(map[string]any); len(bv) != 1 {
		t.Errorf("bodyValues: %v", bv)
	}
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
