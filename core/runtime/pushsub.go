package runtime

// PushSubscription (RFC 8620 section 7.2): clients register an HTTPS
// URL; the server POSTs a PushVerification object immediately, makes
// no further requests until the client confirms the code, and then
// POSTs StateChange objects as data changes. The /get and /set methods
// here are the section 7.2.1/7.2.2 variants of the standard methods:
// no accountId, no state arguments.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/pushsub"
	"github.com/naust-mail/naust-jmap/core/webpush"
)

const (
	// subMaxLifetime caps a push subscription's expiry. The server sets
	// it when the client gives none and clamps client values beyond it.
	// Section 7.2 requires at least 48 hours and recommends at least 7
	// days for non-time-bounded credentials (Basic auth, API tokens);
	// this runtime has no view into credential lifetimes, so it always
	// applies the non-time-bounded policy.
	subMaxLifetime = 7 * 24 * time.Hour
	// pushTTL is the RFC 8030 section 5.2 TTL header value, in seconds,
	// on every POST. A StateChange stays true until the next one - and
	// the next one replaces it - so a long retention is harmless.
	pushTTL = 43200
	// backoff429 is how long delivery pauses after a 429: section 7.2
	// requires reducing push frequency, and pending changes coalesce
	// into one minimal StateChange while paused.
	backoff429 = time.Minute
	// createRateMax/createRateWindow bound subscription creation per
	// credential (section 8.6 requires a creation rate limit: every
	// create sends an unsolicited POST to a client-chosen URL).
	createRateMax    = 10
	createRateWindow = time.Hour
)

type pushSupport struct {
	db     *objectdb.DB
	n      notify.Notifier
	subs   *pushsub.Store
	sender *webpush.Sender

	// ctx parents every delivery goroutine; stop cancels them all.
	ctx  context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup

	mu       sync.Mutex
	watchers map[jmap.Id]*subWatcher
	// rates holds recent creation times per credential.
	rates map[string][]time.Time
}

type subWatcher struct{ cancel context.CancelFunc }

// EnablePush wires push (RFC 8620 section 7): db commits publish their
// touched types to n (the producer side); the /eventsource endpoint
// streams StateChange objects to connected clients (section 7.3); and
// the PushSubscription/get and /set methods with webpush delivery
// cover clients that cannot hold a connection open (section 7.2).
// Call before serving. The embedder's http.Server must not set a
// WriteTimeout that would sever long-lived event streams, and Close
// must be called to stop delivery goroutines.
//
// A nil subs or sender enables the event source only; that is NOT
// RFC 8620 conformant (section 7.2 has no opt-out) and is meant for
// development.
func (s *Server) EnablePush(db *objectdb.DB, n notify.Notifier, subs *pushsub.Store, sender *webpush.Sender) error {
	db.SetNotifier(n)
	ctx, stop := context.WithCancel(context.Background())
	p := &pushSupport{
		db: db, n: n, subs: subs, sender: sender,
		ctx: ctx, stop: stop,
		watchers: make(map[jmap.Id]*subWatcher),
		rates:    make(map[string][]time.Time),
	}
	s.push = p
	if subs == nil || sender == nil {
		return nil
	}
	s.proc.Register("PushSubscription/get", jmap.CoreCapability, p.getSubs)
	s.proc.Register("PushSubscription/set", jmap.CoreCapability, p.setSubs)
	// Deliveries survive restarts: resume watching every verified,
	// unexpired subscription.
	all, err := subs.All(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, sub := range all {
		if sub.Verified() && !sub.Expired(now) {
			p.startWatcher(sub)
		}
	}
	return nil
}

// Close stops push delivery goroutines and waits for them. The HTTP
// handler itself needs no shutdown.
func (s *Server) Close() {
	if s.push != nil {
		s.push.stop()
		s.push.wg.Wait()
	}
}

// pushSubProps is every wire property of a PushSubscription (7.2).
var pushSubProps = map[string]bool{
	"id": true, "deviceClientId": true, "url": true, "keys": true,
	"verificationCode": true, "expires": true, "types": true,
}

// PushSubscription/get (section 7.2.1): standard /get except there is
// no accountId and no state, and the url and keys values are never
// returned - they may be private to a device.
func (p *pushSupport) getSubs(ctx context.Context, call *Call) []jmap.Invocation {
	var args struct {
		Ids        *[]jmap.Id `json:"ids"`
		Properties *[]string  `json:"properties"`
	}
	if err := decodeArgs(call.Args, &args); err != nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}

	// Default properties are all except url and keys; requesting either
	// explicitly MUST be rejected with forbidden (7.2.1).
	props := []string{"id", "deviceClientId", "verificationCode", "expires", "types"}
	if args.Properties != nil {
		props = []string{"id"}
		for _, name := range *args.Properties {
			switch {
			case name == "url" || name == "keys":
				return fail(call.CallID, jmap.ErrForbidden, "the url and keys properties are never returned")
			case !pushSubProps[name]:
				return fail(call.CallID, jmap.ErrInvalidArguments, "unknown property "+name)
			case name != "id":
				props = append(props, name)
			}
		}
	}

	// Only subscriptions created by these credentials are visible (7.2).
	subs, err := p.subs.List(ctx, call.Identity.CredentialKey())
	if err != nil {
		return fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	byID := make(map[jmap.Id]*pushsub.Subscription, len(subs))
	order := make([]jmap.Id, 0, len(subs))
	for _, sub := range subs {
		byID[sub.Id] = sub
		order = append(order, sub.Id)
	}
	if args.Ids != nil {
		order = *args.Ids
	}

	list := make([]map[string]any, 0, len(order))
	notFound := []jmap.Id{}
	seen := make(map[jmap.Id]bool, len(order))
	for _, id := range order {
		if seen[id] {
			continue
		}
		seen[id] = true
		sub, ok := byID[id]
		if !ok {
			notFound = append(notFound, id)
			continue
		}
		obj := map[string]any{"id": sub.Id}
		for _, name := range props {
			switch name {
			case "deviceClientId":
				obj[name] = sub.DeviceClientId
			case "verificationCode":
				obj[name] = orNil(sub.VerificationCode)
			case "expires":
				obj[name] = sub.Expires
			case "types":
				obj[name] = sub.Types
			}
		}
		list = append(list, obj)
	}
	return reply("PushSubscription/get", call.CallID, map[string]any{
		"list": list, "notFound": notFound,
	})
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// PushSubscription/set (section 7.2.2): standard /set except there is
// no accountId, no ifInState, and no oldState/newState.
func (p *pushSupport) setSubs(ctx context.Context, call *Call) []jmap.Invocation {
	var args struct {
		Create  map[jmap.Id]json.RawMessage `json:"create"`
		Update  map[jmap.Id]json.RawMessage `json:"update"`
		Destroy []jmap.Id                   `json:"destroy"`
	}
	if err := decodeArgs(call.Args, &args); err != nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	credential := call.Identity.CredentialKey()
	result := struct {
		Created      map[jmap.Id]any           `json:"created,omitzero"`
		Updated      map[jmap.Id]any           `json:"updated,omitzero"`
		Destroyed    []jmap.Id                 `json:"destroyed,omitzero"`
		NotCreated   map[jmap.Id]jmap.SetError `json:"notCreated,omitzero"`
		NotUpdated   map[jmap.Id]jmap.SetError `json:"notUpdated,omitzero"`
		NotDestroyed map[jmap.Id]jmap.SetError `json:"notDestroyed,omitzero"`
	}{
		Created: map[jmap.Id]any{}, Updated: map[jmap.Id]any{}, Destroyed: []jmap.Id{},
		NotCreated: map[jmap.Id]jmap.SetError{}, NotUpdated: map[jmap.Id]jmap.SetError{},
		NotDestroyed: map[jmap.Id]jmap.SetError{},
	}

	for _, creationID := range sortedIds(mapKeys(args.Create)) {
		sub, echo, serr := p.createSub(ctx, call, credential, args.Create[creationID])
		if serr != nil {
			result.NotCreated[creationID] = *serr
			continue
		}
		call.CreatedIds[creationID] = sub.Id
		result.Created[creationID] = echo
		// The server MUST immediately push a PushVerification object to
		// the URL (7.2.2), and nothing else until the client verifies.
		p.sendVerification(sub)
	}

	for _, id := range sortedIds(mapKeys(args.Update)) {
		amended, serr := p.updateSub(ctx, credential, id, args.Update[id])
		if serr != nil {
			result.NotUpdated[id] = *serr
			continue
		}
		result.Updated[id] = amended
	}

	for _, id := range args.Destroy {
		err := p.subs.Destroy(ctx, credential, id)
		switch {
		case errors.Is(err, pushsub.ErrNotFound):
			result.NotDestroyed[id] = jmap.SetError{Type: jmap.SetErrNotFound}
		case err != nil:
			result.NotDestroyed[id] = jmap.SetError{Type: jmap.SetErrForbidden, Description: err.Error()}
		default:
			p.stopWatcher(id)
			result.Destroyed = append(result.Destroyed, id)
		}
	}
	return reply("PushSubscription/set", call.CallID, result)
}

func setErr(errType string, props ...string) *jmap.SetError {
	return &jmap.SetError{Type: errType, Properties: props}
}

// createSub validates one create (7.2 property rules), stores it, and
// returns the created-response echo: server-set and defaulted values.
func (p *pushSupport) createSub(ctx context.Context, call *Call, credential string, raw json.RawMessage) (*pushsub.Subscription, map[string]any, *jmap.SetError) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, setErr(jmap.SetErrInvalidProperties)
	}
	for name := range fields {
		// id is server-set; unknown properties do not exist.
		if !pushSubProps[name] || name == "id" {
			return nil, nil, setErr(jmap.SetErrInvalidProperties, name)
		}
	}
	sub := &pushsub.Subscription{Id: jmap.NewId(), Credential: credential}
	// The subscription delivers StateChange objects for every account
	// the creating identity can reach (section 7.1: changed covers all
	// accounts the user is subscribed to updates for).
	sub.Accounts = sortedIds(mapKeys(call.Identity.Accounts))
	echo := map[string]any{"id": sub.Id}

	if err := json.Unmarshal(need(fields, "deviceClientId"), &sub.DeviceClientId); err != nil || sub.DeviceClientId == "" {
		return nil, nil, setErr(jmap.SetErrInvalidProperties, "deviceClientId")
	}
	// The URL MUST begin with "https://" (7.2); parsing keeps garbage
	// out of the delivery path early.
	if err := json.Unmarshal(need(fields, "url"), &sub.URL); err != nil {
		return nil, nil, setErr(jmap.SetErrInvalidProperties, "url")
	}
	if u, err := url.Parse(sub.URL); err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return nil, nil, setErr(jmap.SetErrInvalidProperties, "url")
	}
	if rawKeys, ok := fields["keys"]; ok {
		if err := decodeArgs(rawKeys, &sub.Keys); err != nil {
			return nil, nil, setErr(jmap.SetErrInvalidProperties, "keys")
		}
	} else {
		echo["keys"] = nil
	}
	if sub.Keys != nil {
		if _, _, err := webpush.DecodeKeys(sub.Keys.P256dh, sub.Keys.Auth); err != nil {
			return nil, nil, setErr(jmap.SetErrInvalidProperties, "keys")
		}
	}
	// verificationCode MUST be null or omitted on create (7.2).
	if rawCode, ok := fields["verificationCode"]; ok && string(rawCode) != "null" {
		return nil, nil, setErr(jmap.SetErrInvalidProperties, "verificationCode")
	}
	var wantExpires string
	if rawExpires, ok := fields["expires"]; ok && string(rawExpires) != "null" {
		if err := json.Unmarshal(rawExpires, &wantExpires); err != nil || !jmap.ValidUTCDate(wantExpires) {
			return nil, nil, setErr(jmap.SetErrInvalidProperties, "expires")
		}
	}
	sub.Expires = clampExpiry(wantExpires)
	if sub.Expires != wantExpires {
		echo["expires"] = sub.Expires
	}
	if rawTypes, ok := fields["types"]; ok {
		if serr := p.decodeTypes(rawTypes, &sub.Types); serr != nil {
			return nil, nil, serr
		}
	} else {
		echo["types"] = nil
	}

	if !p.allowCreate(credential) {
		return nil, nil, &jmap.SetError{Type: jmap.SetErrRateLimit,
			Description: "push subscriptions are being created too quickly"}
	}
	sub.ExpectedCode = newVerificationCode()
	if err := p.subs.Create(ctx, sub); err != nil {
		if errors.Is(err, pushsub.ErrTooMany) {
			return nil, nil, &jmap.SetError{Type: jmap.SetErrOverQuota,
				Description: "too many push subscriptions for these credentials"}
		}
		return nil, nil, &jmap.SetError{Type: jmap.SetErrForbidden, Description: err.Error()}
	}
	return sub, echo, nil
}

// need returns the field's raw JSON, or "null" when absent so decoding
// fails uniformly for missing required properties.
func need(fields map[string]json.RawMessage, name string) json.RawMessage {
	if raw, ok := fields[name]; ok {
		return raw
	}
	return json.RawMessage("null")
}

// errBadPatch carries a SetError out of a store Update callback.
type errBadPatch struct{ serr *jmap.SetError }

func (e errBadPatch) Error() string { return "invalid patch" }

// updateSub applies one update. url and keys are immutable (7.2.2);
// verificationCode must match the code that was pushed; expires is
// clamped, with the amended value returned to the client.
func (p *pushSupport) updateSub(ctx context.Context, credential string, id jmap.Id, raw json.RawMessage) (any, *jmap.SetError) {
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(raw, &patch); err != nil {
		return nil, setErr(jmap.SetErrInvalidPatch)
	}
	var amended map[string]any
	var verified *pushsub.Subscription
	err := p.subs.Update(ctx, credential, id, func(sub *pushsub.Subscription) error {
		for _, path := range sortedKeys(patch) {
			value := patch[path]
			switch path {
			case "verificationCode":
				var code string
				if err := json.Unmarshal(value, &code); err != nil {
					return errBadPatch{setErr(jmap.SetErrInvalidProperties, path)}
				}
				// An invalid verification code MUST be rejected (7.2.2).
				if subtle.ConstantTimeCompare([]byte(code), []byte(sub.ExpectedCode)) != 1 {
					return errBadPatch{setErr(jmap.SetErrInvalidProperties, path)}
				}
				wasVerified := sub.Verified()
				sub.VerificationCode = code
				if !wasVerified {
					verified = sub
				}
			case "expires":
				var want string
				if string(value) != "null" {
					if err := json.Unmarshal(value, &want); err != nil || !jmap.ValidUTCDate(want) {
						return errBadPatch{setErr(jmap.SetErrInvalidProperties, path)}
					}
				}
				// Extending the lifetime does not require re-verification
				// (7.2.2); the server clamps and reports what it kept.
				sub.Expires = clampExpiry(want)
				if sub.Expires != want {
					if amended == nil {
						amended = map[string]any{}
					}
					amended["expires"] = sub.Expires
				}
			case "types":
				if serr := p.decodeTypes(value, &sub.Types); serr != nil {
					return errBadPatch{serr}
				}
			default:
				// id, deviceClientId, url, and keys are immutable; nothing
				// nested is patchable.
				return errBadPatch{setErr(jmap.SetErrInvalidProperties, path)}
			}
		}
		return nil
	})
	var bad errBadPatch
	switch {
	case errors.As(err, &bad):
		return nil, bad.serr
	case errors.Is(err, pushsub.ErrNotFound):
		return nil, setErr(jmap.SetErrNotFound)
	case err != nil:
		return nil, &jmap.SetError{Type: jmap.SetErrForbidden, Description: err.Error()}
	}
	if verified != nil {
		p.startWatcher(verified)
	}
	if amended == nil {
		return nil, nil
	}
	return amended, nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// decodeTypes validates a types value: null for all types, else a list
// of known type names (7.2 uses the TypeState names of section 7.1).
func (p *pushSupport) decodeTypes(raw json.RawMessage, out *[]string) *jmap.SetError {
	*out = nil
	if string(raw) == "null" {
		return nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return setErr(jmap.SetErrInvalidProperties, "types")
	}
	known := make(map[string]bool)
	for _, name := range p.db.TypeNames() {
		known[name] = true
	}
	for _, name := range names {
		if !known[name] {
			return setErr(jmap.SetErrInvalidProperties, "types")
		}
	}
	*out = names
	return nil
}

// clampExpiry applies the section 7.2 expiry policy: no client value
// (or one beyond the maximum) becomes now+subMaxLifetime.
func clampExpiry(want string) string {
	max := time.Now().UTC().Add(subMaxLifetime).Truncate(time.Second)
	if want != "" {
		if t, err := time.Parse(time.RFC3339, want); err == nil && t.Before(max) {
			return want
		}
	}
	return max.Format(time.RFC3339)
}

// newVerificationCode returns 128 bits of entropy in hex: enough that
// brute-forcing the update endpoint is hopeless (section 8.6).
func newVerificationCode() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("runtime: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// allowCreate is the creation rate limiter (section 8.6).
func (p *pushSupport) allowCreate(credential string) bool {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	recent := p.rates[credential][:0]
	for _, t := range p.rates[credential] {
		if now.Sub(t) < createRateWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= createRateMax {
		p.rates[credential] = recent
		return false
	}
	p.rates[credential] = append(recent, now)
	return true
}

// sendVerification POSTs the PushVerification object (7.2.2) in the
// background: the client MUST be able to handle the push arriving
// while the create response is still in flight, so nothing waits on
// it. One shot - push is lossy, and a client that missed it destroys
// and recreates the subscription.
func (p *pushSupport) sendVerification(sub *pushsub.Subscription) {
	payload, err := json.Marshal(jmap.PushVerification{
		Type:               "PushVerification",
		PushSubscriptionId: sub.Id,
		VerificationCode:   sub.ExpectedCode,
	})
	if err != nil {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
		defer cancel()
		_, _ = p.sender.Send(ctx, sub.URL, sub.Keys, payload, pushTTL)
	}()
}

// startWatcher begins StateChange delivery for a verified subscription.
func (p *pushSupport) startWatcher(sub *pushsub.Subscription) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, running := p.watchers[sub.Id]; running {
		return
	}
	ctx, cancel := context.WithCancel(p.ctx)
	w := &subWatcher{cancel: cancel}
	p.watchers[sub.Id] = w
	p.wg.Add(1)
	go p.watch(ctx, w, sub.Credential, sub.Id, sub.Accounts)
}

func (p *pushSupport) stopWatcher(id jmap.Id) {
	p.mu.Lock()
	w := p.watchers[id]
	delete(p.watchers, id)
	p.mu.Unlock()
	if w != nil {
		w.cancel()
	}
}

// watch is one subscription's delivery loop: wait for coalesced
// changes, re-read the record (updates and destroys take effect here),
// filter to the subscribed types, POST the StateChange.
func (p *pushSupport) watch(ctx context.Context, w *subWatcher, credential string, id jmap.Id, accounts []jmap.Id) {
	defer p.wg.Done()
	defer func() {
		p.mu.Lock()
		if p.watchers[id] == w {
			delete(p.watchers, id)
		}
		p.mu.Unlock()
	}()
	nsub, err := p.n.Subscribe(ctx, accounts)
	if err != nil {
		return
	}
	defer nsub.Close()

	for {
		changes, err := nsub.Wait(ctx)
		if err != nil {
			return
		}
		sub, err := p.subs.Get(ctx, credential, id)
		if err != nil {
			return // destroyed, or storage is failing; either way stop
		}
		// The server MUST NOT make requests past the expiry (7.2).
		if !sub.Verified() || sub.Expired(time.Now()) {
			return
		}
		filtered := filterTypes(changes, sub.Types)
		if len(filtered) == 0 {
			continue
		}
		payload, err := json.Marshal(jmap.StateChange{Type: "StateChange", Changed: filtered})
		if err != nil {
			continue
		}
		status, err := p.sender.Send(ctx, sub.URL, sub.Keys, payload, pushTTL)
		if err != nil {
			continue // push is lossy by design; the client resyncs
		}
		// A 429 MUST reduce push frequency (7.2); changes arriving
		// during the pause coalesce into the next StateChange.
		if status == 429 {
			select {
			case <-time.After(backoff429):
			case <-ctx.Done():
				return
			}
		}
	}
}

// filterTypes drops types the subscription did not ask for; nil means
// all types (7.2). Accounts with nothing left are omitted entirely.
func filterTypes(changes notify.Changes, types []string) map[jmap.Id]jmap.TypeState {
	out := make(map[jmap.Id]jmap.TypeState, len(changes))
	want := map[string]bool{}
	for _, name := range types {
		want[name] = true
	}
	for acct, ts := range changes {
		keep := make(jmap.TypeState, len(ts))
		for name, state := range ts {
			if types == nil || want[name] {
				keep[name] = state
			}
		}
		if len(keep) > 0 {
			out[acct] = keep
		}
	}
	return out
}
