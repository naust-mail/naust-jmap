package runtime

// The event source resource (RFC 8620 section 7.3): a long-running
// authenticated GET returning "text/event-stream", where the server
// pushes a "state" event carrying a StateChange object (section 7.1)
// whenever data changes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

func (s *Server) handleEventSource(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		http.Error(w, "push is not enabled on this server", http.StatusNotImplemented)
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

	// The three URL template variables of section 7.3, all required.
	q := r.URL.Query()
	types, ok := s.parseEventTypes(q.Get("types"))
	if !ok {
		http.Error(w, `types must be "*" or a comma-separated list of known type names`, http.StatusBadRequest)
		return
	}
	closeAfterState := false
	switch q.Get("closeafter") {
	case "state":
		closeAfterState = true
	case "no":
	default:
		http.Error(w, `closeafter must be "state" or "no"`, http.StatusBadRequest)
		return
	}
	ping, err := strconv.ParseUint(q.Get("ping"), 10, 32)
	if err != nil {
		http.Error(w, "ping must be a non-negative integer number of seconds", http.StatusBadRequest)
		return
	}
	if ping > tuning.EventSourceMaxPingInterval {
		ping = tuning.EventSourceMaxPingInterval
	}

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Changes are pushed for every account the user has access to
	// (section 7.1). Subscribe before reading current state so no
	// commit can fall between the two.
	accounts := make([]jmap.Id, 0, len(ident.Accounts))
	for id := range ident.Accounts {
		accounts = append(accounts, id)
	}
	sub, err := s.push.n.Subscribe(r.Context(), accounts)
	if err != nil {
		http.Error(w, "subscribe failed", http.StatusInternalServerError)
		return
	}
	defer sub.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.WriteHeader(http.StatusOK)

	// On connect, push the full current state of every requested type.
	// Section 7.3 SHOULDs event ids encoding the entire server state so
	// a reconnecting client's Last-Event-ID lets the server replay
	// missed changes; pushing current state up front gives the same
	// observable guarantee - a client can never miss changes across a
	// reconnect - so no event ids are sent at all.
	initial := make(notify.Changes, len(accounts))
	for _, acct := range accounts {
		ts := make(jmap.TypeState, len(types))
		for _, name := range types {
			state, err := s.push.db.TypeState(r.Context(), acct, name)
			if err != nil {
				return // headers are sent; nothing left but to drop the stream
			}
			ts[name] = state
		}
		initial[acct] = ts
	}
	if writeStateEvent(w, flusher, initial) != nil || closeAfterState {
		return
	}

	wantType := make(map[string]bool, len(types))
	for _, name := range types {
		wantType[name] = true
	}

	for {
		waitCtx, cancel := r.Context(), context.CancelFunc(func() {})
		if ping > 0 {
			waitCtx, cancel = context.WithTimeout(waitCtx, time.Duration(ping)*time.Second)
		}
		changes, err := sub.Wait(waitCtx)
		cancel()
		switch {
		case err == nil:
		case errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil:
			// The ping interval elapsed since the previous event: send a
			// ping event carrying the interval in use, with no event id
			// (section 7.3).
			if writePing(w, flusher, ping) != nil {
				return
			}
			continue
		default:
			return // client disconnected or subscription closed
		}

		// Only push changes for the requested types; others are omitted
		// from the TypeState object (section 7.3 "types"), and an event
		// with nothing left is not sent at all.
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
		if writeStateEvent(w, flusher, filtered) != nil {
			return
		}
		if closeAfterState {
			return
		}
	}
}

// parseEventTypes resolves the "types" variable (section 7.3): the
// single character "*" means changes to all types are pushed, otherwise
// a comma-separated list of type names. Unknown names are rejected -
// they would silently never fire, which always means a client bug.
func (s *Server) parseEventTypes(raw string) ([]string, bool) {
	all := s.push.db.TypeNames()
	if raw == "*" {
		return all, true
	}
	if raw == "" {
		return nil, false
	}
	known := make(map[string]bool, len(all))
	for _, name := range all {
		known[name] = true
	}
	set := make(map[string]bool)
	for _, name := range strings.Split(raw, ",") {
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

// writeStateEvent sends one "state" event whose data is a StateChange
// object (section 7.1). No id field is set; see the connect-time state
// push in handleEventSource for why.
func writeStateEvent(w io.Writer, flusher http.Flusher, changes notify.Changes) error {
	sc := jmap.StateChange{Type: "StateChange", Changed: map[jmap.Id]jmap.TypeState(changes)}
	data, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writePing sends one "ping" event. Its data MUST be an object with an
// "interval" property holding the interval in use, and it MUST NOT set
// a new event id (section 7.3).
func writePing(w io.Writer, flusher http.Flusher, interval uint64) error {
	if _, err := fmt.Fprintf(w, "event: ping\ndata: {\"interval\":%d}\n\n", interval); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
