package websocket

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/naust-mail/naust-jmap/capabilities/websocket/internal/frame"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
)

// Push notifications over the WebSocket (RFC 8887 section 4.3.5): a
// WebSocketPushEnable subscribes the connection, one full-state
// StateChange snapshot is sent, and further changes stream until
// WebSocketPushDisable or the connection ends.
//
// This server never issues pushState tokens. Section 4.3.5.1
// contemplates exactly that: a server without pushState support leaves
// the client to resync via /changes on reconnection - and the
// connect-time snapshot below gives the equivalent guarantee without
// tokens, because the first StateChange after enabling always carries
// the complete current state for every requested type. An incoming
// pushState property is ignored for the same reason: the snapshot
// already covers everything that happened while the client was away.

// EnablePush wires push support: db supplies type names and current
// states, n delivers post-commit change notifications (the same pair
// handed to the runtime's EnablePush). Call before serving; without
// it, WebSocketPushEnable is answered with a request-level error and
// supportsPush must be advertised false.
func (h *Handler) EnablePush(db *objectdb.DB, n notify.Notifier) {
	db.SetNotifier(n)
	h.db, h.n = db, n
}

// SupportsPush reports whether EnablePush has been called; it is the
// value to advertise in the section 3 capability object.
func (h *Handler) SupportsPush() bool { return h.n != nil }

func (c *conn) handlePushEnable(id *string, payload []byte) {
	if c.h.n == nil {
		c.writeRequestError(id, jmap.RequestError{Type: jmap.ProblemNotRequest, Status: 400,
			Detail: "push is not supported on this connection"})
		return
	}
	var pe struct {
		// dataTypes null (or absent) means all types (section 4.3.5.2);
		// pushState is deliberately not read - see the package note.
		DataTypes *[]string `json:"dataTypes"`
	}
	if err := json.Unmarshal(payload, &pe); err != nil {
		c.writeRequestError(id, jmap.RequestError{Type: jmap.ProblemNotRequest, Status: 400,
			Detail: "malformed WebSocketPushEnable object"})
		return
	}
	types, ok := c.resolveTypes(pe.DataTypes)
	if !ok {
		// Unknown names would silently never fire, which always means a
		// client bug; reject them (the event source does the same).
		c.writeRequestError(id, jmap.RequestError{Type: jmap.ProblemNotRequest, Status: 400,
			Detail: "dataTypes contains an unknown type name"})
		return
	}

	// Reconfigure: a second enable replaces the previous subscription.
	c.pushMu.Lock()
	if c.pushCancel != nil {
		c.pushCancel()
	}
	pushCtx, cancel := context.WithCancel(c.ctx)
	c.pushCancel = cancel
	c.pushMu.Unlock()

	// Changes are pushed for every account the user has access to
	// (RFC 8620 section 7.1). Subscribe BEFORE reading current state so
	// no commit can fall between the two: a change landing during the
	// snapshot read is delivered by the stream as well, and duplicate
	// states are harmless.
	accounts := make([]jmap.Id, 0, len(c.ident.Accounts))
	for id := range c.ident.Accounts {
		accounts = append(accounts, id)
	}
	sub, err := c.h.n.Subscribe(pushCtx, accounts)
	if err != nil {
		c.writeRequestError(id, jmap.RequestError{Type: jmap.ProblemNotRequest, Status: 500,
			Detail: "subscribe failed"})
		return
	}

	snapshot := make(notify.Changes, len(accounts))
	for _, acct := range accounts {
		ts := make(jmap.TypeState, len(types))
		for _, name := range types {
			state, err := c.h.db.TypeState(pushCtx, acct, name)
			if err != nil {
				sub.Close()
				c.writeRequestError(id, jmap.RequestError{Type: jmap.ProblemNotRequest, Status: 500,
					Detail: "state read failed"})
				return
			}
			ts[name] = state
		}
		snapshot[acct] = ts
	}

	c.pushOn.Store(true)
	if !c.writeStateChange(snapshot) {
		sub.Close()
		return
	}
	wantType := make(map[string]bool, len(types))
	for _, name := range types {
		wantType[name] = true
	}
	// The stream starts only after the snapshot is written, so the
	// snapshot is always the connection's first StateChange. It joins
	// no drain group: it is not a request, and it ends when its context
	// (disable, reconfigure, or connection teardown) is canceled.
	go c.streamPush(pushCtx, sub, wantType)
}

func (c *conn) handlePushDisable() {
	// Disabling when nothing is enabled is a no-op (section 4.3.5.3
	// defines no error for it).
	c.pushMu.Lock()
	if c.pushCancel != nil {
		c.pushCancel()
		c.pushCancel = nil
	}
	c.pushMu.Unlock()
	c.pushOn.Store(false)
}

// resolveTypes maps the dataTypes property to concrete type names:
// null means every supported type; unknown names are rejected.
func (c *conn) resolveTypes(dataTypes *[]string) ([]string, bool) {
	all := c.h.db.TypeNames()
	if dataTypes == nil {
		return all, true
	}
	known := make(map[string]bool, len(all))
	for _, name := range all {
		known[name] = true
	}
	set := make(map[string]bool)
	for _, name := range *dataTypes {
		if !known[name] {
			return nil, false
		}
		set[name] = true
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, true
}

// streamPush forwards filtered change notifications until the
// subscription or connection ends.
func (c *conn) streamPush(ctx context.Context, sub notify.Subscription, wantType map[string]bool) {
	defer sub.Close()
	for {
		changes, err := sub.Wait(ctx)
		if err != nil {
			return
		}
		// Only requested types are pushed; others are omitted from the
		// TypeState object, and an empty event is not sent at all
		// (mirrors RFC 8620 section 7.3's types filtering).
		filtered := make(notify.Changes, len(changes))
		for acct, ts := range changes {
			keep := make(jmap.TypeState, len(ts))
			for name, state := range ts {
				if wantType[name] {
					keep[name] = state
				}
			}
			if len(keep) > 0 {
				filtered[acct] = keep
			}
		}
		if len(filtered) == 0 {
			continue
		}
		if !c.writeStateChange(filtered) {
			return
		}
	}
}

// writeStateChange sends one StateChange object (RFC 8620 section 7.1
// with the @type member; RFC 8887 section 4.3.5.1). No pushState
// member is ever included.
func (c *conn) writeStateChange(changes notify.Changes) bool {
	body, err := json.Marshal(jmap.StateChange{Type: "StateChange", Changed: map[jmap.Id]jmap.TypeState(changes)})
	if err != nil {
		return false
	}
	return c.write(frame.OpText, body)
}
