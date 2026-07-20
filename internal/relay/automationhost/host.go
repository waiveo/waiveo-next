// Package automationhost consolidates the relay's four completed edge-automation
// subsystems — the contract-complete edge-rules engine (internal/rules/engine),
// the device-command plane (internal/relay/deviceplane), the durable telemetry
// queue (internal/relay/telemetry over internal/relay/identity), and the
// engine↔device-plane command adapter (internal/relay/automation) — into one
// object the running waiveo-relay binary owns. It compiles + loads the
// feeder-signed edge_rules of a desired-state generation (relay/1 REL-062) into
// the engine, and — driven through an injectable device-state seam (devicestate.go,
// the seam real polling/ECP will feed) — fires a real rule end to end: a device
// observation advances the engine, a firing dispatches its device_command through
// the device plane, and the resulting automation.run is emitted into the durable
// telemetry queue (REL-090/093).
//
// This package REUSES those subsystems rather than re-implementing any of them:
// the engine loads via engine.ApplyGeneration, command dispatch flows through
// automation.CommandSink → deviceplane.CommandSurface, and automation.run rides
// telemetry.AutomationRunEntry into the durable telemetry.Buffer backed by the
// operational store. The registry is device-class-registry/1's own canonical
// built-in (internal/deviceclass.Builtin(), REG-060-066): New builds it ONCE
// and hands it to both the device plane (as deviceplane.CommandVocab, directly
// — REG-052/REL-113) and the engine (adapted via registry.FromDeviceClass —
// REG-032/044/052), so a device_command resolves against the identical
// media-player vocabulary on both paths.
package automationhost

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/relay/automation"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/closure"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/rules/engine"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
)

// telemetryCapacity bounds the durable telemetry buffer's drop-oldest overflow
// policy (REL-096). It is the automation stack's slice of the operational
// store's bounded telemetry queue; a generous bound for the low automation.run
// rate a single relay's edge rules produce.
const telemetryCapacity = 1024

// procStart anchors the host's monotonic clock: sysClock.Mono reports elapsed
// monotonic milliseconds since process start, using time.Since's monotonic
// reading so a wall-clock step (NTP/DST) can never shorten or extend an
// in-flight `for`-hold or `delay` (rules/1 RUL-024/190/361).
var procStart = time.Now()

// sysClock is the host's real clock.Clock: the wall reading (Unix ms) drives
// schedule triggers and time conditions (RUL-130/340) and the monotonic reading
// drives every duration mechanism (RUL-024/190). The engine reads all time
// through it; only tests substitute a clock.FakeClock.
type sysClock struct{}

func (sysClock) WallMillis() int64 { return time.Now().UnixMilli() }
func (sysClock) Mono() int64       { return time.Since(procStart).Milliseconds() }

var _ clock.Clock = sysClock{}

// Host is the running relay's edge-automation stack: the loaded engine, the
// durable telemetry buffer its firings emit automation.run into, and the clock
// the engine reads time through. It is not safe for concurrent use — Observe,
// Run, and ApplyEdgeRules advance the single engine loop serially, exactly as a
// relay's one evaluation loop drives it.
type Host struct {
	engine *engine.Engine
	buf    *telemetry.Buffer
	clk    clock.Clock

	// surface is the device-command surface New wires the canonical registry
	// into as its CommandVocab (REG-052/REL-113) — the app-dispatched
	// device.command entry point (deviceplane.CommandSurface.Execute) a later
	// wire receiver calls into directly, and the one this package's own tests
	// drive to prove command resolution against the real media-player
	// vocabulary end to end.
	surface *deviceplane.CommandSurface

	// appliedGen is the desired-state generation whose edge_rules are currently
	// loaded, and hasApplied whether any generation has been applied yet. Together
	// they make ApplyEdgeRules idempotent: re-applying an already-applied
	// generation is a no-op (REL-070).
	appliedGen int
	hasApplied bool

	// loadedEdge is how many edge-class rules the last apply actually loaded into
	// the engine (app-class and uncompilable rules in the section are skipped),
	// for the binary's "automation engine loaded: N edge rule(s)" log line.
	loadedEdge int
}

// New builds the relay's edge-automation stack over the operational store and
// the device-facing collaborators. dc is the ONE canonical device-class
// registry (internal/deviceclass.Builtin(), REG-060-066) the caller
// constructs; New wires it into BOTH consumers rather than letting each build
// or duplicate its own:
//
//   - the device-command surface (deviceplane.CommandSurface) over controller
//     (the physical-device adapter), dc itself as the CommandVocab (REG-052 —
//     dc's CommandExists(deviceClass, command string) bool satisfies the
//     interface directly, no adapter needed, REL-113), and resolveEntity
//     (entity_id → device_id/device-class, REL-112);
//   - the engine's command sink (automation.CommandSink) over that surface, so a
//     rule's device_command flows through the SAME resolve→serialize→dispatch
//     path (REL-113/114/115) as an app-dispatched command, stamped with relayID;
//   - the durable telemetry buffer (telemetry.NewDurableBuffer) over store, so a
//     committed automation.run survives a power-pull (REL-090/093); and
//   - the engine (engine.New) over registry.FromDeviceClass(dc, registry.Overlay{})
//     — dc adapted to the rules/1 registry.Registry surface with no extra
//     overlay attributes (the three rules-corpus-only ones are a documented
//     test-fixture concern, never mixed into the running relay) — the host's
//     real clock, and that sink.
//
// The upstream telemetry push channel (telemetry.Channel) is the relay/1 wire
// transport that lands later; New builds only the durable queue side the engine
// records into. It returns an error only if the durable buffer fails to resume
// from store (a silently-empty durable buffer would be indistinguishable from
// durable loss, REL-090).
func New(store *identity.Store, dc deviceclass.Registry, controller deviceplane.DeviceController, resolveEntity deviceplane.EntityResolver, relayID string) (*Host, error) {
	buf, err := telemetry.NewDurableBuffer(store, telemetryCapacity)
	if err != nil {
		return nil, fmt.Errorf("automationhost: resume durable telemetry buffer: %w", err)
	}

	surface := deviceplane.NewCommandSurface(controller, dc, resolveEntity)
	sink := automation.NewCommandSink(surface, relayID)
	clk := sysClock{}
	reg := registry.FromDeviceClass(dc, registry.Overlay{})
	eng := engine.New(reg, clk, sink, nil)

	return &Host{engine: eng, buf: buf, clk: clk, surface: surface}, nil
}

// ApplyEdgeRules compiles the desired-state generation's edge_rules and loads
// them into the engine as one generation (REL-062). Each raw rule is compiled
// (compile.Compile): a rule that does not compile, or that classifies app-class
// (queued/parallel run management or any other app-causing member, RUL-240), is
// logged and SKIPPED — the edge engine loads only edge-class rules — rather than
// failing the whole generation. Every edge rule is parsed and its compile-time
// closure frozen (closure.Compute, RUL-390) into the engine.Generation, which is
// then applied via engine.ApplyGeneration (RUL-380/381 diff against the loaded
// generation).
//
// Re-applying an already-applied generation is a no-op (REL-070): the same
// signed generation pulled again neither reloads rules nor perturbs any in-flight
// run. A lower generation is likewise ignored here (desiredstate.Pull already
// rejects a regressed generation, REL-052).
func (h *Host) ApplyEdgeRules(edgeRules []json.RawMessage, generation int) error {
	if h.hasApplied && generation <= h.appliedGen {
		return nil // idempotent re-apply of an already-applied generation (REL-070)
	}

	gen := engine.Generation{Number: generation}
	loaded := 0
	for i, raw := range edgeRules {
		entry, cerr := compile.Compile(raw)
		if cerr != nil {
			// A rule the feeder signed but this relay cannot compile is not loaded
			// (fail-closed) — one bad rule never bricks the rest of the generation.
			log.Printf("automationhost: skipping edge_rules[%d]: does not compile: %s: %s", i, cerr.Code, cerr.Message)
			continue
		}
		if entry.ExecutionClass != "edge" {
			// App-class rules belong to the app engine, never this edge engine (RUL-240).
			log.Printf("automationhost: skipping edge_rules[%d] (rule %s): app-class, not loaded into the edge engine", i, entry.RuleID)
			continue
		}
		rule, err := model.ParseRule(raw)
		if err != nil {
			// Unreachable in practice — Compile already parsed it — but fail-closed.
			log.Printf("automationhost: skipping edge_rules[%d]: parse after compile: %v", i, err)
			continue
		}
		cl, cerr := closure.Compute(rule, closure.Env{})
		if cerr != nil {
			log.Printf("automationhost: skipping edge_rules[%d] (rule %s): closure: %s: %s", i, entry.RuleID, cerr.Code, cerr.Message)
			continue
		}
		gen.Rules = append(gen.Rules, engine.CompiledRule{Entry: entry, Rule: rule, Closure: cl})
		loaded++
	}

	// ApplyGeneration returns the Canceled dispositions of any in-flight run a
	// generation swap tears down (RUL-380). Those are generation-swap teardowns,
	// NOT trigger firings, and have no automation.run representation (EVT-041), so
	// they are deliberately not emitted as telemetry here.
	h.engine.ApplyGeneration(gen)

	h.appliedGen = generation
	h.hasApplied = true
	h.loadedEdge = loaded
	return nil
}

// EdgeRuleCount is how many edge-class rules the last ApplyEdgeRules loaded into
// the engine (app-class and uncompilable rules in the section excluded). The
// binary logs it as "automation engine loaded: N edge rule(s)".
func (h *Host) EdgeRuleCount() int { return h.loadedEdge }

// SetLocation adopts the site this relay is bound to — its effective timezone
// and coordinates — into the running engine (relay/1 REL-036 → rules/1
// engine.SetLocation), so the edge engine's time-based and sun-based triggers
// and conditions (RUL-060/061/340) evaluate against the site's real placement
// rather than a default. The relay learns these authoritative values from the
// app peer's hello-ack (internal/relay/hello) and feeds them here. It returns
// the loader's error for an unknown IANA zone name, leaving the prior location
// unchanged.
func (h *Host) SetLocation(tzName string, lat, lon float64) error {
	return h.engine.SetLocation(tzName, lat, lon)
}

// Engine exposes the loaded engine so a device-state source (Observe/Run) can
// feed it observations. It is the seam the synthetic source drives in tests/demo
// and the real polling/ECP source will feed on hardware.
func (h *Host) Engine() *engine.Engine { return h.engine }
