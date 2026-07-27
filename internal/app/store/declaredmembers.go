package store

import (
	"encoding/json"
	"fmt"
)

// declaredmembers.go completes a row's api/1 REPRESENTATION on write: every
// member api/openapi.yaml declares REQUIRED on the schema this row is served as
// is materialized, with a stated value, when the write left it absent.
//
// # Why this exists
//
// The store persists the exact JSON bytes the api layer later serves verbatim
// (api.go's create/get/list/patch all write res.Body straight out), so a member
// a create body simply did not mention was a member the response simply did not
// carry — while the document told every generated client it would always be
// there. `POST /scope-nodes {kind, name}` answered 201 with no `parent_id` and
// no `labels`, both declared required; `POST /automations` answered 201 with no
// `labels`, `enabled`, `max` or `conditions`. A client written against the
// generated types broke on an ordinary success response.
//
// The fix is to normalize rather than to relax the schema: the server fills the
// member in. That is the same ownership injectBaseline already exercises over
// revision/created_at/updated_at — themselves declared-required members no
// client ever sends — so this is that rule extended to the rest of the
// representation, in the same place, rather than a second mechanism bolted onto
// the api layer.
//
// # Why the store rather than the api layer
//
// Every writer of a row goes through Store.Create/Store.Update: the api
// handlers, and the make-dev seed (seed.go). Normalizing here is what makes a
// SEEDED row's representation conformant too — GET /scope-nodes served the
// seeded demo tree with no `labels` at all, and no amount of care in the create
// handler would have fixed that.
//
// # Update, not only create
//
// The defaults are applied to the effective post-merge body on update as well.
// On a row created since this existed the members are already present, so it is
// a no-op; what it genuinely closes is a patch that sets a required member to
// JSON `null` (`{"labels": null}`), which is present-but-unusable to a client
// whose generated type says `map[string]string`.
//
// # What is NOT here
//
// Members the document does not declare required (`external_id`, `tz`, `lat`,
// `long`, `account_state`, an entity's `state`) are left absent when absent.
// Adding them would be inventing representation the schema explicitly makes
// optional, and `external_id` in particular has a meaning — "this row carries a
// client-assigned identity" — that a materialized null would blur.
type declaredMember struct {
	// key is the response member's JSON name.
	key string
	// value is the JSON literal materialized when the member is absent.
	value json.RawMessage
	// overNull materializes value over an explicit JSON `null` too, not only
	// over an absent member. Set for a member whose declared type does NOT admit
	// null (a LabelMap is an object, `enabled` a boolean, `conditions` an array),
	// where a null is exactly as unusable to a typed client as an absence.
	// Deliberately NOT set for `parent_id` and `max`, whose declared types are
	// `["string","null"]` / `["integer","null"]` — there, null is the value.
	overNull bool
}

// labelsMember is the one default every resource family shares. api/1's
// label-selector grammar evaluates labels on every family (API-040/042) and the
// store already denormalizes them into a column for every kind, so a row whose
// labels were never set carries the empty map that says so rather than nothing
// at all. The families whose response schema is a later minor (schedules,
// dayparts, playlists, preset batches) get it on the same reasoning: the
// principle is normalize-don't-omit, and applying it only where a schema is
// already written would leave the next schema to be written describing rows that
// do not match it.
var labelsMember = declaredMember{key: "labels", value: json.RawMessage(`{}`), overNull: true}

// kindDeclaredMembers is the per-kind default set, beyond labelsMember.
//
// Each value is the CONSERVATIVE reading of what the absent member meant:
//
//   - scope_nodes.parent_id — `null`, which the schema documents as "null only
//     for the single root org node". A create that named no parent WAS creating a
//     root; null states it exactly, and changes no behavior.
//
//   - automations.enabled — `false`. This is the one default that is a decision
//     rather than a transcription: an automation created without saying whether
//     it is on comes back OFF. It matches the fail-closed posture the rest of the
//     platform takes (a request whose authorization cannot be resolved refuses
//     rather than default-permits) and means a half-finished create never starts
//     acting on real screens. Enabling is meant to be deliberate — bulk-enable
//     (API-110/111) exists precisely for that act — and the accepted cost is that
//     creating a LIVE automation is two calls.
//
//   - automations.max — `null`. The declared type is `["integer","null"]` and the
//     member is meaningful only under `parallel` mode, so null is the schema's own
//     spelling of "no cap stated" and is exactly what an absent member meant.
//     Materializing a NUMBER here would invent a concurrency policy the author
//     never wrote.
//
//   - automations.conditions — `[]`. rules/1 runs a rule with no conditions
//     unconditionally, which is what an absent list already meant, so the empty
//     list is the same rule spelled out.
var kindDeclaredMembers = map[Kind][]declaredMember{
	KindScopeNode: {
		{key: "parent_id", value: json.RawMessage(`null`)},
	},
	KindAutomation: {
		{key: "enabled", value: json.RawMessage(`false`), overNull: true},
		{key: "max", value: json.RawMessage(`null`)},
		{key: "conditions", value: json.RawMessage(`[]`), overNull: true},
	},
}

// injectDeclaredMembers returns body with every declared-required member of
// kind's api/1 representation present, materializing the stated default for one
// that is absent (or, where overNull is set, explicitly null). A body that
// already carries them all is returned unchanged, bytes and all.
func injectDeclaredMembers(kind Kind, body json.RawMessage) (json.RawMessage, error) {
	members := declaredMembersFor(kind)
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("store: decode %s body for representation completion: %w", kind, err)
	}
	changed := false
	for _, d := range members {
		cur, present := m[d.key]
		if present && !(d.overNull && isJSONNull(cur)) {
			continue
		}
		m[d.key] = d.value
		changed = true
	}
	if !changed {
		return body, nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("store: re-marshal %s body after representation completion: %w", kind, err)
	}
	return out, nil
}

// declaredMembersFor is kind's full default set: the shared labels member plus
// whatever kindDeclaredMembers adds. Every kind gets labels — an unknown kind
// never reaches here, since tableFor rejects one before any write begins.
func declaredMembersFor(kind Kind) []declaredMember {
	out := make([]declaredMember, 0, 1+len(kindDeclaredMembers[kind]))
	out = append(out, labelsMember)
	return append(out, kindDeclaredMembers[kind]...)
}

// isJSONNull reports whether raw is the JSON null literal, tolerating the
// surrounding whitespace a re-marshal never emits but a hand-written body may.
func isJSONNull(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	return v == nil
}
