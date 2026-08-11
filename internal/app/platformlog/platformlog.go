// Package platformlog is the in-process capture of this box's own log output —
// the thing an operator reads when a screen is dark and they cannot SSH in.
//
// # What it captures, and why capture rather than instrument
//
// It is an io.Writer. The process installs it as a TEE beside stderr on the
// standard logger, and from that moment every line this binary already writes —
// every `log.Printf` in the feeder, the app, the relay-connection server, the
// event pipeline, and net/http's own error logger — lands in a bounded ring
// buffer as well as on stderr.
//
// Capturing at the writer is a deliberate choice over adding a structured
// logging call beside each existing one. There are several hundred log sites in
// this tree; an instrumented subset would produce a diagnostics page that is
// silent about exactly the code nobody remembered to instrument, which is the
// code a novel failure comes from. A tee cannot have that gap: if it is on
// stderr it is in the buffer, with nothing to remember at the call site.
//
// The cost of that choice is that the records are LINES, not structured events,
// so `level` and `source` are DERIVED by reading the line rather than declared
// by whoever wrote it. That is stated plainly on the wire (the api/1 response
// documents both as derived) and the raw message always rides along, so an
// operator is never shown a classification without the text it came from.
//
// # What it deliberately is NOT
//
//   - It is NOT the audit trail. security-model/1 SEC-150 makes events/1's
//     `audit.event` the platform's sole audit mechanism, it is durable, and it
//     is read through the event stream (the console's Activity page). This
//     buffer is volatile, unordered with respect to that log, and holds
//     operational chatter. Nothing here may be relied on as a record of who did
//     what.
//   - It is NOT journald. In production the feeder runs as a systemd unit, so
//     journald holds the same lines PLUS everything from before this process
//     started and everything from every previous boot — including the crash
//     that caused the restart an operator is investigating. This buffer starts
//     empty at boot and can only ever show the CURRENT process's lines. That
//     limitation is published on the api/1 response itself (`retained_from_ms`,
//     `dropped`) rather than left for an operator to infer from a suspiciously
//     short list.
//
// # Bounded, and honest about being bounded
//
// The ring holds a fixed number of records and overwrites the oldest. A
// diagnostics buffer that grew without limit would be a memory leak proportional
// to how noisy a failure is — that is, largest exactly when the box is already
// in trouble. `Dropped` counts what the ring has overwritten since boot, so a
// reader can tell "nothing happened" from "everything happened and scrolled
// away".
package platformlog

import (
	"strings"
	"sync"
)

// Level is a record's DERIVED severity. Three values, because that is as much
// as can be read out of a line honestly: a finer ladder (debug/trace/fatal)
// would be inventing distinctions the source text does not carry.
type Level string

const (
	LevelError Level = "error"
	LevelWarn  Level = "warn"
	LevelInfo  Level = "info"
)

// Levels is the closed set, in descending severity — the order a console lists
// a filter in, and the set an api/1 query parameter is validated against.
var Levels = []Level{LevelError, LevelWarn, LevelInfo}

// ValidLevel reports whether s names a level. Used to refuse a filter value
// rather than silently matching nothing: a filter that answers "no logs" for a
// typo is read by an operator as "the box is quiet", which is the opposite of
// the truth and the worst possible answer from a diagnostics page.
func ValidLevel(s string) bool {
	for _, l := range Levels {
		if string(l) == s {
			return true
		}
	}
	return false
}

// DefaultSource is the source attributed to a line that carries no recognisable
// prefix. Named rather than "" so a console's source filter has something to
// show and a reader is never offered a blank option.
const DefaultSource = "platform"

// Record is one captured line.
type Record struct {
	// Seq is a monotonic per-process counter, assigned at capture. It exists so
	// a reader can tell two identical lines apart and so ordering survives a
	// clock that steps backwards mid-boot (which an appliance's does, before NTP
	// settles) — TS alone cannot do either.
	Seq int64
	// TSMs is the app clock at capture. Not read out of the line: the stdlib
	// logger's own date/time prefix is stripped, because it is written in local
	// time with no zone and every other timestamp this platform publishes is
	// epoch-millis UTC.
	TSMs int64
	// Level and Source are DERIVED from Message by classify (see the package
	// doc). They are a reading aid over the text, never a claim the writer made.
	Level  Level
	Source string
	// Message is the line with the stdlib prefix and the source prefix removed —
	// what is left after the parts that became the fields above.
	Message string
	// Raw is the line exactly as it was written, minus only the stdlib
	// date/time prefix. Kept because every derivation above is a heuristic and
	// an operator diagnosing an unfamiliar failure must be able to see what was
	// actually logged, not this package's reading of it.
	Raw string
}

// DefaultCapacity is the ring's size when a caller passes none: enough to hold
// a boot plus a busy few minutes, small enough that the worst case is a few
// megabytes on a box whose whole job is elsewhere.
const DefaultCapacity = 2000

// maxLineBytes caps one captured line. A single pathological line (a dumped
// payload, a stack trace concatenated by a caller) must not be able to consume
// the buffer's whole memory budget by itself. The tail is dropped and the line
// is marked, rather than the line being discarded: a truncated diagnostic is
// worth far more than none.
const maxLineBytes = 4096

// truncationMarker is appended to a line cut at maxLineBytes, so a reader is
// never shown a sentence that simply stops and left to wonder whether the
// process died mid-write.
const truncationMarker = "… [truncated]"

// Buffer is the ring. Safe for concurrent use: the standard logger serializes
// its own writes, but Records is read from HTTP handlers on other goroutines.
type Buffer struct {
	nowMs func() int64

	mu   sync.Mutex
	ring []Record
	// next is the index the NEXT record is written at; total is how many have
	// ever been written. Together they give both the wrap point and Dropped
	// without a second counter that could disagree.
	next  int
	total int64
	// partial accumulates bytes from a Write that did not end at a newline.
	// io.Writer makes no promise a write is one whole line, and a record built
	// from half a line would be classified on half a sentence.
	partial strings.Builder
}

// New builds a Buffer of the given capacity (<=0 means DefaultCapacity).
// nowMs is required for the same reason internal/app/screens requires one: every
// value this type produces is a timestamp, and a nil clock could only ever
// answer zero.
func New(capacity int, nowMs func() int64) *Buffer {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	if nowMs == nil {
		panic("platformlog: New requires a clock")
	}
	return &Buffer{nowMs: nowMs, ring: make([]Record, capacity)}
}

// Write implements io.Writer, splitting p into lines and capturing each.
//
// It NEVER returns an error and never returns a short write. It is installed
// inside an io.MultiWriter on the standard logger, and io.MultiWriter aborts on
// the first writer that errors — so a failure here would silence stderr (and
// therefore journald) as collateral. A diagnostics buffer that can take the real
// log down with it is worse than no diagnostics buffer.
func (b *Buffer) Write(p []byte) (int, error) {
	n := len(p)
	b.mu.Lock()
	defer b.mu.Unlock()

	s := string(p)
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			b.partial.WriteString(s)
			break
		}
		line := s[:i]
		if b.partial.Len() > 0 {
			line = b.partial.String() + line
			b.partial.Reset()
		}
		b.captureLocked(line)
		s = s[i+1:]
	}
	return n, nil
}

// captureLocked appends one whole line. b.mu is held.
func (b *Buffer) captureLocked(line string) {
	line = strings.TrimRight(line, "\r")
	line = stripStdlibPrefix(line)
	if strings.TrimSpace(line) == "" {
		// A blank line separates paragraphs on a terminal and carries nothing a
		// filtered list can show. Dropping it keeps the ring's capacity spent on
		// records an operator can read.
		return
	}
	if len(line) > maxLineBytes {
		line = line[:maxLineBytes] + truncationMarker
	}
	source, message := splitSource(line)
	rec := Record{
		Seq:     b.total + 1,
		TSMs:    b.nowMs(),
		Level:   classify(line),
		Source:  source,
		Message: message,
		Raw:     line,
	}
	b.ring[b.next] = rec
	b.next = (b.next + 1) % len(b.ring)
	b.total++
}

// Filter narrows a read. A zero Filter matches everything.
type Filter struct {
	// Level, when non-empty, keeps only records at exactly that level. Not "at
	// or above": an operator filtering to `warn` on a page that also offers
	// `error` means the warnings, and a threshold would quietly re-show the
	// errors they just filtered away from.
	Level Level
	// Source, when non-empty, keeps only records from exactly that source.
	Source string
	// Contains, when non-empty, keeps only records whose raw line contains it,
	// case-insensitively.
	Contains string
	// Limit caps how many records are returned, counted from the NEWEST. Zero
	// or negative returns everything retained.
	Limit int
}

// Snapshot is one read of the buffer: the matching records plus the facts a
// reader needs to interpret an empty or short list honestly.
type Snapshot struct {
	// Records match the filter, NEWEST FIRST — the order a diagnostics page
	// reads in, so the line that describes the failure an operator just saw is
	// the first one on screen.
	Records []Record
	// Matched is how many records matched before Limit was applied, so a page
	// showing 200 of 4000 can say so.
	Matched int
	// Retained is how many records the ring currently holds, at any level.
	Retained int
	// Capacity is the ring's size.
	Capacity int
	// Dropped is how many records have been overwritten since this process
	// started. Non-zero means the buffer has wrapped and the oldest lines of
	// this boot are already gone — which is exactly when an operator should be
	// reading journald instead, and the api/1 response says so.
	Dropped int64
	// Sources is every distinct source currently retained, sorted, regardless of
	// the filter — a filter control cannot be built out of the results the
	// filter already excluded.
	Sources []string
	// RetainedFromMs is the timestamp of the OLDEST retained record, or 0 when
	// nothing is retained. It is the honest answer to "how far back does this
	// page see", which is never "since the box booted".
	RetainedFromMs int64
	// LevelCounts is the count of retained records per level, unfiltered — what
	// a header renders ("3 errors, 12 warnings") and what makes an all-quiet box
	// distinguishable from a filter that excluded everything.
	LevelCounts map[Level]int
}

// Read applies f and returns a Snapshot.
func (b *Buffer) Read(f Filter) Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()

	snap := Snapshot{
		Capacity:    len(b.ring),
		LevelCounts: map[Level]int{LevelError: 0, LevelWarn: 0, LevelInfo: 0},
	}
	if b.total > int64(len(b.ring)) {
		snap.Dropped = b.total - int64(len(b.ring))
	}

	// Walk oldest→newest so RetainedFromMs and Sources are computed over the
	// whole retained set, then reverse the kept slice at the end.
	seen := map[string]struct{}{}
	var kept []Record
	for i := int64(0); i < int64(len(b.ring)); i++ {
		idx := (b.next + int(i)) % len(b.ring)
		rec := b.ring[idx]
		if rec.Seq == 0 {
			// An unwritten ring slot. Only possible before the first wrap.
			continue
		}
		snap.Retained++
		snap.LevelCounts[rec.Level]++
		if _, ok := seen[rec.Source]; !ok {
			seen[rec.Source] = struct{}{}
			snap.Sources = append(snap.Sources, rec.Source)
		}
		if snap.RetainedFromMs == 0 || rec.TSMs < snap.RetainedFromMs {
			snap.RetainedFromMs = rec.TSMs
		}
		if !f.matches(rec) {
			continue
		}
		snap.Matched++
		kept = append(kept, rec)
	}

	// Newest first, then Limit — in that order, so a limited page is the most
	// RECENT matches rather than the oldest ones that happen to be first in the
	// ring.
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	if f.Limit > 0 && len(kept) > f.Limit {
		kept = kept[:f.Limit]
	}
	snap.Records = kept
	sortStrings(snap.Sources)
	return snap
}

// matches reports whether rec passes f.
func (f Filter) matches(rec Record) bool {
	if f.Level != "" && rec.Level != f.Level {
		return false
	}
	if f.Source != "" && rec.Source != f.Source {
		return false
	}
	if f.Contains != "" && !strings.Contains(strings.ToLower(rec.Raw), strings.ToLower(f.Contains)) {
		return false
	}
	return true
}

// sortStrings is an insertion sort over the (small, bounded) source set. Used
// instead of pulling in sort for one call on a slice whose length is the number
// of distinct log prefixes in one process.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
