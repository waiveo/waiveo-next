// Package automation wires the edge-rules engine's command dispatch to the
// relay's device-command surface. A device_command an automation fires goes
// through the SAME deviceplane.CommandSurface as an app-dispatched
// device.command, so it gets the same resolve-against-device-class-vocabulary
// (REL-113), per-device serialization (REL-115), and credential hygiene
// (REL-114). This is the seam that makes the contract-complete edge-rules
// engine actually drive real devices through the relay: the engine evaluates a
// rule, fires a device_command through eval.CommandSink, and this adapter
// executes it on the device plane.
package automation

import (
	"fmt"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/rules/eval"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// CommandSink adapts a deviceplane.CommandSurface to the engine's
// eval.CommandSink. A rejected command (unknown vocabulary, controller error)
// surfaces as a non-nil error so the engine's action-outcome aggregation
// (rules/1 RUL-172 partial-failure) sees the failure.
type CommandSink struct {
	surface *deviceplane.CommandSurface
	relayID string
}

// NewCommandSink builds the adapter. relayID stamps each synthesized
// device.command (relay/1 REL-006).
func NewCommandSink(surface *deviceplane.CommandSurface, relayID string) *CommandSink {
	return &CommandSink{surface: surface, relayID: relayID}
}

// Compile-time assertion that this satisfies the engine's sink contract.
var _ eval.CommandSink = (*CommandSink)(nil)

// Dispatch implements eval.CommandSink by synthesizing a device.command and
// executing it through the device-command surface.
func (a *CommandSink) Dispatch(entityID, command string, params map[string]any) error {
	res := a.surface.Execute(deviceplane.DeviceCommand{
		Type:    "device.command",
		ID:      ulid.New(),
		RelayID: a.relayID,
		Body: deviceplane.CommandBody{
			EntityID: entityID,
			Command:  command,
			Params:   params,
		},
	})
	if res.Body.OK {
		return nil
	}
	if res.Body.Error != nil {
		return fmt.Errorf("automation: device command %q on %s rejected: %s: %s",
			command, entityID, res.Body.Error.Code, res.Body.Error.Message)
	}
	return fmt.Errorf("automation: device command %q on %s rejected", command, entityID)
}
