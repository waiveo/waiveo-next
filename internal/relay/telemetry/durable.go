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
	// PruneTelemetry durably removes every persisted entry whose seq is at or
	// below ackThroughSeq, keeping every entry above it (REL-092).
	PruneTelemetry(ackThroughSeq int64) error
	// SaveLossMarkers durably replaces the persisted loss-marker set with
	// markers (the buffer's current authoritative set, REL-100/103).
	SaveLossMarkers(markers []LossMarker) error
	// LoadTelemetry returns the persisted durable entries (seq order) and loss
	// markers, for a Buffer to resume from on construction (REL-090).
	LoadTelemetry() (entries []Entry, markers []LossMarker, err error)
}

// NewDurableBuffer returns a Buffer bounded to capacity durable-class entries
// (REL-096) that writes through to store: it reloads store's persisted
// entries + loss markers on construction (REL-090, resuming a backlog buffered
// while disconnected), continues seq assignment above the highest reloaded seq
// (REL-091 monotonicity across a restart), and mirrors every subsequent
// Record / overflow eviction / ack-prune into store so a committed durable
// entry survives an abrupt process kill.
//
// It returns an error if the initial LoadTelemetry fails — a durable buffer
// that silently started empty after a failed reload would be indistinguishable
// from durable loss, which REL-090 forbids.
func NewDurableBuffer(store DurableStore, capacity int) (*Buffer, error) {
	entries, markers, err := store.LoadTelemetry()
	if err != nil {
		return nil, err
	}
	b := &Buffer{capacity: capacity, store: store}
	b.entries = entries
	b.lossMarkers = markers
	for _, e := range entries {
		if e.Seq > b.nextSeq {
			b.nextSeq = e.Seq // resume monotonic seq above the highest reloaded (REL-091)
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
