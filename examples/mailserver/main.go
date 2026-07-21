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
// Or over Postgres instead of SQLite (-postgres replaces -db entirely; several
// instances may share one database as a fleet - see drivers/postgres):
//
//	go run ./examples/mailserver -postgres 'postgres://user:pass@localhost:5432/naust'
//
// JMAP requests carry a bearer token, not the password directly: log in once
// to mint one (the argon2id password check runs only here, never per request),
// then send the token on every call:
//
//	TOKEN=$(curl -s -X POST -u demo@example.com:demo http://localhost:8080/login)
//
// A fresh account has no mailboxes, so first create an Inbox. Delivery to an
// account with no inbox-role Mailbox tempfails by design, so this step is
// required once:
//
//	curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8080/api \
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
//	curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8080/api \
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
//	curl -s -H "Authorization: Bearer $TOKEN" \
//	  'http://localhost:8080/eventsource?types=EmailDelivery,Email&closeafter=no&ping=30'
//
// Sending is wired too (urn:ietf:params:jmap:submission): demo@example.com
// has an Identity pre-provisioned (find its id with Identity/get) and may
// send as itself. With no -relay flag the server runs in loopback mode -
// "sending" delivers through the local pipeline, so mail addressed to an
// account this server hosts lands in that inbox and anything else is
// rejected per recipient, exactly as a relay would report an unknown
// mailbox. Point -relay at a real smarthost (with -relay-user, -relay-pass,
// -relay-tls) to relay outbound instead.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/chunkstore"
	"github.com/naust-mail/naust-jmap/core/providers/blob/fsstore"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/pushsub"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/core/webpush"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/drivers/postgres"
	"github.com/naust-mail/naust-jmap/drivers/sqlite"
	"github.com/naust-mail/naust-jmap/examples/internal/tokenauth"
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

// loopbackSubmitter is the no-relay default: it "sends" by handing the
// message to the local Deliverer, so the demo works with no smarthost
// and a submission to a locally hosted address genuinely arrives in that
// inbox over the same pipeline as LMTP. It implements the same Submitter
// socket a real embedder plugs (mail.SMTPRelay is the real one), and it
// is honest about scope: a recipient this server does not host is
// rejected per recipient, as a relay would report an unknown mailbox.
type loopbackSubmitter struct {
	d *mail.Deliverer
}

func (l loopbackSubmitter) Submit(ctx context.Context, env mail.SubmissionEnvelope, msg io.Reader) ([]mail.RecipientResult, error) {
	rcpts := make([]string, len(env.Recipients))
	for i, r := range env.Recipients {
		rcpts[i] = r.Email
	}
	events := l.d.Deliver(ctx, mail.Envelope{MailFrom: env.MailFrom, Recipients: rcpts}, msg)
	results := make([]mail.RecipientResult, 0, len(events))
	for _, ev := range events {
		var reply string
		switch ev.Outcome {
		case mail.Accepted:
			reply = "250 2.0.0 delivered to local mailbox"
		case mail.Rejected:
			reply = "550 5.1.1 no such local mailbox (loopback mode delivers to local accounts only)"
		default:
			reply = "451 4.3.0 local delivery failed: " + ev.Reason
		}
		results = append(results, mail.RecipientResult{Recipient: ev.Recipient, Outcome: ev.Outcome, Reply: reply})
	}
	return results, nil
}

// ensureIdentity provisions one Identity for the account through the
// normal front door (Identity/set) unless it already has one. There is
// deliberately no side-door helper API for this: identities are records
// the client can see, so setup creates them the same way a client would.
func ensureIdentity(proc *runtime.Processor, acct jmap.Id, email string) error {
	ident := &auth.Identity{
		Username: email,
		Accounts: map[jmap.Id]auth.Access{acct: {Name: email, Personal: true}},
	}
	using := []string{jmap.CoreCapability, mail.SubmissionCapabilityURI}
	call := func(name, args string) (json.RawMessage, error) {
		resp := proc.Process(context.Background(), &jmap.Request{
			Using:       using,
			MethodCalls: []jmap.Invocation{{Name: name, Args: json.RawMessage(args), CallID: "0"}},
		}, ident, "")
		if len(resp.MethodResponses) == 0 || resp.MethodResponses[0].Name != name {
			return nil, fmt.Errorf("%s failed: %v", name, resp.MethodResponses)
		}
		return resp.MethodResponses[0].Args, nil
	}
	got, err := call("Identity/get", fmt.Sprintf(`{"accountId":%q}`, acct))
	if err != nil {
		return err
	}
	var list struct {
		List []json.RawMessage `json:"list"`
	}
	if err := json.Unmarshal(got, &list); err != nil {
		return err
	}
	if len(list.List) > 0 {
		return nil
	}
	got, err = call("Identity/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"i":{"email":%q}}}`, acct, email))
	if err != nil {
		return err
	}
	var set struct {
		Created map[string]json.RawMessage `json:"created"`
	}
	if err := json.Unmarshal(got, &set); err != nil {
		return err
	}
	if len(set.Created) == 0 {
		return fmt.Errorf("identity for %s not created: %s", email, got)
	}
	return nil
}

func main() {
	dbPath := flag.String("db", "./naust-mail.db", "SQLite database file")
	pgDSN := flag.String("postgres", "", "Postgres DSN (e.g. postgres://user:pass@host:5432/db); when set, replaces the SQLite backend entirely and -db is ignored (instances sharing one database form a fleet: store lease + cross-instance push)")
	httpAddr := flag.String("http", "localhost:8080", "JMAP HTTP listen address")
	lmtpAddr := flag.String("lmtp", "127.0.0.1:2400", "LMTP listen address (never port 25)")
	blobStore := flag.String("blobs", "chunk", "raw-message blob store: chunk (streaming), kv (whole blob in one value), or fs (one file per message)")
	blobDir := flag.String("blob-dir", "./naust-blobs", "directory for raw messages, with -blobs fs")
	accounts := flag.Int("accounts", 0, "also register accounts u1..uN (for benchmarking delivery spread across accounts)")
	ingestInFlight := flag.Int("ingest-inflight", 64, "max concurrent /ingest requests before 503 (benchmarking)")
	relay := flag.String("relay", "", "outbound SMTP relay as host:port; empty runs loopback mode (sending delivers to local accounts only)")
	relayUser := flag.String("relay-user", "", "relay SASL PLAIN username (with -relay-pass)")
	relayPass := flag.String("relay-pass", "", "relay SASL PLAIN password")
	relayTLS := flag.String("relay-tls", "starttls", "relay TLS mode: starttls, implicit, or plain")
	flag.Parse()

	// Persistence: one backend is both the object store and the raw-message
	// blob store. Default is SQLite; -postgres swaps in the Postgres driver
	// instead.
	var store backend.Backend
	var pgStore *postgres.Store
	var err error
	if *pgDSN != "" {
		pgStore, err = postgres.Open(context.Background(), *pgDSN)
		store = pgStore
	} else {
		store, err = sqlite.Open(*dbPath)
	}
	if err != nil {
		log.Fatal(err)
	}

	// Coordination. With -postgres the deployment can be a fleet sharing one
	// database: a single LISTEN/NOTIFY hint transport per process accelerates
	// lease handoff and carries change notifications across instances, and the
	// writer lease becomes the store-backed one, so instances exclude each
	// other at Acquire instead of only fencing at commit. A single node (and
	// every non-Postgres backend) runs the in-process lease and notifier.
	var leases lease.Manager
	var notifier notify.Notifier
	if pgStore != nil {
		hints, err := postgres.OpenHints(context.Background(), pgStore)
		if err != nil {
			log.Fatal(err)
		}
		leases = lease.NewStoreLease(store, lease.StoreLeaseConfig{Waker: hints.Waker()})
		notifier = hints.Notifier()
	} else {
		leases = lease.NewInProcess(store)
		notifier = notify.NewInProcess()
	}
	db := objectdb.New(store, leases)

	// The raw-message blob store shares the same backend. Both options
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

	// Authentication models the production split: POST /login (registered on
	// the mux below) runs the password KDF once and mints a bearer token, and
	// the per-request path only verifies that token - so the argon2id cost is
	// paid at login, never on every JMAP call. Real embedders implement
	// auth.Authenticator against their own accounts, or verify tokens issued
	// by an external identity provider.
	//
	// demo@example.com is always present. -accounts additionally registers
	// u1..uN, which exists so a benchmark can spread delivery across accounts:
	// the writer lease is per ACCOUNT, so delivering everything to one account
	// measures lease contention rather than ingest throughput, and the two are
	// only distinguishable by varying this.
	users := tokenauth.New()
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

	// Sending (urn:ietf:params:jmap:submission): Identity and
	// EmailSubmission gated by a SendPolicy - deny by default, because a
	// permissive sending default is an open relay. Only demo may send here,
	// and only as itself.
	policy := mail.NewStaticSendPolicy()
	policy.Allow("Ademo", "demo@example.com")
	limits := mail.DefaultSubmissionLimits()
	if err := mail.RegisterIdentity(proc, db, policy, core); err != nil {
		log.Fatal(err)
	}
	queue, err := mail.RegisterEmailSubmission(proc, db, blobs, core, policy, limits)
	if err != nil {
		log.Fatal(err)
	}

	srv, err := runtime.NewServer(users, proc, "http://"+*httpAddr, core)
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.RegisterCapability(mail.CapabilityURI, struct{}{}, acctCap); err != nil {
		log.Fatal(err)
	}
	if err := srv.RegisterCapability(mail.SubmissionCapabilityURI, struct{}{}, mail.SubmissionAccountCapabilityFor(limits)); err != nil {
		log.Fatal(err)
	}
	// Binary data (RFC 8620 section 6) and push (section 7): blob
	// upload/download for Email/import, and live StateChange events - the
	// path by which EmailDelivery reaches subscribed clients.
	srv.EnableBlobs(db, blobs)
	if pgStore != nil {
		// A fleet delivers webpush (RFC 8620 section 7.2): the hint Notifier
		// carries changes across instances, and exactly one elected instance
		// POSTs, so N nodes do not each hit the push service (which rate-limits).
		subStore := pushsub.NewStore(store, leases)
		if err := srv.EnablePush(db, notifier, subStore, &webpush.Sender{}); err != nil {
			log.Fatal(err)
		}
		// A booting node is passive until elected, so it does not duplicate
		// another node's POSTs. The window between EnablePush (which starts
		// active) and here can cost one duplicate POST, which section 7.2 makes
		// harmless.
		if err := srv.SetWebpushActive(context.Background(), false); err != nil {
			log.Fatal(err)
		}
		// Elect one webpush sender fleet-wide. The holder sends; if it crashes,
		// another node takes over once the election claim expires.
		go lease.RunSingleton(context.Background(), store, "webpush/0", lease.SingletonConfig{}, func(ctx context.Context) {
			if err := srv.SetWebpushActive(ctx, true); err != nil {
				log.Printf("webpush activate: %v", err)
				return
			}
			<-ctx.Done()
			_ = srv.SetWebpushActive(context.Background(), false)
		})
	} else if err := srv.EnablePush(db, notifier, nil, nil); err != nil {
		// Single node: event source only (no webpush subscriptions or sender),
		// webpush trivially active, no election.
		log.Fatal(err)
	}

	// Delivery: the same Deliverer feeds both adapters, so LMTP and HTTP
	// ingest produce identical Emails and identical per-recipient verdicts.
	deliverer := mail.NewDeliverer(db, blobs, resolve)

	// The sending worker: real relay when -relay is set, loopback through
	// the local Deliverer otherwise. Either way it is the same Submitter
	// socket and the same queue engine.
	var submitter mail.Submitter
	if *relay != "" {
		cfg := mail.SMTPRelayConfig{Addr: *relay}
		switch *relayTLS {
		case "starttls":
			cfg.Mode = mail.RequireSTARTTLS
		case "implicit":
			cfg.Mode = mail.ImplicitTLS
		case "plain":
			// The operator chose plaintext explicitly, so credentials may
			// ride on it too (a localhost relay hop, typically).
			cfg.Mode = mail.Plaintext
			cfg.AllowPlaintextAuth = true
		default:
			log.Fatalf("-relay-tls must be starttls, implicit or plain, got %q", *relayTLS)
		}
		if *relayUser != "" {
			cfg.Auth = &mail.PlainAuth{Username: *relayUser, Password: *relayPass}
		}
		submitter, err = mail.NewSMTPRelay(cfg)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("relaying outbound mail via %s (%s)", *relay, *relayTLS)
	} else {
		submitter = loopbackSubmitter{d: deliverer}
		log.Printf("loopback sending: no -relay set, submissions deliver to local accounts only")
	}
	worker, err := mail.NewSubmissionWorker(queue, submitter, mail.SubmissionWorkerConfig{})
	if err != nil {
		log.Fatal(err)
	}
	go worker.Run(context.Background())

	// Sending needs an Identity; provision demo's up front so the demo
	// works on first login.
	if err := ensureIdentity(proc, "Ademo", "demo@example.com"); err != nil {
		log.Fatal(err)
	}

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
	root.Handle("/ingest", mail.NewHTTPIngest(deliverer, mail.WithMaxIngestInFlight(*ingestInFlight)))
	root.Handle("/login", users.LoginHandler())
	root.Handle("/", srv)

	log.Printf("JMAP at http://%s/.well-known/jmap (POST /login as demo@example.com / demo for a bearer token)", *httpAddr)
	log.Printf("LMTP at %s, HTTP ingest at http://%s/ingest", *lmtpAddr, *httpAddr)
	log.Fatal(http.ListenAndServe(*httpAddr, root))
}
