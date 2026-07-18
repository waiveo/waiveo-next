package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
)

// parseStateTrigger decodes one standalone trigger JSON object into a
// model.Member (via the public ParseRule) and builds a StateTrigger from it.
func parseStateTrigger(t *testing.T, triggerJSON string) *StateTrigger {
	t.Helper()
	ruleJSON := `{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2ZR","triggers":[` + triggerJSON + `]}`
	r, err := model.ParseRule([]byte(ruleJSON))
	if err != nil {
		t.Fatalf("ParseRule(%s): %v", ruleJSON, err)
	}
	if len(r.Triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(r.Triggers))
	}
	st, err := NewStateTrigger(r.Triggers[0])
	if err != nil {
		t.Fatalf("NewStateTrigger: %v", err)
	}
	return st
}

// TestNewStateTriggerClassification checks that the four RUL-023 shapes decode
// into the presence flags the fire logic keys on.
func TestNewStateTriggerClassification(t *testing.T) {
	entity := "01J8Z3K4N5P6Q7R8S9T0V1W2Z2"
	// case 3: state-scoped (attribute absent, to present)
	st := parseStateTrigger(t, `{"type":"state","entity_id":"`+entity+`","to":"playing"}`)
	if st.Attribute != "" || st.HasFrom || !st.HasTo {
		t.Errorf("state-scoped: got attr=%q hasFrom=%v hasTo=%v", st.Attribute, st.HasFrom, st.HasTo)
	}
	if st.EntityID != entity {
		t.Errorf("EntityID = %q, want %q", st.EntityID, entity)
	}
	// case 4: unscoped
	un := parseStateTrigger(t, `{"type":"state","entity_id":"`+entity+`"}`)
	if un.Attribute != "" || un.HasFrom || un.HasTo {
		t.Errorf("unscoped: got attr=%q hasFrom=%v hasTo=%v", un.Attribute, un.HasFrom, un.HasTo)
	}
	// case 1: attribute-scoped bounded
	ab := parseStateTrigger(t, `{"type":"state","entity_id":"`+entity+`","attribute":"volume","from":20,"to":25}`)
	if ab.Attribute != "volume" || !ab.HasFrom || !ab.HasTo {
		t.Errorf("attr-bounded: got attr=%q hasFrom=%v hasTo=%v", ab.Attribute, ab.HasFrom, ab.HasTo)
	}
	// case 2: attribute-scoped unbounded
	au := parseStateTrigger(t, `{"type":"state","entity_id":"`+entity+`","attribute":"volume"}`)
	if au.Attribute != "volume" || au.HasFrom || au.HasTo {
		t.Errorf("attr-unbounded: got attr=%q hasFrom=%v hasTo=%v", au.Attribute, au.HasFrom, au.HasTo)
	}
}

func mkEntity(state0 string, attrs map[string]any) state.Entity {
	return state.Entity{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Z2", DeviceClass: "media-player", State: state0, Attributes: attrs}
}

// TestStateScopedNeverFiresOnAttributeOnly is RUL-023 case 3 + RUL-330: a
// state-scoped trigger never fires on a tick whose state string is unchanged,
// even when the unchanged value equals its declared to:.
func TestStateScopedNeverFiresOnAttributeOnly(t *testing.T) {
	reg := registry.FixtureRegistry{}
	st := parseStateTrigger(t, `{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","to":"playing"}`)
	prev := TriggerBaseline{Known: true, State: "playing"}
	obs := state.Observation{Entity: mkEntity("playing", map[string]any{"media_title": "Episode 4"}), StateChanged: false, ChangedAttrs: []string{"media_title"}}
	fired, next := st.Observe(reg, prev, obs)
	if fired {
		t.Error("state-scoped fired on attribute-only tick, want no fire (RUL-023 case 3)")
	}
	if !next.Known || next.State != "playing" {
		t.Errorf("baseline = %+v, want Known state=playing", next)
	}
}

// TestUnscopedFiresOnAnyChange is RUL-023 case 4: an unscoped trigger fires on
// any reported change, state-string or attribute.
func TestUnscopedFiresOnAnyChange(t *testing.T) {
	reg := registry.FixtureRegistry{}
	un := parseStateTrigger(t, `{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2"}`)
	// attribute-only tick
	fired, _ := un.Observe(reg, TriggerBaseline{Known: true, State: "playing"},
		state.Observation{Entity: mkEntity("playing", nil), StateChanged: false, ChangedAttrs: []string{"media_title"}})
	if !fired {
		t.Error("unscoped did not fire on attribute-only change, want fire")
	}
	// state-change tick
	fired, _ = un.Observe(reg, TriggerBaseline{Known: true, State: "playing"},
		state.Observation{Entity: mkEntity("paused", nil), StateChanged: true})
	if !fired {
		t.Error("unscoped did not fire on state change, want fire")
	}
	// no change at all
	fired, _ = un.Observe(reg, TriggerBaseline{Known: true, State: "playing"},
		state.Observation{Entity: mkEntity("playing", nil), StateChanged: false})
	if fired {
		t.Error("unscoped fired with no change, want no fire")
	}
}

// TestAttributeUnboundedFiresOnValueChange is RUL-023 case 2.
func TestAttributeUnboundedFiresOnValueChange(t *testing.T) {
	reg := registry.FixtureRegistry{}
	au := parseStateTrigger(t, `{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","attribute":"volume"}`)
	prev := TriggerBaseline{Known: true, State: "playing", Attr: float64(20), AttrKnown: true}
	// volume moves 20->25
	fired, next := au.Observe(reg, prev, state.Observation{Entity: mkEntity("playing", map[string]any{"volume": float64(25)}), StateChanged: false, ChangedAttrs: []string{"volume"}})
	if !fired {
		t.Error("attr-unbounded did not fire on value change, want fire")
	}
	// volume unchanged 25->25
	fired, _ = au.Observe(reg, next, state.Observation{Entity: mkEntity("playing", map[string]any{"volume": float64(25)}), StateChanged: false})
	if fired {
		t.Error("attr-unbounded fired on unchanged value, want no fire")
	}
}

// TestFirstObservationBrandNewSuppressed is RUL-300/302/304: the first
// observation of an entity (no durable prior) is never a transition, even
// when the observed value equals to:, and a later genuine transition fires.
func TestFirstObservationBrandNewSuppressed(t *testing.T) {
	reg := registry.FixtureRegistry{}
	st := parseStateTrigger(t, `{"type":"state","entity_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Z2","to":"playing"}`)
	// first observation equals to: must NOT fire
	fired, next := st.Observe(reg, TriggerBaseline{}, // Known:false
		state.Observation{Entity: mkEntity("playing", nil), StateChanged: true})
	if fired {
		t.Error("first observation fired, want suppressed (RUL-300)")
	}
	if !next.Known || next.State != "playing" {
		t.Errorf("baseline after first obs = %+v, want Known state=playing", next)
	}
	// a genuine later transition paused->playing fires
	fired, _ = st.Observe(reg, TriggerBaseline{Known: true, State: "paused"},
		state.Observation{Entity: mkEntity("playing", nil), StateChanged: true})
	if !fired {
		t.Error("genuine transition to playing did not fire, want fire")
	}
}

// --- RUL-023 corpus case ---

type rul023Case struct {
	Input struct {
		Triggers []json.RawMessage `json:"triggers"`
		Events   []struct {
			Seq               int               `json:"seq"`
			TMs               int64             `json:"t_ms"`
			Kind              string            `json:"kind"`
			StateBefore       string            `json:"state_before"`
			StateAfter        string            `json:"state_after"`
			AttributesChanged map[string][2]any `json:"attributes_changed"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []map[string]any `json:"results"`
	} `json:"expected"`
}

// TestRUL023AttributeOnlyVsStateChangeCorpus replays the named corpus case,
// driving all three triggers (state-scoped, unscoped, attribute-scoped) with
// per-trigger threaded baselines (RUL-304) and asserting the ACTUAL expected
// <id>_fired flags for each event.
func TestRUL023AttributeOnlyVsStateChangeCorpus(t *testing.T) {
	reg := registry.FixtureRegistry{}
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "rules-1", "RUL-023-attribute-only-vs-state-change.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul023Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Build the triggers keyed by id.
	type trig struct {
		id string
		st *StateTrigger
	}
	var trigs []trig
	baselines := map[string]TriggerBaseline{}
	running := map[string]any{"volume": float64(20)} // pre-seq1 attribute values
	for _, raw := range c.Input.Triggers {
		var idOnly struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &idOnly)
		st, err := buildStateTrigger(raw)
		if err != nil {
			t.Fatalf("build trigger %s: %v", idOnly.ID, err)
		}
		trigs = append(trigs, trig{id: idOnly.ID, st: st})
		// seq1 has a known state_before, so no trigger is on its first
		// observation: seed each baseline from the pre-seq1 snapshot.
		bl := TriggerBaseline{Known: true, State: c.Input.Events[0].StateBefore}
		if st.Attribute != "" {
			bl.Attr = running[st.Attribute]
			bl.AttrKnown = true
		}
		baselines[idOnly.ID] = bl
	}

	for _, ev := range c.Input.Events {
		// Apply attribute changes to the running snapshot.
		changed := make([]string, 0, len(ev.AttributesChanged))
		for name, pair := range ev.AttributesChanged {
			running[name] = pair[1]
			changed = append(changed, name)
		}
		sort.Strings(changed)
		attrsCopy := map[string]any{}
		for k, v := range running {
			attrsCopy[k] = v
		}
		obs := state.Observation{
			Entity:       state.Entity{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Z2", DeviceClass: "media-player", State: ev.StateAfter, Attributes: attrsCopy},
			StateChanged: ev.StateBefore != ev.StateAfter,
			ChangedAttrs: changed,
		}
		want := c.Expected.Results[ev.Seq-1]
		for _, tr := range trigs {
			fired, next := tr.st.Observe(reg, baselines[tr.id], obs)
			baselines[tr.id] = next
			key := tr.id + "_fired"
			if wf, ok := want[key].(bool); ok {
				if fired != wf {
					t.Errorf("seq %d trigger %s: fired=%v want %v", ev.Seq, tr.id, fired, wf)
				}
			}
		}
	}
}

// buildStateTrigger builds a StateTrigger from a raw trigger JSON object,
// tolerating the corpus's extra "id" field.
func buildStateTrigger(raw json.RawMessage) (*StateTrigger, error) {
	r, err := model.ParseRule([]byte(`{"id":"01J8Z3K4N5P6Q7R8S9T0V1W2ZR","triggers":[` + string(raw) + `]}`))
	if err != nil {
		return nil, err
	}
	return NewStateTrigger(r.Triggers[0])
}

// --- RUL-300 corpus case ---

type rul300Case struct {
	Input struct {
		Fixture struct {
			Entities map[string]struct {
				DeviceClass               string `json:"device_class"`
				DurableStateBeforeRestart string `json:"durable_state_before_restart"`
			} `json:"entities"`
		} `json:"fixture"`
		Trigger json.RawMessage `json:"trigger"`
		Events  []struct {
			Seq           int    `json:"seq"`
			TMs           int64  `json:"t_ms"`
			Kind          string `json:"kind"`
			ReportedState string `json:"reported_state"`
		} `json:"events"`
	} `json:"input"`
	Expected struct {
		Results []struct {
			Seq              int    `json:"seq"`
			OldStateResolved string `json:"old_state_resolved"`
			StateChanged     bool   `json:"state_changed"`
			Fired            bool   `json:"fired"`
		} `json:"results"`
	} `json:"expected"`
}

// TestRUL300BootReconfirmNoFireCorpus replays the named corpus case: an
// engine_restart seeds the durable pre-restart value, the first post-restart
// observation reconfirming it does not fire, and a later genuine transition
// to the same target does.
func TestRUL300BootReconfirmNoFireCorpus(t *testing.T) {
	reg := registry.FixtureRegistry{}
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "rules-1", "RUL-300-boot-reconfirm-no-fire.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var c rul300Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse: %v", err)
	}
	st, err := buildStateTrigger(c.Input.Trigger)
	if err != nil {
		t.Fatalf("build trigger: %v", err)
	}

	// Seed the baseline from the durable pre-restart value (engine_restart).
	var durable, deviceClass string
	for _, ent := range c.Input.Fixture.Entities {
		durable = ent.DurableStateBeforeRestart
		deviceClass = ent.DeviceClass
	}
	bl := TriggerBaseline{Known: true, State: durable}

	resultBySeq := map[int]struct {
		old     string
		changed bool
		fired   bool
	}{}
	for _, ev := range c.Input.Events {
		if ev.Kind != "observation" {
			continue
		}
		obs := state.Observation{
			Entity:       state.Entity{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Z2", DeviceClass: deviceClass, State: ev.ReportedState},
			StateChanged: bl.State != ev.ReportedState,
		}
		old := bl.State
		fired, next := st.Observe(reg, bl, obs)
		bl = next
		resultBySeq[ev.Seq] = struct {
			old     string
			changed bool
			fired   bool
		}{old: old, changed: obs.StateChanged, fired: fired}
	}

	for _, want := range c.Expected.Results {
		got := resultBySeq[want.Seq]
		if got.fired != want.Fired {
			t.Errorf("seq %d: fired=%v want %v", want.Seq, got.fired, want.Fired)
		}
		if got.changed != want.StateChanged {
			t.Errorf("seq %d: state_changed=%v want %v", want.Seq, got.changed, want.StateChanged)
		}
		if got.old != want.OldStateResolved {
			t.Errorf("seq %d: old_state_resolved=%q want %q", want.Seq, got.old, want.OldStateResolved)
		}
	}
}
