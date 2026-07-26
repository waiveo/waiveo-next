// Package keepalive implements the relay's screen keep-alive capability —
// player/1's Screen liveness and recovery (contracts/player-1.md PLY-150–157):
// watching configured screens over ECP and re-launching the player channel
// when — and only when — doing so is safe.
//
// PLY-150-157's own gates are evaluated through
// internal/relay/playerserver.EvaluateRecovery — the same pure,
// corpus-validated decision function conformance/drivers/player1's own driver
// already exercises for PLY-150-157 — rather than re-derived here a second,
// independent way (evaluate calls it directly). On top of that shared
// decision, this package layers three ADDITIONAL pieces of engineering
// policy, paid for against real hardware, that the contract itself does NOT
// require but that a real relay needs:
//
//  1. POWER-ON DELAY SIZING (PLY-154 requires SOME bounded settle wait; this
//     package picks and times one). When a screen transitions from a
//     non-PowerOn raw power_mode into PowerOn, a recovery launch MUST wait a
//     configurable delay (Config.LaunchDelay, default 1500ms) before doing
//     anything — issuing it immediately races the device's own boot/
//     foreground sequence and pends without effect on real firmware. This
//     package tracks the transition itself and feeds the result to
//     EvaluateRecovery as LivenessSignal.JustEnteredOnGroup.
//
//  2. A HOME-CONFIRMATION STREAK — keepalive's OWN policy, NOT something
//     PLY-152/153 themselves require. Before attempting a relaunch, this
//     package additionally requires the screen be observed sitting on the
//     Home/menu surface for at least homeStreakThreshold (2) CONSECUTIVE
//     qualifying polls, using a STRICTER app_type reading than PLY-152
//     itself does: only a literal "home"/"menu" counts here, never PLY-152's
//     own additionally-tolerated absent/unknown app_type. Screen liveness and
//     recovery's own text (PLY-150-158) describes a single-instant joint
//     check with no notion of consecutive polls, confirmation counts, or
//     debouncing anywhere in it — PLY-152/153 do not mandate this streak or
//     this narrower reading; both are purely this package's own, strictly
//     MORE conservative debounce on top of the contract's real PLY-152 gate
//     (which evaluate still checks, unweakened, via
//     EvaluateRecovery.WouldPassAppTypeCheck), so keepalive can only ever
//     fire less permissively than the contract allows, never more.
//
//  3. NEVER WAKE STANDBY (PLY-151, sized to this driver's own attribute). If
//     the target's raw power_mode attribute is anything other than
//     "PowerOn" — a recognized low-power value, an unrecognized one, or
//     absent entirely (an unreachable/unavailable device) — no command is
//     ever issued for that poll, full stop. device-class-registry/1 REG-063
//     makes this equivalent to PLY-151's own "canonical state is a member of
//     the media-player class's `on` semantic group" gate for this specific
//     driver: internal/relay/ecppoll's state derivation (its own package
//     doc) resolves every raw power_mode value OTHER than "PowerOn" to a
//     REG-063 `off`-group state (`standby`, `off`, or `unavailable`), and
//     every "PowerOn" reading to an `on`-group state (`idle` or `on`) — so
//     gating on the raw power_mode attribute directly, rather than on
//     Entity.State, reaches the identical decision while keeping this
//     package attribute-driven end to end. Once this poll-level check
//     passes, evaluate's own EvaluateRecovery call still threads a
//     representative on-group state ("on") through PLY-151's gate too
//     (rather than skipping it), so a future change to REG-063's semantic
//     group membership is honored here rather than silently drifting out of
//     step with the one place that check is supposed to live.
//
// # PLY-155/156: blank-Lease and scheduled-power-off suppression
//
// These ARE implemented, by threading Config.ActiveDisplay's reading into the
// same EvaluateRecovery call: whenever the target screen's currently active
// Lease has display "blank" (PLY-155), EvaluateRecovery's own
// SuppressedDueToBlankDisplay refuses the recovery, and evaluate resets this
// package's own home-confirmation streak (item 2 above) so a later recovery
// is re-confirmed fresh once the suppression lifts, rather than firing
// immediately on stale pre-suppression progress.
//
// PLY-156 (a display-power schedule's own most recent power-off directive) is
// deliberately NOT wired to a separate signal here: by the time either
// reaches a player/1 Lease, this codebase's data model has no observable
// difference between a schedule-commanded "off" and an intentional "blank" —
// data-model/1 DAT-116 projects a daypart/fallback's display_power "off" to
// the identical Lease display:"blank" PLY-155 already covers ("off and blank
// are distinct platform states ... but project to the identical Lease
// display:blank"). So Config.ActiveDisplay's blank reading already carries
// PLY-156's real-world manifestation in this relay; EvaluateRecovery's
// PowerScheduleState input is passed at its zero value here and stands ready
// as the extension point a FUTURE data-model change (one that exposes a
// schedule-off directive distinct from a blank Lease) would wire.
//
// Config.ActiveDisplay is REQUIRED for PLY-155 to have any effect in
// production: cmd/waiveo-relay/main.go wires it from the SAME served-program
// state playerserver.Server.SetProgram/SetServedProgram configure
// (playerserver.Server.CurrentDisplay). Leaving it nil (the zero-value
// Config's own default) means every poll behaves as though the active Lease
// were always content — the safe degrade for a test exercising the
// device-state gates alone, never acceptable for a running relay.
//
// # Dispatch is serialized like every other command (REL-115)
//
// Config.Controller MUST, in production, be wired the same way every other
// dispatch path in this binary already is — through a
// deviceplane.CommandSurface (e.g. via automation.CommandSink, as
// cmd/waiveo-relay/main.go wires it), never a bare deviceplane.DeviceController
// such as internal/relay/ecp.Controller directly — so a keepalive-issued
// launch resolves against the device-class command vocabulary (REL-113) and
// takes the per-device dispatch lock (REL-115) exactly like an app-peer-,
// edge-rule-, or preset-batch-issued command does: cmd/waiveo-relay/main.go
// wires the SAME *deviceplane.CommandSurface instance the schedule-preset
// path uses, so a recovery launch and a preset batch aimed at the same
// physical device can never interleave delivery to it. Dispatch to a
// DIFFERENT screen is, by contrast, deliberately NOT serialized against this
// one — see evaluateAll's own doc.
//
// # Why a second Poller (single-consumer rationale)
//
// internal/relay/automationhost.Host.Run is the relay's one and only consumer
// of the main ecppoll.Poller's stream (cmd/waiveo-relay/main.go's own
// wiring): a Poller's internal channel has exactly one intended drain loop,
// and Poller.Run documents itself as the stream's sole closer. Subscribing a
// second, independent consumer to that same stream would mean racing two
// Next() callers against one channel — undefined which caller receives a
// given Observation — since ecppoll exposes no fan-out. Rather than reach
// into the poller's internals (which the package deliberately does not
// expose), keepalive constructs and drives its OWN ecppoll.Poller instance
// over the same configured targets, polling the same devices a second time
// on its own schedule. The cost is one extra ECP poll cycle per screen per
// interval; the benefit is a clean, supported second consumer with no shared
// mutable state between the two Pollers.
//
// # Poll cadence versus ecppoll's change-only stream
//
// ecppoll.Poller.Next() emits an Observation only for an entity's first
// successful snapshot (a self-transition seed) or a genuine change
// (state.NewObservation reporting StateChanged or a non-empty ChangedAttrs);
// a screen sitting motionlessly at Home never produces a second Observation
// once its arrival has been sent (ecppoll's own package doc, "Channel
// semantics"). Rule 2's "≥2 consecutive polls" could therefore never be
// satisfied by counting stream arrivals alone — nothing would ever arrive to
// confirm a second poll of an unchanging screen, and the very screens this
// capability exists to catch (stuck, unchanging, at Home) are exactly the
// ones that would never trigger it.
//
// Keepalive resolves this by caching the last-known (power_mode, app_type)
// pair per screen, updated on every Observation ecppoll's Next() delivers,
// and re-evaluating that cache against the wall clock on its OWN ticker at
// Config.PollInterval — independent of whether ecppoll emitted anything that
// tick. A genuine change (Next() delivers) is applied and evaluated
// immediately, for prompt reaction (e.g. a person picking up the remote);
// the ticker is what lets rule 1's delay and rule 2's streak progress purely
// from the passage of time for a screen that never changes at all.
package keepalive

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/ecppoll"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// defaultLaunchDelay is Config.LaunchDelay's zero-value default (rule 1):
// the field-observed minimum settle time real Roku firmware needs after a
// power-on before a launch actually renders anything.
const defaultLaunchDelay = 1500 * time.Millisecond

// defaultChannel is Config.Channel's zero-value default: the platform
// player's own dev channel identifier, matching the other WAIVEO_RELAY_*
// dev-stack defaults this binary already assumes elsewhere.
const defaultChannel = "dev"

// defaultPollInterval is Config.PollInterval's zero-value default, matching
// loadConfig's own WAIVEO_RELAY_POLL_MS default (cmd/waiveo-relay/main.go).
const defaultPollInterval = 5 * time.Second

// homeStreakThreshold is rule 2's "≥2 consecutive polls" confirmation —
// keepalive's OWN policy (see the package doc's item 2), NOT something
// PLY-152/153 themselves require: the number of consecutive qualifying polls
// (PowerOn, past the launch delay, app_type literally home/menu) a screen
// must accumulate before keepalive attempts a recovery launch.
const homeStreakThreshold = 2

// launchCommand/channelParam are the device-class-registry/1 REG-066 launch
// command's name and its one required param, as internal/relay/ecp.Dispatch
// resolves them (ecpPath's "launch" case).
const (
	launchCommand = "launch"
	channelParam  = "channel"
)

// poweredOnRawValue is the one raw ECP power_mode value rule 3 treats as
// "genuinely on" — every other value (a recognized low-power reading, an
// unrecognized one, or absent/empty) never issues a command (see the package
// doc's REG-063 equivalence note).
const poweredOnRawValue = "PowerOn"

// Target names one watched screen's ECP address — the same (Host, Port)
// shape internal/relay/ecppoll.Target and internal/relay/ecp.Target each
// declare, kept as its own local type (mirroring how those two packages
// already keep independent copies of one another) so this package's Config
// does not have to import either just to name an address. New converts a
// Config's Targets into ecppoll.Target when constructing this package's own
// second Poller (see the package doc's single-consumer rationale); dispatch
// itself never needs Host/Port at all, only the entityID key.
type Target struct {
	Host string
	Port int
}

// Config configures a Keepalive. Targets, at minimum, must be non-empty for
// Run to watch anything. PollInterval, LaunchDelay, and Channel each default
// (defaultPollInterval, defaultLaunchDelay, defaultChannel) when left at
// their zero value. Controller is required — a nil Controller means Run
// silently never dispatches a launch (see dispatchLaunch), which is never
// useful in production but keeps a zero-value Config from panicking in a
// test that only wants to exercise the state machine.
type Config struct {
	// Targets maps entity_id -> the ECP address keepalive polls for that
	// screen. Every key here is one screen this capability watches.
	Targets map[string]Target

	// PollInterval is how often keepalive's own second Poller (see the
	// package doc) polls every target, and also the cadence of keepalive's
	// own cache re-evaluation ticker. Zero means defaultPollInterval.
	PollInterval time.Duration

	// LaunchDelay is rule 1's settle wait after a screen transitions into
	// PowerOn, before any recovery launch is even considered. Zero means
	// defaultLaunchDelay.
	LaunchDelay time.Duration

	// Channel is the launch command's channel param (device-class-registry/1
	// REG-066) — the app/channel identifier a recovery launch foregrounds.
	// Empty means defaultChannel.
	Channel string

	// Controller is the deviceplane.DeviceController a decided recovery
	// launch dispatches through (REL-112). It MUST, in production, be wired
	// the same way every other dispatch path in this binary already is —
	// wrapped in a deviceplane.CommandSurface (e.g. via automation.CommandSink,
	// as cmd/waiveo-relay/main.go wires it) — so a keepalive-issued launch
	// resolves against the device-class command vocabulary (REL-113) and
	// takes the per-device dispatch lock (REL-115) exactly like an
	// app-peer-, edge-rule-, or preset-batch-issued command does; see the
	// package doc's "Dispatch is serialized" section. A bare
	// deviceplane.DeviceController (e.g. internal/relay/ecp.Controller
	// directly) satisfies this field's type but skips both of those
	// guarantees — fine for a test exercising the state machine alone, never
	// for a running relay. Controller is required for keepalive to do
	// anything at all — a nil Controller means Run silently never dispatches
	// a launch (see dispatchLaunch), which is never useful in production but
	// keeps a zero-value Config from panicking in a test that only wants to
	// exercise the state machine.
	Controller deviceplane.DeviceController

	// ActiveDisplay reports entityID's screen's currently active Lease
	// `display` value (player/1 PLY-093: playerserver.DisplayContent or
	// playerserver.DisplayBlank) as of the call — the real signal PLY-155's
	// blank-Lease suppression gates on (see the package doc's "PLY-155/156"
	// section). In production this is playerserver.Server.CurrentDisplay,
	// wired by cmd/waiveo-relay/main.go from the SAME served-program state
	// SetProgram/SetServedProgram configure. nil (the zero-value Config's
	// own default) or a "" reading are both treated as "not blank" — this
	// gate never suppresses — the safe degrade for a test exercising the
	// device-state gates alone; PLY-155 has NO effect in a relay that leaves
	// this unwired.
	ActiveDisplay func(entityID string) string
}

// pollSnapshot is keepalive's own cached view of one screen's two
// rule-relevant attributes (device-class-registry/1 REG-064), as of the most
// recent Observation ecppoll's Next() delivered for it.
type pollSnapshot struct {
	powerMode string
	appType   string
}

// screenState is the per-screen bookkeeping evaluate folds every poll into:
// whether the screen was already known to be PowerOn as of the prior poll
// (so a false->true edge is detectable), when it most recently transitioned
// into PowerOn (rule 1's delay clock), how many consecutive qualifying polls
// it has accumulated at Home/menu (rule 2's streak), and whether a launch has
// already been dispatched for the CURRENT unbroken streak (the cooldown that
// keeps a still-idling screen from being relaunched every single poll).
type screenState struct {
	wasPoweredOn bool
	poweredOnAt  time.Time
	homeStreak   int
	launched     bool
}

// Keepalive watches Config.Targets over ECP and dispatches a recovery launch
// through Config.Controller per the package doc's three rules. Build one with
// New; drive it with Run.
type Keepalive struct {
	targets       map[string]Target
	pollInterval  time.Duration
	launchDelay   time.Duration
	channel       string
	controller    deviceplane.DeviceController
	activeDisplay func(entityID string) string

	mu      sync.Mutex
	known   map[string]pollSnapshot
	screens map[string]*screenState

	// dispatchWG tracks evaluateAll's own per-screen dispatch goroutines (see
	// its doc). Run never waits on it — a slow dispatch must never delay ctx
	// cancellation — it exists so this package's own tests can wait for a
	// background dispatch to fully complete before asserting on it.
	dispatchWG sync.WaitGroup
}

// New builds a Keepalive from cfg, applying defaultPollInterval,
// defaultLaunchDelay, and defaultChannel wherever cfg leaves the
// corresponding field at its zero value. It does not start polling; call Run.
func New(cfg Config) *Keepalive {
	targets := make(map[string]Target, len(cfg.Targets))
	for id, t := range cfg.Targets {
		targets[id] = t
	}

	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	launchDelay := cfg.LaunchDelay
	if launchDelay <= 0 {
		launchDelay = defaultLaunchDelay
	}
	channel := cfg.Channel
	if channel == "" {
		channel = defaultChannel
	}

	return &Keepalive{
		targets:       targets,
		pollInterval:  pollInterval,
		launchDelay:   launchDelay,
		channel:       channel,
		controller:    cfg.Controller,
		activeDisplay: cfg.ActiveDisplay,
		known:         make(map[string]pollSnapshot, len(targets)),
		screens:       make(map[string]*screenState, len(targets)),
	}
}

// Run drives keepalive's own second Poller (see the package doc's
// single-consumer rationale) over Config.Targets and its own re-evaluation
// ticker, until ctx is done. Every Observation the poller's Next() delivers
// updates this screen's cached snapshot and is evaluated immediately; every
// tick of keepalive's own ticker (at pollInterval) re-evaluates every
// screen's cached snapshot against the current wall clock, which is what lets
// rule 1's delay and rule 2's streak progress for a screen that never
// changes at all (see the package doc's "Poll cadence" section). It returns
// ctx.Err() once ctx is done.
func (k *Keepalive) Run(ctx context.Context) error {
	pollTargets := make(map[string]ecppoll.Target, len(k.targets))
	for id, t := range k.targets {
		pollTargets[id] = ecppoll.Target{Host: t.Host, Port: t.Port}
	}
	poller := ecppoll.New(pollTargets, k.pollInterval)
	go poller.Run(ctx)

	// Drain loop: this goroutine is keepalive's own sole Next() caller (the
	// package doc's single-consumer rationale applies just as much to this
	// second Poller as to the main one — nothing else may call Next() on it).
	// It returns on its own once ctx cancellation closes the poller's channel
	// (Next reporting ok=false), needing no separate cancellation signal.
	go func() {
		for {
			obs, ok := poller.Next()
			if !ok {
				return
			}
			k.recordObservation(obs, time.Now())
		}
	}()

	ticker := time.NewTicker(k.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			k.evaluateAll(now)
		}
	}
}

// recordObservation updates obs.Entity's cached (power_mode, app_type)
// snapshot and evaluates it immediately at now — the prompt-reaction half of
// the package doc's "Poll cadence" section (a genuine change, e.g. a foreign
// app appearing, is acted on as soon as it is observed, not left to the next
// ticker tick).
func (k *Keepalive) recordObservation(obs state.Observation, now time.Time) {
	id := obs.Entity.ID
	powerMode := attrString(obs.Entity.Attributes, "power_mode")
	appType := attrString(obs.Entity.Attributes, "app_type")

	k.mu.Lock()
	k.known[id] = pollSnapshot{powerMode: powerMode, appType: appType}
	k.mu.Unlock()

	if k.evaluate(id, powerMode, appType, now) {
		k.dispatchLaunch(id)
	}
}

// evaluateAll re-evaluates every screen's cached snapshot at now — the
// time-driven half of the package doc's "Poll cadence" section. Screens are
// EVALUATED in sorted entity-ID order (matching ecppoll.Poller.pollAll's own
// deterministic ordering) so a test observing evaluation order gets a stable
// sequence — evaluate itself is a cheap, local, non-blocking state-machine
// step, so there is no reason to run it anywhere but this one loop.
//
// A screen whose evaluation fires, however, has its launch DISPATCHED in its
// own goroutine rather than inline in this loop: Config.Controller's
// Dispatch ultimately makes a real network call (internal/relay/ecp.Controller
// has a several-second HTTP timeout) that can block far longer than a poll
// interval if a device is slow or wedged. Dispatching inline would make one
// slow or unreachable screen delay recovery of every OTHER screen later in
// this same sorted pass — including ones that became eligible for the exact
// same reason in the exact same tick (e.g. a shared network blip that
// knocked several screens to Home at once), which is precisely the
// multi-screen scenario this capability exists to handle. Concurrent
// dispatch across DIFFERENT screens is safe: Config.Controller is required
// to be a deviceplane.CommandSurface-backed dispatcher (the package doc's
// "Dispatch is serialized" section), whose own per-device lock (REL-115)
// still serializes any two dispatches that land on the SAME physical
// device — this loop just no longer imposes a second, needless
// cross-screen serialization on top of that.
func (k *Keepalive) evaluateAll(now time.Time) {
	k.mu.Lock()
	ids := make([]string, 0, len(k.known))
	snaps := make(map[string]pollSnapshot, len(k.known))
	for id, snap := range k.known {
		ids = append(ids, id)
		snaps[id] = snap
	}
	k.mu.Unlock()

	sort.Strings(ids)
	for _, id := range ids {
		snap := snaps[id]
		if k.evaluate(id, snap.powerMode, snap.appType, now) {
			k.dispatchWG.Add(1)
			go func(id string) {
				defer k.dispatchWG.Done()
				k.dispatchLaunch(id)
			}(id)
		}
	}
}

// evaluate is keepalive's per-screen state machine (the package doc's three
// numbered policy items plus PLY-155/156's delegated suppression), folding
// one poll's (powerMode, appType) reading for entityID into its screenState
// at instant now and reporting whether THIS poll should dispatch a recovery
// launch. It is the seam a test drives directly with a canned sequence of
// (powerMode, appType, now) tuples, without any real or synthetic Poller
// involved.
//
//   - Rule 3 (never wake standby, PLY-151): powerMode != "PowerOn" — a
//     recognized low-power value, an unrecognized one, or empty
//     (unreachable/unavailable, ecppoll's own unavailableEntity) — never
//     dispatches, and resets every other piece of state, so a LATER genuine
//     power-on is treated as fully fresh (its own delay window, its own
//     streak).
//   - Rule 1 (power-on delay, PLY-154): the first poll to report "PowerOn"
//     after any poll that did not (including this screen's very first-ever
//     poll, since keepalive cannot know how long it had already been on
//     before keepalive started watching it) starts the delay clock; every
//     poll before poweredOnAt+launchDelay elapses never dispatches, even one
//     already reading Home.
//   - Rule 2 (home-confirmation streak, keepalive's OWN policy — see the
//     package doc's item 2, NOT a PLY-152/153 requirement): once past the
//     delay, appType other than the literal "home" or "menu" (a real foreign
//     app, a screensaver, or absent/unknown) resets the streak and clears
//     any armed cooldown. An appType of "home" or "menu" increments the
//     streak; reaching homeStreakThreshold with no launch already dispatched
//     for this unbroken streak, AND PLY-155/156's own delegated suppression
//     (below) not refusing it, fires exactly once — then the streak's
//     `launched` flag suppresses every further poll on the SAME streak (no
//     machine-gunning) until app_type leaves home/menu and returns.
//   - PLY-155/156 (delegated to playerserver.EvaluateRecovery, see the
//     package doc's own section): once rules 1-3 above would otherwise
//     confirm a streak, Config.ActiveDisplay's reading for entityID is
//     threaded through the SAME EvaluateRecovery call PLY-152's own gate
//     already passed by construction here (isHome, below, is a narrower
//     superset check) — a blank active Lease refuses the attempt and resets
//     the streak, exactly as rule 2's own "not home" case does.
func (k *Keepalive) evaluate(entityID, powerMode, appType string, now time.Time) bool {
	k.mu.Lock()
	defer k.mu.Unlock()

	s, ok := k.screens[entityID]
	if !ok {
		s = &screenState{}
		k.screens[entityID] = s
	}

	if powerMode != poweredOnRawValue {
		// Rule 3: never wake standby — no command, ever, for this poll. Reset
		// so a later transition back into PowerOn starts fresh.
		s.wasPoweredOn = false
		s.poweredOnAt = time.Time{}
		s.homeStreak = 0
		s.launched = false
		return false
	}

	transitioned := !s.wasPoweredOn
	s.wasPoweredOn = true
	if transitioned {
		// Rule 1: (re)start the settle-delay clock from this instant. A fresh
		// power-on also starts a fresh streak — a home/menu reading from
		// before this transition no longer counts toward it.
		s.poweredOnAt = now
		s.homeStreak = 0
		s.launched = false
	}

	isHome := appType == "home" || appType == "menu"
	if !isHome {
		// Rule 2: a foreign app (or screensaver, or absent/unknown) breaks
		// the streak and clears the cooldown — a person may be using the TV,
		// and a later return to Home starts counting fresh.
		s.homeStreak = 0
		s.launched = false
		return false
	}

	if now.Sub(s.poweredOnAt) < k.launchDelay {
		// Rule 1: still inside the post-power-on settle window, even though
		// this poll already reads Home — too soon to act.
		return false
	}

	// PLY-155/156 (see the package doc's own section): delegate to the SAME
	// contract-faithful decision function conformance/drivers/player1 already
	// exercises, rather than re-deriving PLY-150-157 a second, independent
	// way. isHome above is a literal-only subset of PLY-152's own
	// (broader) recoverableAppType, so ev.WouldPassAppTypeCheck is
	// guaranteed true here already; ev.RecoveryAttempted can only still be
	// false because Config.ActiveDisplay reports the active Lease is blank
	// (PLY-155) — PowerScheduleState is deliberately left at its zero value
	// (see the package doc for why that already suffices for PLY-156 too in
	// this codebase's data model).
	activeDisplay := ""
	if k.activeDisplay != nil {
		activeDisplay = k.activeDisplay(entityID)
	}
	ev := playerserver.EvaluateRecovery(playerserver.LivenessSignal{
		ActiveDisplay: activeDisplay,
		DeviceStatus: playerserver.DeviceStatus{
			// "on": a representative on-group member (device-class-registry/1
			// REG-063). Rule 3 above has ALREADY independently confirmed
			// on-group membership via the raw power_mode check (see the
			// package doc's item 3) — this call's own PLY-151 gate is
			// necessarily going to pass — but it is threaded through anyway
			// rather than skipped, so a future change to REG-063's semantic
			// grouping is honored here too.
			State:   "on",
			AppType: appType,
		},
	})
	if !ev.RecoveryAttempted {
		// PLY-155: an intentionally blank Lease suppresses recovery entirely,
		// regardless of how far the streak has already progressed. Reset it
		// — exactly as rule 2's own "not home" case does — so a later
		// recovery is re-confirmed fresh once the suppression lifts, rather
		// than firing immediately on stale pre-suppression progress.
		s.homeStreak = 0
		s.launched = false
		return false
	}

	s.homeStreak++
	if s.homeStreak < homeStreakThreshold || s.launched {
		// Not yet confirmed across homeStreakThreshold consecutive polls, or
		// already dispatched once for this unbroken streak (cooldown).
		return false
	}
	s.launched = true
	return true
}

// dispatchLaunch issues the recovery launch command (device-class-registry/1
// REG-066: "launch", {"channel": k.channel}) for entityID through
// k.controller. A nil controller (a zero-value Config's own default) is a
// silent no-op, never a panic — production always wires one; only a test
// exercising the state machine alone would leave it unset. A dispatch error
// is logged (matching this codebase's other internal/relay/* library
// packages, e.g. automationhost's own log.Printf on a non-fatal problem) and
// otherwise swallowed — keepalive's own next poll gets another chance, it is
// never fatal to the running relay.
func (k *Keepalive) dispatchLaunch(entityID string) {
	if k.controller == nil {
		return
	}
	if err := k.controller.Dispatch(entityID, launchCommand, map[string]any{channelParam: k.channel}); err != nil {
		log.Printf("keepalive: launch dispatch to %s (channel %q) failed: %v", entityID, k.channel, err)
		return
	}
	log.Printf("keepalive: screen %s re-launched channel %q (home-only recovery)", entityID, k.channel)
}

// attrString reads a device-class-registry/1 REG-064 nullable-string
// attribute out of attrs as a plain string, collapsing an absent key, a nil
// value, or a non-string value (never expected from ecppoll, but not trusted
// blindly here either) to "" — which rule 3 and rule 2 above both already
// treat as "not PowerOn" / "not home", the correct fail-closed reading for an
// attribute keepalive cannot make sense of.
func attrString(attrs map[string]any, key string) string {
	v, ok := attrs[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
