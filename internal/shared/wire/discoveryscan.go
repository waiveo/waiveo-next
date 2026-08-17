// discoveryscan.go carries the app-peer→relay `discovery.scan` command and its
// correlated `discovery.scan_result` reply — the frame that makes an ACTIVE
// network scan an explicit operator action rather than something the relay does
// on its own.
//
// # Why this frame exists (the owner's three modes)
//
// Discovery runs in exactly three modes (Discovery spec §4, owner 2026-08-17):
//
//   - PASSIVE, always on: read the relay host's own kernel neighbour table and
//     the host avahi cache, and LISTEN for SSDP NOTIFY. These originate nothing
//     on the wire, so they need no permission and no trigger.
//   - ACTIVE SCAN, on demand: everything that PROBES — the SSDP M-SEARCH
//     multicast query, the per-device identity probe, and (later) the ARP sweep,
//     port scan and banner probe. These run ONLY when an operator asks, and only
//     across what the relay itself can reach.
//   - SCHEDULED: the same active scan on an operator-set schedule.
//
// This is the trigger for the second mode. It is a COMMAND rather than a field
// of the signed desired state on purpose: a scan is a transient action with a
// result, not durable configuration. The durable half — which subnets are
// scannable, the schedule, the rate budget — belongs to desired state, and this
// frame carries only "scan now, optionally scoped to this subnet".
//
// The reply is deliberately an ACCEPTANCE, not a set of findings: a scan takes
// far longer than a request/response exchange should hold a connection open, and
// its findings already have a path upward — the relay reports them through the
// ordinary `device.candidates` report (REL-110/111) exactly as passive sightings
// arrive. So the result says whether the scan STARTED (and refuses if one is
// already running, or if scanning is not enabled for the requested scope), and
// progress is read from the relay's scan status, never from this reply.
package wire

// Frame type discriminators for the on-demand scan exchange. A relay that does
// not implement them ignores an unknown server-initiated frame under REL-004
// additive tolerance, so introducing the pair cannot break an older relay — it
// simply never scans.
const (
	FrameTypeDiscoveryScan       = "discovery.scan"
	FrameTypeDiscoveryScanResult = "discovery.scan_result"
)

// DiscoveryScanBody is `discovery.scan`'s body: an optional scope and nothing
// else. Absent members mean "the relay's own default scope", which is the only
// scope a relay can honour without being told about a network it cannot see.
//
// Subnet, when set, is a CIDR the operator has enabled for scanning. It is
// REQUESTED, never trusted: the relay refuses a subnet that is not in the
// policy its signed desired state carries, and refuses one that is not on an
// interface it actually has — a scan is bounded by what the relay can reach, and
// an app peer naming an arbitrary CIDR must not be able to point a relay's
// probes at a network the operator never enabled.
//
// Lanes, when non-empty, restricts the scan to named active lanes (for example
// only the SSDP M-SEARCH sweep, without a port scan). An unknown lane name is
// ignored rather than refused, under the same additive tolerance as the frame
// itself: a newer app peer naming a lane this relay has not built yet still gets
// the lanes it does have.
type DiscoveryScanBody struct {
	Subnet string   `json:"subnet,omitempty"`
	Lanes  []string `json:"lanes,omitempty"`
}

// DiscoveryScanResultBody is `discovery.scan_result`'s body: whether the scan
// was ACCEPTED and has started, not what it found.
//
// Started distinguishes "this call is what began a scan" from an accepted no-op,
// exactly as the adopt/ignore operations distinguish created-vs-already. ScanID
// correlates the run with the status the relay reports upward while it runs.
type DiscoveryScanResultBody struct {
	OK      bool              `json:"ok"`
	Started bool              `json:"started"`
	ScanID  string            `json:"scan_id,omitempty"`
	Error   *CommandErrorBody `json:"error,omitempty"`
}

// NewDiscoveryScanAccepted builds the accepted result for a scan that has just
// begun under scanID.
func NewDiscoveryScanAccepted(scanID string) DiscoveryScanResultBody {
	return DiscoveryScanResultBody{OK: true, Started: true, ScanID: scanID}
}

// NewDiscoveryScanBusy builds the accepted-but-no-op result for a request that
// arrived while scanID was already running. It is deliberately OK: asking a
// scanning relay to scan is a benign repeat (an operator double-click, a retried
// request), not an error, and reporting it as one would make a harmless race
// look like a failure.
func NewDiscoveryScanBusy(scanID string) DiscoveryScanResultBody {
	return DiscoveryScanResultBody{OK: true, Started: false, ScanID: scanID}
}

// NewDiscoveryScanError builds a refused result carrying an Error-taxonomy code
// — the one constructor a refusal is built through, so an `ok:false` result can
// never be emitted with an absent `error` object.
func NewDiscoveryScanError(code, message string) DiscoveryScanResultBody {
	return DiscoveryScanResultBody{
		OK:    false,
		Error: &CommandErrorBody{Code: code, Message: message},
	}
}
