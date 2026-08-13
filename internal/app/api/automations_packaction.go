package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/rules/eval"
)

// packActionSink is eval.PackActionSink over the invocation queue: a RUL-231
// `pack_action` names an action a pack declared, and this routes it.
//
// # Why a rule can reach a pack at all
//
// The pack surface already carries an action's whole round trip — a management
// caller POSTs to MAN-101's route, the row is queued, the pack leases it, works,
// and reports back. Automation needed none of that built again; it needed a way
// IN. This sink is that way in, and deliberately enqueues through the same
// store call the HTTP route does rather than a parallel path, so an action a
// rule fires and the same action an operator fires are one thing to the pack,
// to the audit trail, and to MAN-103's retry promise.
//
// # The routing, which is the part that is this component's problem
//
// RUL-231 reads the execution class off the MANIFEST entry the action name
// points at. The evaluation core holds no manifests, so it hands the name here
// and this side decides. Two classes:
//
//   - `app-service` — dispatched to the pack's own handler (MAN-101/CTX-110),
//     which is the queue below.
//   - `relay-command` — MUST be dispatched as a device command and MUST NOT go
//     through the pack's handler (RUL-232, restated as MAN-104 and CTX-112).
//
// The relay-command path is NOT wired here, and the refusal below says so in
// those words. That is the honest posture and not a stub: the forbidden thing —
// quietly queueing it to the pack anyway, which is one line and would look like
// it worked — is the single failure this rule exists to prevent, and it would
// fail invisibly, with the pack receiving a command class it was promised it
// would never be handed.
type packActionSink struct {
	srv     *server
	ctx     context.Context
	traceID string
	dry     bool
}

func (s *packActionSink) InvokePackAction(name string, params map[string]any) eval.PackActionOutcome {
	out := eval.PackActionOutcome{Action: "pack_action", Name: name}

	packID, actionName, ok := splitPackActionName(name)
	if !ok {
		out.Error = "a pack action must be publisher-qualified as `<publisher>/<pack>.<action>` (rules/1 RUL-231)"
		return out
	}

	pack, found, err := s.srv.store.GetPack(s.ctx, packID)
	if err != nil {
		out.Error = "the pack could not be read"
		return out
	}
	if !found {
		out.Error = "no pack `" + packID + "` is installed"
		return out
	}

	// Declared as an action at all (MAN-100), which is where the idempotency
	// class lives, AND declared automation-facing (MAN-091), which is where the
	// execution class lives. Both are required and neither implies the other in
	// a document that has not been checked for the two agreeing.
	action, declared := declaredAction(pack.Manifest, actionName)
	if !declared {
		out.Error = "pack `" + packID + "` declares no action `" + actionName + "`"
		return out
	}
	execution, automationFacing := automationActionExecution(pack.Manifest, actionName)
	if !automationFacing {
		out.Error = "pack `" + packID + "` does not expose `" + actionName +
			"` to automation (manifest/1 MAN-091)"
		return out
	}
	// The MAN-100 flag, checked separately from MAN-091 membership because
	// nothing else checks it: the manifest validator does not require the two to
	// agree, so a publisher can list an action under contributes.automation
	// while leaving automationCallable at its default false. Refusing is the
	// fail-closed reading of that contradiction — the flag is the one place the
	// publisher says yes or no in a single word, and CTX-111 speaks of "an
	// `automationCallable: true` action invoked from automation" as the thing
	// that has an automation contract at all.
	if !action.AutomationCallable {
		out.Error = "pack `" + packID + "` declares `" + actionName + "` not automationCallable (manifest/1 MAN-100)"
		return out
	}

	switch execution {
	case "app-service":
		// falls through to the queue below
	case "relay-command":
		out.Error = "`" + name + "` is an execution: relay-command action; this host does not yet dispatch one " +
			"as a device command, and rules/1 RUL-232 forbids sending it to the pack's handler instead"
		return out
	default:
		// Unreachable through a validated manifest (MAN-091 closes the enum) and
		// reported rather than defaulted anyway: defaulting an unknown class to
		// app-service is precisely how a relay-command reaches the pack handler.
		out.Error = "`" + name + "` declares an execution class this host cannot route: " + execution
		return out
	}

	if s.dry {
		// Nothing queued. A dry run reports the routing decision it reached —
		// which is the answer an operator is asking for — without giving the pack
		// work it would really perform.
		out.OK = true
		return out
	}

	encoded, merr := json.Marshal(params)
	if merr != nil {
		out.Error = "the action's params could not be encoded"
		return out
	}

	if _, err := s.srv.store.EnqueuePackInvocation(s.ctx, store.PackInvocation{
		PackID: packID, Action: actionName, Params: encoded,
		// Copied at enqueue for the reason the HTTP route copies it: this
		// invocation's fate on a lease expiry is decided by the promise in force
		// when it was accepted, not by whatever a later pack update says
		// (MAN-103).
		Idempotency: action.IdempotencyClass,
		TraceID:     s.traceID,
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			out.Error = "no pack `" + packID + "` is installed"
			return out
		}
		out.Error = "the invocation could not be queued"
		return out
	}

	// OK means QUEUED, not completed. A pack action is asynchronous by
	// construction — the pack leases the row and reports its result later — so a
	// rule run cannot report the action's outcome without holding the run open
	// for the pack's own working time. Reporting the handoff is the true
	// statement available at this instant; the invocation row carries the rest.
	out.OK = true
	return out
}

// splitPackActionName splits RUL-231's publisher-qualified name into the pack id
// and the pack-local action name.
//
// The split is at the FIRST `.`, which is exact rather than a heuristic: a pack
// id is `<publisher>/<name>` where both halves match `^[a-z][a-z0-9-]{1,38}$`
// (MAN-001), so a pack id can never contain a `.` and the first one always ends
// it. An action name containing dots therefore survives intact, which splitting
// at the last `.` would not manage.
func splitPackActionName(name string) (packID, action string, ok bool) {
	dot := strings.IndexByte(name, '.')
	if dot <= 0 || dot == len(name)-1 {
		return "", "", false
	}
	packID, action = name[:dot], name[dot+1:]
	// The `/` is what makes this a PACK id rather than a bare word, and checking
	// it here keeps a name like `lights.on` — which parses structurally — from
	// being looked up as a pack called "lights".
	if !strings.Contains(packID, "/") {
		return "", "", false
	}
	return packID, action, true
}

// automationActionExecution returns the execution class the manifest declares
// for name under contributes.automation.actions (MAN-091).
func automationActionExecution(rawManifest json.RawMessage, name string) (string, bool) {
	var m struct {
		Contributes struct {
			Automation struct {
				Actions []struct {
					Name      string `json:"name"`
					Execution string `json:"execution"`
				} `json:"actions"`
			} `json:"automation"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(rawManifest, &m); err != nil {
		return "", false
	}
	for _, a := range m.Contributes.Automation.Actions {
		if a.Name == name {
			return a.Execution, true
		}
	}
	return "", false
}
