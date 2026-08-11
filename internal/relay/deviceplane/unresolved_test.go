package deviceplane

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/registry"
)

// unresolved_test.go covers the one outcome this surface produces entirely on
// its own: a command it refuses BEFORE the DeviceController is reached
// (REL-113). It is the outcome nothing downstream can see, because everything
// that observes a dispatch — the relay binary's own logging controller wrapper,
// every adapter-side trace — lives on the far side of a call this path never
// makes.
//
// The defect this file exists for: a schedule preset batch bound to an entity
// the relay could not resolve fired on every daypart transition and produced no
// log line, no journal entry and no event anywhere. Three forced applies, three
// "resolved and served" lines, zero dispatches, and the conclusion drawn from
// that silence — that the preset had never fired — was wrong.

// captureLog redirects the standard logger for one test and returns a reader of
// what was written. The surface logs through `log` deliberately (an operator on
// a box reads journald, and the relay's lines land there), so this is the seam.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return &buf
}

// resolveNothing is a relay that can resolve no entity at all — a box whose
// device was renamed, un-adopted, or never adopted. It is the ordinary shape of
// this failure in the field, not a corner.
func resolveNothing(string) (string, string, bool) { return "", "", false }

// TestAnUnresolvedCommandLeavesAnOperatorVisibleLine is the central case, and
// it does not wire a log sink: the point is that a deployment which wires
// NOTHING — which is what the relay binary did — still gets a line. An
// observability half a caller must remember to connect is how this became
// invisible in the first place.
func TestAnUnresolvedCommandLeavesAnOperatorVisibleLine(t *testing.T) {
	logged := captureLog(t)
	surface := NewCommandSurface(&recordingController{}, registry.FixtureRegistry{},
		resolveNothing, WithCommandSource("preset"))

	res := surface.Execute(DeviceCommand{
		Type: commandMessageType, ID: "01J8Z", RelayID: "relay-1",
		Body: CommandBody{EntityID: "01J8ZENTITY0000000000000001", Command: "launch",
			Params: map[string]any{"channel_id": "12"}},
	})
	if res.Body.OK {
		t.Fatal("an unresolvable entity produced ok:true")
	}

	line := logged.String()
	if line == "" {
		t.Fatal("a preset batch fired at an entity this relay cannot resolve produced NO output at all — which is the whole defect: the schedule fires faithfully, nothing happens, and nothing anywhere says so")
	}
	for _, want := range []string{"preset", "01J8ZENTITY0000000000000001", "launch", "COMMAND_UNRESOLVED"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line does not name %q, so an operator cannot act on it: %s", want, line)
		}
	}
	// REL-114: the params a command carries are credential material and never
	// reach an operator-readable line, whatever else that line says.
	if strings.Contains(line, "channel_id") || strings.Contains(line, "12") {
		t.Errorf("the log line carried the command's params: %s", line)
	}
}

// TestTheRefusalNamesWHICHRefusalItIs: both refusals carry COMMAND_UNRESOLVED
// and want completely different remedies — adopt or rename the device, versus
// fix the command. A line that said only "unresolved" would leave an operator
// guessing which.
func TestTheRefusalNamesWHICHRefusalItIs(t *testing.T) {
	cases := []struct {
		why     string
		resolve EntityResolver
		command string
		mustSay string
		mustNot string
	}{
		{
			why:     "no such entity",
			resolve: resolveNothing,
			command: "launch",
			mustSay: "resolves to no adopted device class",
			mustNot: "is not a command",
		},
		{
			why:     "the class does not declare the command",
			resolve: func(id string) (string, string, bool) { return id, "media-player", true },
			command: "self_destruct",
			mustSay: "is not a command",
			mustNot: "resolves to no adopted device class",
		},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			logged := captureLog(t)
			surface := NewCommandSurface(&recordingController{}, registry.FixtureRegistry{}, tc.resolve)
			surface.Execute(DeviceCommand{
				Type: commandMessageType, ID: "01J8Z", RelayID: "relay-1",
				Body: CommandBody{EntityID: "01J8ZENTITY0000000000000001", Command: tc.command},
			})
			if got := logged.String(); !strings.Contains(got, tc.mustSay) || strings.Contains(got, tc.mustNot) {
				t.Errorf("the %s refusal logged %q, want it to say %q and not %q", tc.why, got, tc.mustSay, tc.mustNot)
			}
		})
	}
}

// TestADispatchedCommandIsNotLoggedTwice: the relay binary already wraps its
// controllers so every command that REACHES a device leaves a line naming the
// subsystem and the parameter keys. This surface must stay quiet about those, or
// the ordinary case doubles in volume to catch the exceptional one.
//
// The case that makes this non-obvious is the loopback controller, which answers
// COMMAND_UNRESOLVED itself for "this relay has no device adapter configured".
// It is a DISPATCHED command with the same taxonomy code as a refusal, so a
// filter written on the code rather than on the fact would log it here as well.
func TestADispatchedCommandIsNotLoggedTwice(t *testing.T) {
	for _, tc := range []struct {
		why        string
		controller DeviceController
	}{
		{"a dispatch that succeeded", &recordingController{}},
		{"a controller that answered COMMAND_UNRESOLVED itself", &recordingController{retErr: &ControllerError{
			Code: "COMMAND_UNRESOLVED", Message: "this relay has no device adapter configured"}}},
	} {
		t.Run(tc.why, func(t *testing.T) {
			logged := captureLog(t)
			surface := NewCommandSurface(tc.controller, registry.FixtureRegistry{},
				func(id string) (string, string, bool) { return id, "media-player", true })
			surface.Execute(DeviceCommand{
				Type: commandMessageType, ID: "01J8Z", RelayID: "relay-1",
				Body: CommandBody{EntityID: "01J8ZENTITY0000000000000001", Command: "launch"},
			})
			if got := logged.String(); got != "" {
				t.Errorf("%s was logged by the surface as well as by the binary's controller wrapper: %s", tc.why, got)
			}
		})
	}
}

// TestTheRecordSaysWhetherTheDeviceWasReached pins the fact the log line and
// every other consumer is derived from. ErrorCode cannot answer it — a
// controller may return COMMAND_UNRESOLVED itself — so the record carries it
// explicitly, and a sink that wants to distinguish "the command failed at the
// device" from "the command never left the relay" has one field to read.
func TestTheRecordSaysWhetherTheDeviceWasReached(t *testing.T) {
	cases := []struct {
		why            string
		resolve        EntityResolver
		controller     DeviceController
		wantDispatched bool
		wantDetail     bool
	}{
		{"refused before the controller", resolveNothing, &recordingController{}, false, true},
		{"dispatched and refused BY the controller, same code",
			func(id string) (string, string, bool) { return id, "media-player", true },
			&recordingController{retErr: &ControllerError{Code: "COMMAND_UNRESOLVED", Message: "no adapter"}}, true, false},
		{"dispatched and accepted",
			func(id string) (string, string, bool) { return id, "media-player", true },
			&recordingController{}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			captureLog(t)
			spy := &spyCommandLog{}
			surface := NewCommandSurface(tc.controller, registry.FixtureRegistry{}, tc.resolve, WithCommandLog(spy))
			surface.Execute(DeviceCommand{
				Type: commandMessageType, ID: "01J8Z", RelayID: "relay-1",
				Body: CommandBody{EntityID: "01J8ZENTITY0000000000000001", Command: "launch",
					Params: map[string]any{"credential": rel114Credential}},
			})
			if len(spy.records) != 1 {
				t.Fatalf("%d record(s) logged, want 1", len(spy.records))
			}
			rec := spy.records[0]
			if rec.Dispatched != tc.wantDispatched {
				t.Errorf("Dispatched = %v, want %v — the code alone cannot answer this (%q)", rec.Dispatched, tc.wantDispatched, rec.ErrorCode)
			}
			if (rec.Detail != "") != tc.wantDetail {
				t.Errorf("Detail = %q, want present=%v: it is this package's OWN words for a refusal it made, and is never copied from the controller (whose message this package cannot promise is credential-free)", rec.Detail, tc.wantDetail)
			}
			assertNoCredential(t, "log", spy.records)
		})
	}
}
