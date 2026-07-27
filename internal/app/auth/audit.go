package auth

import (
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// The audit actions this package emits. Every one of them is on EVT-081's
// mandatory-emission list, which SEC-150 binds this contract's flows to:
// "login success/failure/lockout" and "session and API-key issuance and
// revocation" are named there directly, and grant creation/redemption is added
// by SEC-034.
//
// The names are dotted `<subject>.<verb>` strings, one per distinct act — never
// a shared generic action string with the real act buried in `target`, which is
// the discipline SEC-064 states for `recover`'s two modes and which is worth
// applying uniformly.
const (
	ActionLoginSuccess    = "login.success"
	ActionLoginFailure    = "login.failure"
	ActionLoginLockout    = "login.lockout"
	ActionSessionIssued   = "session.issued"
	ActionSessionRevoked  = "session.revoked"
	ActionAPIKeyIssued    = "api_key.issued"
	ActionAPIKeyRevoked   = "api_key.revoked"
	ActionGrantCreated    = "grant.created"
	ActionGrantRedeemed   = "grant.redeemed"
	ActionSetupClaimed    = "setup.claimed"
	ActionPrincipalCreate = "principal.created"
)

// Clock-accuracy assessments (SEC-068). The app maintains `untrusted` "while it
// holds no independently verified time above the floor, `trusted` once it does".
const (
	ClockTrusted   = "trusted"
	ClockUntrusted = "untrusted"
)

// EventSink is the events/1 append boundary this package publishes audit
// records through — satisfied by the app's eventsse.Hub, which is the
// concurrent-safe transport the EventLog delegates its synchronization to. It is
// an interface so this package neither imports the SSE server nor requires one
// to exist: a deployment with no live subscribers still records.
type EventSink interface {
	Append(events.Envelope)
}

// Auditor emits security-model/1's flows as ordinary events/1 `audit.event`
// records (SEC-150). It deliberately defines NO audit schema of its own —
// SEC-150 is explicit that "this contract adds no second audit schema; every
// audit record this contract's flows produce is an ordinary events/1
// audit.event" — so this type is a small adapter over events.AuditEventEnvelope,
// not a parallel logger.
//
// A nil Auditor is usable and silent: an audit sink is a deployment
// collaborator, and a component constructed without one (a unit test for the
// password KDF, say) should not have to invent a stub. Every emission path here
// nil-checks, so the only way to lose a record is to construct the middleware
// without a sink deliberately.
type Auditor struct {
	sink EventSink
	// scopeNode is the node every audit record is placed at. Authentication is
	// a workspace-level act, not a per-site one, so this is the deployment's own
	// site/org node supplied at construction rather than derived per request.
	scopeNode string
	newID     func() string
	nowMs     func() int64
	// clockAssessment reports the app's current `trusted`/`untrusted` assessment
	// for SEC-091. It is injected because the assessment's real source is the
	// app-tier monotonic clock floor (SEC-066–068), which is a separate piece of
	// work; until that lands, the honest reading is ClockUntrusted — SEC-068
	// defines `untrusted` as exactly "while it holds no independently verified
	// time above the floor", which is precisely the state of a deployment that
	// has no floor yet. Reporting `trusted` by default would be a lie the audit
	// trail cannot be corrected for after the fact.
	clockAssessment func() string
}

// NewAuditor returns an Auditor publishing to sink, placing every record at
// scopeNode and stamping it from nowMs/newID. clockAssessment may be nil, in
// which case ClockUntrusted is reported (see the field's own doc for why that is
// the correct default rather than a placeholder).
func NewAuditor(sink EventSink, scopeNode string, nowMs func() int64, newID func() string, clockAssessment func() string) *Auditor {
	if clockAssessment == nil {
		clockAssessment = func() string { return ClockUntrusted }
	}
	return &Auditor{sink: sink, scopeNode: scopeNode, nowMs: nowMs, newID: newID, clockAssessment: clockAssessment}
}

// Record is one audit emission's variable part. actor/action/target/result are
// EVT-080's own required fields; the rest are the additive members particular
// security-model/1 requirements attach to particular records.
type Record struct {
	TraceID string
	Actor   string
	// OnBehalfOf is EVT-080's optional delegation field, absent for a
	// self-attributed action.
	OnBehalfOf string
	Action     string
	Target     string
	Result     string
	// Purpose and IssuedVia are set on grant records (SEC-034).
	Purpose   string
	IssuedVia string
	// ScopeNode is the node THIS record is filed at, overriding the Auditor's
	// own. Empty means "the Auditor's node", which is the right answer for every
	// flow this package emits: authentication, session and grant handling are
	// workspace-level acts with no narrower subject to place them under.
	//
	// It exists because EVT-012 places an event at "the scope node the event's
	// SUBJECT resource ... is itself placed under", and an audit record ABOUT a
	// resource has a subject that is genuinely placed somewhere — a schedule
	// under a screen, a pack row under a site. Since scope filtering on the event
	// stream is per event against the subscriber's visible set (EVT-120/123),
	// that placement decides who can see the record: filing a mutation of a Site
	// A row at the workspace node would hide it from the Site A operator whose
	// own screens it changed while showing it to a Site B operator with no
	// authority over it. A producer with a placed subject therefore supplies it
	// here rather than accepting the deployment-wide default.
	ScopeNode string
}

// Emit publishes r as an `audit.event`. A login-failure record automatically
// picks up the clock assessment SEC-091 requires, so no call site can forget it.
//
// The envelope is validated before append (EVT-013): an audit record that would
// not survive the delivery gate is a producer bug, and appending it anyway would
// put an undeliverable record in the log where it is invisible to exactly the
// operator who needs it. A rejected record is dropped rather than panicking —
// audit emission must never be able to take down the request path it observes.
func (a *Auditor) Emit(r Record) {
	if a == nil || a.sink == nil {
		return
	}
	clock := ""
	if r.Action == ActionLoginFailure || r.Action == ActionLoginLockout {
		clock = a.clockAssessment()
	}
	scopeNode := r.ScopeNode
	if scopeNode == "" {
		scopeNode = a.scopeNode
	}
	env := events.AuditEventEnvelope(events.AuditEvent{
		ID:              a.newID(),
		TS:              a.nowMs(),
		ScopeNode:       scopeNode,
		TraceID:         a.envelopeTraceID(r.TraceID),
		ActorPrincipal:  r.Actor,
		OnBehalfOf:      r.OnBehalfOf,
		Action:          r.Action,
		Target:          r.Target,
		Result:          r.Result,
		ClockAssessment: clock,
		Purpose:         r.Purpose,
		IssuedVia:       r.IssuedVia,
	})
	if err := events.Validate(env); err != nil {
		return
	}
	a.sink.Append(env)
}

// envelopeTraceID reconciles api/1's Trace-Id GRAMMAR with events/1's trace_id
// TYPE. API-061 accepts any 20–36 character `[A-Za-z0-9-]` value a client
// supplies — a hyphenated UUID satisfies it — while EVT-010 types a durable
// event's `trace_id` as a ULID and events.Validate enforces that. A
// server-generated Trace-Id is now always a ULID (apihttp.newTraceID), so this
// substitution only ever bites for a client that supplied a valid-but-not-ULID
// value of its own.
//
// Substituting is the lesser evil of the two available: dropping the record
// would lose an emission EVT-081 makes mandatory, whereas substituting loses
// only the correlation back to that one request — and the record still carries
// the actor, action, target and timestamp needed to find it.
func (a *Auditor) envelopeTraceID(traceID string) string {
	if ulid.Valid(traceID) {
		return traceID
	}
	return a.newID()
}

// Success and Failure are the two shorthand emitters, so a call site names the
// result by choosing a method rather than by passing a string that can be typoed
// into an out-of-enum value EVT-080 would reject.
func (a *Auditor) Success(traceID, actor, action, target string) {
	a.Emit(Record{TraceID: traceID, Actor: actor, Action: action, Target: target, Result: events.AuditResultSuccess})
}

// Failure records a failed action. EVT-083 is explicit that a failure carries
// every field a success does — "a failed action is exactly as auditable as a
// successful one, never elided for having failed" — so this takes the same
// arguments rather than a reduced set.
func (a *Auditor) Failure(traceID, actor, action, target string) {
	a.Emit(Record{TraceID: traceID, Actor: actor, Action: action, Target: target, Result: events.AuditResultFailure})
}
