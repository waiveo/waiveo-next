package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/automation"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/keepalive"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/relay/schedulehost"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// pullFunc pulls one verified desired-state generation, given the caller's
// last-applied generation as the REL-050 since_generation claim. The running
// binary wraps a state.pull over the persistent connection (pullOverFrames —
// which verifies the snapshot's hash + signature against the enrollment-
// anchored trust anchor and enforces generation monotonicity, REL-052/071);
// a test injects a fake returning a scripted generation sequence. A pull
// failure is returned as an error the live-apply path treats as non-fatal
// (REL-055). A same-generation state.unchanged answer surfaces as an Applied
// whose Generation equals the claim — the caller's own monotonic guard then
// makes it a no-op.
type pullFunc func(sinceGeneration int64) (desiredstate.Applied, error)

// scheduleDriver owns the serving side of a desired-state generation apply for
// one relay: apply installs that generation's revocation view (REL-066), its
// app-authored per-screen program baseline (REL-061), and its schedule
// resolvers + background re-resolve loops over the player server. A subsequent apply cancels the prior generation's
// loops before installing the new ones (the REL-056 atomic-swap discipline).
// Cancellation stops the loops but cannot interrupt a resolver goroutine already
// mid-resolve, so a superseded generation's late write could otherwise land
// after the new serve; that hazard is fenced at the write point instead — every
// resolver stamps its generation onto playerserver.Server.SetProgram, which
// drops a strictly-older generation's write (REL-052/056), so a stale in-flight
// resolver can never revert the new serve. apply is driven from the boot path
// (once) and from rePuller.tick, which serializes every live apply under its own
// lock, so neither the cancel handoff nor the resolver-set handoff below needs a
// lock of its own. (Each RESOLVER has one, for the one field an apply reads
// across the cancel boundary — schedulehost.Resolver.CarryState.)
type scheduleDriver struct {
	srv       *playerserver.Server
	sink      *automation.CommandSink
	site      hello.SiteBinding
	tickEvery time.Duration

	// cancel tears down the currently-installed generation's resolve loops; nil
	// before the first apply.
	cancel context.CancelFunc

	// resolvers is the currently-installed generation's resolvers keyed by the
	// scope node each resolves, and it is here for ONE reason: the next apply
	// hands each node's rising-edge baseline to the resolver replacing it, so a
	// rebuild is not mistaken for a boot and does not re-fire that node's preset
	// batch at real devices. Empty before the first apply — which is what makes
	// the boot apply a genuine resume edge.
	// (schedulehost.Resolver.AdoptCarriedState owns the argument.)
	resolvers map[string]*schedulehost.Resolver
}

// apply installs applied's serving state: this generation's revocation view
// (REL-066) first, then its app-authored per-screen `screen_programs` baseline,
// then its schedule resolution over the top of that (resolveAndServe at nowMs,
// plus the background re-resolve loops).
// The new loops run under a child of ctx, so cancelling ctx (process shutdown)
// stops them too. It returns how many baseline entries were installed, and the
// resolvers built (empty when the schedule governs no carried screen — the
// additive serving policy leaves the app-authored program in place).
//
// # Why the baseline is installed HERE and not only at boot
//
// It used to be installed only at boot (main's own serveAppAuthoredPrograms over
// the persisted copy), and the live re-pull path re-drove the schedule alone.
// That was survivable only while every schedule resolver unconditionally wrote
// one shared program slot, because a new generation's resolution then reached
// served state that way whatever the topology. It stopped being survivable the
// moment a resolution is served only where the relay can attribute it to a
// screen (soleServedScreenID): on any site that is not exactly-one-governed-node
// -and-exactly-one-screen, resolveAndServe now writes NOTHING, so an operator
// edit produced a new generation that the relay pulled, applied, logged — and
// never served, every screen staying on the boot generation's program until the
// process restarted. The generation's own baseline is what carries an edit to
// those screens, so every apply re-installs it, not just the first one.
//
// # What the baseline does NOT carry: a removal
//
// This re-install is purely additive, and the claim above should be read no more
// broadly than that: it carries an edit to every screen the new generation still
// NAMES. A screen DROPPED from the new generation's `screen_programs` is not
// named by any write here, so nothing clears the program it is already serving —
// it keeps serving its last one, live, indefinitely. A restart would serve that
// same screen data-model/1's terminal default (DAT-118), because the boot path
// installs only what the persisted generation carries. So live state and
// restart state disagree about the same persisted generation, and nothing logs
// the divergence.
//
// That is a KNOWN gap, deliberately not closed here rather than an oversight. It
// is not a regression — before this function re-installed the baseline at all, a
// removed screen kept an even staler BOOT-generation program, and a restart was
// the fix then exactly as it is now. Implementing removal means deciding what a
// dropped entry means (deleted screen? unauthored? a generation built from a
// partial view?) and what the relay should serve instead, which is a contract
// question, not a wiring one.
//
// The revocation view installed above is NOT that decision, and must not be
// mistaken for it. `player/1` PLY-075 says a relay presented a `screen_id` it
// "no longer recognizes at all" refuses that credential terminally — but a
// screen's absence from `screen_programs` does not establish that it is
// unrecognized. The feeder OMITS an entry for a screen whose effective timezone
// will not resolve (`data-model/1` DAT-034, snapshot.DeriveScreenPrograms), a
// degrade a live screen row reaches just by being placed under a node carrying
// no geo. A relay that inferred revocation from a dropped entry would answer
// that recoverable authoring mistake with CHANNEL_TOKEN_REVOKED, which PLY-073
// requires the player treat as terminal — clearing its credential and
// re-pairing. `revoked` is the channel the app peer states a deletion on
// explicitly (REL-066), and nothing else in a snapshot carries a screen roster
// to derive one from, so this stays the app peer's statement to make.
//
// # Ordering
//
// Baseline BEFORE resolution, matching the boot path exactly. Both write at
// applied.Generation, and SetProgram's fence admits a same-generation write
// (only a strictly OLDER one is dropped), so whichever runs last wins for a
// screen the schedule attributes a resolution to — which must be the resolver.
// Inverting these two would have a fresh resolution clobbered by the baseline it
// is supposed to replace, on exactly the sites where the resolution is served.
func (d *scheduleDriver) apply(ctx context.Context, applied desiredstate.Applied, nowMs int64) (installed int, resolvers []*schedulehost.Resolver) {
	if d.cancel != nil {
		d.cancel() // tear down the prior generation's resolve loops before the swap
	}
	// Harvest the outgoing generation's rising-edge baselines, one per governed
	// scope node, for the resolvers about to replace them. AFTER the cancel
	// above, so the value harvested is as close to final as cancellation can
	// make it.
	//
	// A DEVICE-PLANE apply cost, not a serving one: without this, every apply
	// rebuilt each resolver from a nil baseline, datamodel.PresetTransition read
	// that as the empty daypart identity, and the currently effective daypart
	// looked like a rising edge — so a full preset batch (display_power, input
	// select, volume) was re-dispatched to real hardware on every apply,
	// including the feeder's 12-hourly content-URL re-mint, which authors
	// nothing (cmd/waiveo-feeder/remint.go).
	//
	// Residual, and bounded: cancellation cannot interrupt a resolve already in
	// flight, so a boundary crossed inside that window could be harvested either
	// side of the tick that fired it. Harvesting the earlier value re-fires that
	// one batch once — a genuine boundary's own batch, at that boundary, which
	// DAT-075 states outright is safely re-applied. Harvesting the later value
	// (the ordinary outcome) fires it exactly once. Neither can miss it.
	carried := d.carryStates()
	// This generation's revocation view (REL-066) BEFORE its programs, and
	// before its schedule resolves over them. Ordering is a real choice on a
	// LIVE apply, where the player/1 routes are already serving: installing
	// revocation first means there is no instant at which a screen this
	// generation revokes can pull the program the same generation carries. The
	// inverse order leaves exactly that window open, narrow but real.
	//
	// Installed here rather than only at boot, for the reason the baseline
	// re-install below is: boot-only wiring goes stale the moment a generation
	// is applied live. A revocation authored after the relay started would
	// otherwise never be enforced until the process restarted — which is the
	// whole failure mode REL-123 exists to prevent, since a relay is expected
	// to run for weeks between restarts and to enforce a synced revocation
	// while disconnected. This one call site covers BOTH paths: boot applies
	// through this same function (main's own driver.apply, before Register
	// mounts any route), so no screen is ever served before it runs — and on
	// the OFFLINE boot path main fills applied.Revoked from the durable store
	// first (desiredstate.ServedRevocation), so a relay restarting mid-outage
	// installs its last-synced set rather than an empty one.
	//
	// A returned error means only that the DURABLE half of the session drop
	// failed; the revocation itself is installed regardless (SetRevokedScreens'
	// own doc). Logged rather than fatal: refusing to serve at all would turn a
	// storage hiccup into the outage, while the revocation this generation
	// carries is already being enforced. What is lost is durability of the
	// drop — the dropped sessions could come back on the next restart — so it
	// says so.
	if err := d.srv.SetRevokedScreens(applied.Generation, applied.Revoked); err != nil {
		log.Printf("waiveo-relay: generation %d's revocation is enforced, but dropping the revoked screens' durable sessions failed (%v); a restart could revive a credential minted before the revocation", applied.Generation, err)
	}
	installed = serveAppAuthoredPrograms(d.srv, applied.Generation, applied.ScreenPrograms)
	loopCtx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	resolvers = resolveAndServe(loopCtx, applied, d.srv, d.sink, d.site, d.tickEvery, nowMs, carried)
	d.resolvers = make(map[string]*schedulehost.Resolver, len(resolvers))
	for _, r := range resolvers {
		d.resolvers[r.ScopeNodeID()] = r
	}
	return installed, resolvers
}

// carryStates is the outgoing generation's rising-edge baselines keyed by scope
// node — each installed resolver's schedulehost.Resolver.CarryState — for the
// resolvers about to replace them. Nil before the first apply, and nil for a
// node whose resolver never completed a resolve: in both cases the replacement
// starts from no baseline, which is the genuine resume edge DAT-075 names.
//
// Each baseline carries the authored rows it was resolved from, because whether
// the replacement may adopt it depends on whether THIS generation re-authored
// them (schedulehost.Resolver.AdoptCarriedState, which owns that decision).
func (d *scheduleDriver) carryStates() map[string]*schedulehost.CarriedBaseline {
	if len(d.resolvers) == 0 {
		return nil
	}
	carried := make(map[string]*schedulehost.CarriedBaseline, len(d.resolvers))
	for nodeID, r := range d.resolvers {
		if st := r.CarryState(); st != nil {
			carried[nodeID] = st
		}
	}
	return carried
}

// rePuller applies a re-pulled desired-state generation to the relay's live
// serving state. It tracks the last-applied generation so a same/lower generation
// is a no-op (REL-052/070) and only a strictly higher one is applied (REL-056):
// the schedule resolvers are re-driven (scheduleDriver.apply) and the automation
// engine's edge rules reloaded (automationhost.Host.ApplyEdgeRules). A pull error
// is non-fatal — the last-applied program keeps being served (REL-055).
//
// tick is driven from two places in the running binary — the state.changed
// nudge dispatcher (REL-057, one goroutine per connection) and the reconnect
// supervisor's pull-on-reconnect (its own goroutine) — so every live apply
// serializes under mu. There is no periodic ticker: the persistent
// connection's nudges replace the legacy 3s poll, a lost nudge is recovered
// by the pull-on-reconnect (REL-057 best-effort delivery), and a nudge that
// arrives mid-tick coalesces in the connection's own latest-wins cell.
type rePuller struct {
	pull  pullFunc
	nowFn func() int64

	mu     sync.Mutex
	driver *scheduleDriver
	host   *automationhost.Host

	// Both of the following consume the SAME applied generation's
	// `device_inventory` (REL-063) — the app peer's adopted-device set — and are
	// refreshed HERE, on every apply, rather than only at boot. Adoption is an
	// authored decision: an operator who adopts a device this afternoon expects
	// the relay to act on it this afternoon, and a boot-only set would leave that
	// decision inert until the process next restarted — the exact staleness class
	// this live-apply path exists to close. It rides the generation apply rather
	// than a clock of its own because adoption IS desired state: a device adopted
	// in the console becomes drivable the moment the generation carrying that
	// decision applies, and one un-adopted stops being drivable at the same
	// instant, with no window where the two views disagree.

	// applyInventory installs the adopted-device set into the relay's
	// drivable-device gate and the pollers configured from it.
	//
	// Optional (nil in tests that drive only the serving side).
	applyInventory func(wire.DeviceInventory)

	// adoption is the screen keep-alive capability's adoption gate
	// (internal/relay/keepalive). nil when keep-alive is disabled — nothing
	// consults the set then, and applying to it would only log.
	adoption *keepalive.AdoptionSet

	// geo re-points the live consumers of the site's effective geo — the
	// automation engine's location and a native slide's weather coordinates —
	// at each freshly adopted site_binding (see siteGeo). Unlike everything
	// above it is driven from adoptSite, not from an apply: the site arrives on
	// the hello-ack, one handshake BEFORE the pull, and an offline boot's
	// missing coordinates must be corrected the moment a connection exists
	// rather than waiting for the next authored generation.
	//
	// Optional (nil in tests that drive only the serving side).
	geo *siteGeo

	lastGen int64
	// lastHash is the section hash of the last snapshot whose apply-time effects
	// actually ran — REL-070's comparison value. Empty until the first apply,
	// which is why the hash guard tests for that explicitly: an empty hash equals
	// an empty hash, and a relay that has applied nothing must not treat its first
	// real snapshot as a repeat of it.
	lastHash string
}

// adoptSite adopts the app peer's authoritative site_binding (REL-036) — the
// reconnect supervisor calls this with each fresh connection's hello-ack site
// before its pull-on-reconnect, so a site rebound while the relay was offline
// takes effect on the very next apply. Serialized under the same lock every
// tick applies under.
//
// It adopts the binding into BOTH of its consumers, and that pairing is the
// point rather than a convenience. The schedule-resolver site (driver.site) was
// re-adopted here from the start; the site's GEO — the engine's location and a
// slide's weather coordinates — was installed once at boot and never again, so
// a relay that booted with no app peer (offline-serve, REL-055/061) kept asking
// the weather at the zero binding's (0,0) forever, correcting itself only on a
// restart. There is exactly one moment a fresh authoritative site exists, and
// this is it: everything derived from the site is re-derived here, together, or
// the next consumer added will inherit the same staleness bug.
func (p *rePuller) adoptSite(site hello.SiteBinding) {
	p.mu.Lock()
	p.driver.site = site
	p.mu.Unlock()
	// Outside p.mu: geo's own consumers take their own locks (the player
	// server's, the automation host's), and nothing in adoptSite's own critical
	// section is read by them.
	p.geo.adopt(site)
}

// tick performs one live pull and returns whether it applied a new generation.
//
//   - A pull error (transport failure, no live connection, or the pull
//     rejecting a regressed generation as desiredstate.ErrGenerationRegressed)
//     is logged and ignored: the last-applied generation stays served
//     (REL-055 / REL-052).
//   - A pulled generation not strictly greater than the last-applied one is a
//     no-op — no re-resolve churn (REL-070; and this guard mirroring REL-052
//     for a lower one). A state.unchanged answer lands here by construction.
//   - A strictly higher generation is applied: the pull has already persisted
//     it atomically (REL-056); tick re-drives the live serving state to
//     match — re-installing the app-authored per-screen baseline and
//     re-resolving the schedule over it (scheduleDriver.apply), refreshing the
//     redeemable pairing grants, and reloading the edge rules.
func (p *rePuller) tick(ctx context.Context) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	applied, err := p.pull(p.lastGen)
	if err != nil {
		if errors.Is(err, desiredstate.ErrGenerationRegressed) {
			log.Printf("waiveo-relay live pull: rejected regressed generation (REL-052); keeping last-applied generation %d", p.lastGen)
		} else {
			log.Printf("waiveo-relay live pull: pull failed (%v); keeping last-applied generation %d (REL-055)", err, p.lastGen)
		}
		return false
	}
	if applied.Generation <= p.lastGen {
		// Same/lower generation — no re-resolve churn (REL-052). A state.unchanged
		// answer lands here: it surfaces as an Applied carrying exactly the
		// generation asked about and NO hash, so this guard — not the hash one
		// below — is what makes it a no-op. Both are needed; neither subsumes the
		// other.
		return false
	}
	// REL-070's actual no-op condition: a snapshot whose hash equals the
	// last-applied hash MUST NOT re-run any apply-time side effect, and that
	// holds "regardless of whether `generation` itself advanced".
	//
	// The generation guard above cannot implement that, because the case the
	// requirement is about is precisely a HIGHER generation carrying byte-identical
	// sections — it passes the guard and re-runs everything below: cancelling the
	// prior generation's in-flight schedule-resolve loops (which REL-070 names
	// outright), re-installing served programs, replacing the pairing-grant set,
	// reloading edge rules.
	//
	// The hash was always available — verified against a recompute over the
	// sections and persisted beside the generation (REL-053/055) — it simply was
	// not carried to the one place that decides whether to re-run.
	if applied.Hash != "" && applied.Hash == p.lastHash {
		log.Printf("waiveo-relay live pull: generation %d carries the same section hash as the last applied generation %d — treating the apply as a no-op, no apply-time side effect re-run (REL-070)",
			applied.Generation, p.lastGen)
		// The generation still advances: REL-070 suppresses the apply-time EFFECTS,
		// not the record of what was applied. The pull already persisted
		// {generation, hash} atomically, and the ack reports the advanced
		// generation — a relay that reported the stale one would look permanently
		// behind to the app peer.
		p.lastGen = applied.Generation
		return false
	}

	// Strictly higher generation — apply it live. The pull already persisted the
	// new generation atomically (REL-056); re-drive the serving side to match —
	// this generation's app-authored per-screen programs AND the schedule
	// resolution over them, in that order (scheduleDriver.apply's own doc).
	installed, _ := p.driver.apply(ctx, applied, p.nowFn())

	// Refresh the redeemable pairing-grant set from this SAME verified
	// generation (relay/1 REL-122: a pairing grant remains redeemable "until a
	// newer generation supersedes it"). Without this, playerserver.Server's
	// grant set stays exactly what NewServer built at boot for the rest of the
	// process's life — a screen needing a grant this live pull just carried
	// would never see it become redeemable.
	p.driver.srv.SetPairingGrants(applied.Generation, applied.PairingGrants)

	// Install this generation's adopted-device set (REL-063) into BOTH consumers,
	// from the SAME verified generation. Both calls are bracketed deliberately:
	//
	//   - AFTER the serving state above, because adopting a screen is permission
	//     to DRIVE it, and driving it before this generation's program is
	//     installed would re-launch a channel into the previous generation's
	//     content.
	//   - BEFORE the edge rules reload below, because a reloaded rule can fire a
	//     device_command on the very next observation, and it must resolve
	//     against the adoption decision of the generation that carries it, not
	//     the previous one's.
	if p.applyInventory != nil {
		p.applyInventory(applied.DeviceInventory)
	}
	if p.adoption != nil {
		p.adoption.Apply(applied.Generation, applied.DeviceInventory)
	}

	if err := p.host.ApplyEdgeRules(applied.EdgeRules, int(applied.Generation)); err != nil {
		// ApplyEdgeRules is fail-closed per rule (a bad rule is skipped, not fatal);
		// a returned error is unexpected, so log and keep serving rather than crash.
		log.Printf("waiveo-relay live pull: reload edge rules for generation %d: %v", applied.Generation, err)
	}
	// installed, not len(applied.ScreenPrograms): an entry carrying no screen_id
	// is skipped by serveAppAuthoredPrograms, and counting the input would report
	// programs re-installed that were discarded.
	log.Printf("waiveo-relay live pull: applied generation %d live (%d screen program(s) re-installed, schedule re-resolved, %d edge rule(s) loaded)",
		applied.Generation, installed, p.host.EdgeRuleCount())
	p.lastGen = applied.Generation
	p.lastHash = applied.Hash
	return true
}
