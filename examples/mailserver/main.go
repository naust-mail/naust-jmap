// Command mailserver is a complete, persistent JMAP mail server built on
// naust-jmap: the RFC 8621 Mailbox, Thread and Email types over the SQLite
// driver, with mail actually arriving through two delivery adapters (LMTP
// behind an MTA, and a plain HTTP ingest endpoint) and live push on the
// event-source stream. It is the integration counterpart to the quickstart:
// quickstart shows one derived datatype; this shows the whole mail plugin
// wired to a real backend and a real delivery path, as one process.
//
// The runtime owns protocol correctness and derives Mailbox/Email/Thread's
// get/query/set/changes from the descriptors; the mail package owns what the
// objects mean; the sqlite driver owns persistence. None of the three knows
// about the others beyond the interfaces below.
//
// Run it (writes ./naust-mail.db, LMTP on 127.0.0.1:2400, JMAP on :8080):
//
//	go run ./examples/mailserver
//
// A fresh account has no mailboxes, so first create an Inbox (user
// demo@example.com, password demo). Delivery to an account with no inbox-role
// Mailbox tempfails by design, so this step is required once:
//
//	curl -su demo@example.com:demo http://localhost:8080/api \
//	  -H 'Content-Type: application/json' -d '{
//	    "using": ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"],
//	    "methodCalls": [["Mailbox/set", {"accountId": "Ademo",
//	      "create": {"i": {"name": "Inbox", "role": "inbox"}}}, "0"]]
//	  }'
//
// Now deliver a message through the HTTP ingest adapter (envelope in headers,
// raw RFC 5322 message as the body). The response is one JSON result per
// recipient, matching what LMTP would answer on the wire:
//
//	printf 'From: a@example.net\r\nTo: demo@example.com\r\nSubject: hi\r\nMessage-ID: <1@example.net>\r\n\r\nfirst message\r\n' \
//	  | curl -s http://localhost:8080/ingest \
//	    -H 'X-Naust-Mail-From: a@example.net' \
//	    -H 'X-Naust-Rcpt-To: demo@example.com' --data-binary @-
//
// Or deliver over LMTP exactly as Postfix would (RFC 2033), e.g. with swaks:
//
//	swaks --server 127.0.0.1:2400 --protocol LMTP \
//	  --from a@example.net --to demo@example.com --header 'Subject: hi'
//
// Then read it back over JMAP, and watch the EmailDelivery push type advance
// on new mail alone (RFC 8621 section 1.5) while you deliver more:
//
//	curl -su demo@example.com:demo http://localhost:8080/api \
//	  -H 'Content-Type: application/json' -d '{
//	    "using": ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"],
//	    "methodCalls": [
//	      ["Email/query", {"accountId": "Ademo", "collapseThreads": true,
//	        "sort": [{"property": "receivedAt", "isAscending": false}]}, "0"],
//	      ["Email/get", {"accountId": "Ademo", "#ids":
//	        {"resultOf": "0", "name": "Email/query", "path": "/ids"},
//	        "properties": ["subject", "from", "receivedAt", "preview"]}, "1"]
//	    ]
//	  }'
//
//	curl -su demo@example.com:demo \
//	  'http://localhost:8080/eventsource?types=EmailDelivery,Email&closeafter=no&ping=30'
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/chunkstore"
	"github.com/naust-mail/naust-jmap/core/providers/blob/fsstore"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/drivers/sqlite"
)

// staticResolver maps envelope recipients to accounts. A real deployment
// resolves against its user directory (which addresses are local, aliases,
// catch-alls); the delivery core bakes in no addressing scheme, so this is
// the host's job. Here one address maps to the demo account.
type staticResolver map[string]jmap.Id

func (m staticResolver) Resolve(_ context.Context, recipient string) (jmap.Id, bool) {
	id, ok := m[recipient]
	return id, ok
}

func main() {
	dbPath := flag.String("db", "./naust-mail.db", "SQLite database file")
	httpAddr := flag.String("http", "localhost:8080", "JMAP HTTP listen address")
	lmtpAddr := flag.String("lmtp", "127.0.0.1:2400", "LMTP listen address (never port 25)")
	blobStore := flag.String("blobs", "chunk", "raw-message blob store: chunk (streaming), kv (whole blob in one value), or fs (one file per message)")
	blobDir := flag.String("blob-dir", "./naust-blobs", "directory for raw messages, with -blobs fs")
	accounts := flag.Int("accounts", 0, "also register accounts u1..uN (for benchmarking delivery spread across accounts)")
	flag.Parse()

	// Persistence: one SQLite file is both the object backend and the
	// raw-message blob store. Swap sqlite.Open for another driver module and
	// nothing else changes.
	store, err := sqlite.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	db := objectdb.New(store, lease.NewInProcess(store))

	// The raw-message blob store shares the same SQLite file. Both options
	// satisfy blob.Store, so nothing downstream changes.
	//
	// chunkstore splits each blob into fixed pieces and never holds a whole
	// blob in memory, at the cost of a few writes per blob. It is the default
	// because the alternative's cost is not a constant factor: kvstore keeps
	// each blob in a SINGLE value, so an arriving message is materialised whole
	// in memory, and the peak scales with message size TIMES concurrent
	// deliveries. Measured on a real-LMTP ingest benchmark, a
	// 32 MiB attachment cost ~158 MB of heap under kvstore and stayed flat
	// under chunkstore. kvstore remains the cheaper choice when messages are
	// known to be small.
	//
	// fsstore is the third option and the only one that leaves the database:
	// each message is a file, written tmp-then-rename. It is the fastest, since
	// a transactional store has to journal every byte it takes, but it gives up
	// having blobs commit in the same transaction as the objects referencing
	// them - see its package doc for what that costs and why it is safe.
	var blobs blob.Store
	switch *blobStore {
	case "chunk":
		// Returns an error: it reclaims crash-orphaned pieces at startup.
		cs, err := chunkstore.New(store)
		if err != nil {
			log.Fatal(err)
		}
		blobs = cs
	case "kv":
		blobs = kvstore.New(store)
	case "fs":
		// Returns an error: it reclaims crashed uploads' temporary files.
		fs, err := fsstore.New(*blobDir)
		if err != nil {
			log.Fatal(err)
		}
		blobs = fs
	default:
		log.Fatalf("-blobs must be chunk, kv or fs, got %q", *blobStore)
	}

	// Authentication: a static user list. Real embedders implement
	// auth.Authenticator against their own accounts.
	//
	// demo@example.com is always present. -accounts additionally registers
	// u1..uN, which exists so a benchmark can spread delivery across accounts:
	// the writer lease is per ACCOUNT, so delivering everything to one account
	// measures lease contention rather than ingest throughput, and the two are
	// only distinguishable by varying this.
	users := auth.NewStatic()
	users.AddUser("demo@example.com", "demo", "Ademo")
	resolve := staticResolver{"demo@example.com": "Ademo"}
	for i := 1; i <= *accounts; i++ {
		addr := fmt.Sprintf("u%d@example.com", i)
		acct := jmap.Id(fmt.Sprintf("Au%d", i))
		users.AddUser(addr, "demo", acct)
		resolve[addr] = acct
	}

	// The mail plugin: Mailbox, Thread and Email registered on the processor,
	// enforcing (and advertising) the same AccountCapability limits. A nil
	// searcher uses the built-in substring Searcher.
	proc := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	acctCap := mail.DefaultAccountCapability()
	if err := mail.RegisterMailbox(proc, db, core); err != nil {
		log.Fatal(err)
	}
	if err := mail.RegisterThread(proc, db, core); err != nil {
		log.Fatal(err)
	}
	if err := mail.RegisterEmail(proc, db, blobs, core, acctCap, nil); err != nil {
		log.Fatal(err)
	}

	srv, err := runtime.NewServer(users, proc, "http://"+*httpAddr, core)
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.RegisterCapability(mail.CapabilityURI, struct{}{}, acctCap); err != nil {
		log.Fatal(err)
	}
	// Binary data (RFC 8620 section 6) and push (section 7): blob
	// upload/download for Email/import, and live StateChange events - the
	// path by which EmailDelivery reaches subscribed clients.
	srv.EnableBlobs(db, blobs)
	if err := srv.EnablePush(db, notify.NewInProcess(), nil, nil); err != nil {
		log.Fatal(err)
	}

	// Delivery: the same Deliverer feeds both adapters, so LMTP and HTTP
	// ingest produce identical Emails and identical per-recipient verdicts.
	deliverer := mail.NewDeliverer(db, blobs, resolve)

	// LMTP for an MTA (RFC 2033 requires a channel other than port 25).
	ln, err := net.Listen("tcp", *lmtpAddr)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		if err := mail.ServeLMTP(ln, deliverer, "mailserver.example"); err != nil {
			log.Fatal(err)
		}
	}()

	// HTTP ingest shares the address with JMAP: /ingest is the ingest
	// adapter, everything else is the JMAP server (/api, /.well-known/jmap,
	// /eventsource, the blob endpoints).
	root := http.NewServeMux()
	root.Handle("/ingest", mail.NewHTTPIngest(deliverer))
	root.Handle("/", srv)

	log.Printf("JMAP at http://%s/.well-known/jmap (demo@example.com / demo)", *httpAddr)
	log.Printf("LMTP at %s, HTTP ingest at http://%s/ingest", *lmtpAddr, *httpAddr)
	log.Fatal(http.ListenAndServe(*httpAddr, root))
}
