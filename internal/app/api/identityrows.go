package api

import (
	"github.com/maaxton/waiveo-next/internal/app/store"
)

// identityrows.go mounts the two data-model/1 identity kinds — a screen's own
// identity row and a device row (DAT-004/DAT-004a/DAT-005b) — as ORDINARY api/1
// resource families, through the same generic resourceConfig every other
// authored kind goes through.
//
// Being ordinary is the entire point of this file's brevity. A screen and an
// adopted device are the two things the platform's whole purpose is expressed
// in terms of, and until now neither was addressable at all: `screen_programs`
// carried a `screen_id` no row anywhere defined, and the desired-state device
// inventory had no rows to compile from. Mounting them here — rather than as a
// bespoke surface — is what gives them, in one step and with no per-kind branch,
// the label selector, the keyset cursor scoped to their own resource type,
// If-Match/revision, `external_id` uniqueness under their placement, the
// visible-set read filter, placement authorization on writes, the audit record
// every mutation files, and the 422/400 validation split.
//
// # Why a screen row is not the `screen`-kind scope node
//
// DAT-004a settles this: a `screen_id` NAMES a screen identity row, and DAT-004
// lets that row sit under a scope node of ANY kind. So the two cannot be the
// same thing — two screens may hang off one `group`-kind node, and a screen may
// hang directly off a `site`, neither of which a one-node-per-screen reading can
// express. The `screen`-kind scope node stays what DAT-001 makes it: a placement
// classification, selectable as an intrinsic label (scopenodes.go), and nothing
// more.
//
// # Why a device row is authored here while /devices stays read-only
//
// The two answer different questions, and both answers are needed.
//
// `GET /devices` and `GET /entities` (devices.go) serve the relay's own LIVE
// report — what a relay currently sees on its LAN and each entity's last-reported
// state. That is genuinely a projection: relay/1 REL-110/111 make the relay's
// candidate report a full-set replace, so a durable copy of it on this side would
// be a second, staler authority for data the relay hands over again on every
// connection.
//
// An adopted-device row is the other direction of travel. REL-063 defines
// `device_inventory` as a section of the desired state the APP PEER signs and
// ships DOWN to a relay, listing adopted-device entries and, per entity, whether
// it is enabled, whether it is hidden, what it is called, and whether it is a
// primary or diagnostic surface. Not one of those is a fact a discovery sweep can
// report — they are decisions, and this row is the only place they can live.
// REL-153 then fixes the identity: a `device_id` is scoped to
// `(site, driver, native_id)` and "the app peer's own records reflect only which
// relay most recently reported it", which is both an acknowledgement that the app
// peer keeps records of its own and the reason those records are not keyed by a
// relay.
//
// So: the relay is authoritative for what EXISTS; this row is authoritative for
// what has been ADOPTED and under what policy. Neither supersedes the other, and
// the pipeline that turns a reported candidate into an adopted row — a separate
// concern — writes here and reads there.

// screensConfig is the resource configuration for the screen identity kind. Its
// own scope_node is its placement, its external_id grouping, and the node a write
// of it is authorized at — the same three-way coincidence every kind but a scope
// node has (scheduling.go explains why they are nonetheless separate projections).
func screensConfig() resourceConfig {
	return resourceConfig{
		kind:         store.KindScreen,
		path:         "screens",
		resourceType: "screens",
		displayName:  "screen",
		selLabels:    func(f resourceFields) map[string]string { return f.Labels },
		placement:    func(f resourceFields) string { return f.ScopeNode },
		extScope:     func(f resourceFields) string { return f.ScopeNode },
		writeScope:   func(f resourceFields) string { return f.ScopeNode },
	}
}

// adoptedDevicesConfig is the resource configuration for the device kind.
//
// The path is `adopted-devices` rather than `devices` because `devices` already
// names the relay's live read model on this surface (devices.go), and the two are
// different resources with different authorities — merging them would have to
// pick one, and either choice loses something real. Naming follows relay/1
// REL-063's own noun for these rows ("adopted-device entries") so the family and
// the desired-state section it compiles into read as the same thing.
func adoptedDevicesConfig() resourceConfig {
	return resourceConfig{
		kind:         store.KindAdoptedDevice,
		path:         "adopted-devices",
		resourceType: "adopted-devices",
		displayName:  "adopted device",
		selLabels:    func(f resourceFields) map[string]string { return f.Labels },
		placement:    func(f resourceFields) string { return f.ScopeNode },
		extScope:     func(f resourceFields) string { return f.ScopeNode },
		writeScope:   func(f resourceFields) string { return f.ScopeNode },
	}
}
