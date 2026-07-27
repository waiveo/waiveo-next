// Package webhookdeliver is the caller of internal/events' webhook mechanics:
// it turns a registered endpoint row plus the durable event log into actual
// signed HTTP POSTs, and persists what it delivered so a restart resumes rather
// than replays from nothing (events/1 EVT-150-158).
//
// # What it does not own
//
// Nothing about signing, backoff arithmetic, the disable threshold or the
// rotation overlap is decided here. All of it is internal/events' EndpointState,
// which this package restores from the store, drives, and writes back. This
// package owns exactly three things that state machine deliberately does not:
// the HTTP request, the clock, and durability.
//
// # Why a Tick rather than a goroutine per endpoint
//
// Delivery is one pass over the endpoint inventory, taken by a caller that
// decides when. A goroutine per endpoint would need a timer per backoff
// deadline, and a test would then have to wait on real time for a backoff to
// expire — the one thing the conformance notes rule out ("timing behavior is
// exercised against an injectable clock in a driver harness, not wall-clock
// sleeps"). A deadline is instead an int64 in the persisted row, compared
// against the injected clock at the top of each pass, which is the same
// lazily-checked-deadline shape the rules engine's stabilization window uses.
//
// A production wiring calls Tick on a ticker; a test calls it directly and
// advances the clock between calls. Both drive the identical code path.
//
// # Why one failing endpoint cannot stall a healthy one
//
// A pass makes at most ONE attempt per endpoint and never blocks on an
// endpoint's own retry schedule: a failure sets that endpoint's
// next_attempt_at_ms and the pass moves on. Every request also carries a
// bounded per-attempt timeout (EVT-153), so an endpoint that accepts a
// connection and then never answers costs one timeout on its own turn rather
// than the whole pass forever.
//
// # Why the cursor is resolved against the WHOLE log
//
// events.Resolve answers over the whole log; events.ResolveVisible answers over
// one principal's visible set. A subscriber-facing binding MUST use the latter,
// because a resume_from is a value the CLIENT supplies and resolving it against
// the whole log would tell that client whether an id it cannot read exists
// (EVT-134a). None of that applies here, and it is worth saying why rather than
// copying the stricter call because it is stricter:
//
//   - The cursor is never client-supplied. last_delivered_id is written by this
//     package after a 2xx and read back by this package. There is no channel on
//     which anyone could supply a probe value, and the resolution's outcome is
//     never echoed to the receiver — a receiver observes deliveries, not resume
//     results. The probe EVT-134a closes does not exist on this path.
//   - Visible-set resolution would misreport RETENTION. AgedOut is an ordering
//     comparison against the retention floor, but the "is it still there?" half
//     is answered over the visible set — so an endpoint whose scope happens to
//     have nothing retained above its cursor would draw RESUME_FROM_INVALID for
//     a perfectly good cursor, and this package would raise an operator alarm
//     about a healthy endpoint.
//   - Scope filtering still happens, exactly where the contract puts it: per
//     event, at delivery time (EVT-123). deliverable() below applies the
//     endpoint's own scope subtree and schemas restriction to every envelope
//     before it is POSTed, so no out-of-scope event reaches a receiver whichever
//     resolution found the backlog.
//
// events.Resolve's own doc records this as the case it was kept for, with an
// explicit everything-visible predicate rather than a nil "unrestricted" one.
//
// # Delivery is at-least-once
//
// The cursor advances only AFTER a 2xx is observed and only via a committed
// write, so a crash between the receiver accepting a delivery and the cursor
// landing redelivers that id on the next pass. That is EVT-156's contract, and
// it is why X-Waiveo-Delivery-Id is stable across the retries of one logical
// delivery: it is the key a receiver deduplicates on. The opposite choice —
// advancing the cursor before the POST — would be at-most-once and would drop
// events silently on exactly the same crash.
package webhookdeliver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
)

// DefaultMaxAttempts is EVT-153's proposed retry budget for ONE logical
// delivery: give up after 15 attempts. Like every other figure here it is a
// draft-note proposal, not a normative number, so it is a Config default rather
// than a baked constant.
const DefaultMaxAttempts = 15

// DefaultRequestTimeout is EVT-153's proposed per-attempt request timeout.
const DefaultRequestTimeout = 10 * time.Second

// Doer is the HTTP client seam — satisfied by *http.Client, and by a test's own
// in-process receiver dialer. It is an interface so a driver can observe the
// exact request bytes without an outbound socket.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config wires a Deliverer. Store, Log, HTTP, NowMs, NewID and Secrets are
// required; the rest fall back to the contract's own proposed figures.
type Config struct {
	Store *store.Store
	// Log is the durable event log the backlog is read from. It is the
	// events.Log INTERFACE rather than one storage choice, so the persistent
	// store implementation and an in-memory one are both drivable.
	Log  events.Log
	HTTP Doer
	// NowMs is the injected clock. Every deadline this package evaluates is
	// compared against it and nothing reads a wall clock directly, so a driver
	// can step across a backoff or a rotation boundary exactly.
	NowMs func() int64
	// NewID mints one X-Waiveo-Delivery-Id per LOGICAL delivery (not per
	// attempt) — a ULID, per EVT-151.
	NewID func() string
	// Secrets opens the sealed signing secrets. Required: an endpoint whose
	// secret cannot be opened is skipped, never delivered to unsigned.
	Secrets *Secrets

	MaxAttempts    int
	RequestTimeout time.Duration
	Endpoint       events.EndpointConfig

	// OnDisabled is EVT-154's operator-facing signal, raised once at the moment
	// an endpoint crosses the consecutive-failure bound. It is a hook rather than
	// a hard-wired emission because "raise a signal" is a deployment's decision
	// (a Repairs issue, a page, a log line) and this package has no business
	// picking one. Optional; the persisted disabled status is durable either way,
	// so a deployment that wires nothing still has the fact, just not the alert.
	OnDisabled func(endpointID, url string)
	// OnError reports a per-endpoint condition that is not a delivery failure —
	// an unopenable secret, a cursor the log cannot resolve. Optional.
	OnError func(endpointID string, err error)
}

// Deliverer makes one delivery pass per Tick over every registered endpoint.
//
// It holds no per-endpoint memory: every pass restores each endpoint's state
// from the store and writes it back, so two processes never disagree about a
// cursor and a restart loses nothing. It is not safe for concurrent Ticks —
// one pass owns the read-modify-write of every row it touches.
type Deliverer struct {
	cfg Config
}

// New validates cfg and applies the contract's proposed defaults to any figure
// the caller left at zero.
func New(cfg Config) (*Deliverer, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("webhookdeliver: a store is required")
	case cfg.Log == nil:
		return nil, errors.New("webhookdeliver: an event log is required")
	case cfg.HTTP == nil:
		return nil, errors.New("webhookdeliver: an HTTP doer is required")
	case cfg.NowMs == nil:
		return nil, errors.New("webhookdeliver: an injected clock is required — this package reads no wall clock of its own")
	case cfg.NewID == nil:
		return nil, errors.New("webhookdeliver: a delivery-id source is required")
	case cfg.Secrets == nil:
		return nil, errors.New("webhookdeliver: a secret opener is required — an endpoint is never delivered to unsigned")
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	if cfg.Endpoint.MaxConsecutiveFailures <= 0 {
		cfg.Endpoint.MaxConsecutiveFailures = events.DefaultMaxConsecutiveFailures
	}
	if cfg.Endpoint.BackoffBaseMs <= 0 {
		cfg.Endpoint.BackoffBaseMs = events.DefaultBackoffBaseMs
	}
	if cfg.Endpoint.BackoffCapMs <= 0 {
		cfg.Endpoint.BackoffCapMs = events.DefaultBackoffCapMs
	}
	if cfg.Endpoint.RotationOverlapMs <= 0 {
		cfg.Endpoint.RotationOverlapMs = events.DefaultRotationOverlapMs
	}
	return &Deliverer{cfg: cfg}, nil
}

// endpointRow is the registered endpoint's public configuration, read out of
// the resource row's body. Fields this package does not act on are ignored.
type endpointRow struct {
	ID        string   `json:"id"`
	ScopeNode string   `json:"scope_node"`
	URL       string   `json:"url"`
	Schemas   []string `json:"schemas"`
}

// Tick makes at most one delivery attempt per eligible endpoint and returns
// when the pass is done.
//
// An error is returned only for a failure that made the PASS impossible (the
// endpoint inventory could not be read). A per-endpoint problem is reported
// through OnError and stepped past — one endpoint's broken configuration is not
// a reason to stop delivering to the others, which is the same per-item failure
// isolation the job runner applies to its own targets.
func (d *Deliverer) Tick(ctx context.Context) error {
	rows, err := d.cfg.Store.List(ctx, store.KindWebhookEndpoint, store.ListFilter{})
	if err != nil {
		return fmt.Errorf("webhookdeliver: read endpoint inventory: %w", err)
	}
	nodes, err := d.cfg.Store.ScopeNodes(ctx)
	if err != nil {
		return fmt.Errorf("webhookdeliver: read scope tree: %w", err)
	}
	tree, _ := datamodel.BuildScopeTree(nodes)

	live := make(map[string]bool, len(rows))
	for _, res := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ep endpointRow
		if err := json.Unmarshal(res.Body, &ep); err != nil {
			d.reportErr(res.ID, fmt.Errorf("decode endpoint row: %w", err))
			continue
		}
		ep.ID = res.ID
		live[res.ID] = true
		if err := d.deliverOne(ctx, ep, tree); err != nil {
			d.reportErr(res.ID, err)
		}
	}
	d.pruneOrphans(ctx, live)
	return nil
}

// pruneOrphans drops delivery state whose endpoint row is gone. Deleting an
// endpoint goes through the generic resource handler, which knows nothing about
// this table, so reconciling here is what stops a deleted endpoint's sealed
// secret from outliving it. It is best-effort: a failure to prune is reported
// and retried on the next pass rather than failing the pass.
func (d *Deliverer) pruneOrphans(ctx context.Context, live map[string]bool) {
	states, err := d.cfg.Store.WebhookDeliveryStates(ctx)
	if err != nil {
		d.reportErr("", fmt.Errorf("read delivery state for orphan pruning: %w", err))
		return
	}
	for _, st := range states {
		if live[st.EndpointID] {
			continue
		}
		if err := d.cfg.Store.DeleteWebhookDeliveryState(ctx, st.EndpointID); err != nil {
			d.reportErr(st.EndpointID, fmt.Errorf("prune delivery state of a deleted endpoint: %w", err))
		}
	}
}

func (d *Deliverer) reportErr(endpointID string, err error) {
	if d.cfg.OnError != nil {
		d.cfg.OnError(endpointID, err)
	}
}

// deliverOne makes at most one attempt for ep.
func (d *Deliverer) deliverOne(ctx context.Context, ep endpointRow, tree datamodel.ScopeTree) error {
	now := d.cfg.NowMs()

	persisted, err := d.cfg.Store.WebhookDeliveryStateFor(ctx, ep.ID)
	if errors.Is(err, store.ErrWebhookStateNotFound) {
		// Registered but never given a signing secret. There is nothing to sign
		// with, and an unsigned delivery is not a delivery this contract defines
		// (EVT-151), so the endpoint simply waits.
		return nil
	}
	if err != nil {
		return err
	}
	// EVT-154: a disabled endpoint receives no further delivery attempts until an
	// operator re-enables it.
	if persisted.Status == events.EndpointDisabled {
		return nil
	}
	// The backoff gate (EVT-153). Checked here rather than slept on, so a failing
	// endpoint costs this pass nothing and the healthy ones behind it are served.
	if now < persisted.NextAttemptAtMs {
		return nil
	}

	secret, prior, err := d.cfg.Secrets.Open(ep.ID, persisted)
	if err != nil {
		// The sealed value is never echoed — it is credential material.
		return err
	}
	if secret == "" {
		return nil
	}

	state := events.RestoreEndpointState(secret, prior, persisted.RotatedAtMs, events.EndpointProgress{
		Status:              persisted.Status,
		ConsecutiveFailures: persisted.ConsecutiveFailures,
		LastDeliveredID:     persisted.LastDeliveredID,
	}, d.cfg.Endpoint)

	pending, gap, rerr := state.PendingAfter(d.cfg.Log)
	if rerr != nil {
		// The cursor names an id this log cannot resolve at all. PendingAfter
		// deliberately does not fall back to a gap-free full backlog, and neither
		// does this: that would be bit-for-bit indistinguishable from an endpoint
		// that never lost anything (EVT-134/143). It is an operator-visible
		// condition, and delivery for this endpoint stops until someone acts.
		return fmt.Errorf("webhookdeliver: this endpoint's delivery cursor did not resolve against the event log (%s)", rerr.Code)
	}
	if gap != nil {
		// EVT-155 routes a webhook resume through the SAME retention-window gap
		// behavior a WS/SSE reconnect gets. The webhook wire carries no gap frame
		// (EVT-151's body is an envelope), so the marker cannot be delivered to
		// the receiver; it is surfaced to the operator instead, and delivery
		// resumes AT to_id inclusive — never a silently truncated backlog.
		d.reportErr(ep.ID, fmt.Errorf("webhookdeliver: delivery resumed with a %s gap from %s to %s — events in that range were never delivered to this endpoint",
			gap.Reason, gapFrom(gap), gap.ToID))
	}

	filter := endpointFilter(ep, tree)
	env, ok := nextDeliverable(pending, filter)
	if !ok {
		// Everything owed has either been delivered or is outside this endpoint's
		// scope. Advance the cursor over what was CONSIDERED so a scope-filtered
		// endpoint does not re-walk the same non-matching tail on every pass.
		return d.advanceOverSkipped(ctx, persisted, pending, state)
	}

	deliveryID := persisted.DeliveryID
	if deliveryID == "" || persisted.Attempt == 0 {
		// One id per LOGICAL delivery, minted when that delivery starts and
		// carried verbatim through every retry of it (EVT-151/156).
		deliveryID = d.cfg.NewID()
	}
	timestamp := strconv.FormatInt(now/1000, 10)

	req, err := state.BuildDelivery(ep.URL, deliveryID, timestamp, env, now)
	if err != nil {
		return fmt.Errorf("webhookdeliver: build signed delivery: %w", err)
	}

	ok = d.post(ctx, req)
	next := persisted
	next.EndpointID = ep.ID

	if ok {
		state.RecordAttempt(true, now)
		state.RecordDelivered(env.ID)
		p := state.Progress()
		next.Status, next.ConsecutiveFailures, next.LastDeliveredID = p.Status, p.ConsecutiveFailures, p.LastDeliveredID
		next.Attempt, next.DeliveryID, next.NextAttemptAtMs = 0, "", 0
		return d.cfg.Store.PutWebhookDeliveryProgress(ctx, next)
	}

	// A failed attempt does NOT advance the cursor, so the next pass hands out
	// the identical head-of-queue envelope: a later id is never attempted ahead
	// of an earlier one still inside its own retry budget (EVT-157).
	next.Attempt = persisted.Attempt + 1
	next.DeliveryID = deliveryID
	if next.Attempt >= d.cfg.MaxAttempts {
		// This logical delivery has exhausted EVT-153's budget. That — not a
		// single failed attempt — is what EVT-154 counts toward the disable
		// threshold, so RecordAttempt(false) is called exactly once per exhausted
		// delivery, never once per retry.
		state.RecordAttempt(false, now)
		next.Attempt, next.DeliveryID = 0, ""
	}
	// NextBackoffMs is 1-based on the RETRY, not on the attempt just made: after
	// the first failure the next attempt is retry 1 and waits the base interval.
	// A budget-exhausted delivery reset Attempt to 0 just above, so the next
	// logical delivery starts back at the base rather than inheriting the cap.
	next.NextAttemptAtMs = now + state.NextBackoffMs(next.Attempt)

	p := state.Progress()
	next.Status, next.ConsecutiveFailures, next.LastDeliveredID = p.Status, p.ConsecutiveFailures, p.LastDeliveredID
	if err := d.cfg.Store.PutWebhookDeliveryProgress(ctx, next); err != nil {
		return err
	}
	if p.Status == events.EndpointDisabled && persisted.Status != events.EndpointDisabled && d.cfg.OnDisabled != nil {
		d.cfg.OnDisabled(ep.ID, ep.URL)
	}
	return nil
}

// advanceOverSkipped moves the cursor past a run of envelopes this endpoint's
// filter excluded, so a narrow endpoint does not re-read the same non-matching
// tail forever.
//
// It advances over what the endpoint CONSIDERED rather than only what it
// received — the same watermark rule a transport's own cursor follows. Nothing
// is lost by it: an excluded envelope was never deliverable to this endpoint,
// so a cursor sitting behind it would only ever produce the identical exclusion
// again.
func (d *Deliverer) advanceOverSkipped(ctx context.Context, persisted store.WebhookDeliveryState, pending []events.Envelope, state *events.EndpointState) error {
	if len(pending) == 0 {
		return nil
	}
	head := pending[len(pending)-1].ID
	if head == persisted.LastDeliveredID {
		return nil
	}
	state.RecordDelivered(head)
	next := persisted
	next.LastDeliveredID = head
	next.Attempt, next.DeliveryID, next.NextAttemptAtMs = 0, "", 0
	return d.cfg.Store.PutWebhookDeliveryProgress(ctx, next)
}

// post issues one attempt and reports whether it drew a 2xx within the bounded
// per-attempt timeout (EVT-153). Anything else — a transport error, a timeout,
// a non-2xx status — is a failure; there is no status this treats as
// "succeeded enough".
func (d *Deliverer) post(ctx context.Context, wr events.WebhookRequest) bool {
	rctx, cancel := context.WithTimeout(ctx, d.cfg.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodPost, wr.URL, bytes.NewReader(wr.Body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range wr.Headers {
		req.Header.Set(k, v)
	}
	resp, err := d.cfg.HTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// The body is drained and discarded: a receiver's response body is not part
	// of this contract, and leaving it unread would leak the connection.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// nextDeliverable returns the first envelope in the owed backlog this endpoint
// may actually receive, preserving id order (EVT-157).
func nextDeliverable(pending []events.Envelope, f events.Filter) (events.Envelope, bool) {
	for _, env := range pending {
		if f.Allows(env) {
			return env, true
		}
	}
	return events.Envelope{}, false
}

// endpointFilter is the endpoint's own delivery predicate (EVT-120/123/124):
// its scope node and everything below it, narrowed by its schemas restriction
// if it declared one.
//
// It is built from events.NewFilter — the same construction the WS/SSE bindings
// use — rather than an open-coded comparison, so "which events does this
// subscriber receive" has one implementation whatever the transport.
func endpointFilter(ep endpointRow, tree datamodel.ScopeTree) events.Filter {
	inSubtree := func(ancestor, node string) bool {
		if ancestor == node {
			return false
		}
		for _, id := range tree.AncestorChain(node) {
			if id == ancestor {
				return true
			}
		}
		return false
	}
	canRead := func(node string) bool {
		if ep.ScopeNode == "" {
			// An endpoint with no placement reads nothing, rather than
			// everything: an unresolvable scope is the empty one (SEC-005).
			return false
		}
		return node == ep.ScopeNode || inSubtree(ep.ScopeNode, node)
	}
	return events.NewFilter(canRead, apiselector.Selector{}, inSubtree, ep.Schemas)
}

func gapFrom(g *events.GapFrame) string {
	if g == nil || g.FromID == nil {
		return "(none)"
	}
	return *g.FromID
}
