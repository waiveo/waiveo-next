package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	stdlog "log"
	"sync"

	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// eventlog.go is the persistent half of events/1's durable event log — the
// implementation of events.Log a deployment wires, where the in-memory
// events.EventLog is what a fixture or a conformance driver wires.
//
// It exists because "durable event" is the envelope's own definition: EVT-010
// records one "retained for a bounded window regardless of whether any
// subscriber is connected when it occurs", and EVT-141 makes a resume from a
// point older than that window a marked retention_expired gap rather than a
// silent hole. A log whose window is a process lifetime satisfies neither: every
// event, every retention boundary and every resumable cursor ends at the next
// restart, so `resume_from` is only meaningful to a client that never
// reconnects across one.
//
// The stake is larger than the stream. security-model/1 SEC-150 makes
// `audit.event` the platform's SOLE audit mechanism — there is deliberately no
// second audit schema — so every login, every session and API-key issuance and
// revocation, and every mutating api/1 request files its only permanent record
// through this log. An audit trail that evaporates on restart is not an audit
// trail.
//
// Two deliberate departures from the resource-table CRUD, both shared with
// jobs.go (which this file follows):
//
//   - An event is NOT a resource Kind. It has no revision, no external_id and
//     no ETag surface; it is an immutable record, never updated after it is
//     written, and it does not belong in the desired-state projection.
//   - An event write does NOT bump the store generation, and so runs through
//     runWriteTx rather than writeTx. The generation is the feeder's
//     desired-state cursor (relay/1 REL-052); a heartbeat landing is not
//     desired state, and bumping it would nudge every connected relay to
//     re-pull an unchanged snapshot once per telemetry record.
//
// Retention is per CLASS (events.RetentionPolicy), not one horizon: a window
// each class's rows expire past, and a per-class row cap as the disk backstop.
// Per-class is what makes EVT-082 hold under pressure — a single global
// oldest-first cap would make an hour of telemetry evict the oldest audit
// records to make room for itself.
//
// Expiry is evaluated against an INJECTED clock (nowMs) and applied by an
// explicit Prune, not by a timer this type owns. A test drives the clock
// forward and calls Prune; the feeder calls it at boot and on a ticker. A
// retention window is a floor ("retained for at least this long"), so a row
// that outlives its window until the next sweep is inside the guarantee, while
// one deleted early would not be.

// eventsSchema creates the durable event log and its retention bookkeeping.
//
// The envelope is stored as COLUMNS for every EVT-010 field plus the payload as
// JSON, rather than as one opaque blob, because retention is a query over
// retention_class and ts and scope filtering is a read of scope_node — all of
// which a blob would force into a full decode of every row. id is the primary
// key, which gives the log its two structural properties for free: appends are
// idempotent on id (EVT-135's dedup key) and every read is already in recording
// order, since an id is a ULID and lexicographic order IS recording order
// (EVT-011).
//
// The two indexes are the two retention sweeps: (retention_class, id) for the
// per-class row cap, (retention_class, ts) for the per-class window.
//
// event_log_meta holds evicted_through: the highest id ever removed from the
// log. It has to be persisted, not merely remembered, or a restart would answer
// "nothing has ever been evicted" and a subscriber resuming across it would get
// a silent hole exactly where EVT-143 requires a marked gap.
const eventsSchema = `
CREATE TABLE IF NOT EXISTS events (
	id               TEXT PRIMARY KEY,
	schema           TEXT NOT NULL,
	ts               INTEGER NOT NULL,
	scope_node       TEXT NOT NULL DEFAULT '',
	trace_id         TEXT NOT NULL DEFAULT '',
	cost_class       TEXT NOT NULL DEFAULT '',
	retention_class  TEXT NOT NULL DEFAULT '',
	origin           TEXT NOT NULL DEFAULT '',
	origin_principal TEXT NOT NULL DEFAULT '',
	payload          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_class_id ON events (retention_class, id);
CREATE INDEX IF NOT EXISTS events_class_ts ON events (retention_class, ts);
CREATE TABLE IF NOT EXISTS event_log_meta (
	id              INTEGER PRIMARY KEY CHECK (id = 1),
	evicted_through TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO event_log_meta (id, evicted_through) VALUES (1, '');
`

// eventColumns is the projection scanEvent reads, in order.
const eventColumns = "id, schema, ts, scope_node, trace_id, cost_class, retention_class, origin, origin_principal, payload"

// EventLog is the SQLite-backed events.Log: the durable event log the
// /events/v1 SSE transport streams from, the relay telemetry ingest and the
// security-model auditor write into, and a restart reads back intact.
//
// It is safe for concurrent use — reads take the store's read lock, writes its
// write lock — but the live transport (internal/app/eventsse.Hub) serializes
// every call anyway, because events.Log's contract is written for the in-memory
// implementation that requires it.
type EventLog struct {
	store  *Store
	policy events.RetentionPolicy
	nowMs  func() int64
	onErr  func(error)

	// mu guards evictedThrough, the cached copy of the persisted watermark.
	// It is cached because every live drain asks for it (EvictedAfter) and it
	// changes only through this type's own eviction paths — a query per wake per
	// subscriber to re-read a value only we write would be pure overhead.
	mu             sync.Mutex
	evictedThrough string
}

// EventLog implements the events/1 log substrate.
var _ events.Log = (*EventLog)(nil)

// OpenEventLog returns the persistent event log over s.
//
// policy is the retention configuration (events.DefaultRetentionPolicy for the
// shipping one). nowMs is the injected clock every expiry decision is evaluated
// against — nil defaults to THE STORE'S OWN clock, the one its opener chose.
//
// That default used to be a bare time.Now, which made a nil here a silent third
// clock in the process: this log rides the store, its retention decides how long
// the store's own audit trail survives, and measuring that against a different
// reading than the events were stamped with is measuring a skew rather than an
// age. Inheriting the store's clock means a nil is a caller declining to make a
// choice — not a caller opting out of the deployment's.
//
// onErr receives every storage failure the events.Log surface cannot return
// (see the type's own doc); nil defaults to the standard logger. It is not
// optional in spirit: a failed append is a durable record that was NOT written,
// and on the audit path that is the platform failing to record something
// SEC-150 says it must. The one thing this layer can do about it is say so.
//
// It reads the persisted eviction watermark up front and FAILS if it cannot: a
// log that silently started at "nothing has ever been evicted" would answer
// every mid-stream loss check with "no gap" (EVT-142/143).
func OpenEventLog(s *Store, policy events.RetentionPolicy, nowMs func() int64, onErr func(error)) (*EventLog, error) {
	if s == nil {
		return nil, errors.New("store: open event log: nil store")
	}
	if nowMs == nil {
		nowMs = s.nowMs
	}
	if onErr == nil {
		onErr = func(err error) { stdlog.Printf("store: event log: %v", err) }
	}
	l := &EventLog{store: s, policy: policy, nowMs: nowMs, onErr: onErr}

	s.mu.RLock()
	defer s.mu.RUnlock()
	var watermark string
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT evicted_through FROM event_log_meta WHERE id = 1`).Scan(&watermark); err != nil {
		return nil, fmt.Errorf("store: read event-log eviction watermark: %w", err)
	}
	l.evictedThrough = watermark
	return l, nil
}

// EventLog returns the persistent event log over this store. It is the
// method form of OpenEventLog, for callers that already hold the store.
func (s *Store) EventLog(policy events.RetentionPolicy, nowMs func() int64, onErr func(error)) (*EventLog, error) {
	return OpenEventLog(s, policy, nowMs, onErr)
}

// --- the events.Log surface -------------------------------------------------

// Append records env durably (events.Log). It is idempotent on id: a
// redelivered id updates nothing and duplicates nothing, which is what lets a
// subscriber use id as its dedup key across a reconnect (EVT-135).
//
// The write and the appended class's row-cap enforcement commit in ONE
// transaction, so the log is never observable holding more rows of a class than
// the policy allows, and an eviction never commits without the watermark that
// records it.
//
// A rejected id is refused rather than stored: every id this store persists is a
// canonical ULID (DAT-005a), and an event id is additionally the ordering key
// every resume and every gap bound is computed from (EVT-011) — a non-ULID here
// would sort by rules nothing else in the system shares.
//
// A storage failure is reported through onErr and the event is LOST. That is
// stated plainly rather than hidden behind a retry: events.Log gives Append no
// error return (see its doc for why), so the only honest thing this layer can do
// with a failed durable write is make sure somebody is told.
func (l *EventLog) Append(env events.Envelope) {
	if err := l.append(env); err != nil {
		l.onErr(fmt.Errorf("append event %s (schema %s) was NOT recorded: %w", env.ID, env.Schema, err))
	}
}

func (l *EventLog) append(env events.Envelope) error {
	if !ulid.Valid(env.ID) {
		return fmt.Errorf("id %q is not a valid ULID (DAT-005a, EVT-011)", env.ID)
	}
	payload := env.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}

	var evicted string
	if err := l.store.runWriteTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(context.Background(),
			`INSERT OR IGNORE INTO events
			 (id, schema, ts, scope_node, trace_id, cost_class, retention_class, origin, origin_principal, payload)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			env.ID, env.Schema, env.TS, env.ScopeNode, env.TraceID,
			env.CostClass, env.RetentionClass, env.Origin, env.OriginPrincipal, string(payload),
		); err != nil {
			return fmt.Errorf("store: insert event %s: %w", env.ID, err)
		}
		_, high, err := enforceRowCap(context.Background(), tx, env.RetentionClass, l.policy.For(env.RetentionClass).MaxRows)
		if err != nil {
			return err
		}
		evicted = high
		return recordEvicted(context.Background(), tx, high)
	}); err != nil {
		return err
	}
	l.advanceWatermark(evicted)
	return nil
}

// Has reports whether id is currently retained (events.Log). A storage failure
// answers false, which resolves a resume from that id as RESUME_FROM_INVALID —
// a refusal the client can see and reconnect from, never a backlog served from
// an answer this log could not actually establish.
func (l *EventLog) Has(id string) bool {
	l.store.mu.RLock()
	defer l.store.mu.RUnlock()
	var one int
	err := l.store.db.QueryRowContext(context.Background(),
		`SELECT 1 FROM events WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		l.onErr(fmt.Errorf("membership check for event %s failed; answering not-retained: %w", id, err))
		return false
	}
	return true
}

// After returns every retained envelope with an id strictly greater than id, in
// id order (events.Log). A storage failure answers an empty tail: the caller's
// own cursor does not advance over events it was not given, so the next drain
// re-reads the same range rather than skipping it.
func (l *EventLog) After(id string) []events.Envelope {
	out, err := l.read(`SELECT `+eventColumns+` FROM events WHERE id > ? ORDER BY id ASC`, id)
	if err != nil {
		l.onErr(fmt.Errorf("reading the event backlog after %q failed; delivering nothing this pass: %w", id, err))
		return nil
	}
	return out
}

// From returns every retained envelope with an id greater than or equal to id,
// in id order (events.Log) — the inclusive backlog a retention_expired resume
// delivers, so delivery resumes AT to_id with no silent loss (EVT-141/143).
func (l *EventLog) From(id string) []events.Envelope {
	out, err := l.read(`SELECT `+eventColumns+` FROM events WHERE id >= ? ORDER BY id ASC`, id)
	if err != nil {
		l.onErr(fmt.Errorf("reading the event backlog from %q failed; delivering nothing this pass: %w", id, err))
		return nil
	}
	return out
}

// HeadID is the newest retained id, "" for an empty log (events.Log).
//
// A storage failure answers "", which a fresh subscriber's watermark reads as
// "nothing seen yet" and so replays the retained log on its first drain.
// Duplicates are explicitly permitted (EVT-135 makes delivery at-least-once and
// id the dedup key); a wrongly-high watermark would instead skip events, which
// is the silent loss EVT-143 forbids. Head is the error-returning form, for the
// boot path that must not guess.
func (l *EventLog) HeadID() string {
	head, err := l.Head()
	if err != nil {
		l.onErr(fmt.Errorf("reading the event-log head failed; answering empty: %w", err))
		return ""
	}
	return head
}

// Head is HeadID with its error, for callers that must not proceed on a guess —
// notably the boot path that seeds the recording-order id generator from it, so
// a restarted process cannot mint an id that sorts below one already stored
// (EVT-011).
func (l *EventLog) Head() (string, error) {
	l.store.mu.RLock()
	defer l.store.mu.RUnlock()
	var head sql.NullString
	if err := l.store.db.QueryRowContext(context.Background(),
		`SELECT MAX(id) FROM events`).Scan(&head); err != nil {
		return "", fmt.Errorf("store: read event-log head: %w", err)
	}
	return head.String, nil
}

// OldestRetainedID is the earliest retained id, "" for an empty log — the
// retention floor AgedOut compares against.
//
// Under per-class retention it is NOT the point an aged-out resume resumes at (a
// long-lived audit record outlives newer telemetry, so it can sit below an
// aged-out point), and neither is any substrate-level "oldest retained after id"
// read: EVT-141's to_id must name the oldest id above the requested point that
// the ASKING SUBSCRIBER may see, which no query here knows how to answer. That
// is why events.Log carries no such method — see internal/events/resume.go's
// oldestVisibleAfter, which walks the retained tail through the subscriber's
// visible set instead (EVT-134a).
func (l *EventLog) OldestRetainedID() string {
	l.store.mu.RLock()
	defer l.store.mu.RUnlock()
	var oldest sql.NullString
	if err := l.store.db.QueryRowContext(context.Background(),
		`SELECT MIN(id) FROM events`).Scan(&oldest); err != nil {
		l.onErr(fmt.Errorf("reading the oldest retained event failed; answering none: %w", err))
		return ""
	}
	return oldest.String
}

// AgedOut reports whether id names a point this log can no longer resume from
// (events.Log) — EVT-141's retention_expired condition.
//
// It is two questions, not one, because retention varies by class. An id at or
// below the eviction watermark was itself removed. An id below everything the
// log still holds predates the log's whole retained range. Either way the point
// is unreconstructible. A RETAINED id is never aged out, which is why membership
// is checked first: with per-class windows a 300-day-old audit record is still
// retained while last week's telemetry has expired, so it sits BELOW the
// watermark while remaining perfectly resumable.
func (l *EventLog) AgedOut(id string) bool {
	if id == "" || l.Has(id) {
		return false
	}
	if w := l.EvictedThrough(); w != "" && id <= w {
		return true
	}
	oldest := l.OldestRetainedID()
	return oldest != "" && id < oldest
}

// EvictedAfter reports whether any event with an id strictly greater than id has
// been evicted (events.Log) — the mid-stream buffer_exceeded condition
// (EVT-142). It reads the cached watermark, so it costs no query and cannot
// fail.
func (l *EventLog) EvictedAfter(id string) bool {
	w := l.EvictedThrough()
	return w != "" && id < w
}

// EvictedThrough is the highest id this log has ever removed, "" if it has never
// removed one. It only ever increases, and it survives a restart — a watermark
// that reset would answer "nothing was ever lost" for every subscriber that
// reconnected across the restart.
func (l *EventLog) EvictedThrough() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.evictedThrough
}

// --- retention --------------------------------------------------------------

// Pruned reports what one Prune removed.
type Pruned struct {
	// Rows is the total number of events deleted.
	Rows int
	// ByClass counts the deletions per retention_class, so an operator can see
	// which tier a sweep actually acted on. Only classes something was actually
	// deleted from appear.
	ByClass map[string]int
	// EvictedThrough is the watermark after the sweep.
	EvictedThrough string
}

// PrunePlan is what a retention sweep WOULD remove, computed without removing
// anything — the same shape Pruned reports afterwards, in the future tense.
//
// It exists for `waiveo-feeder -store-check`, whose whole contract is "what does
// the next boot change in this store", answered over a `mode=ro` handle. The
// boot's FIRST act on the event log is Prune (cmd/waiveo-feeder/main.go), so a
// check that could only learn the sweep's answer by performing it could not
// answer at all — and did not: it reported the RETAINED counts, recorded no
// pending work, and let "VERDICT: NOTHING TO DO … the next boot changes nothing
// in this store" print over a store whose next boot was about to delete rows
// from it. On any box that has been up for a while that is not an edge case: the
// telemetry tier has both a seven-day window and a 4096-row cap, and either one
// fires on an ordinary working store.
type PrunePlan struct {
	// Rows is the total number of events the sweep would delete.
	Rows int
	// ByClass counts them per retention_class. Only classes something would
	// actually be deleted from appear.
	ByClass map[string]int
	// EvictsThrough is the greatest event id the sweep would delete — the
	// watermark the sweep would raise the log to.
	EvictsThrough string
	// AtMs is the clock reading the plan was judged against. It is the log's own
	// injected clock, which for a read-only inspection is the HOST wall clock and
	// not the app clock the boot will judge against (SEC-066) — so a record close
	// to its window boundary can legitimately be planned one way here and decided
	// the other way at the restart. Reported rather than hidden, for the same
	// reason the first-seen seed plan reports its own.
	AtMs int64
}

// Prune applies the retention policy: every event whose class window has
// elapsed at the injected clock's current reading is deleted, and every class
// over its row cap is trimmed oldest-first.
//
// It sweeps the classes actually PRESENT in storage rather than the classes the
// policy names, so a record carrying a class this build does not recognize is
// still swept — under the policy's unrecognized-class answer — instead of being
// retained forever because nothing thought to look for it.
//
// The whole sweep is one transaction ending in the watermark write, so a crash
// mid-sweep either deleted nothing or recorded everything it deleted. Deletion
// without the watermark is precisely the silent loss EVT-143 forbids: the rows
// would be gone with nothing left to tell a resuming subscriber they ever
// existed.
//
// # Why this does not VACUUM, and why that is not an oversight
//
// Deleting rows leaves free pages behind: the database FILE does not shrink, so
// an operator watching disk sees a sweep that appears to reclaim nothing. VACUUM
// would shrink it. It is deliberately not run here, and deliberately not on this
// sweep's cadence:
//
//   - It takes an EXCLUSIVE lock for the length of a whole-file rewrite. This
//     database holds the desired state every relay pulls, the audit trail every
//     mutating request files its only permanent record in, and the pack install
//     records. Blocking every reader and writer of it, hourly, on a box serving
//     screens, is a worse outcome than the free pages are.
//   - It rewrites the file, so it needs free space of roughly the database's own
//     size — precisely at the moment disk pressure is why somebody wanted it. A
//     reclamation that needs the disk it is trying to free is not a remedy.
//   - Free pages are REUSED. Every class this policy configures is bounded by a
//     row cap (events.DefaultRetentionPolicy), so the event tables reach a
//     steady-state size and freed pages are consumed by subsequent appends. The
//     file plateaus rather than growing without bound, and unbounded growth is the
//     condition that would justify the cost.
//
// PRAGMA incremental_vacuum is not the cheap alternative it looks like: it needs
// auto_vacuum=INCREMENTAL set BEFORE the schema was created, which this store did
// not do, so adopting it on an existing box requires a full VACUUM first. If a
// deployment ever does need the file itself to shrink, the honest mechanism is an
// explicit operator-run maintenance step at a moment of their choosing.
//
// None of this applies to the content origin, whose retention sweep
// (internal/feeder/contentgc) unlinks files: those bytes are returned to the
// filesystem the instant they are reclaimed, with no compaction step in between.
// It is the content store, not this one, that was actually growing without bound.
// It PLANS the sweep and then applies the plan, rather than deleting as it goes,
// so that `-store-check`'s PlanPrune and this are not two descriptions of the
// same rules that can drift apart. The plan is recomputed inside this write
// transaction — it is never carried in from an earlier read — so what is deleted
// is what the store holds at the moment of the delete.
func (l *EventLog) Prune() (Pruned, error) {
	ctx := context.Background()
	now := l.nowMs()
	out := Pruned{ByClass: map[string]int{}}

	var evicted string
	if err := l.store.runWriteTx(ctx, func(tx *sql.Tx) error {
		out = Pruned{ByClass: map[string]int{}}
		evicted = ""
		cuts, err := planRetentionSweep(ctx, tx, l.policy, now)
		if err != nil {
			return err
		}
		for _, cut := range cuts {
			n, err := applyRetentionCut(ctx, tx, cut)
			if err != nil {
				return err
			}
			out.Rows += n
			out.ByClass[cut.class] += n
			if cut.high > evicted {
				evicted = cut.high
			}
		}
		return recordEvicted(ctx, tx, evicted)
	}); err != nil {
		return Pruned{}, err
	}

	l.advanceWatermark(evicted)
	out.EvictedThrough = l.EvictedThrough()
	return out, nil
}

// PlanPrune reports what Prune would remove, over a read-only handle, removing
// nothing. It runs the SAME planner Prune applies, so the two cannot describe
// the same store differently.
//
// The one difference from Prune's answer is the clock and the moment: this reads
// the log's injected clock now, the boot reads the app's clock then, and rows
// arrive in between. The plan is what the sweep would do to the store AS IT
// STANDS — which is exactly the question `-store-check` asks of every other
// section, and the caveat it prints alongside them.
func (l *EventLog) PlanPrune() (PrunePlan, error) {
	ctx := context.Background()
	now := l.nowMs()
	out := PrunePlan{ByClass: map[string]int{}, AtMs: now}

	l.store.mu.RLock()
	defer l.store.mu.RUnlock()
	cuts, err := planRetentionSweep(ctx, l.store.db, l.policy, now)
	if err != nil {
		return PrunePlan{}, err
	}
	for _, cut := range cuts {
		out.Rows += cut.rows
		out.ByClass[cut.class] += cut.rows
		if cut.high > out.EvictsThrough {
			out.EvictsThrough = cut.high
		}
	}
	return out, nil
}

// Count returns how many events are currently retained, and how many of each
// retention class — what an operator inspecting a box before an upgrade wants
// to know about the audit trail sitting in the store.
func (l *EventLog) Count() (int, map[string]int, error) {
	l.store.mu.RLock()
	defer l.store.mu.RUnlock()
	rows, err := l.store.db.QueryContext(context.Background(),
		`SELECT retention_class, COUNT(*) FROM events GROUP BY retention_class ORDER BY retention_class ASC`)
	if err != nil {
		return 0, nil, fmt.Errorf("store: count events: %w", err)
	}
	defer rows.Close()
	total := 0
	byClass := map[string]int{}
	for rows.Next() {
		var class string
		var n int
		if err := rows.Scan(&class, &n); err != nil {
			return 0, nil, fmt.Errorf("store: count events: %w", err)
		}
		byClass[class] = n
		total += n
	}
	return total, byClass, rows.Err()
}

// --- internals --------------------------------------------------------------

// read runs an envelope projection and decodes it. It is the single reader
// After/From share, so both answer from exactly the same column set and
// ordering.
func (l *EventLog) read(query string, args ...any) ([]events.Envelope, error) {
	l.store.mu.RLock()
	defer l.store.mu.RUnlock()
	rows, err := l.store.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read events: %w", err)
	}
	defer rows.Close()
	out := []events.Envelope{}
	for rows.Next() {
		env, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, env)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read events: %w", err)
	}
	return out, nil
}

func scanEvent(rows *sql.Rows) (events.Envelope, error) {
	var env events.Envelope
	var payload string
	if err := rows.Scan(&env.ID, &env.Schema, &env.TS, &env.ScopeNode, &env.TraceID,
		&env.CostClass, &env.RetentionClass, &env.Origin, &env.OriginPrincipal, &payload); err != nil {
		return events.Envelope{}, fmt.Errorf("store: scan event: %w", err)
	}
	env.Payload = json.RawMessage(payload)
	return env, nil
}

// advanceWatermark raises the cached eviction watermark to high. It never
// lowers it: entries are evicted oldest-first within a class, but a sweep over
// several classes can report a lower high than an earlier sweep already
// recorded, and a watermark that regressed would stop reporting a loss it has
// already established.
func (l *EventLog) advanceWatermark(high string) {
	if high == "" {
		return
	}
	l.mu.Lock()
	if high > l.evictedThrough {
		l.evictedThrough = high
	}
	l.mu.Unlock()
}

// eventQueryer is the read surface the retention planner needs, satisfied by
// both *sql.DB (the read-only plan) and *sql.Tx (the sweep that applies it) —
// which is what lets one planner serve both.
type eventQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// retentionCut is ONE deletion a retention sweep performs: the predicate that
// selects the rows, and what that predicate matches right now.
//
// Splitting the sweep into cuts is what makes it plannable. A cut can be counted
// without being applied, which is how `-store-check` answers "what does the next
// boot change" about the event log over a `mode=ro` handle, and the same cut is
// what Prune hands to DELETE — so there is no second implementation of the rules
// to drift out of step with the first.
type retentionCut struct {
	class string
	// where is the predicate, WITHOUT the `retention_class = ?` term every cut
	// carries and every consumer prepends.
	where string
	args  []any
	// rows is how many events the cut matches, and high is the greatest id among
	// them. Both are read BEFORE any delete: once the rows are gone, that id is
	// the only surviving record that they existed (EVT-143).
	rows int
	high string
}

// planRetentionSweep computes every deletion the policy calls for over the
// store as it stands, in the order Prune applies them.
//
// The ORDER is load-bearing to the arithmetic, not just to the writes. Prune
// deletes the window's rows first, so the row cap it then enforces sees a table
// the window pass has already thinned. A plan that ran both passes over the
// untrimmed table would count the rows BOTH match twice and report an eviction
// larger than the boot performs — which for the telemetry tier (a seven-day
// window AND a 4096-row cap) is the ordinary case, not a corner. So each later
// cut in a class is narrowed to the rows the earlier ones have not claimed.
func planRetentionSweep(ctx context.Context, q eventQueryer, policy events.RetentionPolicy, now int64) ([]retentionCut, error) {
	classes, err := storedRetentionClasses(ctx, q)
	if err != nil {
		return nil, err
	}
	var cuts []retentionCut
	for _, class := range classes {
		rule := policy.For(class)
		// survives is the predicate for "not already claimed by an earlier cut in
		// this class"; empty means nothing has been claimed yet.
		survives, survivesArgs := "", []any(nil)
		if rule.Window > 0 {
			cutoff := now - rule.Window.Milliseconds()
			cut, err := planRetentionCut(ctx, q, class, `ts < ?`, []any{cutoff})
			if err != nil {
				return nil, err
			}
			if cut.rows > 0 {
				cuts = append(cuts, cut)
			}
			survives, survivesArgs = `ts >= ?`, []any{cutoff}
		}
		cut, ok, err := planRowCapCut(ctx, q, class, rule.MaxRows, survives, survivesArgs)
		if err != nil {
			return nil, err
		}
		if ok && cut.rows > 0 {
			cuts = append(cuts, cut)
		}
	}
	return cuts, nil
}

// planRetentionCut counts what a predicate matches, and the greatest id among
// them, without touching a row.
func planRetentionCut(ctx context.Context, q eventQueryer, class, where string, args []any) (retentionCut, error) {
	cut := retentionCut{class: class, where: where, args: args}
	var high sql.NullString
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*), MAX(id) FROM events WHERE retention_class = ? AND `+where,
		append([]any{class}, args...)...).Scan(&cut.rows, &high); err != nil {
		return retentionCut{}, fmt.Errorf("store: plan the retention cut for class %q: %w", class, err)
	}
	cut.high = high.String
	return cut, nil
}

// planRowCapCut computes the cut that trims class to at most maxRows events,
// oldest-first. A maxRows of 0 (or less) is uncapped and cuts nothing — which is
// how the audit tier is configured, so an audit record is never evicted to admit
// another one.
//
// The floor is derived as "the oldest id among the maxRows newest", so the cut
// is a single indexed range under (retention_class, id) rather than a
// row-count-then-delete pass that could race its own count. survives narrows
// both halves of that to the rows an earlier cut in the same sweep has not
// already claimed (see planRetentionSweep).
func planRowCapCut(ctx context.Context, q eventQueryer, class string, maxRows int, survives string, survivesArgs []any) (retentionCut, bool, error) {
	if maxRows <= 0 {
		return retentionCut{}, false, nil
	}
	keptWhere := `1`
	if survives != "" {
		keptWhere = survives
	}
	floorArgs := append(append([]any{class}, survivesArgs...), maxRows)
	var keepFrom sql.NullString
	if err := q.QueryRowContext(ctx,
		`SELECT MIN(id) FROM (SELECT id FROM events WHERE retention_class = ? AND `+keptWhere+` ORDER BY id DESC LIMIT ?)`,
		floorArgs...).Scan(&keepFrom); err != nil {
		return retentionCut{}, false, fmt.Errorf("store: find the row-cap floor for class %q: %w", class, err)
	}
	if !keepFrom.Valid {
		return retentionCut{}, false, nil
	}
	where, args := `id < ?`, []any{keepFrom.String}
	if survives != "" {
		where += ` AND ` + survives
		args = append(args, survivesArgs...)
	}
	cut, err := planRetentionCut(ctx, q, class, where, args)
	if err != nil {
		return retentionCut{}, false, err
	}
	return cut, true, nil
}

// applyRetentionCut performs one planned cut, returning how many rows went.
func applyRetentionCut(ctx context.Context, tx *sql.Tx, cut retentionCut) (int, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM events WHERE retention_class = ? AND `+cut.where,
		append([]any{cut.class}, cut.args...)...)
	if err != nil {
		return 0, fmt.Errorf("store: apply the retention cut for class %q: %w", cut.class, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// storedRetentionClasses lists the retention classes events are actually stored
// under, ascending.
func storedRetentionClasses(ctx context.Context, q queryer) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT retention_class FROM events ORDER BY retention_class ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list event retention classes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("store: list event retention classes: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// enforceRowCap trims class to at most maxRows events, oldest-first, returning
// how many went and the highest id among them.
//
// It is the APPEND path's cap enforcement (one class, no window pass), expressed
// through the same plan-then-apply pair the sweep uses, so "which rows are over
// the cap" has exactly one implementation in this file.
func enforceRowCap(ctx context.Context, tx *sql.Tx, class string, maxRows int) (int, string, error) {
	cut, ok, err := planRowCapCut(ctx, tx, class, maxRows, "", nil)
	if err != nil || !ok || cut.rows == 0 {
		return 0, "", err
	}
	n, err := applyRetentionCut(ctx, tx, cut)
	if err != nil {
		return 0, "", err
	}
	return n, cut.high, nil
}

// recordEvicted raises the PERSISTED eviction watermark to high, in the caller's
// transaction — so the record of what was dropped commits with the drop itself.
// The MAX in SQL is what keeps it monotonic even if a later sweep's highest
// deletion sits below an earlier one's.
func recordEvicted(ctx context.Context, tx *sql.Tx, high string) error {
	if high == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE event_log_meta SET evicted_through = MAX(evicted_through, ?) WHERE id = 1`, high); err != nil {
		return fmt.Errorf("store: record the event-log eviction watermark: %w", err)
	}
	return nil
}
