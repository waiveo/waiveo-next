package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// schedulingKinds is the subset of resource kinds whose writes are validated
// through datamodel.ValidateRows (the six data-model/1 scheduling-core kinds,
// plus the cast rows a playlist item references — DAT-043).
// A scope-node write is validated separately, through BuildFullScopeTree.
//
// A cast is here rather than in a validator of its own for the reason every
// entry on this list is: ValidateRows judges the RESULTING FULL row-set, so a
// cast write is checked against the playlists that reference it and — the half a
// per-row validator could never see — a cast DELETE is refused while a playlist
// still names it, exactly as deleting a playlist a daypart points at is.
var schedulingKinds = map[Kind]bool{
	KindPlaylist:       true,
	KindCast:           true,
	KindSchedule:       true,
	KindDaypart:        true,
	KindValidityWindow: true,
	KindFallback:       true,
	KindPresetBatch:    true,
}

// identityKinds is the subset of resource kinds whose writes are validated
// through datamodel.ValidateIdentityRows: the screen identity row and the device
// row data-model/1 DAT-004/DAT-005b define.
//
// The two are validated TOGETHER, whichever of them was written, because a
// screen's optional device_id link (player/1 PLY-124) can only be resolved
// against the device table — so deleting a device a screen still points at is
// caught by the screen table's own re-validation, exactly as deleting a playlist
// a daypart still points at is caught by the scheduling set's.
var identityKinds = map[Kind]bool{
	KindScreen:        true,
	KindAdoptedDevice: true,
}

// validateAfterWrite runs, inside the write transaction, the datamodel/1
// validation appropriate to the kind just written, over the RESULTING full
// row-set — so a write is judged against the state it produced, and a failure
// aborts the whole transaction (nothing persisted, generation unchanged). A
// scope-node write is checked with BuildFullScopeTree (DAT-001-003/030-032,
// with DAT-002's parent-resolution enforced — this store IS the full tree, so
// a dangling parent_id here is never a relay-snapshot subtree boundary; it is
// how a re-kinded org or a re-parent-to-garbage write orphans the tree). A
// scheduling-core write with ValidateRows (DAT-005-008/040-101, references,
// DAT-073 daypart partition). Errors are wrapped in *ValidationError for the api
// layer to render as VALIDATION_FAILED.
//
// scopeNode is the placement the write being validated carries — the effective
// post-merge value for an Update, empty for a Delete (which removes a placement
// and can never introduce one) — and it is checked against the scope-node table
// first, ahead of the per-kind switch (DAT-006, checkPlacementResolves).
//
// It is here rather than inside any one arm because it is the one rule none of the
// per-kind validators can express: each is handed rows of its own kinds and never
// sees the scope-node table, so a row's reference INTO the tree is invisible to all
// of them. That is the same blind spot the deletion rules needed a guard for, seen
// from the other end — and it is why a scope_node naming nothing used to be a 201.
// Checking it first also means the refusal a caller reads names the reference
// itself, rather than whichever downstream rule the unplaced row happened to trip.
func validateAfterWrite(ctx context.Context, tx *sql.Tx, kind Kind, scopeNode string) error {
	// NOT for scope nodes. A scope node is the thing a placement points AT, and
	// DAT-006 says so in as many words: "Every row this contract defines OTHER
	// THAN A SCOPE NODE ITSELF MUST carry scope_node". Running the placement
	// check here did two wrong things at once.
	//
	// It refused an undeclared `scope_node` member on a scope-node body with a
	// message naming `table scope_nodes` — the one table that never carries a
	// placement — and, because scope nodes name no createSchema (bodyschema.go
	// records that as deliberate), nothing rejected the stray member first. So
	// one undeclared field collapsed API-013's multi-error answer to a single
	// one, replacing a published, corpus-pinned shape with a poorer one. That is
	// exactly the trade bodyschema.go refuses in writing.
	//
	// And it put scope_nodes on the FORWARD list while placedAt deliberately
	// keeps it off the REVERSE one — the asymmetry placement.go's own doc calls
	// out as a defect: "a table on one list and not the other would be a store
	// where a node cannot be deleted but a row may point at nothing, or the
	// reverse." A group placed at another group resolved on the way in and then
	// survived that node's deletion, since placedAt never looked. One inventory,
	// both directions, or neither.
	// COLLECTED, not short-circuited. Returning here on the first fault would
	// hand back one error where api/1 API-013 publishes a multi-field answer, and
	// that answer is corpus-pinned — a daypart carrying both an unresolvable
	// placement and an unrelated bad field went from two errors to one. Naming
	// the reference is worth having; suppressing everything else to get it is
	// not, and bodyschema.go already refuses that same trade in writing.
	var placementErrs []datamodel.Error
	if kind != KindScopeNode {
		if err := checkPlacementResolves(ctx, tx, string(kind), scopeNode); err != nil {
			var verr *ValidationError
			if !errors.As(err, &verr) {
				return err // a storage failure, not a validation one
			}
			placementErrs = verr.Errors
		}
	}
	// withPlacement prefixes the placement fault onto whatever the per-kind
	// validator found, so the reference error leads and the rest survives.
	withPlacement := func(errs []datamodel.Error) error {
		all := append(append([]datamodel.Error(nil), placementErrs...), errs...)
		if len(all) > 0 {
			return &ValidationError{Errors: all}
		}
		return nil
	}
	switch {
	case kind == KindScopeNode:
		nodes, err := readScopeNodes(ctx, tx)
		if err != nil {
			return err
		}
		_, errs := datamodel.BuildFullScopeTree(nodes)
		return withPlacement(errs)
	case schedulingKinds[kind]:
		raw, err := readRawRows(ctx, tx)
		if err != nil {
			return err
		}
		_, errs := datamodel.ValidateRows(raw)
		return withPlacement(errs)
	case identityKinds[kind]:
		raw, err := readIdentityRows(ctx, tx)
		if err != nil {
			return err
		}
		_, errs := datamodel.ValidateIdentityRows(raw)
		return withPlacement(errs)
	case kind == KindAutomation:
		// Automations are gated by the rules compiler (compile.Compile) at the top
		// of Create/Update, not by datamodel.ValidateRows — a compile failure has
		// already aborted the write before this post-write hook runs. compile.Compile
		// has no notion of a row's own identity, though, so DAT-005a (a row's id
		// MUST be a syntactically valid canonical ULID) is enforced HERE instead,
		// over the resulting full automations table — the same "judge the write
		// against the state it produced" pattern the scheduling-kinds and scope-node
		// cases above use, just narrowed to the one column compile.Compile never
		// looks at. Without this, an in-process writer that bypasses the HTTP
		// layer's own client-supplied-id rejection (a seed, a pack/install pipeline,
		// a future caller) could persist a non-ULID automation id that cursor
		// pagination (DecodeCursor's ulid.Valid gate) would then refuse to page past.
		ids, err := readIDs(ctx, tx, string(KindAutomation))
		if err != nil {
			return err
		}
		var errs []datamodel.Error
		for _, id := range ids {
			if e := datamodel.CheckRowID(id, "id"); e != nil {
				errs = append(errs, *e)
			}
		}
		return withPlacement(errs)
	case kind == KindWebhookEndpoint:
		// A webhook endpoint has no datamodel/1 row schema and no compiler: its
		// body is validated by the api layer's own per-kind check before the
		// write (a URL that is not an absolute http/https origin is a 422, never
		// a stored row). What is enforced here is the same identity rule the
		// automations arm above enforces, for the same reason — an in-process
		// writer that bypasses the HTTP layer must not be able to persist a
		// non-ULID id that cursor pagination would then refuse to page past.
		ids, err := readIDs(ctx, tx, string(KindWebhookEndpoint))
		if err != nil {
			return err
		}
		var errs []datamodel.Error
		for _, id := range ids {
			if e := datamodel.CheckRowID(id, "id"); e != nil {
				errs = append(errs, *e)
			}
		}
		return withPlacement(errs)
	default:
		return fmt.Errorf("store: no validator for kind %q", kind)
	}
}

// readRawRows loads every scheduling-core row body, grouped by kind and ordered
// by id, into a datamodel.RawRows bundle — the exact shape ValidateRows and the
// desired-state derivation consume.
func readRawRows(ctx context.Context, q queryer) (datamodel.RawRows, error) {
	var rr datamodel.RawRows
	var err error
	if rr.Playlists, err = readBodies(ctx, q, string(KindPlaylist)); err != nil {
		return rr, err
	}
	if rr.Casts, err = readBodies(ctx, q, string(KindCast)); err != nil {
		return rr, err
	}
	if rr.Schedules, err = readBodies(ctx, q, string(KindSchedule)); err != nil {
		return rr, err
	}
	if rr.ValidityWindows, err = readBodies(ctx, q, string(KindValidityWindow)); err != nil {
		return rr, err
	}
	if rr.Dayparts, err = readBodies(ctx, q, string(KindDaypart)); err != nil {
		return rr, err
	}
	if rr.Fallbacks, err = readBodies(ctx, q, string(KindFallback)); err != nil {
		return rr, err
	}
	if rr.PresetBatches, err = readBodies(ctx, q, string(KindPresetBatch)); err != nil {
		return rr, err
	}
	return rr, nil
}

// readIdentityRows loads the screen and device row bodies, each ordered by id,
// into the datamodel.RawIdentityRows bundle ValidateIdentityRows consumes — the
// identity-kind counterpart of readRawRows, and the reason a write to EITHER
// table re-validates BOTH (a screen's PLY-124 device link crosses them).
func readIdentityRows(ctx context.Context, q queryer) (datamodel.RawIdentityRows, error) {
	var ri datamodel.RawIdentityRows
	var err error
	if ri.Screens, err = readBodies(ctx, q, string(KindScreen)); err != nil {
		return ri, err
	}
	if ri.Devices, err = readBodies(ctx, q, string(KindAdoptedDevice)); err != nil {
		return ri, err
	}
	return ri, nil
}

// readBodies returns every row body of a table, ordered by id ascending.
func readBodies(ctx context.Context, q queryer, table string) ([]json.RawMessage, error) {
	rows, err := q.QueryContext(ctx, `SELECT body FROM `+table+` ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: read %s bodies: %w", table, err)
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, fmt.Errorf("store: scan %s body: %w", table, err)
		}
		out = append(out, json.RawMessage(body))
	}
	return out, rows.Err()
}

// readIDs returns every id in table, ordered ascending — the minimal
// projection validateAfterWrite's KindAutomation case needs to enforce
// DAT-005a over the resulting full row-set, without paying to decode every
// row's full body the way readBodies/readRawRows do.
func readIDs(ctx context.Context, q queryer, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM `+table+` ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: read %s ids: %w", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan %s id: %w", table, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
