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
//
// A desired-state generation apply is not a clock tick, and MayCarryAcrossApply
// is where that is decided: a consumer already resolving the node carries its
// baseline across the apply — so a generation that authored no scheduling row
// re-asserts nothing — for exactly as long as the rows a fire would assert are
// unchanged (DAT-075).

import "reflect"

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

// MayCarryAcrossApply reports whether a consumer that was ALREADY resolving a
// scope node continuously may carry prev — the rising-edge baseline it had
// reached — across an apply of the desired-state generation applied, instead of
// starting from no baseline (DAT-075).
//
// prevBatch is the preset-batch row prev's effective daypart bound in the
// OUTGOING generation, which the consumer must hand over with prev: applied
// carries the new row, and the old one is otherwise unrecoverable. It is nil
// when prev had no effective daypart or that daypart bound no batch.
//
// # What the carry is for, and the mirror it must not become
//
// DAT-075's resume edge names boot, an apply that BEGINS resolving a node, and
// clock-trust resume — each a discontinuity in the consumer's ability to
// observe, where it may have MISSED an edge and `misfire` says what to do about
// the one it missed. A consumer resolving continuously has missed nothing, so a
// re-apply over it is not a resume: without the carry, the currently effective
// daypart reads as a rising edge on every apply, and a generation advanced for
// reasons that author no scheduling row at all (a content-URL re-mint) re-asserts
// device state at instants nobody scheduled.
//
// That argument reaches exactly as far as "the desired state did not change",
// which is why this is not keyed on effective-daypart identity alone. On the
// ordinary 24/7 shape — one all-day daypart — the identity never changes again
// after boot, so an identity-keyed carry also swallows an operator's edit to
// that daypart's bound batch, with no later edge left for it to ride. So the
// carry holds only while the rows a fire would ASSERT are unchanged: prev's own
// daypart row, and the preset-batch row it binds. An edit anywhere else in the
// section (a playlist's items, a slide's text) leaves both untouched and is
// still carried, so an unrelated edit never re-asserts device state either.
//
// Re-application itself is not the hazard — at a genuine rising edge a batch is
// safely re-applied, DST-repeated hour included (DAT-075/119). What this refuses
// is manufacturing an edge where neither the clock nor the desired state moved.
//
// A prev with no effective daypart is always carryable: it binds no batch, and
// PresetTransition reads it as the same empty identity a nil baseline gives.
func MayCarryAcrossApply(prev *EffectiveState, prevBatch *PresetBatch, applied RowStore) bool {
	if prev == nil {
		return false // nothing to carry: the consumer starts from no baseline.
	}
	if prev.Daypart == nil {
		return true
	}
	cur := applied.daypart(prev.Daypart.ID)
	if cur == nil || !sameDaypart(prev.Daypart, cur) {
		return false
	}
	if cur.PresetBatchID == "" {
		return true // binds no batch: no device state to have changed.
	}
	return samePresetBatch(prevBatch, applied.presetBatch(cur.PresetBatchID))
}

// sameDaypart reports whether two daypart rows state the same desired state, and
// samePresetBatch the same for preset-batch rows.
//
// Both compare the WHOLE row structurally rather than a chosen list of fields,
// so a field added to either row is compared from the day it exists — the
// opposite failure mode from an identity key, which goes silently stale against
// everything it does not name. What is excluded is exactly what is not a
// STATEMENT of desired state: `last_outcome`, which RECORDS an invocation
// (DAT-092) rather than instructing one, and the store's own baseline
// bookkeeping (`revision`, `created_at`, `updated_at`, DAT-005), stamped by
// whatever wrote the row rather than authored into it.
//
// The bookkeeping exclusion is what makes the `last_outcome` one real. DAT-092
// REQUIRES a preset-batch row's `last_outcome` be recorded once the batch has
// been invoked, and the write that records it stamps a fresh `revision` and
// `updated_at` in the same breath. Comparing those would make every invocation
// an authored change and therefore the cause of the next invocation — a
// device-command loop running at generation cadence, on hardware. Excluding
// `last_outcome` by itself would not have stopped it.
//
// Nothing is lost by it: a row whose desired state changed changed one of the
// fields that expresses it, and a row that only got re-stamped desires exactly
// what it desired before.
func sameDaypart(prev, cur *Daypart) bool {
	if prev == nil || cur == nil {
		return prev == nil && cur == nil
	}
	a, b := *prev, *cur
	a.Revision, a.CreatedAt, a.UpdatedAt = 0, 0, 0
	b.Revision, b.CreatedAt, b.UpdatedAt = 0, 0, 0
	return reflect.DeepEqual(a, b)
}

func samePresetBatch(prev, cur *PresetBatch) bool {
	if prev == nil || cur == nil {
		return prev == nil && cur == nil
	}
	a, b := *prev, *cur
	a.LastOutcome, a.Revision, a.CreatedAt, a.UpdatedAt = nil, 0, 0, 0
	b.LastOutcome, b.Revision, b.CreatedAt, b.UpdatedAt = nil, 0, 0, 0
	return reflect.DeepEqual(a, b)
}

// daypart and presetBatch are the referential lookups MayCarryAcrossApply
// compares a carried baseline's rows against: the row of that id in this store,
// or nil when it carries none (a row the applied generation dropped).
func (s RowStore) daypart(daypartID string) *Daypart {
	for i := range s.Rows.Dayparts {
		if s.Rows.Dayparts[i].ID == daypartID {
			return &s.Rows.Dayparts[i]
		}
	}
	return nil
}

func (s RowStore) presetBatch(presetID string) *PresetBatch {
	for i := range s.Rows.PresetBatches {
		if s.Rows.PresetBatches[i].PresetID == presetID {
			return &s.Rows.PresetBatches[i]
		}
	}
	return nil
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
