package identity

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
)

// telemetrySchema creates the relay's bounded durable telemetry queue and its
// loss-marker sidecar — the last of the operational store's durable local
// state relay/1 REL-142 scopes (identity + trust + last-applied + clock floor
// + this bounded telemetry buffer, Telemetry upstream). Both are ordinary
// small-row tables: telemetry_queue holds one row per undelivered
// durable-class event ({seq, schema, payload, trace_id, subject, recorded_at},
// REL-090), telemetry_loss_marker one row per not-yet-acknowledged loss marker.
// Neither can hold asset/media bytes — `payload` is a small event body, never
// content (`#52` gateway posture: the relay caches no media anywhere).
//
// seq is the queue's own primary key: per-relay monotonic (REL-091) and unique,
// so a durable entry is written exactly once.
//
// trace_id is the entry's record-time correlation id (REL-006), persisted for
// the same reason the entry body is: the whole point of the durable queue is a
// backlog that survives a disconnect or a power-pull, and that is precisely the
// window where correlation is most valuable. Dropping the trace on the floor at
// restart would leave a resumed backlog delivering events whose trace_id the app
// peer has to mint fresh — the exact uncorrelated-envelope hole this field
// closes, reappearing at the least convenient moment.
//
// telemetry_seq_high_water is a singleton (id=1) holding the highest seq the
// relay has ever assigned — durable OR latest-only. It is part of the bounded
// telemetry queue's durable cursor state, not a media/asset store: the queue
// row itself only records durable-class seqs (a latest-only entry is never
// persisted, REL-094), so the queue's max(seq) alone cannot tell a restart that
// a latest-only entry consumed a higher seq. Persisting the high-water on every
// record lets a restart resume strictly above every issued seq and never
// reissue one the app peer may already have observed (REL-091).
const telemetrySchema = `
CREATE TABLE IF NOT EXISTS telemetry_queue (
	seq          INTEGER PRIMARY KEY,
	schema       TEXT NOT NULL,
	payload      BLOB NOT NULL,
	subject      TEXT NOT NULL,
	recorded_at  INTEGER NOT NULL,
	trace_id     TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS telemetry_loss_marker (
	from_seq                  INTEGER NOT NULL,
	to_seq                    INTEGER NOT NULL,
	dropped_counts_by_schema  TEXT NOT NULL,
	reason                    TEXT NOT NULL,
	PRIMARY KEY (from_seq, to_seq)
);
CREATE TABLE IF NOT EXISTS telemetry_seq_high_water (
	id   INTEGER PRIMARY KEY CHECK (id = 1),
	seq  INTEGER NOT NULL
);
`

// migrateTelemetrySchema brings a telemetry_queue created by an earlier build up
// to the current column set. `CREATE TABLE IF NOT EXISTS` is a no-op against an
// existing table, so a relay that has been running since before trace_id existed
// would otherwise keep a four-column queue and fail every insert — the whole
// durable backlog silently unwritable. The column check is identity.go's shared
// hasColumn, a PRAGMA read rather than a blind `ALTER TABLE ... ADD COLUMN`
// whose "duplicate column" error is swallowed: swallowing an error class to
// make a statement idempotent also swallows the unrelated failures that share
// it.
func migrateTelemetrySchema(db *sql.DB) error {
	hasTraceID, err := hasColumn(db, "telemetry_queue", "trace_id")
	if err != nil {
		return err
	}
	if hasTraceID {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE telemetry_queue ADD COLUMN trace_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add telemetry_queue.trace_id: %w", err)
	}
	return nil
}

// AppendTelemetry durably persists one durable-class telemetry entry to the
// relay's bounded queue (REL-090/093), so a committed entry survives a
// power-pull. A latest-only entry (device.heartbeat, box.vitals) or a schema
// this channel does not carry is NOT persisted (REL-094/095): it is a periodic
// snapshot superseded in place, never durable local state — the class gate
// lives here so no caller can slip a superseded snapshot into durable storage.
func (s *Store) AppendTelemetry(e telemetry.Entry) error {
	if class, ok := telemetry.ClassOf(e.Schema); !ok || class != telemetry.Durable {
		return nil // latest-only / unknown schema: never durably persisted (REL-094)
	}
	_, err := s.db.Exec(
		`INSERT INTO telemetry_queue (seq, schema, payload, subject, recorded_at, trace_id) VALUES (?, ?, ?, ?, ?, ?)`,
		e.Seq, e.Schema, []byte(e.Payload), e.Subject, e.RecordedAt, e.TraceID,
	)
	if err != nil {
		return fmt.Errorf("identity: AppendTelemetry(seq=%d): %w", e.Seq, err)
	}
	return nil
}

// AppendWithEviction commits one overflow-triggering Record as a SINGLE
// transaction (REL-096/103): it appends the durable-class entry (a latest-only
// or unknown entry is gated out, exactly as AppendTelemetry gates it), prunes
// every queued entry at or below pruneThroughSeq (the drop-oldest-evicted
// lowest durable run), and replaces the persisted loss-marker set with markers.
// Wrapping all three in one WAL-committed transaction is what closes the
// silent-durable-loss window: a power-pull can no longer land the prune (the
// evicted durable row durably deleted) while the loss marker that accounts for
// it is never written — either the whole eviction is durable or none of it is.
func (s *Store) AppendWithEviction(appended telemetry.Entry, pruneThroughSeq int64, markers []telemetry.LossMarker) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("identity: AppendWithEviction: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if class, ok := telemetry.ClassOf(appended.Schema); ok && class == telemetry.Durable {
		if _, err := tx.Exec(
			`INSERT INTO telemetry_queue (seq, schema, payload, subject, recorded_at, trace_id) VALUES (?, ?, ?, ?, ?, ?)`,
			appended.Seq, appended.Schema, []byte(appended.Payload), appended.Subject, appended.RecordedAt, appended.TraceID,
		); err != nil {
			return fmt.Errorf("identity: AppendWithEviction: append(seq=%d): %w", appended.Seq, err)
		}
	}

	if _, err := tx.Exec(`DELETE FROM telemetry_queue WHERE seq <= ?`, pruneThroughSeq); err != nil {
		return fmt.Errorf("identity: AppendWithEviction: prune(%d): %w", pruneThroughSeq, err)
	}

	if _, err := tx.Exec(`DELETE FROM telemetry_loss_marker`); err != nil {
		return fmt.Errorf("identity: AppendWithEviction: clear markers: %w", err)
	}
	for _, m := range markers {
		counts, err := json.Marshal(m.DroppedCountsBySchema)
		if err != nil {
			return fmt.Errorf("identity: AppendWithEviction: marshal counts: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO telemetry_loss_marker (from_seq, to_seq, dropped_counts_by_schema, reason) VALUES (?, ?, ?, ?)`,
			m.FromSeq, m.ToSeq, string(counts), m.Reason,
		); err != nil {
			return fmt.Errorf("identity: AppendWithEviction: insert marker [%d,%d]: %w", m.FromSeq, m.ToSeq, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("identity: AppendWithEviction: commit: %w", err)
	}
	return nil
}

// SaveSeqHighWater durably advances the persisted per-relay seq high-water to
// seq (REL-091), but ONLY if seq exceeds the currently persisted high-water —
// an equal-or-lower value leaves it untouched (advance-only, mirroring the
// clock floor's monotonic discipline). It is written on every Record, including
// a latest-only entry whose body is not persisted (REL-094), so a restart can
// resume above every seq the relay ever issued and never reissue one.
func (s *Store) SaveSeqHighWater(seq int64) error {
	_, err := s.db.Exec(
		`INSERT INTO telemetry_seq_high_water (id, seq) VALUES (1, ?)
		 ON CONFLICT (id) DO UPDATE SET seq = excluded.seq WHERE excluded.seq > telemetry_seq_high_water.seq`,
		seq,
	)
	if err != nil {
		return fmt.Errorf("identity: SaveSeqHighWater(%d): %w", seq, err)
	}
	return nil
}

// LoadSeqHighWater returns the persisted per-relay seq high-water, or 0 if none
// has been persisted yet (REL-091) — the resume point a restarting Buffer
// assigns the next seq strictly above.
func (s *Store) LoadSeqHighWater() (int64, error) {
	var seq int64
	err := s.db.QueryRow(`SELECT seq FROM telemetry_seq_high_water WHERE id = 1`).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("identity: LoadSeqHighWater: %w", err)
	}
	return seq, nil
}

// PruneTelemetry durably discards every queued entry whose seq is at or below
// ackThroughSeq, keeping every entry above it (REL-092: the relay MAY discard
// entries at or below an acknowledged cursor, and MUST NOT discard any above
// it). It is also how drop-oldest overflow eviction (REL-096) is mirrored to
// the store, since drop-oldest removes a contiguous run of the lowest durable
// seqs.
func (s *Store) PruneTelemetry(ackThroughSeq int64) error {
	if _, err := s.db.Exec(`DELETE FROM telemetry_queue WHERE seq <= ?`, ackThroughSeq); err != nil {
		return fmt.Errorf("identity: PruneTelemetry(%d): %w", ackThroughSeq, err)
	}
	return nil
}

// SaveLossMarkers durably replaces the persisted loss-marker set with markers
// (the buffer's authoritative in-memory set, REL-100/103) in a single
// transaction — a full replace so retiring an acknowledged marker (REL-092)
// durably drops it and an overflow's new marker is durably recorded. Each
// marker's dropped_counts_by_schema map is JSON-encoded into its column.
func (s *Store) SaveLossMarkers(markers []telemetry.LossMarker) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("identity: SaveLossMarkers: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM telemetry_loss_marker`); err != nil {
		return fmt.Errorf("identity: SaveLossMarkers: clear: %w", err)
	}
	for _, m := range markers {
		counts, err := json.Marshal(m.DroppedCountsBySchema)
		if err != nil {
			return fmt.Errorf("identity: SaveLossMarkers: marshal counts: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO telemetry_loss_marker (from_seq, to_seq, dropped_counts_by_schema, reason) VALUES (?, ?, ?, ?)`,
			m.FromSeq, m.ToSeq, string(counts), m.Reason,
		); err != nil {
			return fmt.Errorf("identity: SaveLossMarkers: insert [%d,%d]: %w", m.FromSeq, m.ToSeq, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("identity: SaveLossMarkers: commit: %w", err)
	}
	return nil
}

// LoadTelemetry returns the persisted durable queue entries (ascending seq
// order, REL-091) and the persisted loss markers, for a telemetry.Buffer to
// resume from on construction after a restart (REL-090). It is the read half
// of the durable telemetry backing; a fresh store returns two empty slices.
func (s *Store) LoadTelemetry() ([]telemetry.Entry, []telemetry.LossMarker, error) {
	entries, err := s.loadTelemetryEntries()
	if err != nil {
		return nil, nil, err
	}
	markers, err := s.loadLossMarkers()
	if err != nil {
		return nil, nil, err
	}
	return entries, markers, nil
}

func (s *Store) loadTelemetryEntries() ([]telemetry.Entry, error) {
	rows, err := s.db.Query(`SELECT seq, schema, payload, subject, recorded_at, trace_id FROM telemetry_queue ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("identity: LoadTelemetry: query queue: %w", err)
	}
	defer rows.Close()

	var entries []telemetry.Entry
	for rows.Next() {
		var e telemetry.Entry
		var payload []byte
		if err := rows.Scan(&e.Seq, &e.Schema, &payload, &e.Subject, &e.RecordedAt, &e.TraceID); err != nil {
			return nil, fmt.Errorf("identity: LoadTelemetry: scan entry: %w", err)
		}
		e.Payload = append(json.RawMessage(nil), payload...)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: LoadTelemetry: iterate queue: %w", err)
	}
	return entries, nil
}

func (s *Store) loadLossMarkers() ([]telemetry.LossMarker, error) {
	rows, err := s.db.Query(`SELECT from_seq, to_seq, dropped_counts_by_schema, reason FROM telemetry_loss_marker ORDER BY from_seq, to_seq`)
	if err != nil {
		return nil, fmt.Errorf("identity: LoadTelemetry: query markers: %w", err)
	}
	defer rows.Close()

	var markers []telemetry.LossMarker
	for rows.Next() {
		var m telemetry.LossMarker
		var counts string
		if err := rows.Scan(&m.FromSeq, &m.ToSeq, &counts, &m.Reason); err != nil {
			return nil, fmt.Errorf("identity: LoadTelemetry: scan marker: %w", err)
		}
		if err := json.Unmarshal([]byte(counts), &m.DroppedCountsBySchema); err != nil {
			return nil, fmt.Errorf("identity: LoadTelemetry: unmarshal counts: %w", err)
		}
		markers = append(markers, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: LoadTelemetry: iterate markers: %w", err)
	}
	return markers, nil
}
