package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Pack data retention (manifest/1 MAN-054): the bound a pack declares on its own
// collections, enforced by the host.
//
// The field has been declarable since manifest/1 was written and read by nothing
// — one of eight declarations a publisher could make that the runtime ignored.
// An ignored retention bound is worse than an absent one: a pack author who
// declares `{maxRows: 500}` has told the operator their extension is bounded,
// and on an appliance whose disk has filled before, that is a promise the
// platform was not keeping.
//
// # When it runs
//
// After a row lands, for that collection only. The moment new rows arrive is the
// moment old ones become surplus, so this needs no scheduler and no timer nobody
// watches — the same reasoning ARC-124's post-export archive sweep uses. A
// collection nobody writes to stops being swept, and that is correct: nothing
// grew.
//
// # What a malformed or absent bound means
//
// Unbounded. MAN-054 says a collection with no entry defaults to unbounded, and
// this extends the same posture to a bound that does not parse or is not
// positive. The asymmetry is deliberate: failing open leaves rows an operator can
// still delete, and failing closed would delete rows on the strength of a
// declaration nobody could read. A `maxRows` of zero is therefore NOT "keep
// nothing" — a declaration that empties a collection on every write is far more
// likely a typo than an intent, and the cost of guessing wrong is the pack's
// entire dataset.

// retentionRule is one collection's parsed bound.
type retentionRule struct {
	MaxRows int `json:"maxRows"`
	MaxAge  int `json:"maxAge"` // days
}

// packRetentionFor reads one collection's declared bound out of a stored
// manifest. Returns ok=false for anything that is not a positive bound.
func packRetentionFor(rawManifest json.RawMessage, collection string) (retentionRule, bool) {
	var m struct {
		Retention map[string]json.RawMessage `json:"retention"`
	}
	if err := json.Unmarshal(rawManifest, &m); err != nil {
		return retentionRule{}, false
	}
	raw, present := m.Retention[collection]
	if !present {
		return retentionRule{}, false
	}
	// `unbounded` is a bare string, not an object — decoding it into the
	// descriptor would fail, and treating that failure as "no bound" happens to
	// be right, but only by accident. Named here so it stays right on purpose.
	var rule retentionRule
	if err := json.Unmarshal(raw, &rule); err != nil {
		return retentionRule{}, false
	}
	if rule.MaxRows <= 0 && rule.MaxAge <= 0 {
		return retentionRule{}, false
	}
	return rule, true
}

// applyPackRetention deletes the rows a collection's declared bound releases.
//
// Runs inside the caller's transaction, so a row landing and the sweep it
// triggers commit together: a crash between them would otherwise leave a
// collection over its declared bound with nothing scheduled to notice.
func applyPackRetention(ctx context.Context, tx *sql.Tx, packID, collection string, rawManifest json.RawMessage, nowMs int64) error {
	rule, ok := packRetentionFor(rawManifest, collection)
	if !ok {
		return nil
	}

	if rule.MaxAge > 0 {
		cutoff := nowMs - int64(rule.MaxAge)*24*60*60*1000
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM pack_rows WHERE pack_id = ? AND collection = ? AND created_at < ?`,
			packID, collection, cutoff); err != nil {
			return fmt.Errorf("store: sweep %s/%s by age: %w", packID, collection, err)
		}
	}

	if rule.MaxRows > 0 {
		// Keep the NEWEST MaxRows. Ordered by created_at then entity_id so the
		// choice is deterministic when two rows share a millisecond — without the
		// tiebreak, which of two same-instant rows survives would depend on the
		// query planner, and two boxes replaying the same writes could keep
		// different data.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM pack_rows
			  WHERE pack_id = ? AND collection = ? AND entity_id NOT IN (
			        SELECT entity_id FROM pack_rows
			         WHERE pack_id = ? AND collection = ?
			         ORDER BY created_at DESC, entity_id DESC
			         LIMIT ?)`,
			packID, collection, packID, collection, rule.MaxRows); err != nil {
			return fmt.Errorf("store: sweep %s/%s by count: %w", packID, collection, err)
		}
	}
	return nil
}
