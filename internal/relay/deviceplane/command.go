package deviceplane

import (
	"errors"
	"fmt"
	"log"
	"sync"
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
// canonical device-class-registry/1 registry (internal/deviceclass.Registry,
// e.g. deviceclass.Builtin()) satisfies this directly via its own
// CommandExists — the identical method signature — with no adapter needed;
// the engine's internal/rules/registry.Registry also satisfies it. This
// minimal interface keeps the surface from depending on either concrete
// registry type while not importing one just for a type.
type CommandVocab interface {
	CommandExists(deviceClass, command string) bool
}

// EntityResolver maps an already-resolved entity_id (REL-112) to the physical
// device that exposes it and the device class whose command vocabulary governs
// it — both read from the one adopted-entity record. It reports ok=false when
// no adopted entity is known for the id, in which case the command cannot
// resolve against any vocabulary and is rejected without touching a device
// (REL-113).
//
// deviceID names the physical (or virtual) device the entity belongs to
// (data-model/1: a device exposes one or more entities). Because one device
// fans out to many entity_ids, deviceID — not entity_id — is the key
// REL-115's per-device serialization must use: two commands to two DIFFERENT
// entities of the SAME physical device still contend for that one device and
// MUST NOT be dispatched concurrently.
type EntityResolver func(entityID string) (deviceID, deviceClass string, ok bool)

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

// CommandRecord is the redacted, credential-free descriptor of a command the
// surface handed to its observability/persistence sinks (REL-114): it names the
// target entity, the command, and the outcome — and deliberately carries NO
// params field, so a device.command's per-dispatch credential material can never
// reach a log sink or a durable store through it.
type CommandRecord struct {
	EntityID  string
	Command   string
	OK        bool
	ErrorCode string // the taxonomy code when OK is false; empty otherwise

	// Dispatched reports whether the DeviceController was reached at all —
	// false for a command this surface refused on its own (REL-113: an unknown
	// entity, or a command the resolved device class does not declare).
	//
	// It is the field that distinguishes the two ways a command can fail, which
	// look identical in ErrorCode: a controller may legitimately answer
	// COMMAND_UNRESOLVED itself (the loopback adapter does, for "this relay has
	// no device adapter"), so a reader keying on the code alone cannot tell a
	// command that reached hardware from one that never left this function.
	//
	// A consumer needs that distinction because everything downstream of the
	// controller — every wrapper that logs a dispatch, every adapter-side trace —
	// exists only on the dispatched path. An undispatched command is invisible
	// everywhere else by construction.
	Dispatched bool

	// Detail is this SURFACE's own words for why an undispatched command was
	// refused ("entity X resolves to no adopted device class", "launch is not a
	// command media-player declares"), empty on every dispatched one.
	//
	// It is built here from the entity id, the command name and the resolved
	// device class, and never from params and never from the controller — which
	// is what keeps REL-114 intact while making the refusal actionable. The two
	// refusals carry the same taxonomy code and want completely different
	// remedies (adopt/rename the device; fix the command), so a record without
	// this says only "something did not resolve".
	Detail string
}

// CommandLog is the surface's log sink: it receives a redacted CommandRecord for
// every command the surface handles, for operational observability. REL-114: a
// device.command's params (which MAY carry credential material) are NEVER passed
// to a CommandLog — only the credential-free CommandRecord is.
type CommandLog interface {
	LogCommand(rec CommandRecord)
}

// CommandJournal is the surface's durable sink — the relay's own operational
// store / persisted desired state. It receives the same redacted CommandRecord
// as the log. REL-114: credential material MUST NOT be written to any durable
// store, so params never reach a CommandJournal.
type CommandJournal interface {
	PersistCommand(rec CommandRecord)
}

// CommandOption configures an optional CommandSurface collaborator (a log sink
// or a durable journal). Options are applied by NewCommandSurface; omitting them
// leaves the corresponding sink absent (a no-op).
type CommandOption func(*CommandSurface)

// WithCommandLog wires a log sink the surface emits a redacted CommandRecord to
// for every command it handles (REL-114: never the params).
func WithCommandLog(log CommandLog) CommandOption {
	return func(s *CommandSurface) { s.log = log }
}

// WithCommandJournal wires a durable operational store the surface persists a
// redacted CommandRecord to for every command it handles (REL-114: never the
// params).
func WithCommandJournal(journal CommandJournal) CommandOption {
	return func(s *CommandSurface) { s.journal = journal }
}

// WithCommandSource names the subsystem this surface serves — "automation",
// "preset", "keep-alive" — for the operator-visible line an unresolved command
// leaves (logUnresolved).
//
// It is a label, never behaviour: two surfaces with different sources resolve,
// serialize and dispatch identically. It exists because several subsystems drive
// the same devices through identically shaped surfaces, and a refusal that
// cannot say which one issued it makes an operator check all of them.
func WithCommandSource(source string) CommandOption {
	return func(s *CommandSurface) { s.source = source }
}

// CommandSurface resolves and executes app-dispatched device.commands against
// physical devices (REL-112/113). It resolves each command's entity to a
// device class, checks the command against that class's vocabulary, and only
// then dispatches through the DeviceController — never touching a device for a
// command it could not resolve. Dispatch to a single physical device is
// serialized (REL-115), and a dispatch's params credential material is carried
// only in memory to the controller, never to the log or journal sinks (REL-114).
type CommandSurface struct {
	controller    DeviceController
	vocab         CommandVocab
	resolveEntity EntityResolver

	log     CommandLog
	journal CommandJournal
	source  string

	// locksMu guards locks; each entry is the per-device dispatch lock keyed by
	// device_id (REL-115) — every command to any entity of that one physical
	// device holds this same lock across its dispatch, so a second command to
	// the device queues rather than interleaving.
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

// NewCommandSurface builds a CommandSurface from the physical-device adapter,
// the device-class command-vocabulary source (REL-113 / REG-052 — e.g. the
// engine's registry), and the entity_id→(device_id, device-class) resolver
// (REL-112; the device_id it returns is REL-115's serialization key). Optional
// CommandOptions wire the redacted log/journal sinks (REL-114).
func NewCommandSurface(controller DeviceController, vocab CommandVocab, resolveEntity EntityResolver, opts ...CommandOption) *CommandSurface {
	s := &CommandSurface{
		controller:    controller,
		vocab:         vocab,
		resolveEntity: resolveEntity,
		locks:         make(map[string]*sync.Mutex),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
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
	res, dispatched := s.execute(cmd)
	// Observability/persistence carry only a redacted, credential-free record —
	// the command's params (which MAY be credential material) never reach the
	// log or the durable store (REL-114).
	rec := CommandRecord{
		EntityID:   cmd.Body.EntityID,
		Command:    cmd.Body.Command,
		OK:         res.Body.OK,
		Dispatched: dispatched,
	}
	if res.Body.Error != nil {
		rec.ErrorCode = res.Body.Error.Code
		if !dispatched {
			// Surface-authored (see CommandRecord.Detail). A DISPATCHED failure's
			// message comes from the controller and is deliberately not copied
			// here: this record goes to a durable journal, and a message this
			// package did not compose is a message it cannot promise is
			// credential-free.
			rec.Detail = res.Body.Error.Message
		}
	}
	if !dispatched {
		s.logUnresolved(rec)
	}
	if s.log != nil {
		s.log.LogCommand(rec)
	}
	if s.journal != nil {
		s.journal.PersistCommand(rec)
	}
	return res
}

// logUnresolved writes the one operator-visible line for a command that never
// reached a device.
//
// # Why this is not an optional sink
//
// It is unconditional, and it is inside the surface rather than beside it,
// because the alternative is what shipped: WithCommandLog/WithCommandJournal
// existed, were correct, and were wired by NOTHING in the relay binary. A preset
// batch fired at an entity this relay could not resolve therefore produced no
// log line, no journal entry and no event anywhere — and the resolve happens
// before the DeviceController is called, so the dispatch-logging wrapper the
// binary DOES install (cmd/waiveo-relay's loggingController) is never reached
// either. An operator watching a schedule fire correctly and a screen do nothing
// had no signal of any kind; the finding that produced this line was reached by
// reading the source, having first concluded from the silence that the schedule
// had not fired at all.
//
// An observability half that a caller must remember to connect is the same
// half-built pair this defect is an instance of. This one has no wiring step.
//
// A DISPATCHED command is deliberately NOT logged here: the binary's own
// controller wrapper already logs those, with the source that issued it and the
// parameter keys, and duplicating them would double the volume of the ordinary
// case to catch the exceptional one.
//
// REL-114: entity id, command name and this package's own refusal text only.
// Never params, never a value.
func (s *CommandSurface) logUnresolved(rec CommandRecord) {
	log.Printf("waiveo-relay dispatch [%s]: %s %s NOT DISPATCHED (%s): %s",
		s.sourceOrDefault(), rec.EntityID, rec.Command, rec.ErrorCode, rec.Detail)
}

// sourceOrDefault names the subsystem whose command this was, for the line
// above. Unset is spelled out rather than left blank: with several subsystems
// (edge rules, schedule preset batches, keep-alive, the app peer) dispatching
// through identically shaped surfaces, a line that cannot say which one it came
// from sends an operator looking through all four.
func (s *CommandSurface) sourceOrDefault() string {
	if s.source == "" {
		return "device-plane"
	}
	return s.source
}

// execute performs the resolve→serialized-dispatch→result path for a single
// command (REL-112/113/115); Execute wraps it to emit the redacted record.
//
// The second return is whether the DeviceController was reached. It is reported
// out rather than re-derived from the result, because the result cannot answer
// it: a controller may return the same COMMAND_UNRESOLVED code this function
// rejects with, so the only place that knows which side of the dispatch a
// failure came from is this function. See CommandRecord.Dispatched.
func (s *CommandSurface) execute(cmd DeviceCommand) (DeviceCommandResult, bool) {
	deviceID, deviceClass, ok := s.resolveEntity(cmd.Body.EntityID)
	if !ok {
		// No adopted entity for this id → nothing to resolve the command
		// against → unresolved, and the device is never touched (REL-113).
		return s.reject(cmd, codeCommandUnresolved,
			fmt.Sprintf("entity %q resolves to no adopted device class", cmd.Body.EntityID)), false
	}

	if !s.vocab.CommandExists(deviceClass, cmd.Body.Command) {
		// REL-113: reject without attempting the command physically.
		return s.reject(cmd, codeCommandUnresolved,
			fmt.Sprintf("%q is not a command %s declares", cmd.Body.Command, deviceClass)), false
	}

	// REL-115: serialize per target device_id — NOT per entity_id. A device
	// exposes one or more entities (data-model/1), so two commands to two
	// different entities of the same physical device must contend for one lock;
	// keying on entity_id would let them interleave delivery to that one device.
	// The lock is taken only for a resolved command — an unresolved one never
	// reaches here and never touches the device (REL-113).
	lock := s.deviceLock(deviceID)
	lock.Lock()
	defer lock.Unlock()

	// Resolved: dispatch to the physical device. params is carried into this
	// call only (REL-114) — never written to a store or a log by this surface.
	if err := s.controller.Dispatch(cmd.Body.EntityID, cmd.Body.Command, cmd.Body.Params); err != nil {
		var ce *ControllerError
		if errors.As(err, &ce) {
			return s.reject(cmd, ce.Code, ce.Message), true
		}
		return s.reject(cmd, codeInternal, err.Error()), true
	}

	return DeviceCommandResult{
		Type:    commandResultMessageType,
		ID:      cmd.ID,
		RelayID: cmd.RelayID,
		TraceID: cmd.TraceID,
		Body:    CommandResultBody{OK: true},
	}, true
}

// deviceLock returns the per-device dispatch mutex for deviceID, creating it on
// first use. Keying on the physical device_id (the entity's owning device, from
// resolveEntity) — not the entity_id — is what enforces REL-115's
// one-outstanding-command-per-device rule: every entity of one device shares
// this one lock, so a second command to that device can never be dispatched
// while an earlier one to any of its entities is still outstanding.
func (s *CommandSurface) deviceLock(deviceID string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	l, ok := s.locks[deviceID]
	if !ok {
		l = &sync.Mutex{}
		s.locks[deviceID] = l
	}
	return l
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
