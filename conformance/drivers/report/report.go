// Package report is the shared result type the executable conformance
// drivers (conformance/drivers/player1, conformance/drivers/relay1) record
// their per-case outcome into — the §10 differential oracle's ledger.
//
// A Report enumerates EVERY case a driver considered: the DRIVEN ones it
// actually exercised against the live stack (PASS or FAIL, with an
// expected-vs-actual diff on FAIL), and the PENDING ones it deliberately did
// NOT drive, each with an explicit reason. There is no silent-skip path
// (§10 "no silent caps"): a case a driver does not exercise is a PENDING row
// with a stated reason, never an absent one — so a reader sees exactly what
// is and isn't covered, and a future task that implements a feature is
// forced to move its case from PENDING to driven for the honesty check
// (Report.Pending vs an enumerated expected set) to keep passing.
package report

import (
	"fmt"
	"sort"
	"strings"
)

// Status is a single case's outcome.
type Status string

const (
	// PASS: the case was driven against the live stack and every observable
	// expected field matched.
	PASS Status = "PASS"
	// FAIL: the case was driven and at least one observable expected field
	// did not match — Case.Diffs carries the expected-vs-actual detail.
	FAIL Status = "FAIL"
	// PENDING: the case was deliberately NOT driven (its feature is not built
	// in this wave) — Case.Reason states why.
	PENDING Status = "PENDING"
)

// Diff is one expected-vs-actual mismatch on a FAILed case.
type Diff struct {
	Field    string
	Expected any
	Actual   any
}

func (d Diff) String() string {
	return fmt.Sprintf("%s: expected %v, got %v", d.Field, d.Expected, d.Actual)
}

// Case is one corpus case's recorded outcome.
type Case struct {
	CaseID   string
	Contract string
	Status   Status
	// Reason states, for a PENDING case, why it was not driven; for a FAIL
	// case, a short human summary (the structured detail is in Diffs).
	Reason string
	// Diffs carries the expected-vs-actual mismatches on a FAIL. Empty on
	// PASS and PENDING.
	Diffs []Diff
	// Notes records expected fields the live surface cannot observe, so the
	// case was driven for what IS observable and these were explicitly not
	// asserted (never faked to a pass). Present on PASS cases that assert a
	// subset of their corpus's expected block.
	Notes []string
}

// Report is a driver run's full ledger across all the cases it considered.
type Report struct {
	// Driver names the driver that produced this report (e.g. "player1").
	Driver string
	// Target names the implementation-under-test the driver drove (e.g.
	// "virtualplayer") — the pluggable §10 target.
	Target string
	Cases  []Case
}

// Add appends a fully-formed case result.
func (r *Report) Add(c Case) { r.Cases = append(r.Cases, c) }

// Pass records a driven case that matched every observable expected field.
func (r *Report) Pass(caseID, contract string, notes ...string) {
	r.Add(Case{CaseID: caseID, Contract: contract, Status: PASS, Notes: notes})
}

// Fail records a driven case that diverged from its corpus expectation.
func (r *Report) Fail(caseID, contract, reason string, diffs ...Diff) {
	r.Add(Case{CaseID: caseID, Contract: contract, Status: FAIL, Reason: reason, Diffs: diffs})
}

// Pending records a case the driver deliberately did not drive, with a
// mandatory reason (§10 "no silent caps").
func (r *Report) Pending(caseID, contract, reason string) {
	r.Add(Case{CaseID: caseID, Contract: contract, Status: PENDING, Reason: reason})
}

// byStatus returns the case IDs whose status is s, sorted.
func (r *Report) byStatus(s Status) []string {
	var ids []string
	for _, c := range r.Cases {
		if c.Status == s {
			ids = append(ids, c.CaseID)
		}
	}
	sort.Strings(ids)
	return ids
}

// Passed returns the sorted case IDs that PASSed.
func (r *Report) Passed() []string { return r.byStatus(PASS) }

// Failed returns the sorted case IDs that FAILed.
func (r *Report) Failed() []string { return r.byStatus(FAIL) }

// PendingIDs returns the sorted case IDs marked PENDING.
func (r *Report) PendingIDs() []string { return r.byStatus(PENDING) }

// Driven returns the sorted case IDs that were actually exercised (PASS or
// FAIL) — the complement of the PENDING set.
func (r *Report) Driven() []string {
	var ids []string
	for _, c := range r.Cases {
		if c.Status == PASS || c.Status == FAIL {
			ids = append(ids, c.CaseID)
		}
	}
	sort.Strings(ids)
	return ids
}

// OK reports whether every driven case PASSed (no FAILs). PENDING cases do
// not affect OK — they are, by construction, not-yet-driven.
func (r *Report) OK() bool { return len(r.Failed()) == 0 }

// String renders the full ledger — a human-readable audit of exactly what
// was driven, what passed, what failed with its diff, and what is pending
// and why.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "conformance driver %q against target %q:\n", r.Driver, r.Target)
	fmt.Fprintf(&b, "  driven: %d (%d PASS, %d FAIL), pending: %d\n",
		len(r.Driven()), len(r.Passed()), len(r.Failed()), len(r.PendingIDs()))
	for _, c := range r.Cases {
		fmt.Fprintf(&b, "  [%-7s] %s\n", c.Status, c.CaseID)
		if c.Reason != "" {
			fmt.Fprintf(&b, "            reason: %s\n", c.Reason)
		}
		for _, d := range c.Diffs {
			fmt.Fprintf(&b, "            diff:   %s\n", d)
		}
		for _, n := range c.Notes {
			fmt.Fprintf(&b, "            note:   %s\n", n)
		}
	}
	return b.String()
}
