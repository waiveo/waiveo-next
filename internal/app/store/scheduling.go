package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/datamodel"
)

// schedulingKinds is the subset of resource kinds whose writes are validated
// through datamodel.ValidateRows (the six data-model/1 scheduling-core kinds).
// A scope-node write is validated separately, through BuildScopeTree.
var schedulingKinds = map[Kind]bool{
	KindPlaylist:       true,
	KindSchedule:       true,
	KindDaypart:        true,
	KindValidityWindow: true,
	KindFallback:       true,
	KindPresetBatch:    true,
}

// validateAfterWrite runs, inside the write transaction, the datamodel/1
// validation appropriate to the kind just written, over the RESULTING full
// row-set — so a write is judged against the state it produced, and a failure
// aborts the whole transaction (nothing persisted, generation unchanged). A
// scope-node write is checked with BuildScopeTree (DAT-001-003/030-032); a
// scheduling-core write with ValidateRows (DAT-005-008/040-101, references,
// DAT-073 daypart partition). Errors are wrapped in *ValidationError for the api
// layer to render as VALIDATION_FAILED.
func validateAfterWrite(ctx context.Context, tx *sql.Tx, kind Kind) error {
	switch {
	case kind == KindScopeNode:
		nodes, err := readScopeNodes(ctx, tx)
		if err != nil {
			return err
		}
		if _, errs := datamodel.BuildScopeTree(nodes); len(errs) > 0 {
			return &ValidationError{Errors: errs}
		}
		return nil
	case schedulingKinds[kind]:
		raw, err := readRawRows(ctx, tx)
		if err != nil {
			return err
		}
		if _, errs := datamodel.ValidateRows(raw); len(errs) > 0 {
			return &ValidationError{Errors: errs}
		}
		return nil
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
		if len(errs) > 0 {
			return &ValidationError{Errors: errs}
		}
		return nil
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
		if len(errs) > 0 {
			return &ValidationError{Errors: errs}
		}
		return nil
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
