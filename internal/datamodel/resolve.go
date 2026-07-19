package datamodel

import (
	"sort"
	"time"
)

// This file implements the per-instant, cross-schedule resolution engine
// (DAT-051/052/053/111/117/118): given the scheduling-core rows, a scope node,
// and an absolute instant, it resolves the node's effective daypart and projects
// it — or a fallback, or the terminal default — to a player/1 Lease `display`.
//
// The algorithm, for scope node N at instant t:
//  1. effective tz — the DAT-033 ancestor walk (scopetree.go), NEVER box-local.
//  2. applicable schedules — those attached at N or any ancestor on the parent_id
//     chain (DAT-051), a cascade of the same shape as the tz walk.
//  3. in force — an applicable schedule with validity-window rows is in force at t
//     iff at least one contains t; one with no validity window is always in force
//     (DAT-052).
//  4. precedence — the applicable in-force schedules are ranked by the strict total
//     order priority↓, specificity (ancestor-distance)↑, id↑ (DAT-053).
//  5. effective daypart — each schedule contributes its ≤1 holding daypart at t
//     (holding.go, DAT-110/119); the highest-precedence schedule that has one wins,
//     lower schedules' dayparts showing through only where a higher has none
//     (DAT-111 per-instant layering). Only the effective daypart has effect.
//  6. fallback — if none holds, the fallback_id of the highest-precedence schedule
//     that declares one, falling through the same order (DAT-117).
//  7. terminal default — if neither, display:blank, powered on with no content
//     (DAT-118); scheduling resolution always terminates at a defined state.
//
// display_power projects to the Lease `display` (DAT-113–115): on→content (sourced
// from playlist_id where present), blank→blank, off→blank (the off/blank platform
// distinction is preserved on the row, but player/1's `display` vocabulary has no
// power-off value; DAT-116).

// Lease `display` projection values (player/1 PLY-093). displayContent is a
// powered screen showing its playlist; displayBlank is powered, showing nothing —
// the projection of display_power blank AND off, and of the terminal default.
const (
	displayContent = "content"
	displayBlank   = "blank"
)

// RowStore is the resolved input the resolution engine reads: the validated
// scope-node tree (for the effective-tz walk and the applicability cascade) and
// the typed scheduling-core RowSet. It is the reference-impl stand-in for the
// relay/1 `schedule` desired-state section keyed by scope node (REL-065).
type RowStore struct {
	Tree ScopeTree
	Rows RowSet
}

// EffectiveState is the resolved governing state of a scope node at an instant:
// the effective daypart (DAT-111) or, in its absence, the resolved fallback
// (DAT-117), together with the projected Lease `display` and playlist source
// (DAT-113–115) and the id of the schedule the winning layer came from (empty at
// the terminal default, DAT-118). Exactly one of Daypart/Fallback is non-nil, or
// both are nil for the terminal default. Display is always set.
type EffectiveState struct {
	Daypart    *Daypart
	Fallback   *Fallback
	Display    string
	PlaylistID string
	ScheduleID string
}

// AncestorChain returns nodeID and its ancestors on the parent_id chain toward
// the root, nearest first: index i is ancestor-distance i (nodeID itself is 0).
// It stops at a subtree boundary (a parent absent from the tree) and is
// cycle-guarded. An unknown nodeID yields a nil chain.
//
// This is the DAT-051 applicability cascade's own walk — the same shape the
// DAT-033 effective-tz ancestor walk uses (EffectiveGeo) — exported so a
// caller outside this package (e.g. relay/schedulehost's structural Governs
// check, REL-065) can test schedule-attachment applicability without
// re-deriving the walk itself (data-model/1 line 391: derive, don't
// re-implement).
func (t ScopeTree) AncestorChain(nodeID string) []string {
	if _, ok := t.byID[nodeID]; !ok {
		return nil
	}
	var chain []string
	seen := map[string]bool{}
	cur := nodeID
	for {
		if seen[cur] {
			break
		}
		seen[cur] = true
		chain = append(chain, cur)
		n := t.byID[cur]
		if n.ParentID == nil {
			break
		}
		if _, ok := t.byID[*n.ParentID]; !ok {
			break
		}
		cur = *n.ParentID
	}
	return chain
}

// ApplicableSchedules returns the schedules applicable to nodeID and in force at
// tMs, ranked by the strict total precedence order (DAT-053), highest first: (1)
// priority descending; (2) specificity — smaller ancestor-distance of the
// schedule's scope_node from nodeID (DAT-051), nodeID itself nearest; (3) lowest
// schedule id. A schedule is applicable when its scope_node is nodeID or an
// ancestor (DAT-051) and in force when it has no validity window or at least one
// containing tMs (DAT-052). An unknown nodeID yields an empty slice.
func ApplicableSchedules(store RowStore, nodeID string, tMs int64) []Schedule {
	chain := store.Tree.AncestorChain(nodeID)
	if chain == nil {
		return nil
	}
	distByNode := make(map[string]int, len(chain))
	for i, id := range chain {
		distByNode[id] = i
	}

	windowsBySchedule := map[string][]ValidityWindow{}
	for _, w := range store.Rows.ValidityWindows {
		windowsBySchedule[w.ScheduleID] = append(windowsBySchedule[w.ScheduleID], w)
	}

	type ranked struct {
		s    Schedule
		dist int
	}
	var applicable []ranked
	for _, s := range store.Rows.Schedules {
		dist, ok := distByNode[s.ScopeNode]
		if !ok {
			continue // not attached at N or any ancestor.
		}
		if !inForce(windowsBySchedule[s.ID], tMs) {
			continue
		}
		applicable = append(applicable, ranked{s: s, dist: dist})
	}

	sort.SliceStable(applicable, func(i, j int) bool {
		a, b := applicable[i], applicable[j]
		if pa, pb := a.s.EffectivePriority(), b.s.EffectivePriority(); pa != pb {
			return pa > pb // higher priority first (key 1)
		}
		if a.dist != b.dist {
			return a.dist < b.dist // nearer node first (key 2)
		}
		return a.s.ID < b.s.ID // lowest id first (key 3)
	})

	out := make([]Schedule, len(applicable))
	for i, r := range applicable {
		out[i] = r.s
	}
	return out
}

// inForce reports whether a schedule with these validity windows is in force at
// tMs (DAT-052): no windows means always in force; otherwise at least one window
// must contain tMs. A window contains tMs over the half-open range
// [starts_at, ends_at), a null bound meaning unbounded on that side (DAT-061).
func inForce(windows []ValidityWindow, tMs int64) bool {
	if len(windows) == 0 {
		return true
	}
	for _, w := range windows {
		if (w.StartsAt == nil || tMs >= *w.StartsAt) && (w.EndsAt == nil || tMs < *w.EndsAt) {
			return true
		}
	}
	return false
}

// Resolve resolves scope node nodeID's effective state at absolute instant tMs
// (Unix ms) and projects it to a Lease, per the full algorithm (DAT-051/052/053/
// 111/117/118). It returns a non-nil error only when the node's effective tz
// cannot be resolved (DAT-033/034): resolution NEVER substitutes box-local state,
// so an unresolvable tz is an error, not a silent default. Every other outcome —
// including "no schedule applies" — terminates at a defined state (the terminal
// default display:blank, DAT-118).
func Resolve(store RowStore, nodeID string, tMs int64) (EffectiveState, error) {
	tz, _, _, err := EffectiveTZ(store.Tree, nodeID)
	if err != nil {
		return EffectiveState{}, err
	}
	loc, lerr := time.LoadLocation(tz)
	if lerr != nil {
		return EffectiveState{}, &Error{Field: "tz", Code: "EFFECTIVE_GEO_UNRESOLVED", Message: "effective tz " + tz + " is not a loadable IANA zone; resolution never substitutes box-local state (DAT-034): " + lerr.Error()}
	}

	schedules := ApplicableSchedules(store, nodeID, tMs)

	// Daypart layer: the highest-precedence schedule with a holding daypart wins
	// (DAT-111). The layer is resolved across ALL applicable in-force schedules
	// before any fallback is considered (DAT-117).
	for _, s := range schedules {
		if dp := HoldingDaypart(s, store.Rows.Dayparts, loc, tMs); dp != nil {
			display, playlist := projectDisplay(dp.DisplayPower, dp.PlaylistID)
			return EffectiveState{Daypart: dp, Display: display, PlaylistID: playlist, ScheduleID: s.ID}, nil
		}
	}

	// Fallback layer: the fallback_id of the highest-precedence schedule that
	// declares one, falling through each that declares none (DAT-117).
	for _, s := range schedules {
		if s.FallbackID == "" {
			continue
		}
		if fb := findFallback(store.Rows.Fallbacks, s.FallbackID); fb != nil {
			display, playlist := projectDisplay(fb.DisplayPower, fb.PlaylistID)
			return EffectiveState{Fallback: fb, Display: display, PlaylistID: playlist, ScheduleID: s.ID}, nil
		}
	}

	// Terminal default: powered on, showing nothing — never box-local (DAT-118).
	return EffectiveState{Display: displayBlank}, nil
}

// projectDisplay maps a daypart's or fallback's display_power to the Lease
// `display` value and its content playlist (DAT-113–115): on→content (playlist
// where present), blank→blank, off→blank. off and blank are distinct platform
// states (DAT-116) but project to the identical Lease display:blank — player/1's
// vocabulary has no device-power-off value; an off's actual power-down is a preset
// batch's device command (DAT-075/115), not part of this projection.
func projectDisplay(displayPower, playlistID string) (display, playlist string) {
	if displayPower == "on" {
		return displayContent, playlistID
	}
	return displayBlank, ""
}

// findFallback returns the fallback row with the given id, or nil. The returned
// pointer addresses a copy, safe for the caller to retain.
func findFallback(fallbacks []Fallback, id string) *Fallback {
	for i := range fallbacks {
		if fallbacks[i].ID == id {
			fb := fallbacks[i]
			return &fb
		}
	}
	return nil
}
