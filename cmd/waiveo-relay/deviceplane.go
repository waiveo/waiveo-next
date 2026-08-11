// deviceplane.go is the relay binary's device-control wiring: the adoption
// gate, the ECP controller that dispatches through it, and the entity resolver
// both dispatch paths share.
//
// It is a file rather than thirty lines inside run() because of what those
// thirty lines decide. They decide which physical devices on a shared LAN this
// process is willing to command — and during coexistence with the legacy stack,
// getting that wrong does not fail loudly, it produces two controllers fighting
// over one TV. Wiring a decision like that inside a boot function makes it
// unreachable from a test; here, the exact object graph the binary runs is the
// one a test builds (devicecommand_test.go).
package main

import (
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/devicetargets"
	"github.com/maaxton/waiveo-next/internal/relay/ecp"
)

// devicePlane is the relay's assembled device-control stack.
//
// base and controller are the same adapter twice: base is the bare ECP
// controller, controller is base wrapped in the operator-visible dispatch log.
// Both are kept because the keep-alive path wraps base in its OWN log with a
// different source label (main.go), and wrapping the already-wrapped one would
// log every keep-alive command twice under two different sources.
type devicePlane struct {
	targets    *devicetargets.Registry
	base       deviceplane.DeviceController
	controller deviceplane.DeviceController
	resolve    deviceplane.EntityResolver
}

// newDevicePlane builds the device plane over the deployment's ECP overrides
// (WAIVEO_RELAY_ECP_TARGETS, may be empty) and the relay's own candidate store.
//
// The controller is the REAL ECP adapter ALWAYS. It used to be installed only
// when the override env var was set, and the default deployment got a loopback
// stand-in that refused every command — so a relay that had discovered a Roku,
// had it adopted, and was holding a live connection to its app peer still
// answered every operator command "this relay has no device adapter
// configured". Making the adapter unconditional is only safe because its
// targets are gated: it reads them live from the adoption gate on every
// dispatch, and an entity the gate does not resolve is refused
// COMMAND_UNRESOLVED by the adapter itself — the same honest refusal, now on a
// path that can also succeed.
func newDevicePlane(overrides map[string]ecp.Target, candStore *deviceplane.Store) *devicePlane {
	gate := make(map[string]devicetargets.Endpoint, len(overrides))
	for entityID, t := range overrides {
		gate[entityID] = devicetargets.Endpoint{Host: t.Host, Port: t.Port}
	}
	targets := devicetargets.New(gate, candStore)

	base := deviceplane.DeviceController(ecp.New(nil, ecp.WithTargetSource(
		func(entityID string) (ecp.Target, bool) {
			ep, ok := targets.Target(entityID)
			if !ok {
				return ecp.Target{}, false
			}
			return ecp.Target{Host: ep.Host, Port: ep.Port}, true
		})))

	// Resolution is tried in order of authority.
	//
	// The candidate store comes FIRST: it is what this relay actually observed,
	// and REL-110b deliberately admits a command against a
	// discovered-but-unadopted entity so an operator can wake or blink a device
	// while deciding whether to adopt it. Such a command resolves here and is
	// then refused by the controller for having no target — the correct
	// two-stage answer (the entity is real; this relay may not drive it), and
	// distinct from the "no such entity" a genuinely unknown id draws.
	//
	// The adopted inventory comes second: it is what keeps an adopted device
	// addressable across a discovery gap, and its device_id is the app peer's
	// own row id rather than a locally derived one.
	//
	// The override bridge is LAST. It is a deployment assertion about one id and
	// must never answer for a device this relay genuinely knows about — a
	// discovered device having its command silently attributed to an unrelated
	// configured entry is exactly the confusion the ordering prevents.
	resolve := deviceplane.EntityResolver(func(entityID string) (string, string, bool) {
		if deviceID, deviceClass, ok := candStore.ResolveEntity(entityID); ok {
			return deviceID, deviceClass, true
		}
		if deviceID, deviceClass, ok := targets.ResolveEntity(entityID); ok {
			return deviceID, deviceClass, true
		}
		if _, present := overrides[entityID]; present {
			// A configured target IS an adoption decision someone typed into
			// this relay's own configuration (devicetargets' package doc), so it
			// resolves with the entity standing in for its own device id.
			return entityID, mediaPlayerClass, true
		}
		return "", "", false
	})

	return &devicePlane{
		targets:    targets,
		base:       base,
		controller: loggingController{inner: base, source: "automation"},
		resolve:    resolve,
	}
}
