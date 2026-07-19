package identity

import (
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
)

// telemetrySchema creates the relay's bounded durable telemetry queue and its
// loss-marker sidecar — the last of the operational store's durable local
// state relay/1 REL-142 scopes (identity + trust + last-applied + clock floor
// + this bounded telemetry buffer, Telemetry upstream). Both are ordinary
// small-row tables: telemetry_queue holds one row per undelivered
// durable-class event ({seq, schema, payload, subject, recorded_at}, REL-090),
// telemetry_loss_marker one row per not-yet-acknowledged loss marker. Neither
// can hold asset/media bytes — `payload` is a small event body, never content
// (`#52` gateway posture: the relay caches no media anywhere).
//
// seq is the queue's own primary key: per-relay monotonic (REL-091) and unique,
// so a durable entry is written exactly once.
const telemetrySchema = `
CREATE TABLE IF NOT EXISTS telemetry_queue (
	seq          INTEGER PRIMARY KEY,
	schema       TEXT NOT NULL,
	payload      BLOB NOT NULL,
	subject      TEXT NOT NULL,
	recorded_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS telemetry_loss_marker (
	from_seq                  INTEGER NOT NULL,
	to_seq                    INTEGER NOT NULL,
	dropped_counts_by_schema  TEXT NOT NULL,
	reason                    TEXT NOT NULL,
	PRIMARY KEY (from_seq, to_seq)
);
`

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
		`INSERT INTO telemetry_queue (seq, schema, payload, subject, recorded_at) VALUES (?, ?, ?, ?, ?)`,
		e.Seq, e.Schema, []byte(e.Payload), e.Subject, e.RecordedAt,
	)
	if err != nil {
		return fmt.Errorf("identity: AppendTelemetry(seq=%d): %w", e.Seq, err)
	}
	return nil
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
	rows, err := s.db.Query(`SELECT seq, schema, payload, subject, recorded_at FROM telemetry_queue ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("identity: LoadTelemetry: query queue: %w", err)
	}
	defer rows.Close()

	var entries []telemetry.Entry
	for rows.Next() {
		var e telemetry.Entry
		var payload []byte
		if err := rows.Scan(&e.Seq, &e.Schema, &payload, &e.Subject, &e.RecordedAt); err != nil {
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
