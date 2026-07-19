package automationhost

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

const (
	testScreenEntity = "01J8Z3K4N5P6Q7R8S9T0V1SCRN"
	testScreenDevice = "01J8Z3K4N5P6Q7R8S9T0V1DEVA"
	testRelayID      = "01J8Z3K4N5P6Q7R8S9T0V1RELY"
)

// demoEdgeRule mirrors the feeder's Wave-1 first-automation rule (REL-062):
// "when the screen turns on, launch the dev channel on it" — a state trigger
// (to:["on"]) driving a device_command launch, classified edge-class.
const demoEdgeRule = `{
	"id": "01J8Z3K4N5P6Q7R8S9T0V1AUTO",
	"mode": "single",
	"triggers": [{"type":"state","entity_id":"` + testScreenEntity + `","to":["on"]}],
	"conditions": [],
	"actions": [{"type":"device_command","entity_id":"` + testScreenEntity + `","command":"launch","params":{"channel":"dev"}}]
}`

// appClassRule is app-class: mode "queued" is app-only run management (RUL-240),
// so the whole rule classifies app and the edge engine must not load it.
const appClassRule = `{
	"id": "01J8Z3K4N5P6Q7R8S9T0V1APPX",
	"mode": "queued",
	"triggers": [{"type":"state","entity_id":"` + testScreenEntity + `","to":["on"]}],
	"conditions": [],
	"actions": [{"type":"device_command","entity_id":"` + testScreenEntity + `","command":"launch"}]
}`

// recordController records every physical dispatch it receives, so a test can
// assert exactly which device commands reached the (fake) device.
type recordController struct {
	mu   sync.Mutex
	seen []string // "entity/command"
}

func (c *recordController) Dispatch(entityID, command string, params map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, entityID+"/"+command)
	return nil
}

func (c *recordController) commands() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.seen))
	copy(out, c.seen)
	return out
}

// testResolver maps the demo rule's screen entity to a media-player device so a
// launch command resolves against the fixture registry's vocabulary (REL-112/113).
func testResolver(entityID string) (deviceID, deviceClass string, ok bool) {
	if entityID == testScreenEntity {
		return testScreenDevice, "media-player", true
	}
	return "", "", false
}

// newTestHost opens a fresh operational store at dbPath and builds a Host over it
// with the given controller.
func newTestHost(t *testing.T, dbPath string, controller deviceplane.DeviceController) (*Host, *identity.Store) {
	t.Helper()
	store, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	host, err := New(store, registry.FixtureRegistry{}, controller, testResolver, testRelayID)
	if err != nil {
		t.Fatalf("automationhost.New: %v", err)
	}
	return host, store
}

func ent(id, st string) state.Entity {
	return state.Entity{ID: id, DeviceClass: "media-player", State: st}
}

func TestApplyEdgeRulesLoadsEdgeRule(t *testing.T) {
	host, _ := newTestHost(t, filepath.Join(t.TempDir(), "relay.db"), &recordController{})

	if err := host.ApplyEdgeRules([]json.RawMessage{json.RawMessage(demoEdgeRule)}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}
	if got := host.EdgeRuleCount(); got != 1 {
		t.Fatalf("EdgeRuleCount = %d, want 1", got)
	}
}

func TestApplyEdgeRulesSkipsAppClassRule(t *testing.T) {
	host, _ := newTestHost(t, filepath.Join(t.TempDir(), "relay.db"), &recordController{})

	// One edge rule and one app-class rule: only the edge rule loads (RUL-240).
	if err := host.ApplyEdgeRules([]json.RawMessage{
		json.RawMessage(demoEdgeRule),
		json.RawMessage(appClassRule),
	}, 1); err != nil {
		t.Fatalf("ApplyEdgeRules: %v", err)
	}
	if got := host.EdgeRuleCount(); got != 1 {
		t.Fatalf("EdgeRuleCount = %d, want 1 (app-class rule must be skipped)", got)
	}
}

func TestApplyEdgeRulesIdempotentReapply(t *testing.T) {
	controller := &recordController{}
	host, _ := newTestHost(t, filepath.Join(t.TempDir(), "relay.db"), controller)

	rules := []json.RawMessage{json.RawMessage(demoEdgeRule)}
	if err := host.ApplyEdgeRules(rules, 1); err != nil {
		t.Fatalf("ApplyEdgeRules (first): %v", err)
	}
	// Re-applying the same generation is a no-op (REL-070): no reload, no duplicate
	// rule instance.
	if err := host.ApplyEdgeRules(rules, 1); err != nil {
		t.Fatalf("ApplyEdgeRules (re-apply): %v", err)
	}
	if got := host.EdgeRuleCount(); got != 1 {
		t.Fatalf("EdgeRuleCount = %d after idempotent re-apply, want 1", got)
	}

	// A single screen-on transition must fire the rule exactly once, not once per
	// apply — proof the re-apply did not double-load the rule.
	if _, err := host.Observe(state.NewObservation(registry.FixtureRegistry{}, ent(testScreenEntity, "off"), ent(testScreenEntity, "off"))); err != nil {
		t.Fatalf("Observe (baseline): %v", err)
	}
	disps, err := host.Observe(state.NewObservation(registry.FixtureRegistry{}, ent(testScreenEntity, "off"), ent(testScreenEntity, "on")))
	if err != nil {
		t.Fatalf("Observe (screen-on): %v", err)
	}
	if len(disps) != 1 {
		t.Fatalf("screen-on fired %d times, want exactly 1 (idempotent re-apply must not double-load)", len(disps))
	}
	if got := controller.commands(); len(got) != 1 || got[0] != testScreenEntity+"/launch" {
		t.Fatalf("device saw %v, want exactly one launch", got)
	}
}
