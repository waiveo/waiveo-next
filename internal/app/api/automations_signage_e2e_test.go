package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// The device-plane fixture identities the run-now dispatch test drives. The
// device row id and relay id are borrowed from devices_test.go's own fixture
// set (same test package) so there is one canonical-ULID device identity in
// this package rather than two that have to be kept independently valid; the
// PLACEMENT is this file's own, because these rows have to sit at the node the
// automation is authored under for the caller's scope view to reach them.
const (
	sigDeviceID = devDevice1
	sigRelayID  = devRelayA
)

// automations_signage_e2e_test.go is the END-TO-END proof for the two things
// this track exists to deliver: that `POST /automations/{id}/run` ACTS, and that
// a signage action CHANGES WHAT A SCREEN SHOWS.
//
// Every assertion is made against the projection a real screen is actually
// served from — internal/feeder/snapshot.DeriveScreenPrograms over the live
// store, which is the same function cmd/waiveo-feeder calls to build the signed
// desired state a relay pulls and a player leases. Asserting on the screen ROW's
// `override` member alone would prove the write landed and nothing about whether
// a screen would ever see it, which is precisely the half-proof this codebase
// keeps shipping: the mechanism verified, the seam unverified.

// screenProgramOf resolves the CURRENT program the platform would serve to
// screenID: the store's whole desired-state row set, run through the real
// projection at nowMs.
//
// It takes the instant explicitly because a program override lapses on the
// clock (DAT-004d) and a test asserting the lapse has to be able to name the
// instant it is asserting at.
func screenProgramOf(t *testing.T, e *testEnv, screenID string, nowMs int64) wire.ScreenProgram {
	t.Helper()
	rows, err := e.store.DesiredState(context.Background())
	if err != nil {
		t.Fatalf("read desired state: %v", err)
	}
	programs, derrs := snapshot.DeriveScreenPrograms(rows, e.contentBase, nowMs)
	for _, d := range derrs {
		t.Logf("screen-program derive degrade: %s: %s: %s", d.Field, d.Code, d.Message)
	}
	for _, p := range programs {
		if p.ScreenID == screenID {
			return p
		}
	}
	t.Fatalf("no screen_programs entry for screen %s (derived %d entr(ies))", screenID, len(programs))
	return wire.ScreenProgram{}
}

// signageRunResult is the slice of the run response these tests assert on.
type signageRunResult struct {
	Disposition string `json:"disposition"`
	DryRun      bool   `json:"dry_run"`
	Signage     []struct {
		Action  string `json:"action"`
		Outcome string `json:"outcome"`
		Screens []struct {
			ScreenID string `json:"screen_id"`
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
		} `json:"screens"`
	} `json:"signage"`
}

// mintSignageScreen creates a screen identity row at node carrying the label the
// selector-form tests target it by.
func mintSignageScreen(t *testing.T, e *testEnv, node, name string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/screens", mustJSON(t, map[string]any{
		"name":       name,
		"scope_node": node,
		"labels":     map[string]string{"zone": "lobby"},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create screen %q: %d %s", name, resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// mintSignageCast creates a one-slide cast at node.
func mintSignageCast(t *testing.T, e *testEnv, node, name string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/casts", mustJSON(t, map[string]any{
		"name":       name,
		"scope_node": node,
		"slides":     castSlides(),
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create cast %q: %d %s", name, resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// mintSignageAutomation creates an automation at node whose single action is
// the supplied signage action.
func mintSignageAutomation(t *testing.T, e *testEnv, node string, action map[string]any) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", mustJSON(t, map[string]any{
		"name":       "Signage Automation",
		"scope_node": node,
		"enabled":    true,
		"mode":       "single",
		"triggers":   []any{map[string]any{"type": "state", "entity_id": autoScreenEntity, "to": []string{"on"}}},
		"actions":    []any{action},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create signage automation: %d %s", resp.StatusCode, raw)
	}
	return decodeID(t, raw)
}

// runAutomationNowFor posts the run and decodes its effect report.
func runAutomationNowFor(t *testing.T, e *testEnv, automationID string, body []byte) signageRunResult {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations/"+automationID+"/run", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run automation: %d %s", resp.StatusCode, raw)
	}
	var out signageRunResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode run result: %v (body %s)", err, raw)
	}
	return out
}

// TestPlayCastRunChangesTheScreensResolvedProgram is the headline: an operator
// presses Run on an automation whose action is `play_cast`, and the screen's
// resolved program changes from the DAT-118 terminal blank to the cast's slides.
//
// The rule targets by SELECTOR (`zone=lobby`), which is the daily fleet gesture
// — "put the lunch menu on every lobby screen" — and it is asserted across TWO
// screens so that a fan-out that silently wrote only the first would fail. A
// third screen carrying a different label proves the selector narrows rather
// than sweeping the site.
func TestPlayCastRunChangesTheScreensResolvedProgram(t *testing.T) {
	e := newEnv(t)
	node := e.placementNode(t)

	lobbyA := mintSignageScreen(t, e, node, "Lobby A")
	lobbyB := mintSignageScreen(t, e, node, "Lobby B")

	// A screen the selector must NOT match.
	resp, raw := e.do(t, http.MethodPost, "/api/v1/screens", mustJSON(t, map[string]any{
		"name": "Back Office", "scope_node": node, "labels": map[string]string{"zone": "office"},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create off-target screen: %d %s", resp.StatusCode, raw)
	}
	offTarget := decodeID(t, raw)

	castID := mintSignageCast(t, e, node, "Lunch Menu")

	// Before the run: no schedule governs anything, so every screen resolves to
	// data-model/1's terminal default — powered, showing nothing (DAT-118).
	for _, id := range []string{lobbyA, lobbyB, offTarget} {
		before := screenProgramOf(t, e, id, fixedNowMs)
		if before.Display != "blank" || len(before.Content) != 0 || before.Pinned {
			t.Fatalf("screen %s before the run: display=%q content=%d pinned=%v, want the terminal blank",
				id, before.Display, len(before.Content), before.Pinned)
		}
	}

	automationID := mintSignageAutomation(t, e, node, map[string]any{
		"type": "play_cast", "selector": "zone=lobby", "cast_id": castID,
	})

	out := runAutomationNowFor(t, e, automationID, nil)
	if out.Disposition != "ran" {
		t.Fatalf("disposition = %q, want ran", out.Disposition)
	}
	if out.DryRun {
		t.Fatal("run-now defaulted to a dry run; the default must ACT")
	}
	if len(out.Signage) != 1 || out.Signage[0].Action != "play_cast" || out.Signage[0].Outcome != "complete" {
		t.Fatalf("signage report = %+v, want one complete play_cast", out.Signage)
	}
	if len(out.Signage[0].Screens) != 2 {
		t.Fatalf("play_cast reported %d screen(s), want the 2 the selector matches: %+v",
			len(out.Signage[0].Screens), out.Signage[0].Screens)
	}

	// The proof: BOTH matched screens now resolve to the cast's slides, through
	// the real projection, and the entry is marked pinned so the relay's own
	// schedule re-resolution will not take it back.
	for _, id := range []string{lobbyA, lobbyB} {
		after := screenProgramOf(t, e, id, fixedNowMs)
		if after.Display != "content" {
			t.Fatalf("screen %s after play_cast: display = %q, want content", id, after.Display)
		}
		if len(after.Content) != 1 || after.Content[0].ContentType != "slide" {
			t.Fatalf("screen %s after play_cast: content = %+v, want the cast's one slide", id, after.Content)
		}
		if len(after.Content[0].Layers) != 2 {
			t.Fatalf("screen %s: slide carries %d layer(s), want the cast slide's 2", id, len(after.Content[0].Layers))
		}
		if after.Priority != "scheduled" {
			t.Fatalf("screen %s: priority = %q; a play override is an ordinary content change, not a takeover", id, after.Priority)
		}
		if !after.Pinned {
			t.Fatalf("screen %s: program is not pinned, so a relay re-resolving its schedule would revert it (DAT-004d)", id)
		}
	}

	// The unmatched screen is untouched: a selector narrows, it does not sweep.
	if p := screenProgramOf(t, e, offTarget, fixedNowMs); p.Display != "blank" || p.Pinned {
		t.Fatalf("off-target screen changed: display=%q pinned=%v", p.Display, p.Pinned)
	}
}

// TestShowAlertPreemptsAndDismissClearsIt covers the override half of the
// signage vocabulary end to end: `show_alert` with a literal message puts a
// generated slide on the screen at `preempt` priority (so a player interrupts
// what it is mid-way through, PLY-108), and `dismiss_alert` returns the screen
// to whatever its schedule resolves.
func TestShowAlertPreemptsAndDismissClearsIt(t *testing.T) {
	e := newEnv(t)
	node := e.placementNode(t)
	screenID := mintSignageScreen(t, e, node, "Lobby A")

	alertID := mintSignageAutomation(t, e, node, map[string]any{
		"type": "show_alert", "screen_id": screenID, "message": "Kitchen closed — back at 2pm",
	})
	if out := runAutomationNowFor(t, e, alertID, nil); out.Signage[0].Outcome != "complete" {
		t.Fatalf("show_alert outcome = %+v", out.Signage)
	}

	after := screenProgramOf(t, e, screenID, fixedNowMs)
	if after.Priority != "preempt" {
		t.Fatalf("alert priority = %q, want preempt — an alert that waits for a natural item boundary is not an alert", after.Priority)
	}
	if after.Display != "content" || len(after.Content) != 1 {
		t.Fatalf("alert program = %+v, want one content item", after)
	}
	// The generated alert slide: a full-canvas backing plus the operator's own
	// literal text, drawn through the ordinary slide path.
	layers := after.Content[0].Layers
	if len(layers) != 2 || layers[0].Kind != "rect" || layers[1].Kind != "text" {
		t.Fatalf("alert slide layers = %+v, want a rect backing and a text message", layers)
	}
	if layers[1].Text != "Kitchen closed — back at 2pm" {
		t.Fatalf("alert message = %q, want the operator's own text verbatim", layers[1].Text)
	}

	dismissID := mintSignageAutomation(t, e, node, map[string]any{
		"type": "dismiss_alert", "screen_id": screenID,
	})
	if out := runAutomationNowFor(t, e, dismissID, nil); out.Signage[0].Outcome != "complete" {
		t.Fatalf("dismiss_alert outcome = %+v", out.Signage)
	}
	cleared := screenProgramOf(t, e, screenID, fixedNowMs)
	if cleared.Pinned || cleared.Display != "blank" {
		t.Fatalf("after dismiss_alert: display=%q pinned=%v, want the screen back on its schedule",
			cleared.Display, cleared.Pinned)
	}
}

// TestAlertLapsesOnItsOwnTTL: `ttl_seconds` becomes an absolute `expires_at`
// (DAT-004c) and the projection stops honoring the override once the instant
// passes — with NO write anywhere. That is what makes an alert self-limiting on
// a relay that has lost its app peer, and it is asserted here by resolving the
// same unchanged store at two different instants.
func TestAlertLapsesOnItsOwnTTL(t *testing.T) {
	e := newEnv(t)
	node := e.placementNode(t)
	screenID := mintSignageScreen(t, e, node, "Lobby A")

	automationID := mintSignageAutomation(t, e, node, map[string]any{
		"type": "show_alert", "screen_id": screenID, "message": "Fire drill in progress", "ttl_seconds": 60,
	})
	runAutomationNowFor(t, e, automationID, nil)

	if p := screenProgramOf(t, e, screenID, fixedNowMs+30_000); p.Priority != "preempt" {
		t.Fatalf("30s into a 60s alert: priority = %q, want preempt", p.Priority)
	}
	if p := screenProgramOf(t, e, screenID, fixedNowMs+61_000); p.Pinned || p.Display != "blank" {
		t.Fatalf("61s into a 60s alert: display=%q pinned=%v, want the override to have lapsed with no write",
			p.Display, p.Pinned)
	}
}

// TestDryRunWithholdsEveryEffect: `dry_run: true` resolves every target and
// reports exactly what a real run would do, and changes nothing. It is the
// counterpart assertion to the default acting — a flag that is ignored in either
// direction is worse than no flag.
func TestDryRunWithholdsEveryEffect(t *testing.T) {
	e := newEnv(t)
	node := e.placementNode(t)
	screenID := mintSignageScreen(t, e, node, "Lobby A")
	castID := mintSignageCast(t, e, node, "Lunch Menu")

	automationID := mintSignageAutomation(t, e, node, map[string]any{
		"type": "play_cast", "screen_id": screenID, "cast_id": castID,
	})

	out := runAutomationNowFor(t, e, automationID, []byte(`{"dry_run":true}`))
	if !out.DryRun {
		t.Fatal("dry_run was not echoed on the response")
	}
	if len(out.Signage) != 1 || out.Signage[0].Outcome != "complete" || len(out.Signage[0].Screens) != 1 {
		t.Fatalf("dry run must still RESOLVE and report its targets: %+v", out.Signage)
	}
	if p := screenProgramOf(t, e, screenID, fixedNowMs); p.Pinned || p.Display != "blank" {
		t.Fatalf("dry run changed the screen: display=%q pinned=%v", p.Display, p.Pinned)
	}
}

// TestPlayCastNamingAMissingCastIsReportedPerScreen: the DAT-004c reference
// check lives on the surface imposing the override, and its refusal is reported
// rather than swallowed. A run that answered "complete" and left the screen
// unchanged would be the exact defect this whole track is closing.
func TestPlayCastNamingAMissingCastIsReportedPerScreen(t *testing.T) {
	e := newEnv(t)
	node := e.placementNode(t)
	screenID := mintSignageScreen(t, e, node, "Lobby A")

	automationID := mintSignageAutomation(t, e, node, map[string]any{
		"type": "play_cast", "screen_id": screenID, "cast_id": "01J8Z0C0000000000000000000",
	})
	out := runAutomationNowFor(t, e, automationID, nil)
	if len(out.Signage) != 1 || out.Signage[0].Outcome != "failed" {
		t.Fatalf("play_cast on a missing cast: outcome = %+v, want failed", out.Signage)
	}
	if len(out.Signage[0].Screens) != 1 || out.Signage[0].Screens[0].Error == "" {
		t.Fatalf("the refusal must name the screen and the reason: %+v", out.Signage[0].Screens)
	}
	if p := screenProgramOf(t, e, screenID, fixedNowMs); p.Pinned {
		t.Fatal("a refused play_cast still pinned the screen")
	}
}

// TestSignageActionWithBothScreenIdAndSelectorIsRefusedAtCompile pins the
// RUL-233 ambiguity check at the surface that gates authoring. Without it the
// action decodes as a perfectly well-formed selector-only EntityRef and the
// executor silently picks one of the two target sets the author named.
func TestSignageActionWithBothScreenIdAndSelectorIsRefusedAtCompile(t *testing.T) {
	e := newEnv(t)
	node := e.placementNode(t)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", mustJSON(t, map[string]any{
		"name":       "Ambiguous Signage",
		"scope_node": node,
		"enabled":    true,
		"mode":       "single",
		"triggers":   []any{map[string]any{"type": "state", "entity_id": autoScreenEntity, "to": []string{"on"}}},
		"actions": []any{map[string]any{
			"type": "play_cast", "screen_id": "01J8Z0D0000000000000000000",
			"selector": "zone=lobby", "cast_id": "01J8Z0C0000000000000000000",
		}},
	}), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("ambiguous ScreenRef status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	errsAny, _ := p["errors"].([]any)
	if len(errsAny) != 1 {
		t.Fatalf("problem carries %d field error(s), want 1: %s", len(errsAny), raw)
	}
	fe, _ := errsAny[0].(map[string]any)
	if fe["code"] != "SCREEN_REF_AMBIGUOUS" {
		t.Fatalf("field error code = %v, want SCREEN_REF_AMBIGUOUS (body %s)", fe["code"], raw)
	}
}

// TestRunNowDispatchesARealDeviceCommand is 6.2's other half: run-now must
// reach HARDWARE, not just authored rows. The rule's `device_command` has to
// arrive at the dispatcher that carries frames down the owning relay's
// persistent connection — the identical seam `POST /entities/{id}/commands`
// uses — and the response must report the target it reached.
//
// Before this track the same request returned `disposition: "ran"` having
// dispatched into a `nopSink`, so this test failing to see a dispatch is the
// whole regression it guards.
func TestRunNowDispatchesARealDeviceCommand(t *testing.T) {
	registry := devices.New(autoScopeNode, func() int64 { return fixedNowMs })
	if err := registry.PutDevice(devices.Device{
		ID: sigDeviceID, RelayID: sigRelayID, DeviceClass: "media-player",
		Name: "Lobby TV", ScopeNode: autoScopeNode, Labels: map[string]string{},
	}); err != nil {
		t.Fatalf("PutDevice: %v", err)
	}
	if err := registry.PutEntity(devices.Entity{
		ID: autoScreenEntity, DeviceID: sigDeviceID, RelayID: sigRelayID, DeviceClass: "media-player",
		Name: "Lobby TV player", ScopeNode: autoScopeNode, Labels: map[string]string{"zone": "lobby"}, State: "on",
	}); err != nil {
		t.Fatalf("PutEntity: %v", err)
	}
	dispatcher := &fakeDispatcher{result: wire.DeviceCommandResultBody{OK: true}}

	e := newEnvWithOptions(t, api.WithDevicePlane(registry, dispatcher))
	node := e.placementNode(t)

	automationID := mintSignageAutomation(t, e, node, map[string]any{
		"type": "device_command", "entity_id": autoScreenEntity,
		"command": "launch", "params": map[string]any{"channel": "dev"},
	})

	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations/"+automationID+"/run", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run: %d %s", resp.StatusCode, raw)
	}
	var out struct {
		Disposition string `json:"disposition"`
		Commands    []struct {
			EntityID string `json:"entity_id"`
			Command  string `json:"command"`
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}

	calls := dispatcher.dispatched()
	if len(calls) != 1 {
		t.Fatalf("dispatcher saw %d command(s), want 1 — run-now did not reach the device plane", len(calls))
	}
	if calls[0].relayID != sigRelayID {
		t.Errorf("dispatched to relay %q, want the entity's own %q", calls[0].relayID, sigRelayID)
	}
	if calls[0].body.EntityID != autoScreenEntity || calls[0].body.Command != "launch" {
		t.Errorf("dispatched body = %+v, want the rule's own entity and command", calls[0].body)
	}
	if calls[0].body.Params["channel"] != "dev" {
		t.Errorf("dispatched params = %+v, want the rule's own params carried through", calls[0].body.Params)
	}
	if len(out.Commands) != 1 || !out.Commands[0].OK || out.Commands[0].EntityID != autoScreenEntity {
		t.Fatalf("run report = %+v, want one successful command naming the entity", out.Commands)
	}

	// A dry run over the SAME rule resolves and reports the target and does not
	// dispatch a second time.
	if _, raw := e.do(t, http.MethodPost, "/api/v1/automations/"+automationID+"/run",
		[]byte(`{"dry_run":true}`), nil); len(dispatcher.dispatched()) != 1 {
		t.Fatalf("a dry run dispatched to the device plane (%d call(s) total): %s", len(dispatcher.dispatched()), raw)
	}
}

// TestRunNowReportsACommandItCouldNotDispatch: a rule pointing at an entity this
// app peer has never been told about is the single most likely authoring
// mistake, and it used to vanish — eval absorbs a single-target rejection
// (RUL-161 gives it no outcome channel), so the run answered "ran" with an empty
// command list. The report must name the target and the reason.
func TestRunNowReportsACommandItCouldNotDispatch(t *testing.T) {
	e := newEnv(t)
	node := e.placementNode(t)

	automationID := mintSignageAutomation(t, e, node, map[string]any{
		"type": "device_command", "entity_id": autoScreenEntity, "command": "launch",
	})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations/"+automationID+"/run", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run: %d %s", resp.StatusCode, raw)
	}
	var out struct {
		Commands []struct {
			EntityID string `json:"entity_id"`
			Command  string `json:"command"`
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Commands) != 1 {
		t.Fatalf("run report carried %d command(s), want the one the rule declared: %s", len(out.Commands), raw)
	}
	if out.Commands[0].OK || out.Commands[0].Error == "" || out.Commands[0].EntityID != autoScreenEntity {
		t.Fatalf("undispatchable command reported as %+v, want a named failure", out.Commands[0])
	}
}
