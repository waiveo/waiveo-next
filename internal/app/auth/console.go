package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// console.go is the console binding's ADMISSION RULE and VERB POLICY —
// security-model/1 SEC-072/073/075/076/077 — as a transport-independent
// dispatcher.
//
// # Why the peer uid is a parameter and not something this file discovers
//
// SEC-072 is about SO_PEERCRED: "the app MUST verify, via SO_PEERCRED (or the
// platform's equivalent peer-credential mechanism), that a connecting process's
// effective uid is 0 before serving any request on the console binding." Reading
// that credential off a live socket is a per-OS syscall (Linux SO_PEERCRED,
// Darwin LOCAL_PEERCRED) and belongs to whatever listener terminates the
// connection. What this file owns is the DECISION made from that credential, and
// the verb policy applied after it — the parts that are the same on every
// platform and that a conformance case can actually exercise.
//
// The contract sanctions exactly this split in its own Conformance notes:
// "SO_PEERCRED admission (SEC-072) ... conformance cases model them as a given
// input (peer_uid: 0 or peer_uid: 1000) ... the driver harness that actually
// opens a Unix domain socket and checks SO_PEERCRED is a systemd-install-smoke-
// lane concern, not this static corpus's." A listener that reads the real
// credential and calls Dispatch is therefore the remaining half, and it is NOT
// in this tree yet: SEC-070's socket and SEC-071's 0700 mode have no
// implementation here, and this file does not pretend otherwise.
//
// # Admitted vs. executed
//
// Dispatch reports the two separately because SEC-072 and SEC-075 refuse at
// different points and a caller must be able to tell which happened. A non-root
// peer is refused at ACCEPT time with no response body at all (SEC-072); a
// root peer naming an unlisted verb has connected legitimately and is refused
// with a typed Problem code (SEC-075). Collapsing the two into one boolean would
// lose the distinction the error taxonomy draws between CONSOLE_PEER_NOT_ROOT
// and CONSOLE_VERB_NOT_ALLOWED.

// The console binding's verb set (SEC-075). It is a CLOSED enumeration, and the
// closure is the requirement: "general resource data access MUST NOT be exposed
// over this binding, even to a caller who has already proven uid-0."
//
// Every member satisfies SEC-076's admission rule — uid-0 could already perform
// an equivalent action through direct filesystem or OS-level access on the same
// host, so the verb adds audit and a typed shape, never new reach.
const (
	// ConsoleVerbGrantIssue and ConsoleVerbGrantRedeem are SEC-075's "grant
	// issuance and redemption". Root can already write the grants table.
	ConsoleVerbGrantIssue  = "grant.issue"
	ConsoleVerbGrantRedeem = "grant.redeem"
	// ConsoleVerbSessionRevoke and ConsoleVerbAPIKeyRevoke are SEC-075's
	// "session/API-key revocation" (SEC-020). Root can already delete the rows.
	ConsoleVerbSessionRevoke = "session.revoke"
	ConsoleVerbAPIKeyRevoke  = "api_key.revoke"
	// ConsoleVerbServiceStatus is SEC-075's "read-only service status".
	ConsoleVerbServiceStatus = "service.status"
	// ConsoleVerbRestoreSnapshot is SEC-075's `restore-from-snapshot`, the verb
	// SEC-076 uses as its own worked example ("root can already overwrite the
	// workspace file directly; the verb adds audit and safety, never new reach").
	ConsoleVerbRestoreSnapshot = "restore-from-snapshot"
	// ConsoleVerbClockFloorReset is SEC-075's clock-floor reset (SEC-066-068).
	ConsoleVerbClockFloorReset = "clock_floor.reset"
)

// consoleVerbs is SEC-075's enumerated set as a lookup.
var consoleVerbs = map[string]struct{}{
	ConsoleVerbGrantIssue:      {},
	ConsoleVerbGrantRedeem:     {},
	ConsoleVerbSessionRevoke:   {},
	ConsoleVerbAPIKeyRevoke:    {},
	ConsoleVerbServiceStatus:   {},
	ConsoleVerbRestoreSnapshot: {},
	ConsoleVerbClockFloorReset: {},
}

// ConsoleVerbAllowed reports whether verb is inside SEC-075's enumerated set.
func ConsoleVerbAllowed(verb string) bool {
	_, ok := consoleVerbs[verb]
	return ok
}

// ConsoleVerbs returns the enumerated verb set, sorted — for a status response
// or an operator-facing listing that must not hand-maintain a second copy of the
// enumeration and drift from it.
func ConsoleVerbs() []string {
	out := make([]string, 0, len(consoleVerbs))
	for v := range consoleVerbs {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Console-binding error codes (security-model/1 Error taxonomy).
const (
	// ErrCodeConsolePeerNotRoot accompanies a refusal at accept time. SEC-072:
	// "refused at accept time with no response body ... logged, not surfaced to
	// the rejected peer beyond connection closure" — so a transport MUST close
	// the connection rather than serialize this code back to the peer.
	ErrCodeConsolePeerNotRoot = "CONSOLE_PEER_NOT_ROOT"
	// ErrCodeConsoleVerbNotAllowed is SEC-075's refusal, which IS returned to the
	// (root) caller as an ordinary api/1 Problem.
	ErrCodeConsoleVerbNotAllowed = "CONSOLE_VERB_NOT_ALLOWED"
	// ErrCodeValidationFailed types a well-formed verb whose params are not.
	ErrCodeValidationFailed = "VALIDATION_FAILED"
	// ErrCodeNotFound types a verb whose subject does not exist.
	ErrCodeNotFound = "NOT_FOUND"
	// ErrCodeUnimplemented types a verb inside SEC-075's set that this
	// deployment has no implementation for yet. It is a distinct code precisely
	// so "the contract forbids this verb" and "this build cannot serve it" can
	// never be confused by a reader of the audit trail.
	ErrCodeUnimplemented = "UNIMPLEMENTED"
	// ErrCodeInternal types an execution failure.
	ErrCodeInternal = "INTERNAL"
)

// consoleRootUID is the effective uid SEC-072 admits, and the only one.
const consoleRootUID = 0

// ConsoleRequest is the security-model/1 `ConsoleRequest` wire shape (Wire
// shapes): a verb, its params, and the api/1 `Trace-Id` the binding propagates
// unmodified (SEC-074).
type ConsoleRequest struct {
	Verb    string         `json:"verb"`
	Params  map[string]any `json:"params,omitempty"`
	TraceID string         `json:"trace_id,omitempty"`
}

// ConsoleResponse is one dispatch's outcome.
type ConsoleResponse struct {
	// Admitted reports whether the REQUEST was admitted for execution: the peer
	// proved uid-0 (SEC-072) AND named a verb inside SEC-075's set.
	Admitted bool
	// Executed reports whether the verb's effect actually ran. It is false on
	// every refusal and on an execution error, so a caller never has to infer
	// "did anything happen" from an error value.
	Executed bool
	// PrincipalKind is the kind the admitted request was attributed to —
	// KindSystemConsole (SEC-073) — or "" on a refusal.
	PrincipalKind string
	// PrincipalID is the synthetic `system-console` principal's row id, so an
	// audit reader can join the record to the principal relation. That principal
	// carries NO credential row (SEC-002); CredentialCount over it is zero and
	// attaching one is refused at the store.
	PrincipalID string
	// Code is "" on success, otherwise the security-model/1 error code above.
	Code   string
	Detail string
	// Body is the verb's own result payload, absent on every refusal. It is
	// nil — not an empty map — on a SEC-072 refusal, since that refusal carries
	// no response body at all.
	Body map[string]any
}

// Console dispatches console-binding requests: it applies SEC-072's admission
// rule to an injected peer uid, SEC-075's verb policy, SEC-073's attribution,
// and SEC-077's unconditional audit emission, then runs the verb.
//
// The zero value is not usable; construct with NewConsole.
type Console struct {
	store   *Store
	floor   *ClockFloor
	auditor *Auditor
}

// NewConsole returns a dispatcher over store. floor may be nil, in which case
// the clock-floor reset verb answers UNIMPLEMENTED rather than pretending to
// have reset something. auditor may be nil (a unit test), in which case nothing
// is recorded — every deployment path MUST supply one, since SEC-077 admits no
// exception.
func NewConsole(store *Store, floor *ClockFloor, auditor *Auditor) *Console {
	return &Console{store: store, floor: floor, auditor: auditor}
}

// ActionConsoleVerb is the audit action every console-binding invocation is
// recorded under (SEC-077). The verb itself rides in the record's `target`, so a
// reader separates "a console verb ran" from "which one" without a per-verb
// action string proliferating across the taxonomy.
const ActionConsoleVerb = "console.verb"

// Dispatch applies the console binding's admission rule and verb policy to req,
// arriving from a peer whose effective uid the TRANSPORT has already read
// (SEC-072) and passes in here.
//
// Order is the design, and it is SEC-072's order: uid FIRST, before the verb is
// even looked at. A non-root peer must never be able to learn anything about the
// verb set by observing which of its requests are refused differently.
func (c *Console) Dispatch(ctx context.Context, peerUID int, req ConsoleRequest) ConsoleResponse {
	if peerUID != consoleRootUID {
		// SEC-072: refused at accept time, with NO response body. The record is
		// still written — the refusal is exactly the event an operator needs —
		// and it is attributed to no principal, because none was established.
		c.audit(req.TraceID, "", req.Verb, false)
		return ConsoleResponse{Code: ErrCodeConsolePeerNotRoot,
			Detail: "The console binding admits only a peer whose effective uid is 0."}
	}

	// SEC-073: admission over this binding IS the identity. The synthetic
	// principal is materialized before the verb runs so the audit record for a
	// REFUSED verb is attributed exactly as a successful one is (EVT-083: "a
	// failed action is exactly as auditable as a successful one").
	principalID, err := c.store.EnsureSystemConsolePrincipal(ctx)
	if err != nil {
		c.audit(req.TraceID, "", req.Verb, false)
		return ConsoleResponse{Code: ErrCodeInternal, Detail: "The console principal could not be resolved."}
	}

	if !ConsoleVerbAllowed(req.Verb) {
		c.audit(req.TraceID, principalID, req.Verb, false)
		return ConsoleResponse{Code: ErrCodeConsoleVerbNotAllowed,
			Detail: "This verb is outside the console binding's enumerated verb set."}
	}

	code, detail, body := c.exec(ctx, principalID, req)
	c.audit(req.TraceID, principalID, req.Verb, code == "")
	return ConsoleResponse{
		Admitted:      true,
		Executed:      code == "",
		PrincipalKind: KindSystemConsole,
		PrincipalID:   principalID,
		Code:          code,
		Detail:        detail,
		Body:          body,
	}
}

// exec runs one admitted verb. Every branch returns a typed code rather than a
// Go error, so the caller's response shape is decided in one place.
//
// principalID is the synthetic `system-console` principal Dispatch materialized
// (SEC-073); a verb whose effect is auditable in its own right attributes it to
// that principal, so a console-issued grant names an actor that joins to the
// principal relation exactly as an api-issued one does.
func (c *Console) exec(ctx context.Context, principalID string, req ConsoleRequest) (code, detail string, body map[string]any) {
	switch req.Verb {
	case ConsoleVerbGrantIssue:
		return c.execGrantIssue(ctx, principalID, req)

	case ConsoleVerbGrantRedeem:
		// Inside SEC-075's set and deliberately not served by this build.
		//
		// Both redeemable purposes this tree mints have a redeemer who is NOT the
		// console operator. A `setup` code is redeemed by whoever claims the box,
		// which mints their credential and their owner binding (SEC-120); a
		// `credential-reset` code is redeemed by the TARGET, choosing a value
		// SEC-050 says the issuer must have no path to choose. The console
		// redemption SEC-075 enumerates is the break-glass one (SEC-061), whose
		// section is unimplemented here — and issuing plus redeeming in one place
		// is precisely the shape SEC-050 forbids, so it is not something to add
		// ahead of the flow that legitimately needs it.
		return ErrCodeUnimplemented,
			"This build serves no console redemption: the break-glass flow it belongs to (SEC-061) is not implemented, and the two purposes this deployment mints are redeemed by the claimant and by the reset's target, never by the console operator.", nil

	case ConsoleVerbSessionRevoke:
		id, ok := consoleStringParam(req.Params, "session_id")
		if !ok {
			return ErrCodeValidationFailed, "`session_id` is required.", nil
		}
		if err := c.store.RevokeSession(ctx, id); err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				return ErrCodeNotFound, "No such session.", nil
			}
			return ErrCodeInternal, "The session could not be revoked.", nil
		}
		return "", "", map[string]any{"session_id": id, "revoked": true}

	case ConsoleVerbAPIKeyRevoke:
		id, ok := consoleStringParam(req.Params, "session_id")
		if !ok {
			return ErrCodeValidationFailed, "`session_id` is required.", nil
		}
		// An API key IS a session row carrying its own credential id (SEC-020's
		// "revocable through the same mechanism"), so this is deliberately the
		// same call rather than a parallel path that could drift from it.
		if err := c.store.RevokeSession(ctx, id); err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				return ErrCodeNotFound, "No such API key.", nil
			}
			return ErrCodeInternal, "The API key could not be revoked.", nil
		}
		return "", "", map[string]any{"session_id": id, "revoked": true}

	case ConsoleVerbClockFloorReset:
		if c.floor == nil {
			return ErrCodeUnimplemented, "This deployment maintains no clock floor.", nil
		}
		if err := c.floor.Reset(); err != nil {
			return ErrCodeInternal, "The clock floor could not be reset.", nil
		}
		return "", "", map[string]any{"floor_ms": c.floor.FloorMs(), "clock_assessment": c.floor.Assessment()}

	case ConsoleVerbServiceStatus:
		assessment := ClockUntrusted
		var floorMs int64
		if c.floor != nil {
			assessment, floorMs = c.floor.Assessment(), c.floor.FloorMs()
		}
		return "", "", map[string]any{
			"clock_assessment": assessment,
			"clock_floor_ms":   floorMs,
			"verbs":            ConsoleVerbs(),
		}

	default:
		// Inside SEC-075's set, but with no implementation in this build. Named
		// as such rather than silently succeeding: a console verb that reports
		// success without acting is worse than one that reports it cannot act.
		return ErrCodeUnimplemented, fmt.Sprintf("The %q verb is not implemented in this build.", req.Verb), nil
	}
}

// execGrantIssue serves SEC-075's "grant issuance" over the console binding.
//
// # Why the purpose set here is NARROWER than SEC-030's seven
//
// SEC-076 would admit any of them — root can write the grants table directly, so
// no purpose adds reach. The narrowing is about what this BUILD can perform
// completely, and one purpose in particular cannot be:
//
// A `recovery`-purpose grant issued via the console carries SEC-063, which is an
// unconditional, un-suppressible obligation to notify every `owner`-role
// principal and raise a persistent UI banner — "the compensating detection
// control for a threat model that concedes prevention at the root boundary".
// This deployment has no notification plane and no banner store, so issuing one
// here would produce exactly the artifact SEC-063 exists to make impossible: a
// root-issued recovery credential that nobody is told about. It is refused as
// UNIMPLEMENTED, which the error taxonomy distinguishes from "the contract
// forbids this verb" for precisely this reason.
//
// The remaining purposes are refused as validation failures rather than
// unimplemented: `setup` is minted by the first-boot bootstrap and is not an
// operator-issued thing, `pairing`/`relay-claim` belong to relay/1's own flows,
// `invite` has no redemption flow in this tree, and `support` has no redemption
// mechanics defined in the contract at all (SEC-030's own draft-note).
func (c *Console) execGrantIssue(ctx context.Context, principalID string, req ConsoleRequest) (code, detail string, body map[string]any) {
	purpose, ok := consoleStringParam(req.Params, "purpose")
	if !ok {
		return ErrCodeValidationFailed, "`purpose` is required.", nil
	}
	switch purpose {
	case PurposeCredentialReset:
	case PurposeRecovery:
		return ErrCodeUnimplemented,
			"A console-issued recovery grant carries SEC-063's unconditional owner notification and persistent banner; this build has neither, and issuing the grant without them would remove the compensating control the requirement exists to guarantee.", nil
	default:
		return ErrCodeValidationFailed,
			fmt.Sprintf("The console binding issues no %q-purpose grant in this build.", purpose), nil
	}

	target, ok := consoleStringParam(req.Params, "target_principal_id")
	if !ok {
		return ErrCodeValidationFailed, "`target_principal_id` is required.", nil
	}
	keep, ok := consoleBoolParam(req.Params, "keep_existing_sessions")
	if !ok {
		return ErrCodeValidationFailed, "`keep_existing_sessions`, when present, must be a boolean.", nil
	}
	// base_url is OPTIONAL and operator-supplied. It is never derived from
	// anything the request carries about where it came from — a Unix socket peer
	// has no Host header to poison, and this is the same discipline the api route
	// holds to for the reason its own doc gives.
	baseURL, _ := consoleStringParam(req.Params, "base_url")

	handoff, err := c.store.IssueCredentialResetGrantViaConsole(ctx, principalID, target, CredentialResetOptions{
		KeepExistingSessions: keep,
		BaseURL:              baseURL,
		TraceID:              req.TraceID,
	})
	switch {
	case err == nil:
	case errors.Is(err, ErrPrincipalNotFound), errors.Is(err, ErrResetIdentifierUnknown):
		// One code for "no such principal" and "that principal holds no password
		// credential to reset". Root could distinguish them by reading the
		// database, so this is not a disclosure boundary — it is that both mean
		// the same thing to the operator: there is nothing here to reset.
		return ErrCodeNotFound, "No such principal holds a password credential to reset.", nil
	case errors.Is(err, ErrResetTargetNotUser):
		return ErrCodeValidationFailed, "A credential-reset grant targets a `user` principal (SEC-012).", nil
	default:
		return ErrCodeInternal, "The credential-reset grant could not be issued.", nil
	}

	// The one-time code rides in the RESPONSE, which is SEC-050's own shape
	// ("return a one-time code or URL for the issuing admin to hand to the target
	// user"). It reaches the operator's terminal and nothing else: it is not in
	// the audit record this dispatch emits, not in the grant row (only its hash
	// is), and the console binding takes no argv.
	out := map[string]any{
		"grant_id":            handoff.GrantID,
		"code":                handoff.Code,
		"target_principal_id": handoff.TargetPrincipalID,
		"expires_at_ms":       handoff.ExpiresAtMs,
	}
	if handoff.URL != "" {
		out["url"] = handoff.URL
	}
	return "", "", out
}

// audit emits SEC-077's unconditional record: "Every verb invocation over the
// console binding MUST emit an audit.event attributing the action to
// system-console, with no exception."
func (c *Console) audit(traceID, principalID, verb string, ok bool) {
	if c.auditor == nil {
		return
	}
	actor := principalID
	if actor == "" {
		actor = UnresolvedActorPrincipal
	}
	result := "failure"
	if ok {
		result = "success"
	}
	// A connection refused by the admission rule is recorded before any request
	// body has been read, so it names no verb at all. The record still has to say
	// what it is about: an empty `target` would read as a missing field rather
	// than as "the peer never got to name one".
	if verb == "" {
		verb = "<unread>"
	}
	c.auditor.Emit(Record{
		TraceID: traceID,
		Actor:   actor,
		Action:  ActionConsoleVerb,
		Target:  "console:" + verb,
		Result:  result,
		// SEC-030's issuance channel, carried so a console-issued act stays
		// distinguishable in the trail from the same act over /api/v1 (SEC-034).
		IssuedVia: IssuedViaConsole,
	})
}

func consoleStringParam(params map[string]any, key string) (string, bool) {
	v, ok := params[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok && s != ""
}

// consoleBoolParam reads an OPTIONAL boolean param. Absent reads as false and is
// valid; present-but-not-a-boolean is invalid, so a caller that sent
// `"keep_existing_sessions": "true"` (a string) is told so rather than having
// SEC-053's opt-out silently ignored and every session of the target evicted.
func consoleBoolParam(params map[string]any, key string) (bool, bool) {
	v, present := params[key]
	if !present || v == nil {
		return false, true
	}
	b, ok := v.(bool)
	return b, ok
}

// EnsureSystemConsolePrincipal returns the deployment's single synthetic
// `system-console` principal (SEC-002/073), creating it on first use.
//
// It is a real row rather than a hard-coded sentinel id so an `audit.event`'s
// `actor_principal` joins to the principal relation like every other actor's
// does, and so the "carries no credential row" property is a fact about a row
// somebody can query (CredentialCount) rather than about a value that exists
// nowhere.
func (s *Store) EnsureSystemConsolePrincipal(ctx context.Context) (string, error) {
	s.mu.RLock()
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT principal_id FROM principals WHERE kind = ? ORDER BY principal_id ASC LIMIT 1`,
		KindSystemConsole).Scan(&id)
	s.mu.RUnlock()
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("auth: resolve system-console principal: %w", err)
	}
	p, err := s.CreatePrincipal(ctx, KindSystemConsole, "system-console")
	if err != nil {
		return "", err
	}
	return p.PrincipalID, nil
}
