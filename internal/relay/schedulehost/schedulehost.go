// Package schedulehost is the relay-side driver for data-model/1's
// scheduling-core resolution engine (contracts/relay-1.md REL-065,
// contracts/data-model-1.md DAT-051/111/113-118): it parses the
// opaquely-carried `schedule` desired-state section into a
// datamodel.RowStore and, in later files of this package, resolves it into
// the player/1 Lease a screen is served and fires preset batches on daypart
// rising edges.
//
// This package DERIVES, it does not re-implement (data-model/1 line 391):
// every parse/validate/resolve step here calls straight through to
// internal/datamodel's own, corpus-proven functions
// (datamodel.BuildScopeTree, datamodel.ValidateRows). No scheduling
// semantics — precedence, holding, fallback, or terminal-default rules —
// are expressed in this package.
package schedulehost

import (
	"encoding/json"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// BuildStore parses a carried `schedule` desired-state section (REL-065)
// into a data-model/1 RowStore: it unmarshals the carried scope-node array
// and the six scheduling-core row arrays, then runs them through
// datamodel's own parse/validate path — datamodel.BuildScopeTree for the
// scope-node tree, datamodel.ValidateRows for the six row kinds — never
// re-expressing either's rules here (data-model/1 line 391).
//
// BuildStore is degrade-safe: it MUST NOT panic or brick on a bad schedule
// section. A scope-node entry that fails to unmarshal is recorded as a
// ROW_MALFORMED error and excluded from the tree; every other parse or
// validation failure datamodel.BuildScopeTree/ValidateRows themselves
// report is passed through unchanged. The returned RowStore is always
// usable — built from whatever good nodes and rows survived — even when
// the returned error slice is non-empty; the caller (the relay boot path,
// a later task) logs the errors and degrades to serving the app-authored
// program rather than treating a bad schedule as fatal.
func BuildStore(sec wire.ScheduleSection) (datamodel.RowStore, []datamodel.Error) {
	var errs []datamodel.Error

	nodes := make([]datamodel.ScopeNode, 0, len(sec.ScopeNodes))
	for _, raw := range sec.ScopeNodes {
		var n datamodel.ScopeNode
		if err := json.Unmarshal(raw, &n); err != nil {
			errs = append(errs, datamodel.Error{
				Field:   "scope_nodes",
				Code:    "ROW_MALFORMED",
				Message: err.Error(),
			})
			continue
		}
		nodes = append(nodes, n)
	}

	tree, treeErrs := datamodel.BuildScopeTree(nodes)
	errs = append(errs, treeErrs...)

	raw := datamodel.RawRows{
		Playlists:       sec.Playlists,
		Schedules:       sec.Schedules,
		ValidityWindows: sec.ValidityWindows,
		Dayparts:        sec.Dayparts,
		Fallbacks:       sec.Fallbacks,
		PresetBatches:   sec.PresetBatches,
	}
	rows, rowErrs := datamodel.ValidateRows(raw)
	errs = append(errs, rowErrs...)

	return datamodel.RowStore{Tree: tree, Rows: rows}, errs
}

// Governs reports whether the carried schedule governs screenNodeID, per
// the relay's additive serving policy (Global Constraints): the screen's
// scope node MUST be present in the carried scope tree, AND at least one
// schedule row MUST be applicable to it per DAT-051 — its own scope_node is
// the screen itself OR any ancestor of the screen on the parent_id chain
// (contracts/data-model-1.md DAT-051: "a site-wide base schedule governs
// every screen beneath it"). Governs delegates the ancestor walk to
// datamodel.ScopeTree.AncestorChain, the same cascade ApplicableSchedules
// itself walks, rather than re-deriving it here (data-model/1 line 391).
//
// Governs is a purely structural, time-independent check — it says nothing
// about whether a schedule is currently in force (DAT-052) or a daypart
// currently HOLDS (that per-instant question is Resolve's, a later task) —
// it only decides whether the relay should treat this screen as
// schedule-driven at all.
//
// When Governs is false (no scope node carried for the screen, or no
// schedule is applicable to it at any ancestor distance), the relay's
// stated policy is to keep serving the app-authored screen_programs
// program unchanged (REL-061) — behavior is UNCHANGED for an empty or
// non-applicable schedule. Governs does not itself decide what to serve;
// callers do.
func Governs(store datamodel.RowStore, screenNodeID string) bool {
	chain := store.Tree.AncestorChain(screenNodeID)
	if chain == nil {
		return false
	}
	inChain := make(map[string]bool, len(chain))
	for _, id := range chain {
		inChain[id] = true
	}
	for _, s := range store.Rows.Schedules {
		if inChain[s.ScopeNode] {
			return true
		}
	}
	return false
}
