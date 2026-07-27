package events

// webhook.go is events/1's outbound webhook delivery layer (EVT-150-158): a
// registered endpoint is, from the durable-event log's perspective, just
// another subscriber (EVT-155) — it receives the same envelopes a WS/SSE
// binding would, POSTed instead of framed, signed so the receiver can verify
// authenticity, retried with a capped backoff, disabled after too many
// consecutive failures, and resumed from its own last_delivered_id under the
// exact same retention-window gap behavior resume.go already implements (no
// gap logic is reimplemented here — PendingAfter/RecordDelivered feed
// straight into EventLog.After / Resolve).
//
// EVT-153's backoff numbers, EVT-154's consecutive-failure bound, and
// EVT-158's rotation overlap window are draft-notes, not fixed by any
// normative source yet (see contracts/events-1.md) — exactly like EVT-153/154
// influenced eventlog.go's own retention horizon, they are NewEndpointState
// constructor inputs, never baked constants; the Default* constants below only
// name the contract's own proposed starting points for a caller with no
// operator-configured figure of its own (the stabilization-window precedent,
// internal/rules/engine/stabilization.go).
//
// This file builds the exact signed request and drives the per-endpoint state
// machine; it issues no I/O of its own and reads no clock of its own. The
// caller that turns a WebhookRequest into an actual POST, persists
// EndpointProgress across a restart, and paces the retries is
// internal/app/webhookdeliver.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Webhook delivery header names (EVT-151) — present on every POST.
const (
	HeaderDeliveryID = "X-Waiveo-Delivery-Id"
	HeaderTimestamp  = "X-Waiveo-Timestamp"
	HeaderSignature  = "X-Waiveo-Signature"
)

// HeaderPriorSignature carries the SAME signed material as HeaderSignature
// computed under the immediately prior signing secret, and rides a delivery
// ONLY while a rotation's overlap window is open (EVT-158).
//
// It exists because EVT-158 requires rotation "without a delivery gap", and a
// single-valued signature header cannot deliver that on its own. Whichever one
// secret the sender picks during the overlap, the receiver holding the other
// one rejects every delivery — which is precisely the gap the requirement
// forbids. The three shapes that could close it are: a multi-valued
// X-Waiveo-Signature, signing under the prior secret until the window closes,
// or an additive second header. Only the third leaves EVT-151 untouched:
// X-Waiveo-Signature keeps carrying exactly one hex HMAC-SHA256 under exactly
// the endpoint's current signing secret, on every delivery, in and out of a
// rotation — so a receiver that verifies it today verifies it unchanged
// tomorrow, and the corpus's own single-valued expectation still describes the
// wire. EVT-151's "MUST carry three headers" is a floor on what a delivery
// carries, not a ceiling (the contract's own wire shape shows Host and
// Content-Type beside them), so an additional header is additive rather than a
// departure.
//
// What it asks of a receiver is stated plainly: to adopt a rotated secret
// without an outage, a receiver accepts a delivery whose EITHER signature
// header verifies under a secret it holds. Outside a rotation window the header
// is absent and there is nothing to consider.
const HeaderPriorSignature = "X-Waiveo-Signature-Prior"

// Endpoint status values (EVT-154).
const (
	EndpointActive   = "active"
	EndpointDisabled = "disabled"
)

// Default* names the contract's own draft-note-proposed starting points
// (contracts/events-1.md EVT-153/154/158) — a caller with no
// operator-configured figure of its own may pass these to NewEndpointState.
// None of the three numbers is fixed by any normative source yet, so none is
// a baked constant the state machine imposes on its own.
const (
	// DefaultMaxConsecutiveFailures is EVT-154's proposed disable threshold:
	// 10 consecutive fully-exhausted deliveries.
	DefaultMaxConsecutiveFailures = 10
	// DefaultBackoffBaseMs / DefaultBackoffCapMs are EVT-153's proposed
	// backoff: starting at 30 seconds, doubling, capped at 1 hour.
	DefaultBackoffBaseMs = 30_000
	DefaultBackoffCapMs  = 3_600_000
	// DefaultRotationOverlapMs is EVT-158's proposed secret-rotation overlap
	// window: 24 hours.
	DefaultRotationOverlapMs = 24 * 60 * 60 * 1000
)

// WebhookSignature computes EVT-151's X-Waiveo-Signature: the hex-encoded
// HMAC-SHA256, keyed by the endpoint's own signing secret, of the ASCII
// string "<timestamp>.<body>" — timestamp already formatted exactly as it
// appears in the X-Waiveo-Timestamp header, body the exact raw JSON bytes
// POSTed. Both signing and verification (AcceptSignature) route through this
// one function, so the two can never compute the signature differently.
func WebhookSignature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// WebhookRequest is one signed delivery attempt: the endpoint URL, the raw
// JSON body (the delivered envelope, EVT-151), and the three headers every
// delivery POST carries. The method is always POST (EVT-151) — the live
// transport (deferred) is the only thing that needs a Method field, so this
// record carries none.
type WebhookRequest struct {
	URL     string
	Body    []byte
	Headers map[string]string
}

// BuildWebhookDelivery builds one signed delivery attempt for env (EVT-151):
// the envelope as the JSON body, and the three headers
// X-Waiveo-Delivery-Id/X-Waiveo-Timestamp/X-Waiveo-Signature. deliveryID is
// caller-supplied and carried through verbatim — the caller (the endpoint's
// own retry loop) is responsible for passing the SAME deliveryID on every
// retry of one logical delivery, which is what makes the id stable across
// retries (EVT-156); this function does not mint or remember one itself.
// timestampSec is likewise caller-formatted (the exact string the
// X-Waiveo-Timestamp header carries and the exact string signed over), so
// signing and the header can never disagree on formatting.
func BuildWebhookDelivery(url, deliveryID, timestampSec, secret string, env Envelope) (WebhookRequest, error) {
	body, err := json.Marshal(env)
	if err != nil {
		return WebhookRequest{}, err
	}
	sig := WebhookSignature(secret, timestampSec, body)
	return WebhookRequest{
		URL:  url,
		Body: body,
		Headers: map[string]string{
			HeaderDeliveryID: deliveryID,
			HeaderTimestamp:  timestampSec,
			HeaderSignature:  sig,
		},
	}, nil
}

// EndpointState is one registered webhook endpoint's delivery state machine,
// driven entirely over an injected clock (nowMs parameters, never
// time.Now()) so a driver can exercise backoff/disable/rotation-overlap
// boundaries deterministically — the same posture reenroll.RateLimiter
// documents for its own injected-clock window.
//
// It is not safe for concurrent use — the deferred delivery loop owns the
// synchronization boundary, exactly like EventLog.
type EndpointState struct {
	// Status is EndpointActive or EndpointDisabled (EVT-154).
	Status string
	// ConsecutiveFailures counts the current unbroken run of failed attempts
	// (reset to 0 by a successful RecordAttempt); reaching
	// maxConsecutiveFailures transitions Status to EndpointDisabled.
	ConsecutiveFailures int
	// LastDeliveredID is this endpoint's own resume cursor (EVT-155): the id
	// of the most recently successfully delivered envelope, advanced only by
	// RecordDelivered. Re-enabling a disabled endpoint, or resuming one
	// recovering from transient failures, resumes from exactly this id via
	// PendingAfter, which itself routes through resume.Resolve — so a
	// LastDeliveredID that has aged past the log's retention window surfaces
	// the EXACT SAME retention_expired gap a WS/SSE reconnect would (EVT-155),
	// never a silently truncated backlog.
	LastDeliveredID string

	maxConsecutiveFailures int
	backoffBaseMs          int64
	backoffCapMs           int64

	secret            string
	priorSecret       string
	hasPriorSecret    bool
	rotatedAtMs       int64
	rotationOverlapMs int64
}

// NewEndpointState constructs an active EndpointState keyed by secret, with
// the given consecutive-failure disable bound (EVT-154), backoff base/cap in
// milliseconds (EVT-153), and secret-rotation overlap window in milliseconds
// (EVT-158). None of these four numbers is fixed by any normative source yet
// (contracts/events-1.md draft-notes) — pass the Default* constants above
// when the caller has no operator-configured figure of its own.
func NewEndpointState(secret string, maxConsecutiveFailures int, backoffBaseMs, backoffCapMs, rotationOverlapMs int64) *EndpointState {
	return &EndpointState{
		Status:                 EndpointActive,
		maxConsecutiveFailures: maxConsecutiveFailures,
		backoffBaseMs:          backoffBaseMs,
		backoffCapMs:           backoffCapMs,
		secret:                 secret,
		rotationOverlapMs:      rotationOverlapMs,
	}
}

// EndpointProgress is the part of an EndpointState a delivery loop must persist
// to survive a restart: the disable/enable status (EVT-154), the current
// unbroken failure run, and the resume cursor (EVT-155). It deliberately
// carries NO secret — a caller that needs to write the secrets writes them
// through its own sealed-at-rest path, and a progress record that happened to
// include them would be one accidental log line away from disclosing one.
type EndpointProgress struct {
	Status              string
	ConsecutiveFailures int
	LastDeliveredID     string
}

// EndpointConfig names the four figures none of which is fixed by any normative
// source yet (contracts/events-1.md EVT-153/154/158 draft-notes), so a
// deployment can carry its own without any of them becoming a baked constant.
// The Default* values above are the contract's own proposals.
type EndpointConfig struct {
	MaxConsecutiveFailures int
	BackoffBaseMs          int64
	BackoffCapMs           int64
	RotationOverlapMs      int64
}

// Progress returns this endpoint's persistable delivery progress.
func (e *EndpointState) Progress() EndpointProgress {
	return EndpointProgress{
		Status:              e.Status,
		ConsecutiveFailures: e.ConsecutiveFailures,
		LastDeliveredID:     e.LastDeliveredID,
	}
}

// RestoreEndpointState rebuilds an EndpointState from persisted progress and
// the endpoint's current signing state — the constructor a delivery loop uses
// on every pass, so nothing about an endpoint's delivery position lives only in
// one process's memory (EVT-155: re-enabling or resuming an endpoint resumes
// from last_delivered_id, which a restart must therefore not forget).
//
// priorSecret is empty when no rotation has happened yet; rotatedAtMs is the
// instant the current secret took over, which is what bounds the overlap
// window. An empty progress.Status restores as active — a row written before
// any delivery attempt has no status to remember and is not disabled.
func RestoreEndpointState(secret, priorSecret string, rotatedAtMs int64, p EndpointProgress, cfg EndpointConfig) *EndpointState {
	status := p.Status
	if status == "" {
		status = EndpointActive
	}
	return &EndpointState{
		Status:                 status,
		ConsecutiveFailures:    p.ConsecutiveFailures,
		LastDeliveredID:        p.LastDeliveredID,
		maxConsecutiveFailures: cfg.MaxConsecutiveFailures,
		backoffBaseMs:          cfg.BackoffBaseMs,
		backoffCapMs:           cfg.BackoffCapMs,
		secret:                 secret,
		priorSecret:            priorSecret,
		hasPriorSecret:         priorSecret != "",
		rotatedAtMs:            rotatedAtMs,
		rotationOverlapMs:      cfg.RotationOverlapMs,
	}
}

// BuildDelivery builds one signed delivery attempt for env under THIS
// endpoint's own signing state as of nowMs — BuildWebhookDelivery's three
// EVT-151 headers, plus HeaderPriorSignature while a rotation's overlap window
// is still open (EVT-158). It is the constructor a delivery loop uses, so
// whether a rotation is in overlap is read from the endpoint's own state rather
// than decided at each call site.
func (e *EndpointState) BuildDelivery(url, deliveryID, timestampSec string, env Envelope, nowMs int64) (WebhookRequest, error) {
	req, err := BuildWebhookDelivery(url, deliveryID, timestampSec, e.secret, env)
	if err != nil {
		return WebhookRequest{}, err
	}
	if e.inRotationOverlap(nowMs) {
		req.Headers[HeaderPriorSignature] = WebhookSignature(e.priorSecret, timestampSec, req.Body)
	}
	return req, nil
}

// inRotationOverlap reports whether a rotation has happened and its overlap
// window (EVT-158) is still open at nowMs — the one place that boundary is
// evaluated, so emission (BuildDelivery) and acceptance (AcceptSignature) can
// never disagree about when the prior secret stops working.
func (e *EndpointState) inRotationOverlap(nowMs int64) bool {
	return e.hasPriorSecret && nowMs-e.rotatedAtMs <= e.rotationOverlapMs
}

// RecordAttempt records the outcome of one delivery attempt at nowMs
// (EVT-153/154). A success (ok=true) resets ConsecutiveFailures to 0 — it
// does NOT by itself advance LastDeliveredID or reactivate a disabled
// endpoint; RecordDelivered and Enable own those transitions respectively, so
// a disabled endpoint's state cannot drift back to active except through the
// explicit operator re-enable EVT-154 requires. A failure increments
// ConsecutiveFailures and, once it reaches maxConsecutiveFailures, transitions
// Status to EndpointDisabled — from that point the endpoint MUST receive no
// further delivery attempts until Enable is called (EVT-154).
func (e *EndpointState) RecordAttempt(ok bool, nowMs int64) {
	if ok {
		e.ConsecutiveFailures = 0
		return
	}
	e.ConsecutiveFailures++
	if e.ConsecutiveFailures >= e.maxConsecutiveFailures {
		e.Status = EndpointDisabled
	}
}

// RecordDelivered advances LastDeliveredID to id (EVT-155) — called once a
// delivery attempt for id has actually succeeded, never speculatively. This
// is the only thing that moves the head of PendingAfter's queue forward, so a
// retried-but-not-yet-successful id keeps being handed out as the queue head
// (EVT-157).
func (e *EndpointState) RecordDelivered(id string) {
	e.LastDeliveredID = id
}

// Enable transitions a disabled endpoint back to active and clears its
// failure run — the explicit operator action EVT-154 requires before any
// further delivery attempt is made. LastDeliveredID is left untouched: a
// re-enabled endpoint resumes from exactly where it left off (EVT-155).
func (e *EndpointState) Enable() {
	e.Status = EndpointActive
	e.ConsecutiveFailures = 0
}

// PendingAfter returns this endpoint's owed backlog against log, in
// ascending id order (EVT-157), together with the gap frame EVT-155 requires
// when LastDeliveredID has aged past log's retention window. A registered
// webhook endpoint is, from the durable-event log's perspective, just
// another subscriber (EVT-155): it MUST get the EXACT SAME retention-window
// gap behavior (Loss markers) a WS/SSE reconnect gets from resume.Resolve —
// never a silently truncated backlog that is bit-for-bit indistinguishable
// from an ordinary clean resume (EVT-143). Gap is nil in exactly two cases:
// a never-yet-delivered endpoint (LastDeliveredID == "", which owes the
// whole retained log — there is no prior point to gap against, unlike
// Resolve's own empty-resume_from "fresh" case which owes nothing), and a
// LastDeliveredID still within retention (an ordinary clean resume). It is
// non-nil, Reason ReasonRetentionExpired, exactly when LastDeliveredID has
// aged out; Events then starts AT the oldest retained id inclusive, matching
// Resolve's own gap delivery (EVT-140/141/143, no silent loss).
//
// It is also the ordered queue a delivery loop drains one id at a time —
// LastDeliveredID only advances via RecordDelivered once an attempt for the
// CURRENT head succeeds, so a retry of that head keeps returning the
// identical head-of-queue envelope: a later id is never attempted ahead of
// an earlier one still within its own retry budget.
//
// rerr is non-nil only if Resolve itself cannot resolve LastDeliveredID at
// all (its own RESUME_FROM_INVALID case) — expected only if the log this
// call is given was reconstructed without the envelope LastDeliveredID names
// (nothing in EndpointState pins log to the same instance across calls, since
// PendingAfter takes it as a parameter every time; a log built over the
// in-memory EventLog rather than the persistent store implementation is
// exactly such a case, since it starts empty on every process). In that case
// Events and Gap are both nil: PendingAfter never
// falls back to log.After("") — a gap-free full backlog would be
// bit-for-bit indistinguishable from an endpoint that has never lost
// anything, which is exactly the "silently treated as fresh" behavior
// EVT-134 forbids for resume_from and the silent-loss behavior EVT-143
// forbids for any discontinuity. The caller (the deferred delivery loop)
// must surface rerr as an operator-visible condition, never resolve it as
// delivery-as-normal.
func (e *EndpointState) PendingAfter(log Log) ([]Envelope, *GapFrame, *ResumeError) {
	if e.LastDeliveredID == "" {
		// Nothing has ever been delivered to this endpoint, so it owes the
		// entire retained log — there is no prior point to gap against.
		return log.After(""), nil, nil
	}

	// Route through the identical mechanism a WS/SSE reconnect uses
	// (EVT-155) — no gap logic is reimplemented here.
	out, rerr := Resolve(log, e.LastDeliveredID)
	if rerr != nil {
		return nil, nil, rerr
	}
	return out.Events, out.Gap, nil
}

// NextBackoffMs returns the capped-exponential backoff (EVT-153) before
// attempt number attempt (1-based: attempt 1 is the first retry after an
// initial failure). It doubles from backoffBaseMs on each successive attempt
// and never exceeds backoffCapMs — a delivery loop retries an endpoint with
// exponential spacing that flattens out at the cap rather than growing
// unbounded.
func (e *EndpointState) NextBackoffMs(attempt int) int64 {
	if attempt < 1 {
		attempt = 1
	}
	backoff := e.backoffBaseMs
	for i := 1; i < attempt && backoff < e.backoffCapMs; i++ {
		backoff *= 2
	}
	if backoff > e.backoffCapMs {
		backoff = e.backoffCapMs
	}
	return backoff
}

// RotateSecret rotates the endpoint's signing secret at atMs (EVT-158): the
// previously current secret becomes the accepted "prior" secret for the
// configured rotationOverlapMs window, and secret becomes newSecret. Only the
// immediately prior secret is ever remembered — a second rotation before the
// first's overlap window elapses replaces it, exactly matching EVT-158's
// "current or the immediately prior secret" wording (never two generations
// back).
func (e *EndpointState) RotateSecret(newSecret string, atMs int64) {
	e.priorSecret = e.secret
	e.hasPriorSecret = true
	e.secret = newSecret
	e.rotatedAtMs = atMs
}

// AcceptSignature reports whether sig is a valid EVT-151 signature over body
// signed at timestamp, evaluated at nowMs (EVT-158): it always accepts the
// current secret, and additionally accepts the immediately prior secret
// (only once RotateSecret has been called) as long as nowMs is still within
// rotationOverlapMs of the rotation. Comparison is constant-time
// (hmac.Equal) — this is a signature check, not a plain string diff.
func (e *EndpointState) AcceptSignature(body []byte, timestamp, sig string, nowMs int64) bool {
	want := []byte(WebhookSignature(e.secret, timestamp, body))
	if hmac.Equal([]byte(sig), want) {
		return true
	}
	if e.inRotationOverlap(nowMs) {
		wantPrior := []byte(WebhookSignature(e.priorSecret, timestamp, body))
		if hmac.Equal([]byte(sig), wantPrior) {
			return true
		}
	}
	return false
}
