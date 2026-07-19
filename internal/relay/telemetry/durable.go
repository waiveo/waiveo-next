package telemetry

// DurableStore is the persistent backing a Buffer writes its durable-class
// telemetry through, so a committed entry survives a power-pull (REL-090: the
// relay MUST buffer telemetry durably while disconnected). The relay's
// operational store (internal/relay/identity) implements it against SQLite
// under WAL + FULL fsync; tests fake it in memory. Only durable-class entries
// (REL-093) are ever persisted — the store's AppendTelemetry drops a
// latest-only entry (REL-094), so a caller cannot slip a superseded snapshot
// into durable state.
//
// The Buffer holds the authoritative in-memory copy of its loss markers and
// writes the whole set through with SaveLossMarkers (a full replace) whenever
// it changes, so retiring an acknowledged marker (REL-092) durably drops it
// and an overflow's new marker (REL-096/103) is durably recorded.
type DurableStore interface {
	// AppendTelemetry durably persists one durable-class entry; a latest-only
	// or unknown-schema entry is a no-op (REL-093/094).
	AppendTelemetry(e Entry) error
	// AppendWithEviction durably persists one overflow-triggering Record as a
	// SINGLE atomic transaction (REL-096/103): it appends the durable-class
	// entry (a latest-only/unknown entry is gated out, as in AppendTelemetry),
	// prunes the drop-oldest-evicted lowest durable run (every seq at or below
	// pruneThroughSeq), and replaces the persisted loss-marker set with markers
	// — all committed together. Because the three effects share one transaction,
	// an abrupt process kill can never leave the evicted durable entry gone from
	// the queue with no loss marker accounting for it (the silent, unrecoverable
	// durable-class loss REL-103 forbids); either the whole eviction lands
	// durably or none of it does.
	AppendWithEviction(appended Entry, pruneThroughSeq int64, markers []LossMarker) error
	// PruneTelemetry durably removes every persisted entry whose seq is at or
	// below ackThroughSeq, keeping every entry above it (REL-092).
	PruneTelemetry(ackThroughSeq int64) error
	// SaveLossMarkers durably replaces the persisted loss-marker set with
	// markers (the buffer's current authoritative set, REL-100/103).
	SaveLossMarkers(markers []LossMarker) error
	// SaveSeqHighWater durably advances the persisted per-relay seq high-water
	// to seq (advance-only; a lower value is ignored). It is written on EVERY
	// Record — including a latest-only entry whose body is not persisted
	// (REL-094) — because that entry still consumed a seq from the shared
	// monotonic counter and may already have reached the app peer, so a restart
	// MUST resume above it and never reissue it (REL-091).
	SaveSeqHighWater(seq int64) error
	// LoadTelemetry returns the persisted durable entries (seq order) and loss
	// markers, for a Buffer to resume from on construction (REL-090).
	LoadTelemetry() (entries []Entry, markers []LossMarker, err error)
	// LoadSeqHighWater returns the persisted per-relay seq high-water (0 if none
	// has been persisted yet), so a restarted Buffer resumes seq assignment
	// strictly above every seq it ever issued — durable AND latest-only
	// (REL-091 monotonicity across a restart).
	LoadSeqHighWater() (int64, error)
}

// NewDurableBuffer returns a Buffer bounded to capacity durable-class entries
// (REL-096) that writes through to store: it reloads store's persisted
// entries + loss markers on construction (REL-090, resuming a backlog buffered
// while disconnected), resumes seq assignment above the persisted seq
// high-water (REL-091 monotonicity across a restart), and mirrors every
// subsequent Record / overflow eviction / ack-prune into store so a committed
// durable entry survives an abrupt process kill.
//
// The resumed nextSeq is taken from the persisted seq high-water, NOT merely
// from the highest reloaded durable entry: the reload set holds only
// durable-class rows (REL-094 keeps latest-only entries out of durable state),
// yet a latest-only entry consumed a seq from the SAME shared counter before
// the crash and may already have reached the app peer — resuming only above the
// durable max would reissue that seq to a different entry (a REL-091 uniqueness
// break the app peer's seq-keyed dedup can turn into silent durable-class loss,
// REL-103). The reloaded durable entries still floor the resume point (belt and
// suspenders, in case the high-water write for the newest durable append had
// not yet committed when the queue row had).
//
// It returns an error if the initial reload fails — a durable buffer that
// silently started empty after a failed reload would be indistinguishable from
// durable loss, which REL-090 forbids.
func NewDurableBuffer(store DurableStore, capacity int) (*Buffer, error) {
	entries, markers, err := store.LoadTelemetry()
	if err != nil {
		return nil, err
	}
	highWater, err := store.LoadSeqHighWater()
	if err != nil {
		return nil, err
	}
	b := &Buffer{capacity: capacity, store: store}
	b.entries = entries
	b.lossMarkers = markers
	b.nextSeq = highWater // resume above every seq ever issued (durable + latest-only), REL-091
	for _, e := range entries {
		if e.Seq > b.nextSeq {
			b.nextSeq = e.Seq // floor: never below a still-buffered durable seq
		}
	}
	return b, nil
}

// StoreErr returns the first error a durable write-through hit since the last
// call (Record / overflow / ack-prune cannot themselves return an error, so a
// persist failure is surfaced here rather than silently swallowed — a caller
// polls it to detect a store that has stopped accepting durable writes).
// Reading it clears it. A non-durable Buffer (NewBuffer) never sets it.
func (b *Buffer) StoreErr() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	err := b.storeErr
	b.storeErr = nil
	return err
}

// noteStoreErr records the first write-through error (first-error-wins until
// StoreErr clears it). The caller holds b.mu.
func (b *Buffer) noteStoreErr(err error) {
	if err != nil && b.storeErr == nil {
		b.storeErr = err
	}
}
