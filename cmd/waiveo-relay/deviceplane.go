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
	"github.com/maaxton/waiveo-next/internal/shared/wire"
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

// ecpNameRank maps what the ECP probe read a device's name OUT OF onto the
// device plane's merge rank. It lives here, at the composition root, for the
// same reason the Identify closure does: internal/relay/discovery is the generic
// SSDP control point and must not know about one vendor's document, while
// internal/relay/ecp speaks that document and should not import the plane's
// merge policy to describe it. This is the one place that already knows both.
//
// The mapping is a JUDGEMENT and worth stating twice over.
//
// A Roku's `default-device-name` is a factory string ("onn•Roku TV -
// X029009JC6LF", read off 192.168.50.31), so it ranks BELOW a friendly mDNS
// instance name for the same device — a box nobody has renamed should show the
// name its AirPlay record announces, not its product label.
//
// `user-device-name` and `friendly-device-name` map to the SAME rank as a
// friendly mDNS label, NOT above it, and that is the correction of a real
// defect rather than a nicety. An owner-set name is unarguably the best
// statement about what a device is called — but this relay only ever hears it
// during an operator's `discovery.scan` (Discoverer.Run is passive-only), so a
// rank above mDNS would be one nothing could ever restate, and the candidate
// store remembers a rank until the process exits. 192.168.50.31 answers BOTH
// `<user-device-name>The Hanger</user-device-name>` over ECP and
// `_airplay._tcp | The Hanger` over mDNS: they are one fact through two records,
// and the record that repeats itself every 30 seconds is the one that has to be
// able to carry a rename. Ranking them equal makes recency decide between two
// statements of the same quality, which is exactly deviceplane.keepClass's rule
// for two specific classes.
func ecpNameRank(src ecp.NameSource) deviceplane.NameRank {
	switch src {
	case ecp.NameSourceUser, ecp.NameSourceFriendly:
		return deviceplane.NameRankFriendly
	case ecp.NameSourceDefault:
		return deviceplane.NameRankModel
	default:
		return deviceplane.NameRankNone
	}
}

// nameRankToken projects the plane's ordered merge rank onto REL-110c's wire
// token — the OTHER half of the same composition-root job ecpNameRank does, and
// it lives here beside it for the same reason: internal/shared/wire must not
// import a relay package, and internal/relay/deviceplane must not learn the
// contract's spelling of its own private ladder.
//
// The ordinal deliberately does not travel; see the token block in
// internal/shared/wire for why REL-004 forbids that. This is the ONE place the
// two vocabularies meet, so a rank added to the ladder without a token to carry
// it is a compile-visible gap here rather than a silent downgrade on the wire.
//
// The default arm is NameRankNone's own meaning, and it must stay a real token
// rather than the empty string: an ABSENT `name_rank` means "this relay does not
// rank names at all" and a consumer is required to read it that way (REL-110c),
// so a relay that DOES rank names must never report absence — it would be
// claiming to be an older peer than it is, and would hand its own report a
// licence to overwrite a better name it had just refused.
func nameRankToken(r deviceplane.NameRank) string {
	switch r {
	case deviceplane.NameRankFriendly:
		return wire.CandidateNameRankFriendly
	case deviceplane.NameRankModel:
		return wire.CandidateNameRankModel
	case deviceplane.NameRankMachine:
		return wire.CandidateNameRankMachine
	default:
		return wire.CandidateNameRankNone
	}
}
