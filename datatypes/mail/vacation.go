package mail

// VacationResponse (RFC 8621 section 8): the singleton per-account
// auto-reply configuration, capability
// urn:ietf:params:jmap:vacationresponse (section 1.3.3). The type has
// exactly two methods, get and set (section 8.1-8.2), and a single object
// whose id is "singleton" - which does not fit the derived standard
// machinery (a fixed-id record that exists, with defaults, before anything
// was ever stored), so both methods are registered as custom handlers. The
// stored record, when one exists, lives under the account like any other;
// the wire id is always "singleton".
//
// The delivery-side responder that acts on this configuration is
// vacationresponder.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// VacationCapabilityURI is the RFC 8621 vacation response capability
// (section 1.3.3). Its session-level and account-level values are both
// empty JSON objects.
const VacationCapabilityURI = "urn:ietf:params:jmap:vacationresponse"

// TypeVacationResponse is the VacationResponse datatype name.
const TypeVacationResponse = record.TypeVacationResponse

// vacationSingletonId is the fixed wire id of the one VacationResponse
// object (RFC 8621 section 8: "THE VacationResponse object", id
// "singleton").
const vacationSingletonId = "singleton"

// vacationResponseType is the stored shape of the section 8 object. The
// record holds every property concretely (defaults applied at first
// write); absence of a record means the defaults.
func vacationResponseType() *descriptor.Type {
	null := json.RawMessage(`null`)
	return &descriptor.Type{
		Name:       TypeVacationResponse,
		Capability: VacationCapabilityURI,
		Properties: map[string]descriptor.Property{
			"isEnabled": {Kind: descriptor.KindBool},
			"fromDate":  {Kind: descriptor.KindDate, Nullable: true, Default: null},
			"toDate":    {Kind: descriptor.KindDate, Nullable: true, Default: null},
			"subject":   {Kind: descriptor.KindString, Nullable: true, Default: null},
			"textBody":  {Kind: descriptor.KindString, Nullable: true, Default: null},
			"htmlBody":  {Kind: descriptor.KindString, Nullable: true, Default: null},
		},
	}
}

// vacationWireProps is every property of the wire object, id excluded.
var vacationWireProps = []string{"isEnabled", "fromDate", "toDate", "subject", "textBody", "htmlBody"}

// VacationResponseConfig configures RegisterVacationResponse.
type VacationResponseConfig struct {
	// DB is the object database the VacationResponse singleton lives in.
	// Required.
	DB *objectdb.DB
}

// RegisterVacationResponse registers the VacationResponse type and its two
// methods (RFC 8621 sections 8.1-8.2) on the processor. The embedder
// advertises VacationCapabilityURI on its server for the type to be
// callable; the delivery-side responder, and the suppression ledger it
// owns, are registered separately (deliver.New with Config.VacationQueue).
func RegisterVacationResponse(p *runtime.Processor, cfg VacationResponseConfig) error {
	if cfg.DB == nil {
		return errors.New("mail: RegisterVacationResponse: VacationResponseConfig missing required field: DB")
	}
	db := cfg.DB
	if err := db.RegisterType(vacationResponseType()); err != nil {
		return err
	}
	h := vacationMethods{db: db}
	p.Register("VacationResponse/get", VacationCapabilityURI, h.get)
	p.Register("VacationResponse/set", VacationCapabilityURI, h.set)
	return nil
}

type vacationMethods struct{ db *objectdb.DB }

// vacationDefaults is the wire object before anything was stored (section
// 8: isEnabled false, everything else null).
func vacationDefaults() map[string]json.RawMessage {
	obj := map[string]json.RawMessage{
		"id":        record.MustJSON(vacationSingletonId),
		"isEnabled": json.RawMessage(`false`),
	}
	for _, p := range vacationWireProps[1:] {
		obj[p] = json.RawMessage(`null`)
	}
	return obj
}

// loadVacation returns the wire object and, when a record exists, its
// stored id ("" otherwise). At most one record exists per account: writes
// go through the one set handler, which reuses the existing record.
func loadVacation(ctx context.Context, db *objectdb.DB, acct jmap.Id) (map[string]json.RawMessage, jmap.Id, error) {
	ids, err := db.AllIds(ctx, acct, TypeVacationResponse, 2)
	if err != nil {
		return nil, "", err
	}
	obj := vacationDefaults()
	if len(ids) == 0 {
		return obj, "", nil
	}
	rec, err := db.Get(ctx, acct, TypeVacationResponse, ids[0])
	if err != nil {
		return nil, "", err
	}
	for _, p := range vacationWireProps {
		if v, has := rec[p]; has {
			obj[p] = v
		}
	}
	return obj, ids[0], nil
}

func (h vacationMethods) get(ctx context.Context, call *runtime.Call) []jmap.Invocation {
	if errType, desc := checkArgNames(call.Args, "accountId", "ids", "properties"); errType != "" {
		return runtime.Fail(call.CallID, errType, desc)
	}
	var args struct {
		AccountId  jmap.Id    `json:"accountId"`
		Ids        *[]jmap.Id `json:"ids"`
		Properties *[]string  `json:"properties"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	if errType, desc := runtime.CheckAccount(call, args.AccountId, false); errType != "" {
		return runtime.Fail(call.CallID, errType, desc)
	}
	props := vacationWireProps
	if args.Properties != nil {
		props = *args.Properties
		for _, p := range props {
			if !vacationAccepts(p) {
				return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, fmt.Sprintf("unknown property %q", p))
			}
		}
	}
	full, _, err := loadVacation(ctx, h.db, args.AccountId)
	if err != nil {
		return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	obj := map[string]json.RawMessage{"id": full["id"]}
	for _, p := range props {
		if p != "id" {
			obj[p] = full[p]
		}
	}
	state, err := h.db.TypeState(ctx, args.AccountId, TypeVacationResponse)
	if err != nil {
		return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	list := []map[string]json.RawMessage{}
	notFound := []jmap.Id{}
	if args.Ids == nil {
		// "ids: null fetches all" - all is the one singleton (section 8.1).
		list = append(list, obj)
	} else {
		seen := false
		for _, id := range *args.Ids {
			if id == vacationSingletonId {
				if !seen {
					list = append(list, obj)
					seen = true
				}
			} else {
				notFound = append(notFound, id)
			}
		}
	}
	return runtime.Reply("VacationResponse/get", call.CallID, map[string]any{
		"accountId": args.AccountId,
		"state":     state,
		"list":      list,
		"notFound":  notFound,
	})
}

// checkArgNames rejects a call carrying arguments outside the allowed set
// (RFC 8620 section 3.6.2: unknown arguments are invalidArguments).
func checkArgNames(raw json.RawMessage, allowed ...string) (errType, desc string) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return jmap.ErrInvalidArguments, err.Error()
	}
	for name := range all {
		ok := false
		for _, a := range allowed {
			if name == a {
				ok = true
				break
			}
		}
		if !ok {
			return jmap.ErrInvalidArguments, fmt.Sprintf("unknown argument %q", name)
		}
	}
	return "", ""
}

func vacationAccepts(name string) bool {
	if name == "id" {
		return true
	}
	for _, p := range vacationWireProps {
		if p == name {
			return true
		}
	}
	return false
}

func (h vacationMethods) set(ctx context.Context, call *runtime.Call) []jmap.Invocation {
	if errType, desc := checkArgNames(call.Args, "accountId", "ifInState", "create", "update", "destroy"); errType != "" {
		return runtime.Fail(call.CallID, errType, desc)
	}
	var args struct {
		AccountId jmap.Id                     `json:"accountId"`
		IfInState *string                     `json:"ifInState"`
		Create    map[jmap.Id]json.RawMessage `json:"create"`
		Update    map[jmap.Id]objectdb.Object `json:"update"`
		Destroy   []jmap.Id                   `json:"destroy"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	if errType, desc := runtime.CheckAccount(call, args.AccountId, true); errType != "" {
		return runtime.Fail(call.CallID, errType, desc)
	}
	oldState, err := h.db.TypeState(ctx, args.AccountId, TypeVacationResponse)
	if err != nil {
		return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	if args.IfInState != nil && *args.IfInState != oldState {
		return runtime.Fail(call.CallID, jmap.ErrStateMismatch, "")
	}

	resp := map[string]any{"accountId": args.AccountId, "oldState": oldState}
	// The singleton can never be created or destroyed (RFC 8620 section
	// 5.3's "singleton" SetError exists for exactly this).
	if len(args.Create) > 0 {
		nc := map[jmap.Id]*jmap.SetError{}
		for cid := range args.Create {
			nc[cid] = &jmap.SetError{Type: jmap.SetErrSingleton, Description: "VacationResponse is a singleton"}
		}
		resp["notCreated"] = nc
	}
	if len(args.Destroy) > 0 {
		nd := map[jmap.Id]*jmap.SetError{}
		for _, id := range args.Destroy {
			nd[id] = &jmap.SetError{Type: jmap.SetErrSingleton, Description: "VacationResponse is a singleton"}
		}
		resp["notDestroyed"] = nd
	}

	updated := map[jmap.Id]json.RawMessage{}
	notUpdated := map[jmap.Id]*jmap.SetError{}
	for id, patch := range args.Update {
		if id != vacationSingletonId {
			notUpdated[id] = &jmap.SetError{Type: jmap.SetErrNotFound, Description: "only the singleton exists"}
			continue
		}
		if serr := h.applyVacationUpdate(ctx, args.AccountId, patch); serr != nil {
			notUpdated[id] = serr
			continue
		}
		updated[id] = json.RawMessage(`null`)
	}
	if len(updated) > 0 {
		resp["updated"] = updated
	}
	if len(notUpdated) > 0 {
		resp["notUpdated"] = notUpdated
	}
	newState, err := h.db.TypeState(ctx, args.AccountId, TypeVacationResponse)
	if err != nil {
		return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	resp["newState"] = newState
	return runtime.Reply("VacationResponse/set", call.CallID, resp)
}

// applyVacationUpdate validates one update's properties and writes the
// record (creating it, defaults applied, on the account's first write).
func (h vacationMethods) applyVacationUpdate(ctx context.Context, acct jmap.Id, patch objectdb.Object) *jmap.SetError {
	var bad []string
	for name, raw := range patch {
		switch name {
		case "id":
			if s, ok := decodeString(raw); !ok || s != vacationSingletonId {
				bad = append(bad, name)
			}
		case "isEnabled":
			var b bool
			if json.Unmarshal(raw, &b) != nil {
				bad = append(bad, name)
			}
		case "fromDate", "toDate":
			if !isNullRaw(raw) {
				if s, ok := decodeString(raw); !ok || !jmap.ValidUTCDate(s) {
					bad = append(bad, name)
				}
			}
		case "subject", "textBody", "htmlBody":
			if !isNullRaw(raw) {
				if _, ok := decodeString(raw); !ok {
					bad = append(bad, name)
				}
			}
		default:
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		return &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: bad}
	}
	full, storedId, err := loadVacation(ctx, h.db, acct)
	if err != nil {
		return &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	_, err = h.db.Update(ctx, acct, func(u *objectdb.Update) error {
		obj := objectdb.Object{}
		for _, p := range vacationWireProps {
			obj[p] = full[p]
		}
		for name, raw := range patch {
			if name != "id" {
				obj[name] = raw
			}
		}
		if storedId == "" {
			_, err := u.Create(TypeVacationResponse, obj)
			return err
		}
		obj["id"] = record.MustJSON(storedId)
		return u.Put(TypeVacationResponse, storedId, obj)
	})
	if err != nil {
		return &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	return nil
}
