package datamodel

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// ScopeNode is a scope-node-tree row (DAT-001): the platform's unit of placement
// and the anchor every scheduling-core row hangs off (DAT-006). kind is one of the
// closed vocabulary {org, site, group, screen} (DAT-001); parent_id is null iff
// kind is org (DAT-002). tz/lat/long are the nullable Time-as-data columns
// (DAT-030): required non-null on a site (DAT-031), an optional override on a
// group or screen (DAT-032), forbidden on an org (DAT-032). account_state and
// entitlements are org-only columns (DAT-010/013); this file carries them on the
// struct for round-trip fidelity but leaves their vocabulary checks to api/1.
type ScopeNode struct {
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	ParentID     *string           `json:"parent_id"`
	Name         string            `json:"name"`
	TZ           *string           `json:"tz,omitempty"`
	Lat          *float64          `json:"lat,omitempty"`
	Long         *float64          `json:"long,omitempty"`
	AccountState string            `json:"account_state,omitempty"`
	Entitlements json.RawMessage   `json:"entitlements,omitempty"`
	ExternalID   string            `json:"external_id,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Revision     int               `json:"revision"`
	CreatedAt    int64             `json:"created_at"`
	UpdatedAt    int64             `json:"updated_at"`
}

// ScopeTree is a validated (or subtree-tolerant) index of scope nodes by id — the
// structure the effective-tz/geo ancestor walk (DAT-033) and the schedule
// applicability cascade (DAT-051, a later task) traverse. A relay/1 desired-state
// snapshot is keyed by scope node and may present only a subtree (REL-065), so a
// node whose parent_id is not in the set is treated as that subtree's boundary,
// not a referential-integrity violation.
type ScopeTree struct {
	byID map[string]ScopeNode
}

// EffectiveGeo is the resolved Time-as-data unit for one scope node (DAT-033): the
// three columns tz/lat/long, always resolved together from ONE node, plus the
// source they came from — "own" (the node's own declared override) or
// "ancestor-site:<id>" (the nearest ancestor site-kind node walked to per DAT-033).
type EffectiveGeo struct {
	TZ     string
	Lat    float64
	Long   float64
	Source string
}

// permittedChildKinds is DAT-003's closed parent-kind table: the kinds each
// parent kind may carry. The tree root's parent is absent; a present parent must
// satisfy this table. screen is a leaf kind (no permitted children).
var permittedChildKinds = map[string]map[string]bool{
	"org":    {"site": true},
	"site":   {"group": true, "screen": true},
	"group":  {"group": true, "screen": true},
	"screen": {},
}

var validScopeKind = map[string]bool{"org": true, "site": true, "group": true, "screen": true}

// maxScopeNodeNameLen is DAT-001a's upper bound on a scope node's name.
const maxScopeNodeNameLen = 200

// BuildScopeTree indexes and validates a set of scope nodes, returning the tree and
// every structural error found. It enforces the scope-node-tree MUSTs this task
// owns:
//   - DAT-001a: name is a non-empty, non-whitespace-only string of at most 200
//     characters (SCOPE_NODE_NAME_INVALID);
//   - DAT-005a: id is a syntactically valid canonical ULID (ROW_ID_INVALID);
//   - DAT-001: kind is one of the closed vocabulary (SCOPE_NODE_KIND_INVALID);
//   - DAT-002: parent_id null iff kind is org (SCOPE_NODE_PARENT_INVALID); at most
//     one org node, and no org node parented under another (SCOPE_NODE_MULTIPLE_ORG);
//   - DAT-003: a present parent's kind permits the child's kind (SCOPE_NODE_PARENT_INVALID);
//   - DAT-031: a site declares all three geo columns non-null (SCOPE_NODE_GEO_REQUIRED);
//   - DAT-032: an org declares none of the three (SCOPE_NODE_GEO_FORBIDDEN), and a
//     group/screen override is all-three-or-none, never partial (SCOPE_NODE_GEO_PARTIAL,
//     the "resolve together as one unit" rule of DAT-033).
//
// It is subtree-tolerant: a node whose parent_id references a node absent from the
// set is a subtree boundary (a relay/1 per-scope snapshot need not carry the org
// root), not an error. An ancestor chain that cannot reach a site is surfaced later,
// per DAT-034, by EffectiveGeo — never by substituting box-local state.
func BuildScopeTree(nodes []ScopeNode) (ScopeTree, []Error) {
	return buildScopeTree(nodes, false)
}

// BuildFullScopeTree is BuildScopeTree for a holder of the COMPLETE tree — the
// app store's post-write validation. The one difference: a parent_id that does
// not resolve is a DAT-002 violation (SCOPE_NODE_PARENT_INVALID), never a
// subtree boundary — "every non-org scope node's parent_id MUST reference an
// existing scope node" is stated of the tree itself, and only a relay/1
// snapshot has an excuse to be missing the rest of it.
//
// What this DOES close, beyond the dangling reference itself: re-kinding the org
// node away from `org` can no longer produce a conformant full tree. A non-org
// node must name a parent that RESOLVES, and the only kinds a re-kinded root
// could take must sit under a node whose own kind permits them (DAT-003) — which,
// on a populated tree, its own children then refuse. DAT-022's delete refusal is
// decided from the row's `kind`, so it is only as strong as that field's
// integrity: this is the write that used to be able to erode it.
//
// What it does NOT close, stated so the gap is not mistaken for coverage:
// DAT-002's exactly-one-org clause. Both variants share only the
// SCOPE_NODE_MULTIPLE_ORG upper bound (more than one org). Neither requires that
// an org exist — an empty set is a valid tree, the state before the root is
// created — and "every parent resolves" does not by itself imply a root: DAT-003
// permits a group under a group, so a group→group cycle has every parent present
// and no null-parented node anywhere. Such a component is unreachable from the
// org rather than dangling, which is a different defect from the one this
// variant refuses; AncestorChain is cycle-guarded, so it is a reachability
// question, not a liveness one.
func BuildFullScopeTree(nodes []ScopeNode) (ScopeTree, []Error) {
	return buildScopeTree(nodes, true)
}

func buildScopeTree(nodes []ScopeNode, fullTree bool) (ScopeTree, []Error) {
	tree := ScopeTree{byID: make(map[string]ScopeNode, len(nodes))}
	var errs []Error

	orgCount := 0
	for _, n := range nodes {
		tree.byID[n.ID] = n
		if n.Kind == "org" {
			orgCount++
		}
	}
	if orgCount > 1 {
		errs = append(errs, Error{Field: "kind", Code: "SCOPE_NODE_MULTIPLE_ORG", Message: "a scope-node tree MUST contain at most one org-kind node (DAT-002)"})
	}

	for _, n := range nodes {
		// DAT-001a: name is a non-empty, non-whitespace-only string of at most 200
		// characters. This check MUST sit above the kind check below: the kind
		// check's continue short-circuits the rest of this node's checks on an
		// invalid kind, and a node can independently fail both name and kind at
		// once (API-013's multi-field aggregation) — appending the name error
		// first, without a continue of its own, is what lets both survive into
		// errs. The 200 limit is a character (Unicode code point) count, not a
		// byte count: len() on a Go string counts bytes, which would wrongly
		// reject a multi-byte-rune name (e.g. 150 "é" characters is 300 bytes but
		// only 150 characters) — utf8.RuneCountInString counts code points instead.
		if strings.TrimSpace(n.Name) == "" || utf8.RuneCountInString(n.Name) > maxScopeNodeNameLen {
			errs = append(errs, Error{Field: "name", Code: "SCOPE_NODE_NAME_INVALID", Message: "a scope node's name MUST be a non-empty string of at most 200 characters (DAT-001a)"})
		}

		// DAT-005a: id MUST be a syntactically valid canonical ULID. Same
		// independent-check placement as the name check above — it neither
		// short-circuits nor is short-circuited by the kind check below, so a
		// node can surface a bad id alongside a bad kind in one pass.
		if e := CheckRowID(n.ID, "id"); e != nil {
			errs = append(errs, *e)
		}

		if !validScopeKind[n.Kind] {
			errs = append(errs, Error{Field: "kind", Code: "SCOPE_NODE_KIND_INVALID", Message: "scope-node kind must be one of org, site, group, screen (DAT-001)"})
			continue
		}

		// DAT-002: parent_id null iff org.
		if n.Kind == "org" {
			if n.ParentID != nil {
				errs = append(errs, Error{Field: "parent_id", Code: "SCOPE_NODE_PARENT_INVALID", Message: "an org-kind scope node MUST have a null parent_id (DAT-002)"})
			}
		} else {
			if n.ParentID == nil {
				errs = append(errs, Error{Field: "parent_id", Code: "SCOPE_NODE_PARENT_INVALID", Message: "a non-org scope node MUST have a non-null parent_id (DAT-002)"})
			} else if parent, ok := tree.byID[*n.ParentID]; ok {
				// DAT-003: a present parent must permit this child's kind. If the
				// parent is absent, this is a subtree boundary, not a violation —
				// unless the caller holds the full tree, where absent means
				// nonexistent and DAT-002 makes that a violation in its own right.
				if !permittedChildKinds[parent.Kind][n.Kind] {
					errs = append(errs, Error{Field: "parent_id", Code: "SCOPE_NODE_PARENT_INVALID", Message: "scope-node kind is not a permitted child of its parent's kind (DAT-003)"})
				}
			} else if fullTree {
				errs = append(errs, Error{Field: "parent_id", Code: "SCOPE_NODE_PARENT_INVALID", Message: "a non-org scope node's parent_id MUST reference an existing scope node (DAT-002)"})
			}
		}

		errs = append(errs, validateGeo(n)...)
		if fullTree {
			errs = append(errs, validateAccountFields(n)...)
		}
	}

	if fullTree {
		errs = append(errs, unreachableFromRoot(nodes, tree.byID)...)
	}

	return tree, errs
}

// unreachableFromRoot reports every node whose parent_id chain CYCLES instead of
// reaching the tree's org root — DAT-002's exactly-one-org clause read as a
// property of the whole tree rather than of one row.
//
// Deliberately narrower than its name suggests, and the boundary is worth stating.
// It catches a CYCLE. It does not report "this tree has no org at all", because
// every non-cyclic way to be detached is already an error from a per-row rule: a
// non-org with a nil parent_id, or one whose parent does not resolve. The only
// detached shape those two miss is a cycle, where every reference is real.
//
// A version of this that also flagged an org-less tree fired on the bootstrap
// case — creating the first node of an empty store as a non-org — which is
// contract-correct but collides with a frozen corpus case testing multi-field
// validation, adding a third error about the tree's shape to a case about two bad
// fields. Enforcing that half is a separate change with a corpus decision in it.
//
// It runs ONLY for the full tree, and that restriction is the point. A relay
// receives a SUBTREE in its snapshot — REL-065 carries the scope nodes a site's
// schedule needs, not the tree above them — so nothing in a relay's node set
// reaches an org, and asking would reject every legitimate snapshot. The
// subtree-tolerant builder must never call this.
//
// Why this is not covered by anything above it: every check so far is about ONE
// row's own fields. A cycle passes all of them. Two group nodes each naming the
// other have parent_id non-null (DAT-002), a resolving parent (the resolution
// check added for the full tree), and a permitted parent kind, because DAT-003
// allows group under group. Every reference is real; the component simply hangs
// off nothing. That is why closing the four ways a reference can DANGLE did not
// close this — reachability is a different question from resolution, and only a
// walk answers it.
//
// Cost: one upward walk per node, each step a map lookup, stopping at the org, at
// a missing parent, or on revisiting a node. Worst case is O(n·depth) with depth
// bounded by the tree's own height — org→site→group…→screen, single digits in any
// real deployment — and it runs on scope-node writes only, which are rare
// compared with the scheduling-core writes this same validator serves.
func unreachableFromRoot(nodes []ScopeNode, byID map[string]ScopeNode) []Error {
	var errs []Error
	for _, n := range nodes {
		if n.Kind == "org" {
			continue
		}
		seen := map[string]bool{n.ID: true}
		cur := n
		for {
			if cur.ParentID == nil {
				break // a non-org with no parent is already reported above
			}
			parent, ok := byID[*cur.ParentID]
			if !ok {
				break // a missing parent is already reported above
			}
			if parent.Kind == "org" {
				break // reached the root
			}
			if seen[parent.ID] {
				errs = append(errs, Error{Field: "parent_id", Code: "SCOPE_NODE_PARENT_INVALID",
					Message: "a scope node's parent_id chain MUST reach the tree's org-kind root (DAT-002); this one cycles without reaching it"})
				break
			}
			seen[parent.ID] = true
			cur = parent
		}
	}
	return errs
}

// validAccountStates is DAT-010's closed set.
var validAccountStates = map[string]bool{
	"trial": true, "active": true, "suspended": true, "closed": true, "purged": true,
}

// validateAccountFields enforces DAT-010 (account_state) and DAT-013
// (entitlements): both are mandatory on an org-kind node and forbidden on every
// other kind. Each half of each rule is a distinct failure with its own message,
// because "you left it off the org" and "you put it on a site" are opposite
// mistakes with opposite remedies.
//
// FULL-TREE ONLY, and that is load-bearing rather than cautious. These are
// account-level fields: a relay/1 desired-state snapshot carries neither (the
// relay has no reference to either field anywhere) and may present a subtree
// including the org node, so enforcing DAT-010 on a snapshot would reject every
// legitimate one for the absence of a field the wire never carries. Only the
// authority that holds the whole tree — and mints these rows from requests — can
// judge them, which is also where DAT-010/013 place the obligation: on "a create
// or update request".
//
// Reported here rather than as a fail-fast body-schema check so both codes arrive
// in API-013's multi-field errors[] alongside every other failing member, which
// is the shape data-model/1's own per-field codes are published for.
func validateAccountFields(n ScopeNode) []Error {
	var errs []Error

	// Within each field the two org-side cases report the SAME code and differ only
	// in message — "" is not a member of the valid set, and an absent entitlements
	// document is not an object — so neither is independently load-bearing and
	// removing either alone changes no outcome. They are split for the message an
	// operator reads, not for the decision. Worth writing down: a mutation run that
	// disables one case sees the other answer identically and reads as a test with
	// no teeth, which is the opposite of what is happening.
	if n.Kind == "org" {
		switch {
		case n.AccountState == "":
			errs = append(errs, Error{Field: "account_state", Code: "SCOPE_NODE_ACCOUNT_STATE_INVALID",
				Message: "an org-kind scope node MUST carry account_state (DAT-010)"})
		case !validAccountStates[n.AccountState]:
			errs = append(errs, Error{Field: "account_state", Code: "SCOPE_NODE_ACCOUNT_STATE_INVALID",
				Message: "account_state must be one of trial, active, suspended, closed, purged (DAT-010)"})
		}
		// "present (possibly {})" — an empty object satisfies DAT-013, an absent
		// field does not, and a non-object does not. The document's own internal
		// schema is explicitly defined elsewhere, so this checks presence and JSON
		// type and nothing further.
		switch {
		case len(n.Entitlements) == 0:
			errs = append(errs, Error{Field: "entitlements", Code: "SCOPE_NODE_ENTITLEMENTS_INVALID",
				Message: "an org-kind scope node MUST carry entitlements, an object (possibly empty) (DAT-013)"})
		case !isJSONObject(n.Entitlements):
			errs = append(errs, Error{Field: "entitlements", Code: "SCOPE_NODE_ENTITLEMENTS_INVALID",
				Message: "entitlements MUST be a JSON object (DAT-013)"})
		}
		return errs
	}

	if n.AccountState != "" {
		errs = append(errs, Error{Field: "account_state", Code: "SCOPE_NODE_ACCOUNT_STATE_INVALID",
			Message: "a non-org scope node MUST NOT carry account_state (DAT-010)"})
	}
	if len(n.Entitlements) > 0 {
		errs = append(errs, Error{Field: "entitlements", Code: "SCOPE_NODE_ENTITLEMENTS_INVALID",
			Message: "a non-org scope node MUST NOT carry entitlements (DAT-013)"})
	}
	return errs
}

// isJSONObject reports whether raw is a JSON object rather than an array, string,
// number, bool, or null. Decoding into a map is how the object-ness is decided,
// so a syntactically invalid document answers false here rather than reaching the
// store as an opaque blob.
func isJSONObject(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil && obj != nil
}

// validateGeo enforces DAT-031 (site geo required) and DAT-032 (org geo forbidden;
// group/screen override is all-three-or-none — the DAT-033 "one unit" rule).
func validateGeo(n ScopeNode) []Error {
	present := 0
	for _, p := range []bool{n.TZ != nil, n.Lat != nil, n.Long != nil} {
		if p {
			present++
		}
	}
	switch n.Kind {
	case "site":
		if present != 3 {
			return []Error{{Field: "tz", Code: "SCOPE_NODE_GEO_REQUIRED", Message: "a site-kind scope node MUST declare non-null tz, lat, and long (DAT-031)"}}
		}
	case "org":
		if present != 0 {
			return []Error{{Field: "tz", Code: "SCOPE_NODE_GEO_FORBIDDEN", Message: "an org-kind scope node MUST NOT declare tz, lat, or long (DAT-032)"}}
		}
	case "group", "screen":
		if present != 0 && present != 3 {
			return []Error{{Field: "tz", Code: "SCOPE_NODE_GEO_PARTIAL", Message: "a group/screen geo override MUST declare all three of tz, lat, long together or none — they resolve as one unit (DAT-032/DAT-033)"}}
		}
	}
	return nil
}

// KindOf returns the kind of the scope node a row is attached to (DAT-004): a
// device or a screen-resource row MAY be placed at a node of ANY kind, so this
// simply reports the placement node's own kind. ok is false for an unknown id.
func (t ScopeTree) KindOf(nodeID string) (string, bool) {
	n, ok := t.byID[nodeID]
	if !ok {
		return "", false
	}
	return n.Kind, true
}

// EffectiveGeo resolves a scope node's effective tz/lat/long as one unit (DAT-033):
// the node's own declared override when present, else the nearest ancestor
// SITE-kind node's own geo, walking parent_id toward the root. An intermediate
// group's own override is NOT consulted for a descendant — DAT-033 walks to the
// nearest site-kind node specifically, not the nearest declaring node.
//
// Per DAT-034 there is no further terminal default: an unknown node id, or an
// ancestor chain that reaches neither the node's own override nor any site-kind
// ancestor within the provided set, is an error. Resolution NEVER substitutes the
// evaluating machine's OS locale/timezone or any other box-local state.
func (t ScopeTree) EffectiveGeo(nodeID string) (EffectiveGeo, *Error) {
	n, ok := t.byID[nodeID]
	if !ok {
		return EffectiveGeo{}, &Error{Field: "scope_node", Code: "SCOPE_NODE_PARENT_INVALID", Message: "scope node id not found in the tree (DAT-004)"}
	}

	// DAT-033: the node's own override wins — but only for the node itself.
	if n.TZ != nil && n.Lat != nil && n.Long != nil {
		return EffectiveGeo{TZ: *n.TZ, Lat: *n.Lat, Long: *n.Long, Source: "own"}, nil
	}

	// Walk parent_id toward the root for the nearest ancestor site-kind node.
	// A bounded loop guards against a malformed parent cycle.
	seen := map[string]bool{nodeID: true}
	cur := n
	for cur.ParentID != nil {
		parent, ok := t.byID[*cur.ParentID]
		if !ok {
			break // subtree boundary: parent absent from the provided set.
		}
		if seen[parent.ID] {
			break // defensive: a parent cycle cannot resolve.
		}
		seen[parent.ID] = true
		if parent.Kind == "site" && parent.TZ != nil && parent.Lat != nil && parent.Long != nil {
			return EffectiveGeo{TZ: *parent.TZ, Lat: *parent.Lat, Long: *parent.Long, Source: "ancestor-site:" + parent.ID}, nil
		}
		cur = parent
	}

	return EffectiveGeo{}, &Error{Field: "tz", Code: "EFFECTIVE_GEO_UNRESOLVED", Message: "effective tz/lat/long could not be resolved: no site-kind ancestor with declared geo in the provided tree; the platform NEVER substitutes box-local state (DAT-034)"}
}

// EffectiveTZ is the free-function form the resolution engine calls (DAT-033): the
// effective tz/lat/long for a scope node as one unit, discarding the source tag.
// It returns a non-nil error exactly when EffectiveGeo does (DAT-034: no box-local
// fallback).
func EffectiveTZ(tree ScopeTree, nodeID string) (tz string, lat, long float64, err error) {
	g, e := tree.EffectiveGeo(nodeID)
	if e != nil {
		return "", 0, 0, e
	}
	return g.TZ, g.Lat, g.Long, nil
}
