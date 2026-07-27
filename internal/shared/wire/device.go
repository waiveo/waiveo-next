// device.go carries relay/1's device-plane command frames — the app-peer→relay
// `device.command` and its correlated `device.command_result` reply (REL-112).
// Like the rest of this package these are contract-derived data shapes only; the
// carriage lives in internal/feeder/relayconn (the app peer's outbound dispatch)
// and internal/relay/relayconn (the relay's inbound handler), and the resolution
// and execution the relay performs on receipt live in internal/relay/deviceplane.
//
// `device.command` is the ONLY app-initiated request/response exchange this
// contract defines: every other app-peer→relay frame is either a handshake step
// or REL-057's fire-and-forget nudge. It is therefore the frame that makes the
// app peer's own side of the connection a request originator, mirroring the
// relay's `state.pull` correlation in the opposite direction (REL-006: one
// correlation `id` shared by the pair, plus the originating operation's
// `trace_id`).
package wire

// Frame type discriminators for the device plane's command surface (REL-112).
const (
	FrameTypeDeviceCommand       = "device.command"
	FrameTypeDeviceCommandResult = "device.command_result"
)

// DeviceCommandBody is `device.command`'s body (REL-112):
// `{entity_id, command, params}`. `entity_id` is ALREADY resolved to one
// specific adopted entity — relay/1 accepts a single entity id and no selector
// or device-class filter (rules/1 Entity targeting resolves those before a
// command reaches this contract). `params` MAY carry credential material scoped
// to this one dispatch (REL-114): it is never persisted and never logged, so
// nothing on either side of the wire may copy it into a durable store or a log
// line.
type DeviceCommandBody struct {
	EntityID string         `json:"entity_id"`
	Command  string         `json:"command"`
	Params   map[string]any `json:"params,omitempty"`
}

// DeviceCommandResultBody is `device.command_result`'s body (REL-112):
// `{ok, error}` — `error` present if and only if `ok` is false, carrying an
// Error-taxonomy code (COMMAND_UNRESOLVED for a command the target entity's
// device class does not declare, REL-113; COMMAND_TARGET_UNREACHABLE for a
// device the relay could not reach). Per REL-007 a typed refusal already
// carried as this ack's own `error` field is NOT additionally sent as a
// top-level error frame.
type DeviceCommandResultBody struct {
	OK    bool              `json:"ok"`
	Error *CommandErrorBody `json:"error,omitempty"`
}

// CommandErrorBody is the `{code, message}` object a rejected
// `device.command_result` carries (REL-112/113), the same shape `state.ack`'s
// own error uses (AckErrorBody) — kept a distinct type so the two verbs' bodies
// evolve independently, exactly as the contract defines them separately.
type CommandErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NewDeviceCommandError builds a rejected result body carrying the given
// Error-taxonomy code and message — the one constructor both peers build a
// refusal through, so an `ok:false` result can never be emitted with an absent
// `error` object.
func NewDeviceCommandError(code, message string) DeviceCommandResultBody {
	return DeviceCommandResultBody{
		OK:    false,
		Error: &CommandErrorBody{Code: code, Message: message},
	}
}
