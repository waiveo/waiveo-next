// Package ecp is the real Roku ECP DeviceController: the hardware half of the
// device plane's controller seam (deviceplane.DeviceController), replacing the
// Wave-1 loopbackController stand-in (cmd/waiveo-relay/main.go) with actual
// HTTP dispatch against a Roku device's External Control Protocol port.
//
// It implements exactly the media-player device class's command vocabulary
// (device-class-registry/1 REG-066: launch, home, keypress, power) as plain
// unauthenticated ECP POSTs (REL-114 — ECP itself carries no credential; this
// package never logs a dispatch, so nothing it handles can leak one). A
// command that resolves against the surface's vocabulary check (REL-112/113)
// but that this controller still cannot execute — an unknown entityID, an
// unknown command, a missing/malformed param, an unreachable device, or a
// non-2xx ECP response — is failed closed with a *deviceplane.ControllerError
// carrying one of relay/1's own Error-taxonomy codes, never a silent no-op.
package ecp

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
)

// codeTargetUnreachable is relay/1's Error-taxonomy code for "the relay could
// not reach the target device to attempt the command" (contracts/relay-1.md
// Error taxonomy, retryable) — a TRANSIENT condition: the entity has a
// configured Target, but dialing or exchanging with it over ECP failed
// outright (connection refused/reset, timeout, DNS failure). This controller
// raises it ONLY for that case.
//
// I2: it must NOT be reused for "no Target is configured for this entityID at
// all" — that is a config/wiring gap between this controller's own address
// book and the caller, discovered because there is nowhere to send the
// command, not because a device was dialed and failed to answer. See
// codeUnresolved below, which this controller uses for that case instead.
const codeTargetUnreachable = "COMMAND_TARGET_UNREACHABLE"

// codeUnresolved is relay/1's REL-113 taxonomy code for a command that does
// not resolve. Two shapes both use it here:
//
//  1. The device-command surface (deviceplane.CommandSurface) already rejects
//     an out-of-vocabulary command before ever calling Dispatch (REL-113), but
//     this controller fails closed with the same code for an unknown command
//     name or a missing/invalid param — REG-066's own vocabulary and shapes
//     are what it resolves against here, so the code fits identically even
//     though the failure surfaces one layer deeper than the surface's own
//     check.
//
//  2. (I2) An entityID this Controller has no configured Target for at all.
//     deviceplane.CommandSurface's own execute() already uses this exact code
//     (command.go's codeCommandUnresolved) for its "no adopted entity" check —
//     an entityID missing from THIS controller's targets map, caught one
//     layer deeper, is the identical shape of problem: the system does not
//     know how to route to this entity, a config/wiring gap, not a device
//     that was reached and failed. An earlier revision of this controller
//     reused codeTargetUnreachable for this case; that made "can't route to
//     this entity" surface under two different codes depending on which layer
//     detected it, which would read as inconsistent to anything built on the
//     taxonomy for observability/alerting/retry policy. Using
//     codeUnresolved here instead keeps both layers consistent.
const codeUnresolved = "COMMAND_UNRESOLVED"

// defaultPort is Roku ECP's well-known port, used whenever a Target's Port is
// the zero value.
const defaultPort = 8060

// defaultTimeout bounds a single ECP HTTP round trip. Roku's ECP has no
// documented SLA; 3s keeps a dead or wedged device from hanging a dispatch
// (and, transitively via REL-115, every other command queued against the same
// physical device) indefinitely.
const defaultTimeout = 3 * time.Second

// Target names one Roku ECP endpoint — the (host, port) a media-player
// entity's commands dispatch to over plain HTTP (REL-114: ECP is
// unauthenticated, so there is no credential here to protect). Port's zero
// value means Roku ECP's own default, 8060.
type Target struct {
	Host string
	Port int
}

// addr renders t as a dispatchable "host:port" pair, substituting
// defaultPort for a zero Port. Uses net.JoinHostPort (M1) so an IPv6 literal
// host is correctly bracketed (e.g. "[::1]:8060") rather than producing a
// malformed authority.
func (t Target) addr() string {
	port := t.Port
	if port == 0 {
		port = defaultPort
	}
	return net.JoinHostPort(t.Host, strconv.Itoa(port))
}

// Option configures an optional Controller collaborator. New applies each in
// order after establishing its defaults, so a later Option can override an
// earlier one.
type Option func(*Controller)

// WithHTTPClient overrides the *http.Client Dispatch issues ECP requests
// through — tests point it at an httptest.Server-backed client or one with a
// shorter timeout; production leaves it at New's default (a plain client with
// defaultTimeout). A nil client is ignored (M2), matching
// ecppoll.WithHTTPClient's own convention, so New's constructed default is
// retained rather than leaving Dispatch to panic against a nil *http.Client.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Controller) {
		if client != nil {
			c.client = client
		}
	}
}

// Controller is the ECP-backed deviceplane.DeviceController: it dispatches a
// resolved media-player command (REG-066) to the physical Roku device its
// entityID maps to, over ECP's plain HTTP control surface.
type Controller struct {
	targets map[string]Target
	client  *http.Client
}

// New builds a Controller that dispatches to targets, keyed by entityID.
// entityIDs absent from targets fail every Dispatch closed with
// COMMAND_UNRESOLVED (I2: no Target configured for this entityID is a
// config/wiring gap, not a device the relay tried and failed to reach) — the
// device plane's own EntityResolver may know an entity, but if this
// controller has no Target for it there is nowhere to send the command. The
// default *http.Client has a defaultTimeout deadline; opts (e.g.
// WithHTTPClient) may override it.
func New(targets map[string]Target, opts ...Option) *Controller {
	c := &Controller{
		targets: targets,
		client:  &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Dispatch implements deviceplane.DeviceController. It resolves entityID to a
// Target, builds the ECP request REG-066's command vocabulary defines for
// command/params, and POSTs it. A nil return means the device accepted the
// command (a 2xx ECP response); any other outcome — unknown entityID, unknown
// command, an invalid param, a network failure, or a non-2xx response — is a
// failure, and a *deviceplane.ControllerError carries a taxonomy code wherever
// one fits (REL-112/113/114).
func (c *Controller) Dispatch(entityID, command string, params map[string]any) error {
	target, ok := c.targets[entityID]
	if !ok {
		// I2: no configured Target is a config/wiring gap (this controller's
		// own address book has nothing for entityID), not a device that was
		// dialed and failed to answer — codeUnresolved, not
		// codeTargetUnreachable, matches deviceplane.CommandSurface's own
		// "unknown entity" code one layer up.
		return &deviceplane.ControllerError{
			Code:    codeUnresolved,
			Message: fmt.Sprintf("no ECP target configured for entity %q", entityID),
		}
	}

	path, err := ecpPath(command, params)
	if err != nil {
		return err
	}

	reqURL := fmt.Sprintf("http://%s%s", target.addr(), path)
	req, err := http.NewRequest(http.MethodPost, reqURL, nil)
	if err != nil {
		// Only a malformed URL (e.g. an unescapable host) reaches here; the
		// path itself is always url.PathEscape'd below.
		return fmt.Errorf("ecp: building request for %s to entity %q: %w", command, entityID, err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		// Dial/timeout/connection-reset — the relay could not reach the
		// device at all (REL-114: wrapped with context, never the params).
		return &deviceplane.ControllerError{
			Code:    codeTargetUnreachable,
			Message: fmt.Sprintf("dispatching %s to entity %q at %s: %v", command, entityID, target.addr(), err),
		}
	}
	defer func() {
		// M3: drain before Close so http.Client's Transport can return the
		// underlying TCP connection to its keep-alive pool for reuse by the
		// next Dispatch to this same device, instead of being forced to tear
		// it down. ECP responses are small, so the cost here is negligible;
		// discarding a body-read error is deliberate — Dispatch has already
		// captured everything it needs (the status code) by this point.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// M4: the device was reachable but refused/failed the command — not
		// really "an unclassified SERVER-side failure" in the sense
		// contracts/relay-1.md's Error taxonomy documents INTERNAL ("An
		// unclassified server-side failure"), but no taxonomy code names
		// "device reachable but rejected the command" today (a gap this
		// controller is the first real driver to hit — see also I2, a second
		// data point that the taxonomy may want a dedicated code here). Until
		// one exists, returning a plain (non-ControllerError) error is the
		// deliberate, tested choice (TestDispatchNon2xxResponse): the
		// device-command surface's own fallback buckets any plain error as
		// INTERNAL (deviceplane/command.go's codeInternal), which is the
		// closest existing fit, not a misclassification introduced here.
		return fmt.Errorf("ecp: %s to entity %q returned status %d", command, entityID, resp.StatusCode)
	}
	return nil
}

// ecpPath resolves command/params to the ECP request path REG-066's
// media-player command vocabulary defines, or a *deviceplane.ControllerError
// (COMMAND_UNRESOLVED) when command is not one this controller knows, or a
// required param is missing, not a string, empty, or (for power) not a
// recognized enum value. The device-command surface pre-validates a
// command's vocabulary membership before ever calling Dispatch (REL-113), but
// this controller still fails closed rather than trust that upstream check.
func ecpPath(command string, params map[string]any) (string, error) {
	switch command {
	case "launch":
		channel, err := requiredString(command, params, "channel")
		if err != nil {
			return "", err
		}
		return "/launch/" + url.PathEscape(channel), nil

	case "home":
		// REG-066: home takes no params — nothing to validate.
		return "/keypress/Home", nil

	case "keypress":
		key, err := requiredString(command, params, "key")
		if err != nil {
			return "", err
		}
		return "/keypress/" + url.PathEscape(key), nil

	case "power":
		state, err := requiredString(command, params, "state")
		if err != nil {
			return "", err
		}
		switch state {
		case "on":
			return "/keypress/PowerOn", nil
		case "off":
			return "/keypress/PowerOff", nil
		default:
			return "", &deviceplane.ControllerError{
				Code:    codeUnresolved,
				Message: fmt.Sprintf("power: state %q is not \"on\" or \"off\"", state),
			}
		}

	default:
		return "", &deviceplane.ControllerError{
			Code:    codeUnresolved,
			Message: fmt.Sprintf("%q is not a command this ECP controller dispatches", command),
		}
	}
}

// requiredString reads name out of params as a required, non-empty string
// param, or returns a COMMAND_UNRESOLVED *deviceplane.ControllerError
// describing why it could not: absent, the wrong Go type, or the empty
// string.
func requiredString(command string, params map[string]any, name string) (string, error) {
	raw, ok := params[name]
	if !ok {
		return "", &deviceplane.ControllerError{
			Code:    codeUnresolved,
			Message: fmt.Sprintf("%s: missing required param %q", command, name),
		}
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return "", &deviceplane.ControllerError{
			Code:    codeUnresolved,
			Message: fmt.Sprintf("%s: param %q must be a non-empty string", command, name),
		}
	}
	return s, nil
}
