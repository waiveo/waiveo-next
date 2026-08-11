package store

import (
	"context"
	"database/sql"
	stdlog "log"

	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// priorfaults.go answers one question the post-write validation could not:
// WHICH of the faults in the row-set a write leaves behind did that write cause?
//
// validateAfterWrite (scheduling.go) judges a write by re-validating the whole
// row-set of the kind it touched. That is what makes a cross-row rule
// enforceable — a cast delete is refused while a playlist still names it — and
// it is also, unmodified, a rule that lets any row already in the store veto
// every future write of its kind.
//
// The store has already been in that state. Inline `source: "slide"` playlist
// items shipped with no authoring validation, so a layer stack the gate now
// rejects could be stored with a 201; the day the gate landed, each of those
// rows became a permanent veto. One of them refused an unrelated, valid playlist
// CREATE — naming an item index of the submitted body, which contained no slide.
// Two of them refused CREATE, UPDATE and DELETE alike, and with DELETE gone
// there is no repair path left through the API at all: recovery meant raw SQL
// against the database file.
//
// A validator added after the fact is a normal event — a contract clarifies, a
// projection's silent drop becomes an authoring refusal, a restore or a seed
// carries a bundle written by an older build (workspacerestore.go, seed.go).
// What must not be normal is that event bricking the surface. So:
//
//   - A WRITE IS JUDGED ON WHAT IT SUBMITS. capturePriorFaults records the fault
//     multiset of the pre-write state; introducedFaults subtracts it from the
//     post-write set; only the difference refuses the transaction. New breakage
//     is refused, inherited breakage is not attributed to a caller who did not
//     cause it, and DELETE — the repair path — is always available.
//
//   - AN INHERITED FAULT IS REPORTED, NEVER HIDDEN AND NEVER "REPAIRED". It is
//     readable on demand (StoredFaults), reported once at Open, and logged at
//     each write that steps over it. Quarantining the row (moving or deleting it
//     to satisfy a rule written after it was authored) was the alternative and it
//     is rejected: the row is an operator's content, the store cannot know what
//     they meant, and silently discarding it turns "your playlist has a bad
//     slide" into "your playlist is missing an item" — the failure this whole
//     track exists to stop.
//
// # The identity of a fault, and the one case the multiset cannot separate
//
// A datamodel.Error is three strings and is comparable, so the difference is a
// multiset difference keyed by the whole value. Field paths are row-relative
// (`items[1].slide.layers`), not row-qualified, so two different rows can produce
// byte-identical faults; counting rather than set-membership is what keeps a
// write that ADDS a second copy of an existing fault refused.
//
// The case it cannot separate: one transaction that simultaneously CLEARS a
// pre-existing fault and INTRODUCES a byte-identical one on another row leaves
// the count unchanged and is allowed. Reaching it requires a single write that
// both repairs one row and breaks another into exactly the same shape — the
// store's per-kind writes touch one row, so this needs a pack install or a
// restore, and the outcome is a store no worse off than before by this
// validator's own measure. Stated rather than papered over; the alternative is
// threading a row identity through every datamodel validator, which is a change
// to the shape of every error the API publishes.
type priorFaults struct {
	// counts is the pre-write fault multiset. A nil map means "nothing was
	// captured", which is NOT the same as "the store was clean": a zero
	// priorFaults subtracts nothing, so a caller that forgets to capture gets
	// the old, strictly harsher behaviour rather than a silently permissive one.
	counts map[datamodel.Error]int
	// captured distinguishes those two states, so reportStepover can stay quiet
	// about a diff nobody computed.
	captured bool
}

// capturePriorFaults records the faults the stored row-set of kind ALREADY
// carries, read through tx before that transaction's mutation is applied.
//
// It runs the validators EAGERLY rather than stashing the raw rows for a
// lazy pass. The cost is one extra decode+validate of one kind's rows per write,
// on a store whose row counts are an operator's authored content; the benefit is
// that "the state before" is captured as a value at the one instant it is true,
// with no second code path that could read it after the mutation and quietly
// measure the wrong thing.
func capturePriorFaults(ctx context.Context, tx *sql.Tx, kind Kind) (priorFaults, error) {
	errs, err := rowSetFaults(ctx, tx, kind)
	if err != nil {
		return priorFaults{}, err
	}
	counts := make(map[datamodel.Error]int, len(errs))
	for _, e := range errs {
		counts[e]++
	}
	return priorFaults{counts: counts, captured: true}, nil
}

// introducedFaults returns the faults in `after` that `prior` did not already
// account for, in `after`'s own order — the multiset difference described above.
//
// An uncaptured prior subtracts nothing: the write is then judged on the whole
// resulting set, which is the behaviour every call site had before this file
// existed. Failing in that direction is deliberate — a missed capture makes a
// write harder to accept, never easier.
func introducedFaults(after []datamodel.Error, prior priorFaults) []datamodel.Error {
	if len(after) == 0 || len(prior.counts) == 0 {
		return after
	}
	remaining := make(map[datamodel.Error]int, len(prior.counts))
	for e, n := range prior.counts {
		remaining[e] = n
	}
	out := make([]datamodel.Error, 0, len(after))
	for _, e := range after {
		if remaining[e] > 0 {
			remaining[e]--
			continue
		}
		out = append(out, e)
	}
	return out
}

// reportStepover logs, once per write, that a write was allowed to proceed over
// faults it did not cause — naming the kind, how many were inherited, and the
// first of them.
//
// This is the "report" half of the decision above, and it is the half that keeps
// the subtraction honest rather than merely convenient: without it, an inherited
// fault would become invisible the moment it stopped refusing anything, which is
// how a store drifts into a state nobody knows is broken. It is silent when the
// resulting set is clean, silent when nothing was captured, and silent when the
// write is being refused on its own faults (that refusal reaches the caller and
// says more than a log line would).
func (p priorFaults) reportStepover(kind Kind, resulting, introduced []datamodel.Error) {
	inherited := len(resulting) - len(introduced)
	if !p.captured || inherited <= 0 || len(introduced) > 0 {
		return
	}
	first := resulting[0]
	stdlog.Printf("store: %s write proceeded over %d pre-existing validation fault(s) this write did not cause "+
		"(first: field=%s code=%s: %s); they are not attributed to this caller, they still stand, and "+
		"StoredFaults lists them in full — fix or delete the offending row to clear them",
		kind, inherited, first.Field, first.Code, first.Message)
}

// StoredFaults reports every datamodel/1 validation fault the CURRENTLY STORED
// rows carry, across every kind this store validates — the read half of the
// "report, do not quarantine" decision at the top of this file.
//
// It is what makes an inherited fault fixable rather than merely tolerated: it
// names the field and the code of each one, which is what an operator (or an
// operator's support contact reading the boot log) needs to know which row to
// edit or delete. It takes the READ lock and opens no transaction: it is a
// diagnostic over a quiescent store, never a step inside a write.
//
// The kinds are walked in a fixed order so two readings of an unchanged store
// are byte-identical, and each kind's arm is the SAME rowSetFaults switch a write
// is judged by — a diagnostic that answered differently from the enforcement
// would be worse than none.
func (s *Store) StoredFaults(ctx context.Context) ([]datamodel.Error, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var out []datamodel.Error
	// One representative kind per VALIDATOR, not per table: rowSetFaults's
	// scheduling arm validates all seven scheduling tables together and its
	// identity arm both identity tables together, so listing every kind here
	// would report each fault as many times as its family has members.
	for _, kind := range []Kind{KindScopeNode, KindPlaylist, KindScreen, KindAutomation, KindWebhookEndpoint} {
		errs, err := rowSetFaults(ctx, tx, kind)
		if err != nil {
			return nil, err
		}
		out = append(out, errs...)
	}
	return out, nil
}

// reportStoredFaults logs the store's inherited faults once, at Open, so a
// deployment whose rows predate a validator learns it from its own boot log
// rather than from the first write that trips over one.
//
// A clean store logs nothing at all. A failure to READ the faults is logged and
// swallowed: this is a diagnostic, and a store that opens fine must not be
// refused because its self-report could not be produced.
func (s *Store) reportStoredFaults(ctx context.Context) {
	errs, err := s.StoredFaults(ctx)
	if err != nil {
		stdlog.Printf("store: could not read the stored-row fault report at open: %v", err)
		return
	}
	if len(errs) == 0 {
		return
	}
	stdlog.Printf("store: %d stored row(s) carry validation fault(s) that predate this build's validators; "+
		"they do not block unrelated writes, and each stands until its row is fixed or deleted through the API", len(errs))
	// Bounded. A store broken wholesale (a bad restore, a seed from a much older
	// build) can carry a fault per row, and a boot log that scrolls the rest of
	// startup off the screen to say so is a report nobody can use. StoredFaults
	// is the complete list and takes no lock a caller cannot take.
	const shown = 20
	for i, e := range errs {
		if i == shown {
			stdlog.Printf("store: … and %d more; StoredFaults returns the full list", len(errs)-shown)
			break
		}
		stdlog.Printf("store: stored-row fault: field=%s code=%s: %s", e.Field, e.Code, e.Message)
	}
}
