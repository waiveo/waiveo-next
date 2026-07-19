package datamodel

// This file implements the preset-firing layer of the scheduling core: when a
// scope node's effective daypart changes, its bound preset batch fires (DAT-075),
// the batch's recorded outcome vocabulary (DAT-092), and the effective misfire of
// a scheduling-core row by kind (DAT-076/121). A preset batch is a STATE-driven
// event: it fires on the rising edge of the node's effective-daypart IDENTITY, the
// desired-device-state counterpart to the Lease the same transition projects.
//
// The firing model is deliberately distinct from rules/1's one-shot triggers. A
// daypart is a state (holding.go, DAT-119): membership is decided per absolute
// instant by that instant's unambiguous local reading. The preset is the ONE event
// that state emits — on entry. Because only the highest-precedence holding daypart
// is effective (DAT-111), a masked holding-but-not-effective daypart emits nothing;
// and because identity, not wall-clock, keys the edge, a fall-back re-entry that
// changes the effective daypart re-fires with NO de-duplication by DST ambiguity
// (DAT-075), while a single window spanning a whole repeated hour keeps one
// identity and does not re-fire.

// PresetFire records a preset batch fired on a rising edge of effective-daypart
// identity (DAT-075): DaypartID is the effective daypart whose transition
// triggered it, PresetBatchID its bound preset_batch_id (the row a rules/1
// preset_batch action would resolve, RUL-170).
type PresetFire struct {
	DaypartID     string
	PresetBatchID string
}

// PresetTransition returns the preset batch to fire when a scope node's effective
// state advances from prev to cur, or nil when nothing fires (DAT-075/111). It
// fires exactly on a RISING EDGE of effective-daypart IDENTITY: cur has an
// effective daypart, that daypart binds a preset_batch_id, and its identity
// differs from prev's effective daypart (prev nil, or prev with no effective
// daypart, counts as the empty identity). Consequences, all by construction:
//
//   - Fall-through (a higher-precedence daypart ends and a lower shows through)
//     is an identity change and fires the newly-effective daypart's preset.
//   - A masked daypart never appears as cur.Daypart (Resolve surfaces only the
//     effective one), so a masked schedule's preset never fires.
//   - A fall-back re-entry that flips the effective daypart back to an earlier
//     identity is a genuine change and re-fires — no DST de-duplication (DAT-075).
//   - A single daypart whose window spans a whole fall-back repeated hour keeps
//     one identity across the repeat, so PresetTransition returns nil throughout
//     (DAT-119's spanning-window carve-out).
//   - A transition to no effective daypart (fallback/terminal/none) fires nothing.
func PresetTransition(prev, cur *EffectiveState) *PresetFire {
	curID := effectiveDaypartID(cur)
	if curID == "" {
		return nil // no effective daypart at cur — nothing to fire.
	}
	if curID == effectiveDaypartID(prev) {
		return nil // identity unchanged — not a rising edge.
	}
	batch := cur.Daypart.PresetBatchID
	if batch == "" {
		return nil // rising edge, but this daypart binds no preset batch.
	}
	return &PresetFire{DaypartID: curID, PresetBatchID: batch}
}

// effectiveDaypartID is the identity keying a rising edge: the effective daypart's
// id, or the empty string for a state with no effective daypart (nil state, or a
// fallback/terminal state).
func effectiveDaypartID(s *EffectiveState) string {
	if s == nil || s.Daypart == nil {
		return ""
	}
	return s.Daypart.ID
}

// BatchOutcome computes a preset batch's recorded invocation outcome (DAT-092)
// from its per-command results and the evaluation instant (Unix ms), in the
// identical three-value vocabulary and per-command result shape rules/1 RUL-172
// fixes: complete (every command ok), failed (none ok), partial (a mix). Every
// command is attempted independently — one failure neither halts the batch nor
// discards the successes — so the results carry the full per-command disposition.
// An empty result set is complete vacuously. results is retained, not copied.
func BatchOutcome(results []CommandResult, evaluatedAtMs int64) PresetBatchOutcome {
	return PresetBatchOutcome{Outcome: classifyOutcome(results), Results: results, EvaluatedAt: evaluatedAtMs}
}

// classifyOutcome maps a per-command result set to the RUL-172 three-value
// outcome: complete (no failures), failed (no successes), partial (a mix).
func classifyOutcome(results []CommandResult) string {
	anyOK, anyFail := false, false
	for _, r := range results {
		if r.OK {
			anyOK = true
		} else {
			anyFail = true
		}
	}
	switch {
	case anyOK && anyFail:
		return "partial"
	case anyFail:
		return "failed"
	default:
		return "complete"
	}
}

// EffectiveMisfire resolves a schedule's effective misfire (DAT-076/120): its own
// declared value, else catch_up_once — this contract's recurring-state default
// (DAT-121). It is deliberately distinct from rules/1's one-shot trigger default
// of skip (RUL-354): a schedule/daypart is a continuous state a relay re-resolves
// fresh on resume, not a queued one-shot occurrence, so catching up the CURRENT
// state once is the safe default; the rules/1 default is untouched by this row.
func (s Schedule) EffectiveMisfire() string {
	if s.Misfire != "" {
		return s.Misfire
	}
	return "catch_up_once"
}
