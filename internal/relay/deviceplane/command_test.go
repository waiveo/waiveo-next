package deviceplane

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/registry"
)

// recordingController is a fake DeviceController that records every Dispatch it
// receives (so a test can assert a physical dispatch happened — or, for
// REL-113, that it never did) and returns a configurable error.
type recordingController struct {
	calls  []recordedDispatch
	retErr error
}

type recordedDispatch struct {
	entityID string
	command  string
	params   map[string]any
}

func (c *recordingController) Dispatch(entityID, command string, params map[string]any) error {
	c.calls = append(c.calls, recordedDispatch{entityID: entityID, command: command, params: params})
	return c.retErr
}

// corpusCommandsFile decodes the pieces of the REL-110 corpus this task (the
// device-command surface) is the oracle for: input.commands, the fixture vocab
// (target_entity_device_class + media_player_commands), and expected.results
// plus expected.unresolved_command_attempted_against_device.
type corpusCommandsFile struct {
	Input struct {
		Commands                []json.RawMessage `json:"commands"`
		TargetEntityDeviceClass string            `json:"target_entity_device_class"`
		MediaPlayerCommands     []string          `json:"media_player_commands"`
	} `json:"input"`
	Expected struct {
		Results                                 []json.RawMessage `json:"results"`
		UnresolvedCommandAttemptedAgainstDevice bool              `json:"unresolved_command_attempted_against_device"`
	} `json:"expected"`
}

func loadRel110Commands(t *testing.T) corpusCommandsFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(rel110Corpus))
	if err != nil {
		t.Fatalf("reading REL-110 corpus: %v", err)
	}
	var f corpusCommandsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decoding REL-110 corpus: %v", err)
	}
	if len(f.Input.Commands) != 2 || len(f.Expected.Results) != 2 {
		t.Fatalf("REL-110 corpus: want 2 commands + 2 results, got %d/%d",
			len(f.Input.Commands), len(f.Expected.Results))
	}
	return f
}

// rel110Surface builds the CommandSurface the REL-110 corpus describes: the
// FixtureRegistry as the command vocab (its media-player commands are exactly
// the corpus's media_player_commands, REL-113 / device-class-registry REG-052),
// and a resolver mapping the corpus's one entity to target_entity_device_class.
func rel110Surface(t *testing.T, controller DeviceController) *CommandSurface {
	t.Helper()
	f := loadRel110Commands(t)
	resolve := func(entityID string) (string, string, bool) {
		// The corpus dispatches both commands against the one adopted entity.
		if entityID == "01J8Z3K4N5P6Q7R8S9T0V1W2Y2" {
			return entityID, f.Input.TargetEntityDeviceClass, true
		}
		return "", "", false
	}
	return NewCommandSurface(controller, registry.FixtureRegistry{}, resolve)
}

// assertJSONShape marshals got and compares it, as a generic tree, to wantRaw.
func assertJSONShape(t *testing.T, got any, wantRaw json.RawMessage) {
	t.Helper()
	gotRaw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var gotTree, wantTree any
	if err := json.Unmarshal(gotRaw, &gotTree); err != nil {
		t.Fatalf("re-decode our result: %v", err)
	}
	if err := json.Unmarshal(wantRaw, &wantTree); err != nil {
		t.Fatalf("decode corpus result: %v", err)
	}
	if !reflect.DeepEqual(gotTree, wantTree) {
		t.Errorf("result shape mismatch:\n got=%s\nwant=%s", gotRaw, wantRaw)
	}
}

// TestFixtureVocabMatchesCorpus ties the reused registry to the corpus: the
// FixtureRegistry accepts exactly the corpus's media_player_commands and
// nothing else the corpus's own commands name (REL-113 vocab source).
func TestFixtureVocabMatchesCorpus(t *testing.T) {
	f := loadRel110Commands(t)
	reg := registry.FixtureRegistry{}
	for _, cmd := range f.Input.MediaPlayerCommands {
		if !reg.CommandExists(f.Input.TargetEntityDeviceClass, cmd) {
			t.Errorf("registry rejects corpus-declared command %q", cmd)
		}
	}
	if reg.CommandExists(f.Input.TargetEntityDeviceClass, "blast") {
		t.Error("registry accepts \"blast\" — corpus expects it unresolved")
	}
}

// TestExecuteResolvedLaunchDispatches asserts the first REL-110 command (a
// launch on a media-player) resolves, dispatches to the physical device, and
// acknowledges {ok:true} with id/relay_id/trace_id echoed (REL-006/112).
func TestExecuteResolvedLaunchDispatches(t *testing.T) {
	f := loadRel110Commands(t)
	controller := &recordingController{}
	surface := rel110Surface(t, controller)

	var cmd DeviceCommand
	if err := json.Unmarshal(f.Input.Commands[0], &cmd); err != nil {
		t.Fatalf("decode command[0]: %v", err)
	}

	got := surface.Execute(cmd)
	assertJSONShape(t, got, f.Expected.Results[0])

	if len(controller.calls) != 1 {
		t.Fatalf("controller dispatch count = %d, want 1", len(controller.calls))
	}
	call := controller.calls[0]
	if call.entityID != "01J8Z3K4N5P6Q7R8S9T0V1W2Y2" || call.command != "launch" {
		t.Errorf("dispatched %q/%q, want entity 01J8Z3K4N5P6Q7R8S9T0V1W2Y2 / launch", call.entityID, call.command)
	}
	if call.params["channel"] != "dev" {
		t.Errorf("dispatched params = %v, want channel=dev carried through", call.params)
	}
}

// TestExecuteUnresolvedBlastRejectedWithoutTouchingDevice asserts the second
// REL-110 command (blast, not in the media-player vocab) is rejected as
// COMMAND_UNRESOLVED and the physical device is NEVER touched (REL-113); the
// result carries no trace_id because the command carried none.
func TestExecuteUnresolvedBlastRejectedWithoutTouchingDevice(t *testing.T) {
	f := loadRel110Commands(t)
	controller := &recordingController{}
	surface := rel110Surface(t, controller)

	var cmd DeviceCommand
	if err := json.Unmarshal(f.Input.Commands[1], &cmd); err != nil {
		t.Fatalf("decode command[1]: %v", err)
	}

	got := surface.Execute(cmd)
	assertJSONShape(t, got, f.Expected.Results[1])

	if len(controller.calls) != 0 {
		t.Fatalf("controller was called %d time(s) for an unresolved command — REL-113 forbids touching the device", len(controller.calls))
	}
	// The corpus's own oracle field for exactly this invariant.
	if f.Expected.UnresolvedCommandAttemptedAgainstDevice {
		t.Fatal("corpus expects unresolved_command_attempted_against_device:false")
	}
	if got.Body.OK || got.Body.Error == nil || got.Body.Error.Code != "COMMAND_UNRESOLVED" {
		t.Errorf("result = %+v, want ok:false COMMAND_UNRESOLVED", got.Body)
	}
}

// TestExecuteEchoesEnvelope asserts the result mirrors the request envelope:
// type=device.command_result, id/relay_id echoed, trace_id echoed when present
// and omitted when the request carried none (REL-006/112).
func TestExecuteEchoesEnvelope(t *testing.T) {
	surface := NewCommandSurface(&recordingController{}, registry.FixtureRegistry{},
		func(entityID string) (string, string, bool) { return entityID, "media-player", true })

	withTrace := surface.Execute(DeviceCommand{
		Type: "device.command", ID: "cmd-1", RelayID: "relay-1", TraceID: "trace-1",
		Body: CommandBody{EntityID: "e1", Command: "home"},
	})
	if withTrace.Type != "device.command_result" || withTrace.ID != "cmd-1" ||
		withTrace.RelayID != "relay-1" || withTrace.TraceID != "trace-1" {
		t.Errorf("envelope not echoed: %+v", withTrace)
	}
	// trace_id must serialize when present and disappear when absent.
	raw, _ := json.Marshal(withTrace)
	if !contains(raw, "trace_id") {
		t.Errorf("trace_id missing from result JSON: %s", raw)
	}

	noTrace := surface.Execute(DeviceCommand{
		Type: "device.command", ID: "cmd-2", RelayID: "relay-1",
		Body: CommandBody{EntityID: "e1", Command: "home"},
	})
	raw2, _ := json.Marshal(noTrace)
	if contains(raw2, "trace_id") {
		t.Errorf("trace_id present in result for a trace-less command: %s", raw2)
	}
}

func contains(b []byte, sub string) bool {
	return len(b) > 0 && json.Valid(b) && indexOf(string(b), sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestExecuteUnknownEntityIsUnresolvedWithoutTouchingDevice asserts that when
// the entity resolves to no device class, the command cannot resolve against a
// vocabulary and is rejected COMMAND_UNRESOLVED without touching the device
// (REL-113: never attempt an unresolved command physically).
func TestExecuteUnknownEntityIsUnresolvedWithoutTouchingDevice(t *testing.T) {
	controller := &recordingController{}
	surface := NewCommandSurface(controller, registry.FixtureRegistry{},
		func(string) (string, string, bool) { return "", "", false })

	got := surface.Execute(DeviceCommand{
		Type: "device.command", ID: "cmd-9", RelayID: "relay-1",
		Body: CommandBody{EntityID: "ghost", Command: "launch"},
	})
	if got.Body.OK || got.Body.Error == nil || got.Body.Error.Code != "COMMAND_UNRESOLVED" {
		t.Errorf("result = %+v, want ok:false COMMAND_UNRESOLVED for unknown entity", got.Body)
	}
	if len(controller.calls) != 0 {
		t.Fatalf("controller touched for an unresolvable entity — %d call(s)", len(controller.calls))
	}
}

// TestExecuteControllerTypedErrorPropagates asserts a resolved command whose
// physical dispatch fails returns {ok:false} carrying the controller's typed
// error code (or INTERNAL for an untyped failure) — the "or the controller's
// typed error" arm of REL-112's result.
func TestExecuteControllerTypedErrorPropagates(t *testing.T) {
	// A typed ControllerError propagates its own code + message.
	typed := &recordingController{retErr: &ControllerError{
		Code: "COMMAND_TARGET_UNREACHABLE", Message: "no route to device",
	}}
	surface := NewCommandSurface(typed, registry.FixtureRegistry{},
		func(entityID string) (string, string, bool) { return entityID, "media-player", true })
	got := surface.Execute(DeviceCommand{
		ID: "c", RelayID: "r", Body: CommandBody{EntityID: "e", Command: "power"},
	})
	if got.Body.OK || got.Body.Error == nil || got.Body.Error.Code != "COMMAND_TARGET_UNREACHABLE" {
		t.Errorf("result = %+v, want ok:false COMMAND_TARGET_UNREACHABLE", got.Body)
	}
	if got.Body.Error.Message != "no route to device" {
		t.Errorf("message = %q, want the controller's own message", got.Body.Error.Message)
	}
	if len(typed.calls) != 1 {
		t.Errorf("controller call count = %d, want 1 (a resolved command IS dispatched)", len(typed.calls))
	}

	// An untyped error is bucketed as INTERNAL (the unclassified taxonomy code).
	plain := &recordingController{retErr: errors.New("boom")}
	surface2 := NewCommandSurface(plain, registry.FixtureRegistry{},
		func(entityID string) (string, string, bool) { return entityID, "media-player", true })
	got2 := surface2.Execute(DeviceCommand{
		ID: "c", RelayID: "r", Body: CommandBody{EntityID: "e", Command: "power"},
	})
	if got2.Body.OK || got2.Body.Error == nil || got2.Body.Error.Code != "INTERNAL" {
		t.Errorf("result = %+v, want ok:false INTERNAL for an untyped controller error", got2.Body)
	}
}
