package schedule

import "sort"

// Fire is one dispatch a misfire policy resolves a batch of missed occurrences
// into (RUL-350–355). Wall is the absolute instant (Unix ms) the firing is
// attributed to — for `fire_each` the missed occurrence's own instant, for
// `catch_up_once` the most recent collapsed instant. MisfireCaught marks that the
// firing's origin was a caught-up misfire rather than a live observation, the
// orthogonal RunDisposition marker RUL-355 requires; it is always true on a Fire
// (a live occurrence is dispatched directly, not through ApplyMisfire).
type Fire struct {
	Wall          int64
	MisfireCaught bool
}

// Misfire policy strings (RUL-350). An absent declaration ("") defaults to skip
// (RUL-354); ValidMisfire treats only these three as declarable.
const (
	misfireSkip        = "skip"
	misfireCatchUpOnce = "catch_up_once"
	misfireFireEach    = "fire_each"
)

// ValidMisfire reports whether policy is one of the three declarable misfire
// values `skip`, `catch_up_once`, `fire_each` (RUL-350). An absent field ("")
// is NOT reported valid here — it is legal on the wire (defaulting to skip,
// RUL-354) but is not itself a declared value; a caller validating a PRESENT
// misfire declaration uses this to raise MISFIRE_INVALID, so it must guard on the
// field's presence separately.
func ValidMisfire(policy string) bool {
	switch policy {
	case misfireSkip, misfireCatchUpOnce, misfireFireEach:
		return true
	default:
		return false
	}
}

// ApplyMisfire resolves a set of missed occurrences (occurrences the engine could
// not evaluate at their scheduled instant — offline, mid-generation-swap, or
// clock untrusted, RUL-350/371) into the firings to dispatch on resume, per the
// trigger's declared policy:
//
//   - skip (and the "" default, RUL-351/354): none fire — a missed one-shot does
//     not fire late.
//   - catch_up_once (RUL-352): exactly one fire, collapsing any number of missed
//     occurrences into a single firing at the most recent missed instant, marked
//     misfire_caught.
//   - fire_each (RUL-353): one fire per missed occurrence, in ascending
//     chronological order, each marked misfire_caught — each independently subject
//     to full mode evaluation by the caller.
//
// An empty missed set never fires. An unrecognized policy fires nothing
// (fail-closed: compile rejects it as MISFIRE_INVALID before evaluation, and
// no-fire beats mis-fire, RUL-350). The input slice is not mutated.
func ApplyMisfire(policy string, missed []int64) []Fire {
	if len(missed) == 0 {
		return nil
	}
	switch policy {
	case misfireCatchUpOnce:
		latest := missed[0]
		for _, w := range missed[1:] {
			if w > latest {
				latest = w
			}
		}
		return []Fire{{Wall: latest, MisfireCaught: true}}
	case misfireFireEach:
		ordered := append([]int64(nil), missed...)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
		out := make([]Fire, 0, len(ordered))
		for _, w := range ordered {
			out = append(out, Fire{Wall: w, MisfireCaught: true})
		}
		return out
	default: // "", "skip", or an unrecognized policy: fail-closed to no-fire.
		return nil
	}
}
