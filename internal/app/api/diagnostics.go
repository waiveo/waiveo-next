package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/diskspace"
	"github.com/maaxton/waiveo-next/internal/app/platformlog"
	"github.com/maaxton/waiveo-next/internal/app/screens"
	"github.com/maaxton/waiveo-next/internal/app/store"
)

// diagnostics.go serves the two operator surfaces that answer "what is wrong
// with this box" without an SSH session (parity row 7.4, the legacy stack's
// `/logs` and `/system` pages):
//
//	GET /api/v1/platform-logs   — the running process's own log lines
//	GET /api/v1/system-health   — services, disk headroom, relays, screens
//
// Before this, the only health signal a deployment published was `/healthz`,
// which answers "this process is listening". That is the one failure mode
// nobody needs help diagnosing: if the box were not listening, the console
// would not have loaded.
//
// # Who may read them: `owner`, and why that is not over-cautious
//
// Both operations are workspace-wide and neither is scope-narrowable, so they
// take the SAME authorization the data-subject operations take
// (authorizeWorkspaceOwner) and for the first of that function's two published
// reasons: there is no node to narrow to, so admitting an `admin` bound at one
// site would hand a principal with authority over one site the whole
// deployment's diagnostics.
//
// And these really are confidential. A log line names relay ids, LAN addresses,
// screen ids, principal ids, pack names and file paths; a health summary names
// every connected relay's advertised address. That is a map of the deployment.
// The console's Activity page remains the scoped, per-event surface an admin
// reads — the audit trail, filtered by what they may see. This is the
// unfiltered process log, and it is the owner's.
//
// # What the logs surface is NOT
//
// It is not journald, and it does not pretend to be. See
// internal/app/platformlog's package doc for the full statement; the part that
// matters at this boundary is that the response PUBLISHES its own limits —
// `retained_from_ms` (how far back this buffer sees, which is never "since
// boot"), `dropped` (how much has already scrolled away), and `capacity` — so
// an operator reading a short list knows whether the box was quiet or the
// buffer wrapped. A diagnostics page that let someone conclude "no errors" from
// a buffer that had discarded them would be worse than no page.
//
// In production the feeder runs as a systemd unit and journald holds the same
// lines plus every previous boot's. Reading journald from this process is
// deliberately out of scope here: it would mean either shelling out to
// `journalctl` (a subprocess exec on the API path) or linking a native
// libsystemd reader, and neither belongs behind a first pass at the surface.
// What is missing as a result, stated so nobody has to discover it: lines from
// BEFORE this process started, and therefore the crash that caused the restart
// an operator is most likely investigating.

// PlatformLogSource is the seam the logs operation reads from.
// *platformlog.Buffer satisfies it directly via Read — the identical signature,
// no adapter — so this layer depends on the READ capability rather than the
// buffer's concrete type, and a deployment that wires none still serves the
// route (see WithPlatformLog).
type PlatformLogSource interface {
	Read(platformlog.Filter) platformlog.Snapshot
}

// WithPlatformLog wires the captured process log the logs operation serves.
//
// Optional, with the degrade-not-404 posture WithScreenStatus and
// WithDevicePlane take: without it the route still mounts, still authorizes,
// and answers an EMPTY page whose `capacity` is 0 — which is the true statement
// about a deployment that captures nothing, and is distinguishable from a
// wired-but-quiet box (capacity > 0, retained 0).
func WithPlatformLog(src PlatformLogSource) Option {
	return func(srv *server) { srv.platformLog = src }
}

// SystemHealthConfig is the deployment-shaped half of the health summary: the
// facts this process cannot derive from its own state.
type SystemHealthConfig struct {
	// StartedAtMs is when this process began serving, for the uptime line. A
	// health page's single most useful number when a screen went dark five
	// minutes ago is "this box restarted four minutes ago".
	StartedAtMs int64
	// Version identifies the running build. Empty renders as "unknown" rather
	// than as an empty string a reader would take for a rendering bug.
	Version string
	// DataDir is the directory whose FILESYSTEM the disk headroom is measured
	// on — the one holding the store, the content origin and the archives.
	// Empty means headroom is reported `unknown`, never zero (see
	// internal/app/diskspace: a zero would render as a full disk and
	// manufacture the emergency the check exists to detect).
	DataDir string
}

// WithSystemHealth wires the health summary's deployment facts. Without it the
// route still mounts and reports what it CAN observe — services, relays,
// screens — with uptime and disk `unknown`.
func WithSystemHealth(cfg SystemHealthConfig) Option {
	return func(srv *server) { srv.health = &cfg }
}

// mountDiagnostics registers both reads.
func (srv *server) mountDiagnostics(rt *router) {
	rt.HandleFunc("GET "+apiPrefix+"/platform-logs", srv.listPlatformLogs)
	rt.HandleFunc("GET "+apiPrefix+"/system-health", srv.getSystemHealth)
}

// ── platform-logs ───────────────────────────────────────────────────────────

// platformLogRecord is one line on the wire (openapi PlatformLogRecord).
type platformLogRecord struct {
	Seq  int64 `json:"seq"`
	TSMs int64 `json:"ts_ms"`
	// Level and Source are DERIVED from the line's text, not declared by
	// whoever wrote it — see internal/app/platformlog. `message` is the line
	// with the parts that became these two fields removed, and `raw` is the
	// whole line, so an operator is never shown a classification without the
	// text it was read out of.
	Level   string `json:"level"`
	Source  string `json:"source"`
	Message string `json:"message"`
	Raw     string `json:"raw"`
}

// platformLogPage is the response (openapi PlatformLogPage).
//
// It is deliberately NOT the standard `{items, next_cursor}` page. A keyset
// cursor names a position in a stable ordering, and this buffer's oldest end is
// being overwritten while a client reads it: a cursor into it would resolve to a
// record that no longer exists, and API-033's contract that a cursor names a
// position would be false. So the read is a bounded window over the newest
// matches, and the fields below publish enough for a client to know exactly what
// it is NOT being shown.
type platformLogPage struct {
	Items []platformLogRecord `json:"items"`
	// Matched is how many records matched the filter before `limit` cut the
	// window; Retained/Capacity/Dropped describe the buffer itself.
	Matched  int   `json:"matched"`
	Retained int   `json:"retained"`
	Capacity int   `json:"capacity"`
	Dropped  int64 `json:"dropped"`
	// RetainedFromMs is the oldest retained record's instant, or 0 when nothing
	// is retained: how far back this page can see, which is never "since boot".
	RetainedFromMs int64 `json:"retained_from_ms"`
	// Sources is every distinct source currently retained, sorted — the whole
	// set, NOT the filtered one. A filter control built from filtered results
	// can only ever offer the option already chosen, which is a dead end an
	// operator cannot get out of without editing the URL.
	Sources []string `json:"sources"`
	// LevelCounts is the retained count per level, unfiltered, so a header can
	// say "3 errors" while the page shows an info-only view.
	LevelCounts map[string]int `json:"level_counts"`
}

// maxPlatformLogLimit / defaultPlatformLogLimit bound one read.
const (
	defaultPlatformLogLimit = 200
	maxPlatformLogLimit     = 1000
)

// listPlatformLogs handles GET /api/v1/platform-logs (openapi listPlatformLogs).
func (srv *server) listPlatformLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := srv.authorizeWorkspaceOwner(w, r); !ok {
		return
	}
	q := r.URL.Query()

	filter := platformlog.Filter{
		Source:   strings.TrimSpace(q.Get("source")),
		Contains: strings.TrimSpace(q.Get("contains")),
		Limit:    defaultPlatformLogLimit,
	}
	// An unknown level is REFUSED, never silently matched-nothing. A filter that
	// answers "no logs" for a typo is read by an operator as "the box is quiet",
	// which is the opposite of the truth and the worst answer a diagnostics page
	// can give.
	if lv := strings.TrimSpace(q.Get("level")); lv != "" {
		if !platformlog.ValidLevel(lv) {
			writeProblem(w, r, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed",
				"The level parameter must be one of error, warn, info; got "+strconv.Quote(lv)+".")
			return
		}
		filter.Level = platformlog.Level(lv)
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxPlatformLogLimit {
			writeProblem(w, r, http.StatusBadRequest, "VALIDATION_FAILED", "Validation Failed",
				"The limit parameter must be an integer between 1 and "+strconv.Itoa(maxPlatformLogLimit)+"; got "+strconv.Quote(raw)+".")
			return
		}
		filter.Limit = n
	}

	page := platformLogPage{
		Items: []platformLogRecord{},
		// Never nil on the wire: `[]` is "no sources retained" and null would
		// make every consumer nil-check a list it can iterate.
		Sources:     []string{},
		LevelCounts: map[string]int{},
	}
	if srv.platformLog != nil {
		snap := srv.platformLog.Read(filter)
		for _, rec := range snap.Records {
			page.Items = append(page.Items, platformLogRecord{
				Seq: rec.Seq, TSMs: rec.TSMs,
				Level: string(rec.Level), Source: rec.Source,
				Message: rec.Message, Raw: rec.Raw,
			})
		}
		page.Matched = snap.Matched
		page.Retained = snap.Retained
		page.Capacity = snap.Capacity
		page.Dropped = snap.Dropped
		page.RetainedFromMs = snap.RetainedFromMs
		if snap.Sources != nil {
			page.Sources = snap.Sources
		}
		for lv, n := range snap.LevelCounts {
			page.LevelCounts[string(lv)] = n
		}
	} else {
		// An unwired deployment answers the same shape with a zero capacity —
		// distinguishable from a wired-but-quiet box, which reports a capacity.
		for _, lv := range platformlog.Levels {
			page.LevelCounts[string(lv)] = 0
		}
	}
	writeJSONValue(w, http.StatusOK, page)
}

// ── system-health ───────────────────────────────────────────────────────────

// healthStatus is the three-valued grade every component and the summary carry.
const (
	healthOK       = "ok"
	healthDegraded = "degraded"
	healthDown     = "down"
	healthUnknown  = "unknown"
)

// serviceHealth is one named component's grade (openapi ServiceHealth).
type serviceHealth struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Detail is always present and always says something an operator can act on
	// — including for `ok`, where "reachable, 42 rows" is the difference between
	// a check that ran and a check that was skipped.
	Detail string `json:"detail"`
}

// storageHealth is the disk-headroom half (openapi StorageHealth).
type storageHealth struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	// The three numbers are OMITTED, never zeroed, when the filesystem could not
	// be measured. `free_bytes: 0` on a page whose job is to warn about a full
	// disk reads as "the disk is full" — the check would manufacture the
	// emergency it exists to detect.
	TotalBytes  *int64   `json:"total_bytes,omitempty"`
	FreeBytes   *int64   `json:"free_bytes,omitempty"`
	UsedPercent *float64 `json:"used_percent,omitempty"`
	Detail      string   `json:"detail"`
}

// relayHealth is one relay's connectivity (openapi RelayHealth).
type relayHealth struct {
	RelayID string `json:"relay_id"`
	// Address is the canonical address the relay declared at hello (REL-037),
	// which is what a player is told to dial — so an operator comparing it with
	// what a screen is actually reaching is comparing the right two things.
	Address string `json:"address"`
	// ScreenCount is how many screens this relay is currently reporting on. A
	// connected relay reporting zero screens is a real and distinct failure from
	// a relay that is not connected at all.
	ScreenCount int `json:"screen_count"`
}

// screenHealth is the fleet roll-up (openapi ScreenHealth).
type screenHealth struct {
	Total int `json:"total"`
	Live  int `json:"live"`
	// Fetching is counted separately from both neighbours for the reason the
	// read model separates them: a screen mid content transfer is working (the
	// previous program is still on the wall) but unconfirmed, so folding it into
	// `live` would overstate the fleet and folding it into `stale` would send an
	// operator to look at a screen that is doing exactly what it should.
	Fetching int `json:"fetching"`
	// Rejected counts screens that are IN CONTACT and not taking what they are
	// handed. It is counted apart from `stale` because the two send an operator
	// to different places — one screen has gone quiet, the other is talking to
	// its relay constantly and refusing every program — and apart from `live`
	// because folding it there is exactly the roll-up that reported a healthy
	// fleet on 2026-08-11 while no screen on it had accepted anything for an
	// hour.
	Rejected   int `json:"rejected"`
	Stale      int `json:"stale"`
	NeverSeen  int `json:"never_seen"`
	Paired     int `json:"paired"`
	Overridden int `json:"overridden"`
	// LiveWindowMs is the threshold Live/Stale was decided by, republished here
	// for the same reason /screen-status publishes it: a roll-up without the
	// line it was drawn at is a number nobody can check.
	LiveWindowMs int64 `json:"live_window_ms"`
	// ContentTransferWindowMs is the AGE line Fetching/Stale was decided by, and
	// it was missing: this roll-up published a `fetching` COUNT and no way to
	// redraw the line it was counted at, so a consumer that wanted to treat those
	// screens as stale — which is a defensible reading, since nothing has been
	// heard back from them — had a number it could not reinterpret.
	ContentTransferWindowMs int64 `json:"content_transfer_window_ms"`
	// FetchingMaxUnackedPulls is the PROGRESS line, and it was the third of three
	// and the second to be left out. `fetching` is decided by both bounds — an
	// age and a count — and the count is the one that does the work the age bound
	// cannot (a screen that keeps pulling and never acknowledges never ages out).
	// Publishing the age and withholding the count restates the same defect one
	// line along: a reader with two of three thresholds believes they have the
	// whole rule and reproduces a different one.
	//
	// /screen-status publishes all three per row; this is the fleet summary of
	// the same judgement and must publish the same three.
	FetchingMaxUnackedPulls int64 `json:"fetching_max_unacked_pulls"`
}

// systemHealth is the whole response (openapi SystemHealth).
type systemHealth struct {
	// Status is the WORST grade any component below carries. It is derived, not
	// asserted: a summary that could read `ok` while a component read `down`
	// would be the one line an operator trusts and the one line that is wrong.
	Status      string `json:"status"`
	CheckedAtMs int64  `json:"checked_at_ms"`
	// StartedAtMs is THIS process instance's start instant, and it is the
	// restart operation's completion signal (API-153, restart.go): a client
	// holds the value the acceptance echoed and learns the restart finished when
	// a successful read here returns a DIFFERENT one.
	//
	// It is published ALONGSIDE UptimeMs rather than left to be derived from it,
	// because the derivation is not sound for that purpose. UptimeMs is
	// `checked_at_ms - started_at_ms` recomputed per request and clamped at zero,
	// so a clock stepping backwards — which an appliance's does before NTP
	// settles — makes it fall without any restart having happened, and a client
	// watching for a drop would report one that did not occur. This value is
	// captured once at boot and never recomputed, so it moves if and only if the
	// process did.
	//
	// -1 when the deployment wired no start time, matching the never-observed
	// sentinel /screen-status uses for an age nobody measured.
	StartedAtMs int64  `json:"started_at_ms"`
	UptimeMs    int64  `json:"uptime_ms"`
	Version     string `json:"version"`

	Services []serviceHealth `json:"services"`
	Storage  storageHealth   `json:"storage"`
	Relays   []relayHealth   `json:"relays"`
	Screens  screenHealth    `json:"screens"`
}

// getSystemHealth handles GET /api/v1/system-health (openapi getSystemHealth).
func (srv *server) getSystemHealth(w http.ResponseWriter, r *http.Request) {
	if _, ok := srv.authorizeWorkspaceOwner(w, r); !ok {
		return
	}
	out := systemHealth{
		CheckedAtMs: srv.nowMs(),
		StartedAtMs: srv.processStartedAtMs(),
		UptimeMs:    screens.NeverObserved,
		Version:     healthUnknown,
		Services:    []serviceHealth{},
		Relays:      []relayHealth{},
	}
	if srv.health != nil {
		if srv.health.Version != "" {
			out.Version = srv.health.Version
		}
		if srv.health.StartedAtMs > 0 {
			up := out.CheckedAtMs - srv.health.StartedAtMs
			if up < 0 {
				// The clock stepped backwards since boot, which an appliance's
				// does before NTP settles. Zero rather than a negative uptime,
				// which every renderer would print as nonsense.
				up = 0
			}
			out.UptimeMs = up
		}
	}

	out.Services = append(out.Services, srv.storeHealth(r))
	out.Services = append(out.Services, srv.contentHealth())
	relays, relaySvc := srv.relayHealth()
	out.Relays = relays
	out.Services = append(out.Services, relaySvc)
	screenRollup, screenSvc := srv.screenRollup(r)
	out.Screens = screenRollup
	out.Services = append(out.Services, screenSvc)
	out.Storage = srv.storageHealth()

	// The summary is the WORST component grade, with storage folded in on the
	// same ladder. Derived last, over everything above it, so a component added
	// tomorrow is included by construction rather than by remembering to widen a
	// hand-written expression.
	worst := healthOK
	for _, s := range out.Services {
		worst = worseHealth(worst, s.Status)
	}
	worst = worseHealth(worst, storageGrade(out.Storage.Status))
	out.Status = worst

	writeJSONValue(w, http.StatusOK, out)
}

// healthRank orders the grades so "worst wins" is a comparison rather than a
// nest of conditionals. `unknown` ranks ABOVE ok and below degraded: a check
// that could not run is not a passing check, and it is not an outage either.
var healthRank = map[string]int{healthOK: 0, healthUnknown: 1, healthDegraded: 2, healthDown: 3}

func worseHealth(a, b string) string {
	if healthRank[b] > healthRank[a] {
		return b
	}
	return a
}

// storageGrade maps diskspace's own vocabulary onto the health ladder. The two
// are kept distinct because they answer different questions — `critical` is a
// statement about bytes, `down` is a statement about a service — and collapsing
// them would make the disk's own status field unreadable.
func storageGrade(s string) string {
	switch s {
	case string(diskspace.StatusCritical):
		return healthDown
	case string(diskspace.StatusLow):
		return healthDegraded
	case string(diskspace.StatusUnknown):
		return healthUnknown
	default:
		return healthOK
	}
}

// storeHealth probes the relational store by performing a real read.
//
// A real read, not a handle check: the store handle is non-nil for the whole
// process lifetime, so a nil check would report `ok` against a database whose
// file had been deleted out from under it — which is precisely the state an
// operator would be trying to diagnose.
func (srv *server) storeHealth(r *http.Request) serviceHealth {
	rows, err := srv.store.List(r.Context(), store.KindScreen, store.ListFilter{})
	if err != nil {
		return serviceHealth{Name: "store", Status: healthDown, Detail: "The relational store could not be read: " + err.Error()}
	}
	return serviceHealth{Name: "store", Status: healthOK,
		Detail: "Readable; " + strconv.Itoa(len(rows)) + " authored screen row(s)."}
}

// contentHealth reports the content origin — the asset store every screen
// fetches its images and video from DIRECTLY (REL-140), so an origin that
// cannot be listed is a fleet showing nothing, whatever the schedule says.
func (srv *server) contentHealth() serviceHealth {
	if srv.content == nil {
		return serviceHealth{Name: "content-origin", Status: healthDown,
			Detail: "No content origin is configured; no screen can fetch an asset."}
	}
	// Entries() is a real enumeration of the origin directory, not a handle
	// check: the handle is non-nil for the whole process lifetime, so a nil
	// check would report `ok` for an origin whose directory had been unmounted.
	entries := srv.content.Entries()
	var total int64
	for _, e := range entries {
		total += int64(e.SizeBytes)
	}
	return serviceHealth{Name: "content-origin", Status: healthOK,
		Detail: strconv.Itoa(len(entries)) + " asset(s), " + humanBytes(total) + "."}
}

// relayHealth lists the connected relays and grades the relay plane.
//
// NO connected relay is `down`, not `degraded`, and that is the honest grade:
// the relays are the only path between this process and every screen and every
// device. With none connected, nothing this console does reaches hardware —
// a schedule change is written and never delivered.
func (srv *server) relayHealth() ([]relayHealth, serviceHealth) {
	out := []relayHealth{}
	if srv.pairingRelays.ConnectedRelays == nil {
		return out, serviceHealth{Name: "relay-plane", Status: healthUnknown,
			Detail: "This deployment publishes no relay directory, so relay connectivity cannot be reported."}
	}
	// Screens per relay, from the live status read model, so a connected relay
	// that is reporting nothing is visible as such.
	perRelay := map[string]int{}
	if srv.screenStatus != nil {
		for _, st := range srv.screenStatus.Statuses() {
			if st.RelayID != "" {
				perRelay[st.RelayID]++
			}
		}
	}
	for _, c := range srv.pairingRelays.ConnectedRelays() {
		out = append(out, relayHealth{RelayID: c.RelayID, Address: c.AdvertisedAddress, ScreenCount: perRelay[c.RelayID]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelayID < out[j].RelayID })

	if len(out) == 0 {
		return out, serviceHealth{Name: "relay-plane", Status: healthDown,
			Detail: "No relay is connected; nothing this console does can reach a screen or a device."}
	}
	return out, serviceHealth{Name: "relay-plane", Status: healthOK,
		Detail: strconv.Itoa(len(out)) + " relay(s) connected."}
}

// screenRollup counts the fleet by reachability and grades it.
//
// It is built from the SAME join /screen-status serves — authored rows filled in
// by relay reports — rather than from the reports alone, because a screen no
// relay has ever mentioned is the most alarming row on the page and a count
// built from reports is silent about exactly it.
func (srv *server) screenRollup(r *http.Request) (screenHealth, serviceHealth) {
	out := screenHealth{
		LiveWindowMs:            screens.LiveWindowMs,
		ContentTransferWindowMs: screens.ContentTransferWindowMs,
		FetchingMaxUnackedPulls: screens.MaxFetchingUnackedPulls,
	}
	rows, err := srv.store.List(r.Context(), store.KindScreen, store.ListFilter{})
	if err != nil {
		return out, serviceHealth{Name: "screens", Status: healthUnknown,
			Detail: "The authored screen rows could not be read, so the fleet cannot be counted."}
	}
	observed := map[string]screens.Status{}
	if srv.screenStatus != nil {
		for _, st := range srv.screenStatus.Statuses() {
			observed[st.ScreenID] = st
		}
	}
	now := srv.nowMs()
	for _, row := range rows {
		st := screenStatusRowOf(row, observed[row.ID], now)
		out.Total++
		switch st.Reachability {
		case string(screens.ReachabilityLive):
			out.Live++
		case string(screens.ReachabilityFetching):
			out.Fetching++
		case string(screens.ReachabilityRejected):
			out.Rejected++
		case string(screens.ReachabilityStale):
			out.Stale++
		case string(screens.ReachabilityNeverSeen):
			out.NeverSeen++
		default:
			// A grade this roll-up does not know. Counted as never-seen because
			// that is the bucket that claims the least: an unrecognised judgement
			// is not evidence of contact, and the grading below treats never-seen
			// as "waiting", never as "working". A new reachability value must be
			// given its own case here — reaching this line means the fleet page
			// is silently describing those screens as ones nobody has heard from.
			out.NeverSeen++
		}
		if st.Paired {
			out.Paired++
		}
		if st.Now != nil {
			out.Overridden++
		}
	}

	// The grading keeps NEVER-SEEN separate from STALE, which is the same
	// distinction the read model itself is built around (internal/app/screens):
	// a screen nobody has ever heard from is one that has not been switched on
	// yet, not one that broke. Collapsing them makes a freshly authored fleet —
	// the state every deployment passes through — greet its owner with a red
	// banner about screens that are merely waiting to be plugged in.
	switch {
	case out.Total == 0:
		// Nothing authored is not a fault. A fresh box has no screens and must
		// not greet its owner with a red banner either.
		return out, serviceHealth{Name: "screens", Status: healthOK, Detail: "No screens are authored yet."}
	case out.Live == out.Total:
		return out, serviceHealth{Name: "screens", Status: healthOK,
			Detail: "All " + strconv.Itoa(out.Total) + " screen(s) are live."}
	case out.Live == 0 && out.Stale == 0 && out.Fetching == 0 && out.Rejected == 0:
		// Every screen is never-seen: authored and waiting, not broken.
		//
		// `rejected` has to be excluded here or it lands in this clause by
		// omission — a fleet of screens ALL refusing their programs would be
		// described to their owner as never having been switched on, which is
		// both false and the opposite of actionable. Every state that means "we
		// have heard from this screen" belongs on this line.
		return out, serviceHealth{Name: "screens", Status: healthDegraded,
			Detail: "None of the " + strconv.Itoa(out.Total) + " authored screen(s) has ever been seen — pair them and switch them on."}
	case out.Live == 0 && out.Fetching == 0:
		// At least one screen HAS been seen and none is live or transferring.
		// That is a fleet that went dark, which is the case `down` exists for.
		// A FETCHING screen keeps it out of `down` on purpose: that screen is
		// still talking to its relay and still showing its previous program, so
		// "the fleet is dark" would be false.
		//
		// That last sentence is a DEPENDENCY on internal/app/screens, not an
		// observation, and it is the load-bearing consumer of the progress bound
		// added to `fetching` this round. While `fetching` merely meant "the last
		// pull is unacknowledged", the 2026-08 failure — every screen answering
		// its program pull and failing every content fetch — pinned every row at
		// `fetching` permanently, so this clause never fired and a whole dark
		// site read `degraded` forever. The alarm was off for the exact failure
		// it exists for. Anything that widens `fetching` again has to come back
		// here first: this grade is only honest while a fetching screen is one
		// that is genuinely getting somewhere.
		//
		// A REJECTING screen keeps the fleet in this grade rather than out of it,
		// which is the opposite of what a fetching one does and is deliberate: it
		// is talking to its relay, so it is not "quiet", but nothing the platform
		// sends it is being accepted, so the wall is as unreachable as a dark one.
		// The detail names both populations, because "have gone quiet" about a
		// fleet that is answering every poll sends an operator to the network when
		// the problem is the content.
		return out, serviceHealth{Name: "screens", Status: healthDown,
			Detail: strconv.Itoa(out.Stale) + " screen(s) were being seen and have gone quiet, " +
				strconv.Itoa(out.Rejected) + " are refusing the program they are handed; none of the " +
				strconv.Itoa(out.Total) + " authored screen(s) is live."}
	default:
		return out, serviceHealth{Name: "screens", Status: healthDegraded,
			Detail: strconv.Itoa(out.Total-out.Live) + " of " + strconv.Itoa(out.Total) +
				" screen(s) are not live (stale, fetching new content, refusing their program, or never seen)."}
	}
}

// humanBytes renders a byte count in the largest unit that keeps it readable.
//
// A `detail` string is rendered verbatim by the console, so "0 KiB" for two
// small assets and "39584301056 bytes" for a disk are both answers an operator
// has to decode rather than read.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	whole := n / div
	tenths := (n % div) * 10 / div
	return strconv.FormatInt(whole, 10) + "." + strconv.FormatInt(tenths, 10) + " " + [...]string{"KiB", "MiB", "GiB", "TiB"}[exp]
}

// archiveAdvice quantifies the half of the disk advice this platform can
// actually act on, or returns "" when there is nothing to say.
//
// The advice above says "prune old images and archives", and half of it was a
// dead end for as long as it has existed: images are the host's business, and
// nothing on this box could delete an archive at all. That half now has an
// operation (DELETE /workspace/archives/{name}) and a console control, and this
// sentence is what connects the grade to them — it names how much the backups
// are actually holding and where to go, so an operator reading a `low` disk does
// not have to guess whether archives are even part of their problem.
//
// It is deliberately SILENT when there are no containers. "0 backups are using
// 0 B" on a box with none would send an operator to a page that cannot help
// them, which is the same defect as the advice it replaces, one step along.
func (srv *server) archiveAdvice() string {
	if srv.workspaceArchive == nil {
		return ""
	}
	count, bytes := archiveFootprint(srv.workspaceArchive.Dir)
	if count == 0 {
		return ""
	}
	noun := " backups are"
	if count == 1 {
		noun = " backup is"
	}
	return " " + strconv.Itoa(count) + noun + " using " + humanBytes(bytes) +
		" here; delete the ones you have already copied off this box on the Backup page."
}

// storageHealth measures the filesystem the workspace's own data lives on.
func (srv *server) storageHealth() storageHealth {
	if srv.health == nil || srv.health.DataDir == "" {
		return storageHealth{Path: "", Status: string(diskspace.StatusUnknown),
			Detail: "This deployment does not publish a data directory, so disk headroom cannot be measured."}
	}
	path := srv.health.DataDir
	u, err := diskspace.Of(path)
	if err != nil {
		return storageHealth{Path: path, Status: string(diskspace.StatusUnknown),
			Detail: "The filesystem could not be measured: " + err.Error()}
	}
	total, free := u.TotalBytes, u.AvailBytes
	pct := u.UsedPercent()
	grade := u.Classify()
	detail := humanBytes(free) + " available of " + humanBytes(total) + "."
	switch grade {
	case diskspace.StatusCritical:
		detail += " Ordinary operation (an upload, a snapshot, an export) can now fail." + srv.archiveAdvice()
	case diskspace.StatusLow:
		detail += " An image deploy is getting tight; prune old images and archives." + srv.archiveAdvice()
	}
	return storageHealth{Path: path, Status: string(grade),
		TotalBytes: &total, FreeBytes: &free, UsedPercent: &pct, Detail: detail}
}
