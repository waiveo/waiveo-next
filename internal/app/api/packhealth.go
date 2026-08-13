package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
)

// Pack health: an extension reporting its own condition into the summary an
// operator already reads.
//
// The host can observe that a pack's PROCESS is alive — the supervisor does, and
// records it when one dies. It cannot observe whether the pack is working: a
// backup extension whose credentials expired is running perfectly and doing
// nothing. Only the pack knows that, so only the pack can say it.
//
// # Why a report goes STALE
//
// A report that never expires is worse than no report. A pack that reported `ok`
// and then wedged keeps a green line on the health page forever, and the page an
// operator opens to find out what is wrong actively tells them nothing is. So a
// report carries the instant it arrived, and a stale one degrades to `unknown` —
// which is the true statement: we were told once, and we have not been told
// since.
//
// `unknown` rather than `down` deliberately. A missed report is evidence about
// the REPORTING, not about the pack: a busy extension that skipped a beat has
// not failed, and grading it down would train an operator to ignore the colour.

// packHealthTTLMs is how long a report stays current. Comfortably longer than
// any sane reporting interval, so an extension that reports every minute is
// never marked stale by ordinary jitter.
const packHealthTTLMs int64 = 5 * 60_000

// packHealthReport is one extension's last word about itself.
type packHealthReport struct {
	Status     string
	Detail     string
	ReportedAt int64
}

// packHealthRegistry holds the latest report per pack.
//
// In memory, and deliberately not persisted: a report describes a RUNNING
// process, and a report that survived a restart would describe a process that no
// longer exists. After a restart nothing has reported yet, which is exactly what
// an empty registry says.
type packHealthRegistry struct {
	mu      sync.Mutex
	reports map[string]packHealthReport
}

func newPackHealthRegistry() *packHealthRegistry {
	return &packHealthRegistry{reports: map[string]packHealthReport{}}
}

func (r *packHealthRegistry) put(packID string, rep packHealthReport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports[packID] = rep
}

// snapshot renders every report as a service line, oldest-reporting packs
// included, sorted by pack id so a health page does not reshuffle between reads.
func (r *packHealthRegistry) snapshot(nowMs int64) []serviceHealth {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]serviceHealth, 0, len(r.reports))
	for id, rep := range r.reports {
		status, detail := rep.Status, rep.Detail
		if nowMs-rep.ReportedAt > packHealthTTLMs {
			// Said in the detail, not just in the colour: an operator seeing
			// `unknown` needs to know it is silence rather than a pack that
			// reported uncertainty about itself.
			status = healthUnknown
			detail = "no health report since this extension last checked in; the process may be wedged or gone"
		}
		out = append(out, serviceHealth{Name: packLogSource(id), Status: status, Detail: detail})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// validPackHealthStatus is the closed set an extension may report.
//
// Closed because these values drive the page's overall grade through
// healthRank: a pack inventing "mostly-fine" would land in a rank lookup that
// does not have it, and an unranked status silently counts as the BEST one —
// so an extension could report its own failure in a way that improves the
// summary.
func validPackHealthStatus(s string) bool {
	switch s {
	case healthOK, healthDegraded, healthDown:
		return true
	}
	return false
}

// reportPackHealth records the calling extension's own condition.
func (srv *server) reportPackHealth(w http.ResponseWriter, r *http.Request) {
	packID, ok := srv.callerPackID(w, r)
	if !ok {
		return
	}
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	// The declared schema owns `status`'s enum and `detail`'s presence. They were
	// restated in Go below and are not any more: two statements of one rule drift,
	// and this package has already been bitten by exactly that (a ttl_seconds
	// minimum the document declared and the handler did not enforce).
	if srv.schemaRejected(w, r, "PackHealthReportRequest", raw) {
		return
	}
	var body struct {
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		srv.packProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"The request body could not be parsed.")
		return
	}
	// Belt to the schema's braces, and NOT a restatement of the enum: this asserts
	// the document and the rank table agree, which no schema can. An unranked
	// status counts as the best one (healthRank returns the zero value), so a
	// status the document admitted and the page cannot rank would let an
	// extension report failure in a way that improves the summary.
	if !validPackHealthStatus(body.Status) {
		srv.packProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed",
			"`status` is not one this deployment can grade.")
		return
	}

	srv.packHealth.put(packID, packHealthReport{
		Status: body.Status, Detail: body.Detail, ReportedAt: srv.nowMs(),
	})
	w.WriteHeader(http.StatusNoContent)
}
