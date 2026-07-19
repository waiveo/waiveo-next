package deviceplane

import (
	"errors"
	"fmt"
)

// This file (Task 3) carries the device-command surface: the app-dispatched
// entry point that resolves a device.command against the target entity's
// device-class command vocabulary and, only if it resolves, executes it
// against the physical device — returning a typed device.command_result
// (contracts/relay-1.md REL-112/113). The edge-rules engine's own
// device_command dispatch (rules/1 eval.CommandSink) is the automation-side
// entry to this same resolve→execute path; both share the resolution and
// dispatch rules modeled here.

// commandMessageType / commandResultMessageType are the device.command and
// device.command_result envelope types (REL-112).
const (
	commandMessageType       = "device.command"
	commandResultMessageType = "device.command_result"
)

// codeCommandUnresolved is relay/1's Error-taxonomy code for a command that
// does not resolve against the target entity's device class (REL-113).
const codeCommandUnresolved = "COMMAND_UNRESOLVED"

// codeInternal is relay/1's Error-taxonomy bucket for an unclassified
// server-side dispatch failure — the fallback when a DeviceController returns
// a plain (non-ControllerError) error.
const codeInternal = "INTERNAL"

// DeviceController is the physical-device adapter the command surface
// dispatches a resolved operation through (the real ECP/Roku adapter is a
// deferred follow-up; tests fake it). Dispatch delivers command with its
// params to the single already-resolved entity (REL-112) and returns nil on
// success, or an error — a *ControllerError to carry a specific taxonomy code,
// or any other error (bucketed INTERNAL).
type DeviceController interface {
	Dispatch(entityID, command string, params map[string]any) error
}

// CommandVocab is the device-class command-vocabulary source the surface
// resolves a command against (REL-113, device-class-registry/1 REG-052). The
// engine's internal/rules/registry.Registry satisfies this (via CommandExists);
// this minimal interface keeps the surface from re-implementing the registry
// while not importing it just for a type.
type CommandVocab interface {
	CommandExists(deviceClass, command string) bool
}

// DeviceClassResolver maps an already-resolved entity_id (REL-112) to the
// device class whose command vocabulary governs it, reporting ok=false when no
// adopted entity is known for the id — in which case the command cannot
// resolve against any vocabulary and is rejected without touching a device
// (REL-113).
type DeviceClassResolver func(entityID string) (deviceClass string, ok bool)

// ControllerError is the typed error a DeviceController returns to surface a
// specific relay/1 Error-taxonomy code (e.g. COMMAND_TARGET_UNREACHABLE) in the
// device.command_result, rather than being bucketed as INTERNAL.
type ControllerError struct {
	Code    string
	Message string
}

// Error implements error.
func (e *ControllerError) Error() string { return e.Code + ": " + e.Message }

// CommandBody is a device.command's body: the single resolved entity, the
// command name, and its params (which MAY carry per-dispatch credential
// material — REL-114, never persisted or logged; the surface holds it only for
// the dispatch call).
type CommandBody struct {
	EntityID string         `json:"entity_id"`
	Command  string         `json:"command"`
	Params   map[string]any `json:"params,omitempty"`
}

// DeviceCommand is the app-peer→relay device.command message (REL-112). trace_id
// is present when the command traces to a single originating operation (REL-006)
// and is echoed onto the result.
type DeviceCommand struct {
	Type    string      `json:"type"`
	ID      string      `json:"id"`
	RelayID string      `json:"relay_id"`
	TraceID string      `json:"trace_id,omitempty"`
	Body    CommandBody `json:"body"`
}

// CommandError is the typed error carried in a rejected device.command_result
// (REL-113/007): the taxonomy code and its message.
type CommandError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CommandResultBody is a device.command_result's body: ok, plus a typed error
// present iff ok is false.
type CommandResultBody struct {
	OK    bool          `json:"ok"`
	Error *CommandError `json:"error,omitempty"`
}

// DeviceCommandResult is the relay→app-peer device.command_result (REL-112). It
// mirrors the request envelope: same id and relay_id, the request's trace_id
// when it carried one (REL-006), and a body carrying ok/error.
type DeviceCommandResult struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	RelayID string            `json:"relay_id"`
	TraceID string            `json:"trace_id,omitempty"`
	Body    CommandResultBody `json:"body"`
}

// CommandSurface resolves and executes app-dispatched device.commands against
// physical devices (REL-112/113). It resolves each command's entity to a
// device class, checks the command against that class's vocabulary, and only
// then dispatches through the DeviceController — never touching a device for a
// command it could not resolve.
type CommandSurface struct {
	controller   DeviceController
	vocab        CommandVocab
	resolveClass DeviceClassResolver
}

// NewCommandSurface builds a CommandSurface from the physical-device adapter,
// the device-class command-vocabulary source (REL-113 / REG-052 — e.g. the
// engine's registry), and the entity_id→device-class resolver (REL-112).
func NewCommandSurface(controller DeviceController, vocab CommandVocab, resolveClass DeviceClassResolver) *CommandSurface {
	return &CommandSurface{controller: controller, vocab: vocab, resolveClass: resolveClass}
}

// Execute resolves cmd against the target entity's device-class command
// vocabulary and, only if it resolves, dispatches it to the physical device,
// returning the typed device.command_result (REL-112/113).
//
// A command that names an unknown entity, or that is not in the resolved device
// class's vocabulary, is rejected {ok:false, COMMAND_UNRESOLVED} and the
// DeviceController is NEVER called (REL-113). A resolved command dispatches and
// returns {ok:true}, or {ok:false} carrying the controller's typed error (a
// *ControllerError's own code, else INTERNAL). The result echoes cmd's id,
// relay_id, and trace_id (REL-006). cmd.Body.Params is passed straight to the
// controller and never logged or persisted here (REL-114).
func (s *CommandSurface) Execute(cmd DeviceCommand) DeviceCommandResult {
	deviceClass, ok := s.resolveClass(cmd.Body.EntityID)
	if !ok {
		// No adopted entity for this id → nothing to resolve the command
		// against → unresolved, and the device is never touched (REL-113).
		return s.reject(cmd, codeCommandUnresolved,
			fmt.Sprintf("entity %q resolves to no adopted device class", cmd.Body.EntityID))
	}

	if !s.vocab.CommandExists(deviceClass, cmd.Body.Command) {
		// REL-113: reject without attempting the command physically.
		return s.reject(cmd, codeCommandUnresolved,
			fmt.Sprintf("%q is not a command %s declares", cmd.Body.Command, deviceClass))
	}

	// Resolved: dispatch to the physical device. params is carried into this
	// call only (REL-114) — never written to a store or a log by this surface.
	if err := s.controller.Dispatch(cmd.Body.EntityID, cmd.Body.Command, cmd.Body.Params); err != nil {
		var ce *ControllerError
		if errors.As(err, &ce) {
			return s.reject(cmd, ce.Code, ce.Message)
		}
		return s.reject(cmd, codeInternal, err.Error())
	}

	return DeviceCommandResult{
		Type:    commandResultMessageType,
		ID:      cmd.ID,
		RelayID: cmd.RelayID,
		TraceID: cmd.TraceID,
		Body:    CommandResultBody{OK: true},
	}
}

// reject builds a {ok:false} device.command_result carrying the given typed
// error, echoing cmd's correlation envelope (REL-006/007).
func (s *CommandSurface) reject(cmd DeviceCommand, code, message string) DeviceCommandResult {
	return DeviceCommandResult{
		Type:    commandResultMessageType,
		ID:      cmd.ID,
		RelayID: cmd.RelayID,
		TraceID: cmd.TraceID,
		Body: CommandResultBody{
			OK:    false,
			Error: &CommandError{Code: code, Message: message},
		},
	}
}
