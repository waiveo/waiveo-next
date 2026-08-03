package api_test

// This is the automation-authoring end-to-end oracle: it wires the app store,
// the api/1 authoring handler (POST /api/v1/automations), the feeder's
// store-derived signed desired-state (snapshot.BuildFromStore, carrying the
// edge_rules section it derives from the store's edge-classified automations,
// REL-062), the relay's edge-automation host (automationhost.ApplyEdgeRules +
// the rules engine), and a FAKE device-plane command surface — all IN-PROCESS,
// no network — and proves the whole automation loop: an authored edge rule made
// over the api handler is carried into edge_rules at a new generation, loaded
// into the engine, and — on the triggering observation — actually FIRES a
// device command that reaches the (fake) device with the authored command and
// params.
//
// THE FIRE IS THE ORACLE. A green build where an authored rule LOADS but never
// FIRES is exactly the failure this test guards against: if the store did not
// carry the authored rule into edge_rules, or ApplyEdgeRules skipped it, or the
// engine did not resolve the trigger, or the device_command did not reach the
// surface, the device would see no command and this test would fail. Merely
// asserting "N rules loaded" is not enough — the device must see the authored
// launch, with its authored params.

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/rules/engine"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// automationE2EInstantMs is the fixed instant this oracle resolves desired state
// at. Building a snapshot resolves every screen's program as well, and that
// resolution is per-instant (data-model/1 DAT-111), so the instant is pinned
// rather than read from a wall clock — nothing here asserts on a program, and a
// test that silently changed shape with the hour it ran at would be worse than
// one that does.
const automationE2EInstantMs = int64(1_784_120_400_000) // 2026-07-15T12:00:00 America/Chicago

// e2eRelayID is the relay identity stamped onto every device.command the fired
// rule dispatches (fixture-ULID; no secrets).
const e2eRelayID = "01J8ZE2E0000000000000RELAY0"

// e2eDeviceID is the physical device the trigger/target entity resolves to, so a
// launch command resolves against the canonical media-player vocabulary
// (REL-112/113). It is distinct from the entity id (a device exposes entities).
const e2eDeviceID = "01J8ZE2E00000000000DEVICE01"

// e2eDeviceClass is the device class the trigger/target entity resolves to — a
// media-player, whose canonical vocabulary (device-class-registry/1 REG-066)
// declares the authored `launch` command.
const e2eDeviceClass = "media-player"

// fakeDevicePlane is the FAKE device-plane command surface the fired rule's
// device_command reaches: a deviceplane.DeviceController that records every
// physical dispatch (entity, command, and the authored params) so the oracle can
// assert the authored command actually reached the device. It stands in for the
// hardware-gated real controller (polling/ECP), exactly as the relay's own tests
// do — the SAME device plane an app-dispatched command flows through.
type fakeDevicePlane struct {
	mu   sync.Mutex
	seen []recordedCommand
}

type recordedCommand struct {
	entityID string
	command  string
	params   map[string]any
}

func (f *fakeDevicePlane) Dispatch(entityID, command string, params map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, recordedCommand{entityID: entityID, command: command, params: params})
	return nil
}

func (f *fakeDevicePlane) commands() []recordedCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedCommand, len(f.seen))
	copy(out, f.seen)
	return out
}

// e2eEntity is a media-player entity snapshot at state st — the shape the real
// device-state feed (polling/ECP) delivers to the engine (RUL-330).
func e2eEntity(id, st string) state.Entity {
	return state.Entity{ID: id, DeviceClass: e2eDeviceClass, State: st}
}

// TestAutomationAuthoringLoopAuthoredRuleLoadsAndFires is the end-to-end oracle:
// author an edge rule over the api handler, carry it through the store-derived
// signed desired-state into edge_rules, load it into the relay's engine, and
// drive the triggering observation — the rule MUST fire (a RunDisposition) AND
// its authored device_command MUST reach the fake device plane with the authored
// command and params. A non-triggering observation must fire nothing.
func TestAutomationAuthoringLoopAuthoredRuleLoadsAndFires(t *testing.T) {
	e := newEnv(t)
	e.placementNode(t)
	ctx := context.Background()

	// --- Author an edge rule over the api/1 handler: a state trigger on
	// autoScreenEntity rising to "on" firing a device_command launch{channel:dev}
	// on that same entity (the demo rule's shape; RUL-002 edge).
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations",
		edgeAutomationBody("", autoScopeNode, nil), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("author automation: status %d, body %s", resp.StatusCode, raw)
	}
	automationAID := decodeID(t, raw)

	// --- The feeder derives the signed desired state from the store: the authored
	// edge rule rides edge_rules at the store's current generation (REL-062).
	ds, err := e.store.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	snap, _, err := snapshot.BuildFromStore(ds, e.contentBase, id, automationE2EInstantMs, nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	if n := len(snap.Sections.EdgeRules.Rules); n != 1 {
		t.Fatalf("edge_rules carried %d authored rule(s), want exactly 1 (the authored edge rule)", n)
	}
	if snap.Generation <= 0 {
		t.Fatalf("snapshot generation = %d, want a positive store generation carrying the authored rule", snap.Generation)
	}

	// --- Boot the relay's edge-automation host over a fresh operational store and
	// the FAKE device plane, then load the authored edge_rules — the existing relay
	// load path (automationhost.ApplyEdgeRules), no new mechanism.
	fake := &fakeDevicePlane{}
	relayStore, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = relayStore.Close() })

	resolveEntity := func(entityID string) (deviceID, deviceClass string, ok bool) {
		if entityID == autoScreenEntity {
			return e2eDeviceID, e2eDeviceClass, true
		}
		return "", "", false
	}
	host, err := automationhost.New(relayStore, deviceclass.Builtin(), fake, resolveEntity, e2eRelayID)
	if err != nil {
		t.Fatalf("automationhost.New: %v", err)
	}
	if err := host.ApplyEdgeRules(snap.Sections.EdgeRules.Rules, int(snap.Generation)); err != nil {
		t.Fatalf("ApplyEdgeRules from the store-derived snapshot: %v", err)
	}
	if got := host.EdgeRuleCount(); got != 1 {
		t.Fatalf("engine loaded %d edge rule(s) from the authored snapshot, want 1", got)
	}

	// --- Negative control: a baseline non-transition (off→off) must fire nothing
	// and dispatch nothing — so the fire below is genuinely the off→on trigger, not
	// an unconditional dispatch.
	baseline, err := host.Observe(state.NewObservation(registry.FixtureRegistry{},
		e2eEntity(autoScreenEntity, "off"), e2eEntity(autoScreenEntity, "off")))
	if err != nil {
		t.Fatalf("Observe (baseline): %v", err)
	}
	if len(baseline) != 0 {
		t.Fatalf("baseline off→off fired %d rule(s), want 0", len(baseline))
	}
	if got := fake.commands(); len(got) != 0 {
		t.Fatalf("baseline off→off dispatched %v, want nothing", got)
	}

	// --- THE FIRE: feed the triggering off→on transition. The authored rule MUST
	// fire (a RunDisposition, disposition ran) AND its authored device_command MUST
	// reach the fake device plane with the authored command + params.
	disps, err := host.Observe(state.NewObservation(registry.FixtureRegistry{},
		e2eEntity(autoScreenEntity, "off"), e2eEntity(autoScreenEntity, "on")))
	if err != nil {
		t.Fatalf("Observe (screen-on trigger): %v", err)
	}
	if len(disps) != 1 {
		t.Fatalf("triggering observation produced %d dispositions, want exactly 1 (the authored rule fired)", len(disps))
	}
	if disps[0].Disposition != engine.Ran {
		t.Fatalf("authored rule disposition = %q, want %q (the rule started a run)", disps[0].Disposition, engine.Ran)
	}
	if disps[0].RuleID != automationAID {
		t.Fatalf("fired disposition rule_id = %q, want the authored rule %q", disps[0].RuleID, automationAID)
	}

	// The oracle: the authored device_command reached the fake device plane, with
	// the authored command and params — not merely "loaded".
	got := fake.commands()
	if len(got) != 1 {
		t.Fatalf("device plane saw %d command(s), want exactly 1 (the authored launch)", len(got))
	}
	cmd := got[0]
	if cmd.entityID != autoScreenEntity {
		t.Fatalf("dispatched entity = %q, want the authored trigger/target entity %q", cmd.entityID, autoScreenEntity)
	}
	if cmd.command != "launch" {
		t.Fatalf("dispatched command = %q, want the authored command %q", cmd.command, "launch")
	}
	if ch, _ := cmd.params["channel"].(string); ch != "dev" {
		t.Fatalf("dispatched params[channel] = %v, want the authored %q (params: %v)", cmd.params["channel"], "dev", cmd.params)
	}
}

// TestAutomationAuthoringLoopDisabledRuleNeverFires is the negative oracle for the
// resource-envelope `enabled` gate (openapi Automation.enabled): author a
// compile-clean edge rule with `enabled:false`, run it through the SAME store →
// signed desired-state → relay-load path the positive oracle uses, and prove it
// NEVER fires. THE ABSENCE OF THE FIRE IS THE ORACLE: a disabled rule that the API
// reports as inactive must not reach the relay's edge_rules and must not dispatch a
// device command — otherwise it silently rides edge_rules and fires anyway, the
// exact defect this guards against. The rule is still stored and edge-classified;
// only the carry path drops it.
func TestAutomationAuthoringLoopDisabledRuleNeverFires(t *testing.T) {
	e := newEnv(t)
	e.placementNode(t)
	ctx := context.Background()

	// --- Author an edge rule identical to the positive oracle's, but enabled:false.
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations",
		disabledEdgeAutomationBody("", autoScopeNode), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("author disabled automation: status %d, body %s", resp.StatusCode, raw)
	}
	automationAID := decodeID(t, raw)
	// GET reports it disabled — the API-visible state the carry path must agree with.
	resp, raw = e.do(t, http.MethodGet, "/api/v1/automations/"+automationAID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get authored automation: status %d, body %s", resp.StatusCode, raw)
	}
	var got struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode automation: %v", err)
	}
	if got.Enabled {
		t.Fatalf("authored automation reports enabled=true, want false")
	}

	// --- The feeder derives the signed desired state from the store: a disabled edge
	// automation must NOT ride edge_rules (REL-062).
	ds, err := e.store.DesiredState(ctx)
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	snap, _, err := snapshot.BuildFromStore(ds, e.contentBase, mustSigning(t), automationE2EInstantMs, nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	if n := len(snap.Sections.EdgeRules.Rules); n != 0 {
		t.Fatalf("edge_rules carried %d rule(s) for a disabled automation, want 0", n)
	}

	// --- Boot the relay's edge-automation host and load the (empty) edge_rules — the
	// existing load path, no new mechanism. Nothing loads, so nothing can fire.
	fake := &fakeDevicePlane{}
	relayStore, err := identity.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = relayStore.Close() })

	resolveEntity := func(entityID string) (deviceID, deviceClass string, ok bool) {
		if entityID == autoScreenEntity {
			return e2eDeviceID, e2eDeviceClass, true
		}
		return "", "", false
	}
	host, err := automationhost.New(relayStore, deviceclass.Builtin(), fake, resolveEntity, e2eRelayID)
	if err != nil {
		t.Fatalf("automationhost.New: %v", err)
	}
	if err := host.ApplyEdgeRules(snap.Sections.EdgeRules.Rules, int(snap.Generation)); err != nil {
		t.Fatalf("ApplyEdgeRules from the store-derived snapshot: %v", err)
	}
	if got := host.EdgeRuleCount(); got != 0 {
		t.Fatalf("engine loaded %d edge rule(s) from a disabled authored snapshot, want 0", got)
	}

	// --- Feed the transition that WOULD trigger the rule were it enabled. It must
	// fire nothing and dispatch nothing — the disabled rule never reached the relay.
	disps, err := host.Observe(state.NewObservation(registry.FixtureRegistry{},
		e2eEntity(autoScreenEntity, "off"), e2eEntity(autoScreenEntity, "on")))
	if err != nil {
		t.Fatalf("Observe (would-trigger transition): %v", err)
	}
	if len(disps) != 0 {
		t.Fatalf("a disabled automation fired %d disposition(s) on its trigger, want 0", len(disps))
	}
	if cmds := fake.commands(); len(cmds) != 0 {
		t.Fatalf("a disabled automation dispatched %v to the device plane, want nothing", cmds)
	}
}

// mustSigning loads (or creates) a feeder signing identity in a per-test temp dir,
// failing the test on error — the same identity setup the positive oracle does
// inline, factored out so both e2e cases share it.
func mustSigning(t *testing.T) *signing.Identity {
	t.Helper()
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	return id
}
