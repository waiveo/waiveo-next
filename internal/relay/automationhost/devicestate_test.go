package automationhost

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// TestObserveFiresRuleDispatchesAndDurablyBuffers is the Task-3 proof: a
// synthetic "screen off→on" observation fed to a Host loaded with the demo rule
// fires it — the device controller receives launch, and an automation.run entry
// (disposition "ran") is durably buffered in the operational store and SURVIVES a
// store reopen (REL-090/093).
func TestObserveFiresRuleDispatchesAndDurablyBuffers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	controller := &recordController{}
	host, store := newTestHost(t, dbPath, controller)

	if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(demoEdgeRule)}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}

	reg := registry.FixtureRegistry{}

	// First observation establishes the durable baseline ("off"), never a
	// transition (RUL-300): it must not fire and must emit nothing.
	if disps, err := host.Observe(state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, "off"))); err != nil {
		t.Fatalf("Observe (baseline): %v", err)
	} else if len(disps) != 0 {
		t.Fatalf("baseline observation fired %+v, want no firing (RUL-300)", disps)
	}
	if got := controller.commands(); len(got) != 0 {
		t.Fatalf("device saw %v before any transition, want nothing", got)
	}

	// The screen turns on: the trigger matches, the rule runs, launch flows
	// engine → adapter → surface → device, and an automation.run is emitted.
	disps, err := host.Observe(state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, "on")))
	if err != nil {
		t.Fatalf("Observe (screen-on): %v", err)
	}
	if len(disps) != 1 || disps[0].Disposition != "ran" {
		t.Fatalf("screen-on produced %+v, want exactly one 'ran'", disps)
	}
	if got := controller.commands(); len(got) != 1 || got[0] != testScreenEntity+"/launch" {
		t.Fatalf("device saw %v, want exactly one launch", got)
	}

	// The automation.run must be durably persisted in the operational store.
	assertOneDurableAutomationRun(t, store)

	// And it must survive a store reopen (power-pull durability, REL-090): reopen
	// the SAME db file and resume the durable buffer — the entry is still there.
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	reopened, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open (reopen): %v", err)
	}
	defer reopened.Close()
	assertOneDurableAutomationRun(t, reopened)
}

// assertOneDurableAutomationRun asserts the store holds exactly one durable
// telemetry entry and that it is an automation.run whose payload records the
// firing (rule id + mode_disposition "ran").
func assertOneDurableAutomationRun(t *testing.T, store *identity.Store) {
	t.Helper()
	entries, _, err := store.LoadTelemetry()
	if err != nil {
		t.Fatalf("store.LoadTelemetry: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("durable telemetry has %d entries, want exactly 1", len(entries))
	}
	e := entries[0]
	if e.Schema != telemetry.SchemaAutomationRun {
		t.Fatalf("durable entry schema = %q, want %q", e.Schema, telemetry.SchemaAutomationRun)
	}
	var p struct {
		RuleID          string `json:"rule_id"`
		ModeDisposition string `json:"mode_disposition"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("unmarshal automation.run payload: %v", err)
	}
	if p.ModeDisposition != "ran" {
		t.Fatalf("automation.run mode_disposition = %q, want %q", p.ModeDisposition, "ran")
	}
	if p.RuleID == "" {
		t.Fatalf("automation.run payload carried no rule_id: %s", e.Payload)
	}
}

// TestRunFeedsSyntheticSource proves the Run loop drives observations from a
// DeviceStateSource: a synthetic "baseline then screen-on" sequence fires the
// rule once and durably records one automation.run.
func TestRunFeedsSyntheticSource(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")
	controller := &recordController{}
	host, store := newTestHost(t, dbPath, controller)

	if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(demoEdgeRule)}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}

	reg := registry.FixtureRegistry{}
	src := NewSyntheticSource(
		state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, "off")),
		state.NewObservation(reg, ent(testScreenEntity, "off"), ent(testScreenEntity, "on")),
	)
	if err := host.Run(context.Background(), src); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := controller.commands(); len(got) != 1 || got[0] != testScreenEntity+"/launch" {
		t.Fatalf("device saw %v, want exactly one launch", got)
	}
	assertOneDurableAutomationRun(t, store)
}
