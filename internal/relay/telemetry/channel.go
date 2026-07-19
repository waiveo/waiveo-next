package telemetry

import (
	"sync"
	"time"
)

// Upstream is the app-peer transport the Channel pushes telemetry batches over
// (REL-090). Push delivers one telemetry.push and returns the app peer's
// telemetry.ack (REL-092), or an error if the batch was not delivered. It is
// an interface so the real relay/1 wire (which lands later) is injected here,
// and tests fake it — this package holds only the in-memory buffer/cursor logic.
type Upstream interface {
	Push(PushBatch) (Ack, error)
}

// Backoff is REL-097's retry policy: given the 1-based retry attempt number
// (attempt 1 is the first retry after the initial failed push), it returns how
// long to wait before that attempt and whether to retry at all. retry == false
// stops retrying and leaves the still-unacknowledged batch buffered for the
// next reconnect — REL-097 requires the retry span reconnects, not that a
// single Flush block forever.
type Backoff interface {
	Next(attempt int) (wait time.Duration, retry bool)
}

// ExponentialBackoff is a Backoff that starts at Base and doubles each retry
// attempt, clamped to Max (when Max > 0), for at most MaxAttempts retries
// (MaxAttempts <= 0 means unbounded). It is the default REL-097 backoff policy;
// the injectable-clock discipline (relay/1's "Timing-dependent behavior …
// against an injectable clock") means the Channel takes the wait from here and
// sleeps via an injected sleep func rather than reading wall-clock time itself.
type ExponentialBackoff struct {
	Base        time.Duration
	Max         time.Duration
	MaxAttempts int
}

// Next reports the wait before retry attempt n (1-based) and whether to retry.
func (e ExponentialBackoff) Next(attempt int) (time.Duration, bool) {
	if e.MaxAttempts > 0 && attempt > e.MaxAttempts {
		return 0, false
	}
	d := e.Base
	for i := 1; i < attempt; i++ {
		d *= 2
		if e.Max > 0 && d >= e.Max {
			return e.Max, true
		}
	}
	if e.Max > 0 && d > e.Max {
		d = e.Max
	}
	return d, true
}

// Channel is the relay's telemetry upstream channel (REL-090/092/097): it wraps
// a Buffer and an Upstream transport, builds a telemetry.push batch from the
// buffer's pending entries plus its loss markers (loss_markers present on every
// push, REL-090), pushes it, and on a received Ack advances retention — discard
// entries whose seq is at or below ack_through_seq, NEVER those above it
// (REL-092), and retire acked loss markers (REL-092/102). A failed push is
// retried with backoff (REL-097) without re-sending any entry already covered
// by a received ack_through_seq. A Channel is safe for concurrent Flush calls;
// its Buffer may be written concurrently by the entry producer (the engine /
// device plane).
type Channel struct {
	mu      sync.Mutex
	buf     *Buffer
	up      Upstream
	backoff Backoff
	// sleep waits out a backoff interval; it is a field (defaulting to
	// time.Sleep) so the injectable-clock test harness can substitute a
	// non-blocking recorder without wall-clock sleeps (relay/1 §Conformance).
	sleep func(time.Duration)
	// ackedThrough is the high-water mark of every ack_through_seq received so
	// far — the cursor REL-097 uses to guarantee an entry already covered by a
	// prior ack is never re-sent, even if it briefly lingered in the buffer.
	ackedThrough int64
}

// NewChannel returns a Channel pushing buf's telemetry to up, retrying failed
// pushes under backoff (REL-097). If backoff is nil, a single-attempt policy is
// used (a failed push is not retried within one Flush; the batch rides the next
// Flush/reconnect). sleep defaults to time.Sleep.
func NewChannel(buf *Buffer, up Upstream, backoff Backoff) *Channel {
	if backoff == nil {
		backoff = ExponentialBackoff{MaxAttempts: 1}
	}
	return &Channel{buf: buf, up: up, backoff: backoff, sleep: time.Sleep}
}

// Flush builds a telemetry.push from the buffer's pending entries plus its loss
// markers (REL-090) and delivers it, retrying under backoff on failure (REL-097).
// On a received Ack it advances retention: entries with seq <= ack_through_seq
// are discarded and acked loss markers retired, while entries above ack_through_seq
// are retained for a later push (REL-092). It returns the Ack received on success,
// or the last upstream error once backoff attempts are exhausted (leaving the
// unacknowledged batch buffered for the next reconnect). A Flush with nothing to
// send performs no push and returns a zero Ack.
func (c *Channel) Flush() (Ack, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var lastErr error
	for attempt := 0; ; attempt++ {
		batch := c.buildBatch()
		if len(batch.Entries) == 0 && len(batch.LossMarkers) == 0 {
			return Ack{}, nil // nothing pending — no wire traffic
		}
		ack, err := c.up.Push(batch)
		if err == nil {
			c.applyAck(ack)
			return ack, nil
		}
		lastErr = err
		wait, retry := c.backoff.Next(attempt + 1)
		if !retry {
			return Ack{}, lastErr
		}
		c.sleep(wait)
	}
}

// buildBatch assembles the next telemetry.push (REL-090): the buffer's pending
// entries — filtered to exclude any already covered by a received ack_through_seq
// (REL-097; retention already removes those, this is a defensive re-assertion of
// the cursor) — plus the buffer's not-yet-acknowledged loss markers, which are
// present (non-nil, possibly empty) on every push (REL-090) and re-sent until
// acknowledged (REL-102). The caller holds c.mu.
func (c *Channel) buildBatch() PushBatch {
	pending := c.buf.Pending()
	kept := pending[:0]
	for _, e := range pending {
		if e.Seq > c.ackedThrough {
			kept = append(kept, e)
		}
	}
	return PushBatch{
		Entries:     kept,
		LossMarkers: c.buf.PendingLossMarkers(), // always non-nil (REL-090)
	}
}

// applyAck advances the cursor and retention from a received telemetry.ack: it
// raises the ackedThrough high-water mark (REL-097) and tells the buffer to
// discard entries at/below ack_through_seq while keeping every entry above it
// (REL-092), and to retire the loss markers the ack names (REL-092/102). The
// caller holds c.mu.
func (c *Channel) applyAck(ack Ack) {
	if ack.AckThroughSeq > c.ackedThrough {
		c.ackedThrough = ack.AckThroughSeq
	}
	c.buf.applyAck(ack.AckThroughSeq, ack.LossMarkersAcked)
}

// applyAck advances the buffer's retention to a received telemetry.ack cursor
// (REL-092): it discards every buffered entry whose seq is at or below
// ackThroughSeq and retains every entry whose seq exceeds it (the relay MUST NOT
// discard an entry above the ack). It also retires each buffered loss marker
// whose {from_seq, to_seq} pair appears in markersAcked (REL-092/102); a marker
// not yet named rides the next push again (resend-until-acknowledged, REL-102).
// This method lives with the Channel (Task 4) because retention is the channel's
// cursor semantics acting on the buffer's storage; it takes b.mu itself.
func (b *Buffer) applyAck(ackThroughSeq int64, markersAcked []SeqRange) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Discard entries at/below the cursor; keep everything above it (REL-092).
	kept := b.entries[:0]
	for _, e := range b.entries {
		if e.Seq > ackThroughSeq {
			kept = append(kept, e)
		}
	}
	for i := len(kept); i < len(b.entries); i++ {
		b.entries[i] = Entry{} // release the discarded Entry (and its payload)
	}
	b.entries = kept

	if len(markersAcked) == 0 {
		return
	}
	acked := make(map[SeqRange]bool, len(markersAcked))
	for _, r := range markersAcked {
		acked[r] = true
	}
	keptM := b.lossMarkers[:0]
	for _, m := range b.lossMarkers {
		if acked[SeqRange{FromSeq: m.FromSeq, ToSeq: m.ToSeq}] {
			continue // acknowledged (REL-092/102) — retire it
		}
		keptM = append(keptM, m)
	}
	for i := len(keptM); i < len(b.lossMarkers); i++ {
		b.lossMarkers[i] = LossMarker{}
	}
	b.lossMarkers = keptM
}
