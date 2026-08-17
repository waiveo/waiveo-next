// discoveryscanstatus.go carries `discovery.scan_status` — the relay's
// unsolicited upward report of what its scan engine is doing.
//
// # Why a report rather than a reply
//
// `discovery.scan` answers immediately with an ACCEPTANCE (discoveryscan.go):
// a scan probes a whole segment and outlives the request/response exchange that
// started it, so the reply cannot carry the outcome. That leaves an operator
// with a scan id and no way to tell "still sweeping" from "finished" from
// "died" — the exact ambiguity the passive lanes' own honesty rules exist to
// prevent elsewhere in this contract.
//
// So scan state travels the same way screen status does (screenstatus.go): the
// relay pushes an unsolicited frame whenever the state changes, the app peer
// holds the latest per relay, and the console reads that. Nothing polls a relay
// for it, and a relay that never scans simply never reports.
//
// # Why the app's view of a scan can be stale, and why that is honest
//
// A relay that dies mid-scan sends no completion, so the app's last-known state
// stays `scanning` until that relay reconnects and reports again. That is not a
// bug to paper over with a timeout: the app genuinely does not know whether the
// scan finished, and the connected-relay set (`/discovery/relays`) already tells
// an operator whether the relay is there at all. A fabricated "probably done"
// would be the same lie an empty device list tells when nothing is discovering.
package wire

// Frame type discriminator for the upward scan-state report. A relay that never
// sends it is not an error — an app peer that receives an unknown frame ignores
// it under REL-004 additive tolerance, and one that receives none simply has no
// scan state to show.
const FrameTypeDiscoveryScanStatus = "discovery.scan_status"

// Scan states. `idle` covers both "never scanned" and "finished": the two are
// told apart by FinishedAt, not by a third state, so a console never has to
// decide what an unfamiliar state string means.
const (
	DiscoveryScanStateIdle     = "idle"
	DiscoveryScanStateScanning = "scanning"
)

// DiscoveryScanStatusBody is `discovery.scan_status`'s body: the relay's current
// scan state and what the last run did.
//
// The counts are deliberately of CANDIDATES the relay knows, not of "new
// devices": a scan that finds nothing new is a successful scan of a network that
// has not changed, and reporting it as zero-anything would read as a failure.
type DiscoveryScanStatusBody struct {
	// State is DiscoveryScanStateScanning or DiscoveryScanStateIdle.
	State string `json:"state"`
	// ScanID correlates with the id `discovery.scan_result` returned. Present
	// while scanning and retained afterwards, so an operator can tell WHICH scan
	// the last result describes.
	ScanID string `json:"scan_id,omitempty"`
	// StartedAt / FinishedAt are epoch ms. FinishedAt is zero while a scan runs
	// and while no scan has ever run — the pair is what distinguishes "never
	// scanned" from "finished", so neither is omitted once set.
	StartedAt  int64 `json:"started_at,omitempty"`
	FinishedAt int64 `json:"finished_at,omitempty"`
	// Candidates is how many candidates the relay holds now (its whole current
	// view, the same set `device.candidates` reports), so a completed scan can
	// say what the network looks like without the console joining two reports.
	Candidates int `json:"candidates"`
	// Error, when non-empty, is why the last scan ended badly. A scan that ran
	// and found nothing leaves this empty — "nothing found" is a result, not an
	// error, and conflating them is what makes an operator distrust the surface.
	Error string `json:"error,omitempty"`
}
