// Package store is the app-side persistence layer for the authoring loop: a
// pure-Go SQLite store (modernc.org/sqlite, the same cgo-free driver the relay's
// identity store uses, keeping CGO_ENABLED=0 building) holding the scope-node
// tree and the six data-model/1 scheduling-core row kinds, each carrying the
// api/1 resource baseline (id/revision/external_id/labels/scope_node/timestamps)
// plus its full JSON body, behind a store-level monotonic `generation`.
//
// Every write is validated before commit — a scheduling-core write through
// datamodel.ValidateRows, a scope-node write through datamodel.BuildScopeTree —
// so a row that fails validation is never persisted (the api layer surfaces the
// carried errors as VALIDATION_FAILED). Every accepted write bumps the row's
// revision and the store generation in the SAME transaction, and the write path
// is serialized (a single writer under a mutex, atomic-immediate transactions),
// so a revision/generation bump is atomic against a concurrent read: a reader
// never observes a half-applied write, and the generation is strictly monotonic
// (REL-052 relies on it). Update/Delete are optimistic — a stale expected
// revision is rejected with ErrRevisionMismatch and leaves the store untouched.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	_ "modernc.org/sqlite"
)

// Kind names one resource table. The string value IS the table name; the closed
// set below is the only accepted input (tableFor rejects anything else, so a
// Kind never reaches a non-parameterizable table-name interpolation unchecked).
type Kind string

const (
	KindScopeNode      Kind = "scope_nodes"
	KindPlaylist       Kind = "playlists"
	KindSchedule       Kind = "schedules"
	KindDaypart        Kind = "dayparts"
	KindValidityWindow Kind = "validity_windows"
	KindFallback       Kind = "fallbacks"
	KindPresetBatch    Kind = "preset_batches"
	// KindAutomation is the rules/1 authored-rule kind: the api/1 resource
	// baseline + the rule-definition JSON body + an execution_class column
	// (edge/app). Its writes are gated by the rules compiler rather than
	// datamodel.ValidateRows (see automations.go).
	KindAutomation Kind = "automations"
)

// allKinds is every resource table in schema order (scope_nodes first, then the
// six scheduling-core kinds, then automations). schedulingKinds is the subset
// validated through datamodel.ValidateRows; automations are compile-gated
// (compile.Compile) instead — see automations.go.
var allKinds = []Kind{
	KindScopeNode, KindPlaylist, KindSchedule, KindDaypart,
	KindValidityWindow, KindFallback, KindPresetBatch, KindAutomation,
}

var kindSet = func() map[Kind]bool {
	m := make(map[Kind]bool, len(allKinds))
	for _, k := range allKinds {
		m[k] = true
	}
	return m
}()

// tableFor validates a Kind against the closed set and returns its table name.
// This guard is what makes the `"... "+table` interpolation in the CRUD SQL safe:
// only a known-good constant table name ever reaches it.
func tableFor(kind Kind) (string, error) {
	if !kindSet[kind] {
		return "", fmt.Errorf("store: unknown kind %q", kind)
	}
	return string(kind), nil
}

// Resource is one stored row: the api/1 resource baseline the store owns and
// denormalizes into columns, plus the row's full canonical JSON body (the
// data-model/1 fields, with the store-authoritative revision/timestamps injected).
type Resource struct {
	ID         string
	Revision   int64
	ExternalID string
	ScopeNode  string
	Labels     map[string]string
	CreatedAt  int64
	UpdatedAt  int64
	Body       json.RawMessage
	// ExecutionClass is the compiler's edge/app classification of an automation
	// row (compile.Classify). It is empty for kinds that carry no execution class
	// and is set by a compile-gated Create/Update (see automations.go).
	ExecutionClass string
}

// ListFilter narrows a List. The zero value returns every row of the kind in id
// order; ScopeNode, when set, restricts to rows attached to exactly that scope
// node. Label/subtree selection is deliberately NOT done here — the api layer
// applies apiselector over the returned set, so the store never re-implements
// the selector engine.
type ListFilter struct {
	ScopeNode string
}

// ErrNotFound is returned by Update/Delete for an id that is not present.
var ErrNotFound = errors.New("store: resource not found")

// ErrRevisionMismatch is the sentinel a stale-revision Update/Delete unwraps to
// (errors.Is). RevisionMismatchError carries the current revision for the api
// layer's REVISION_CONFLICT problem.
var ErrRevisionMismatch = errors.New("store: revision mismatch")

// RevisionMismatchError reports an optimistic-concurrency conflict: the caller's
// Expected revision did not match the stored Current revision. It unwraps to
// ErrRevisionMismatch.
type RevisionMismatchError struct {
	Expected int64
	Current  int64
}

func (e *RevisionMismatchError) Error() string {
	return fmt.Sprintf("store: revision mismatch: expected %d, current %d", e.Expected, e.Current)
}

func (e *RevisionMismatchError) Unwrap() error { return ErrRevisionMismatch }

// ValidationError carries the datamodel/1 validation failures a write triggered.
// A write that returns this changed nothing (the transaction rolled back); the
// api layer renders it as a VALIDATION_FAILED problem with per-field errors.
type ValidationError struct {
	Errors []datamodel.Error
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("store: validation failed (%d error(s))", len(e.Errors))
}

// WriteGuard is a caller-supplied invariant check run INSIDE a Create/Update write
// transaction, against the kind's existing rows as read under the SAME write lock
// the write commits under (readResources over the transaction). Because the write
// path serializes under Store.mu, no other writer can interleave between a guard's
// read and the row write — so a guard closes the check-then-write (TOCTOU) race a
// pre-write snapshot taken in a separate critical section leaves open. Returning a
// non-nil error rolls the transaction back before anything is written and is
// propagated to the caller VERBATIM, so the api layer can supply a guard that calls
// apihttp.CheckExternalIDUnique and recognize the *apihttp.ExternalIDError it
// returns — enforcing external_id uniqueness (API-101/102) atomically rather than
// re-implementing the rule in the store.
type WriteGuard func(existing []Resource) error

// Store is the app-side SQLite store. Safe for concurrent use: writes serialize
// under mu (write lock) and reads take mu (read lock), so a revision/generation
// bump is atomic against any concurrent read.
type Store struct {
	db    *sql.DB
	mu    sync.RWMutex
	nowMs func() int64

	// onCommit (OnCommit) runs after every COMMITTED write transaction —
	// the single post-commit seam every generation advance rides, since
	// every bumpGeneration call site (resource CRUD and pack
	// install/uninstall/row-CRUD alike) commits through writeTx. Guarded by
	// hookMu, and invoked AFTER writeTx releases the write lock, so a hook
	// that re-reads the store (e.g. a snapshot provider taking the read
	// lock) can never deadlock against the write path.
	hookMu   sync.Mutex
	onCommit func()
}

// schema creates the scope-node table, one table per scheduling-core kind, and
// the singleton generation counter. Every resource table shares the api/1
// baseline column set (id PK / revision / external_id / labels JSON / scope_node
// / created_at / updated_at) plus a body JSON column carrying the kind's own
// data-model fields. meta holds the store-level monotonic generation, seeded at 0.
const schema = `
CREATE TABLE IF NOT EXISTS meta (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	generation INTEGER NOT NULL
);
INSERT OR IGNORE INTO meta (id, generation) VALUES (1, 0);
`

// resourceTableDDL is the shared baseline DDL, instantiated once per resource
// table name.
const resourceTableDDL = `
CREATE TABLE IF NOT EXISTS %s (
	id          TEXT PRIMARY KEY,
	revision    INTEGER NOT NULL,
	external_id TEXT NOT NULL DEFAULT '',
	labels      TEXT NOT NULL DEFAULT '{}',
	scope_node  TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL,
	body        TEXT NOT NULL
);
`

// Open opens (creating if necessary) the SQLite store at dsn, migrating the
// schema. Pass ":memory:" for an ephemeral, test-only store, or a filesystem
// path for the feeder's persistent store (its parent dir is created 0700). A
// file-backed store runs under WAL + synchronous=FULL; every store uses
// _txlock=immediate so a write transaction takes its write lock up front (no
// read→write upgrade deadlock), reinforcing the serialized write path.
func Open(dsn string) (*Store, error) {
	var connString string
	switch dsn {
	case ":memory:":
		connString = "file::memory:?_txlock=immediate"
	default:
		if dir := filepath.Dir(dsn); dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("store: create dir %s: %w", dir, err)
			}
		}
		connString = "file:" + dsn +
			"?_txlock=immediate" +
			"&_pragma=journal_mode(WAL)" +
			"&_pragma=synchronous(FULL)" +
			"&_pragma=busy_timeout(5000)"
	}

	db, err := sql.Open("sqlite", connString)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", dsn, err)
	}
	// A single underlying connection: keeps a ":memory:" store a single shared
	// database and avoids modernc.org/sqlite's SQLITE_BUSY surface under
	// concurrent writers on one file handle. The RWMutex provides the logical
	// serialization; this bounds the driver.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate meta: %w", err)
	}
	// The automations table carries the baseline PLUS an execution_class column,
	// so it is created from its own DDL first; the shared-baseline loop below then
	// no-ops over it (CREATE TABLE IF NOT EXISTS).
	if _, err := db.Exec(automationsTableDDL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate automations: %w", err)
	}
	// The declarative-packs tables (packs + pack_files + pack_rows) are a
	// self-contained subsystem with their own dedicated CRUD (packs.go), not a
	// generic resource Kind, so they are migrated from their own DDL here.
	if _, err := db.Exec(packsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate packs: %w", err)
	}
	// The api/1 Job tables (jobs + job_targets) are likewise their own
	// subsystem with their own CRUD (jobs.go): a Job is an execution record
	// over other rows, not a resource Kind, so it carries no revision,
	// external_id or scope_node of its own and never joins the desired-state
	// projection.
	if _, err := db.Exec(jobsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate jobs: %w", err)
	}
	// The events/1 durable event log (events + event_log_meta) is likewise its
	// own subsystem with its own reader/writer (eventlog.go): an event is an
	// immutable record, never updated after it is written, carrying no revision
	// and no place in the desired-state projection. It rides this store — rather
	// than a database of its own — so a box has ONE file to back up and one to
	// restore, and so the audit trail cannot be separated from the authored rows
	// it describes.
	if _, err := db.Exec(eventsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate events: %w", err)
	}
	for _, k := range allKinds {
		if _, err := db.Exec(fmt.Sprintf(resourceTableDDL, string(k))); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: migrate %s: %w", k, err)
		}
	}

	return &Store{db: db, nowMs: func() int64 { return time.Now().UnixMilli() }}, nil
}

// Close flushes the WAL (best-effort) and closes the database handle.
func (s *Store) Close() error {
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return s.db.Close()
}

// queryer is the read surface shared by *sql.DB and *sql.Tx, so the row readers
// serve both the read path (Get/List/DesiredStateRows) and the in-transaction
// pre-commit validation reads.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Generation returns the store's current monotonic generation (0 before any
// write).
func (s *Store) Generation(ctx context.Context) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readGeneration(ctx, s.db)
}

func readGeneration(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (int64, error) {
	var g int64
	if err := q.QueryRowContext(ctx, `SELECT generation FROM meta WHERE id = 1`).Scan(&g); err != nil {
		return 0, fmt.Errorf("store: read generation: %w", err)
	}
	return g, nil
}

// bumpGeneration increments the singleton generation counter by one, in the
// caller's transaction. Because it rides the same tx as the row write, the two
// commit together or roll back together — the generation is strictly monotonic
// and never advances for a rejected write.
func bumpGeneration(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET generation = generation + 1 WHERE id = 1`); err != nil {
		return fmt.Errorf("store: bump generation: %w", err)
	}
	return nil
}

// OnCommit registers fn to run after every committed write transaction — the
// feeder's one seam for nudging live relay connections when a generation
// advances (relay/1 REL-057: the hook is safe to over-call; a nudge for an
// unchanged generation is a no-op at the relay). fn runs on the writing
// goroutine AFTER the write lock is released, so it may re-read the store;
// anything slow (or coupled to a peer's socket) must be made async by the
// hook itself. Passing nil clears the hook. Intended to be set once at
// startup, before the listener accepts writes; guarded so a concurrent write
// observes either the old or the new hook, never a torn value.
func (s *Store) OnCommit(fn func()) {
	s.hookMu.Lock()
	s.onCommit = fn
	s.hookMu.Unlock()
}

// writeTx runs fn inside a serialized, immediate transaction. On any error from
// fn the transaction rolls back (nothing persisted, generation unchanged); only
// a clean fn commits — and only a commit fires the OnCommit hook, after the
// write lock is released (a rolled-back write nudges nobody).
func (s *Store) writeTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if err := s.runWriteTx(ctx, fn); err != nil {
		return err
	}
	s.hookMu.Lock()
	hook := s.onCommit
	s.hookMu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

// runWriteTx is writeTx's locked transactional body, split out so the
// post-commit hook above runs outside the write lock.
func (s *Store) runWriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Create inserts a new row of kind from its data-model JSON body. The store owns
// the baseline: it sets revision to 1 and created_at/updated_at to now, injects
// them into the stored body, and denormalizes external_id/scope_node/labels into
// columns. The generation is bumped and the resulting full row-set validated
// (BuildScopeTree for scope nodes, ValidateRows for scheduling kinds) before
// commit — a validation failure rolls the whole thing back.
//
// The baseline it owns is the whole api/1 representation, not only the three
// timestamps/revision: every member the openapi schema declares REQUIRED is
// materialized here when the write left it absent (declaredmembers.go), so the
// bytes this row is later served as carry what every generated client was told
// they would.
func (s *Store) Create(ctx context.Context, kind Kind, body json.RawMessage, guards ...WriteGuard) (Resource, error) {
	table, err := tableFor(kind)
	if err != nil {
		return Resource{}, err
	}
	body, err = injectDeclaredMembers(kind, body)
	if err != nil {
		return Resource{}, err
	}
	bf, err := parseBaseline(kind, body)
	if err != nil {
		return Resource{}, err
	}
	id := bf.identity(kind)
	if id == "" {
		return Resource{}, fmt.Errorf("store: create %s: row is missing its identity field", kind)
	}

	now := s.nowMs()
	fullBody, err := injectBaseline(body, 1, now, now)
	if err != nil {
		return Resource{}, err
	}
	// Compile-gate an automation before the write is even opened: a non-compiling
	// rule returns the compiler's typed error and nothing is stored (the tx is
	// never begun). The classification rides the row's execution_class column.
	executionClass, err := compileGate(kind, fullBody)
	if err != nil {
		return Resource{}, err
	}
	labelsJSON := marshalLabels(bf.Labels)

	if err := s.writeTx(ctx, func(tx *sql.Tx) error {
		// The existing rows, read under the write lock this tx holds — the atomic
		// snapshot the id-uniqueness check and every WriteGuard evaluate against, so
		// a check-then-write invariant cannot be raced past by a concurrent writer.
		existing, err := readResources(ctx, tx, table)
		if err != nil {
			return err
		}
		// A client-supplied id that already names a row is a well-defined client
		// error (VALIDATION_FAILED with an identity-field error), not a raw PRIMARY
		// KEY constraint the api layer would otherwise mask as a 500 INTERNAL.
		for _, e := range existing {
			if e.ID == id {
				return &ValidationError{Errors: []datamodel.Error{{
					Field:   identityFieldName(kind),
					Code:    "already_exists",
					Message: fmt.Sprintf("a %s with this identifier already exists", kind),
				}}}
			}
		}
		for _, g := range guards {
			if err := g(existing); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO `+table+` (id, revision, external_id, labels, scope_node, created_at, updated_at, body)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, 1, bf.ExternalID, labelsJSON, bf.ScopeNode, now, now, string(fullBody),
		)
		if err != nil {
			return fmt.Errorf("store: insert %s: %w", kind, err)
		}
		if err := setExecutionClass(ctx, tx, kind, id, executionClass); err != nil {
			return err
		}
		if err := bumpGeneration(ctx, tx); err != nil {
			return err
		}
		return validateAfterWrite(ctx, tx, kind)
	}); err != nil {
		return Resource{}, err
	}

	return Resource{
		ID: id, Revision: 1, ExternalID: bf.ExternalID, ScopeNode: bf.ScopeNode,
		Labels: bf.Labels, CreatedAt: now, UpdatedAt: now, Body: fullBody,
		ExecutionClass: executionClass,
	}, nil
}

// Get returns the row of kind with id, and whether it exists.
func (s *Store) Get(ctx context.Context, kind Kind, id string) (Resource, bool, error) {
	table, err := tableFor(kind)
	if err != nil {
		return Resource{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+selectColumns(kind)+`
		 FROM `+table+` WHERE id = ?`, id)
	if err != nil {
		return Resource{}, false, fmt.Errorf("store: get %s: %w", kind, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Resource{}, false, fmt.Errorf("store: get %s: %w", kind, err)
		}
		return Resource{}, false, nil
	}
	res, err := scanResource(kind, rows)
	if err != nil {
		return Resource{}, false, err
	}
	return res, true, nil
}

// List returns every row of kind matching filter, ordered by id ascending.
func (s *Store) List(ctx context.Context, kind Kind, filter ListFilter) ([]Resource, error) {
	table, err := tableFor(kind)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := `SELECT ` + selectColumns(kind) + ` FROM ` + table
	var args []any
	if filter.ScopeNode != "" {
		q += ` WHERE scope_node = ?`
		args = append(args, filter.ScopeNode)
	}
	q += ` ORDER BY id ASC`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list %s: %w", kind, err)
	}
	defer rows.Close()

	out := []Resource{}
	for rows.Next() {
		res, err := scanResource(kind, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, rows.Err()
}

// Update optimistically patches the row of kind with id: rev MUST equal the
// stored revision (else ErrRevisionMismatch, no write). patch is a partial JSON
// object shallow-merged over the stored body (the store-owned baseline keys are
// never overwritten by it). The store bumps revision to current+1, refreshes
// updated_at, re-denormalizes the columns, bumps the generation, and re-validates
// the full row-set before commit.
func (s *Store) Update(ctx context.Context, kind Kind, id string, rev int64, patch json.RawMessage, guards ...WriteGuard) (Resource, error) {
	table, err := tableFor(kind)
	if err != nil {
		return Resource{}, err
	}

	var res Resource
	if err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var curRev int64
		var curBody string
		err := tx.QueryRowContext(ctx, `SELECT revision, body FROM `+table+` WHERE id = ?`, id).Scan(&curRev, &curBody)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: update read %s: %w", kind, err)
		}
		if curRev != rev {
			return &RevisionMismatchError{Expected: rev, Current: curRev}
		}

		merged, err := mergeBody(json.RawMessage(curBody), patch)
		if err != nil {
			return err
		}
		// The same representation completion a create runs, over the EFFECTIVE
		// post-merge body: a patch that nulls a required member whose declared type
		// does not admit null (`{"labels": null}`) is normalized back to the stated
		// default rather than persisted as a present-but-unusable member
		// (declaredmembers.go).
		merged, err = injectDeclaredMembers(kind, merged)
		if err != nil {
			return err
		}
		curBaseline, err := parseBaseline(kind, json.RawMessage(curBody))
		if err != nil {
			return err
		}
		now := s.nowMs()
		newRev := curRev + 1
		fullBody, err := injectBaseline(merged, newRev, curBaseline.CreatedAt, now)
		if err != nil {
			return err
		}
		bf, err := parseBaseline(kind, fullBody)
		if err != nil {
			return err
		}
		// Re-run the compile gate over the merged body: an Update is re-validated
		// and re-classified exactly like a Create, so a patch that breaks
		// compilation rolls the whole tx back (nothing changed) and a patch that
		// crosses the edge/app line updates the execution_class column.
		executionClass, err := compileGate(kind, fullBody)
		if err != nil {
			return err
		}
		labelsJSON := marshalLabels(bf.Labels)

		// WriteGuards evaluate the effective post-merge write against the existing
		// rows read under this same write lock — atomically with the UPDATE, so an
		// external_id-uniqueness check (API-101/102) cannot be raced past. The guard
		// excludes this row by its own id, so an unchanged external_id never collides.
		if len(guards) > 0 {
			existing, err := readResources(ctx, tx, table)
			if err != nil {
				return err
			}
			for _, g := range guards {
				if err := g(existing); err != nil {
					return err
				}
			}
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE `+table+` SET revision = ?, external_id = ?, labels = ?, scope_node = ?, updated_at = ?, body = ? WHERE id = ?`,
			newRev, bf.ExternalID, labelsJSON, bf.ScopeNode, now, string(fullBody), id,
		); err != nil {
			return fmt.Errorf("store: update %s: %w", kind, err)
		}
		if err := setExecutionClass(ctx, tx, kind, id, executionClass); err != nil {
			return err
		}
		if err := bumpGeneration(ctx, tx); err != nil {
			return err
		}
		if err := validateAfterWrite(ctx, tx, kind); err != nil {
			return err
		}
		res = Resource{
			ID: id, Revision: newRev, ExternalID: bf.ExternalID, ScopeNode: bf.ScopeNode,
			Labels: bf.Labels, CreatedAt: curBaseline.CreatedAt, UpdatedAt: now, Body: fullBody,
			ExecutionClass: executionClass,
		}
		return nil
	}); err != nil {
		return Resource{}, err
	}
	return res, nil
}

// Delete optimistically removes the row of kind with id: rev MUST equal the
// stored revision (else ErrRevisionMismatch, no write). The generation is bumped
// and the remaining row-set re-validated before commit, so a delete that would
// leave a dangling reference (e.g. removing a playlist a daypart still points at)
// is rejected with a ValidationError and rolls back.
func (s *Store) Delete(ctx context.Context, kind Kind, id string, rev int64) error {
	table, err := tableFor(kind)
	if err != nil {
		return err
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var curRev int64
		err := tx.QueryRowContext(ctx, `SELECT revision FROM `+table+` WHERE id = ?`, id).Scan(&curRev)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: delete read %s: %w", kind, err)
		}
		if curRev != rev {
			return &RevisionMismatchError{Expected: rev, Current: curRev}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE id = ?`, id); err != nil {
			return fmt.Errorf("store: delete %s: %w", kind, err)
		}
		if err := bumpGeneration(ctx, tx); err != nil {
			return err
		}
		return validateAfterWrite(ctx, tx, kind)
	})
}

// readResources reads every row of table (ordered by id ascending) into Resources
// via q, which may be *sql.DB (the read path) or *sql.Tx (an in-transaction guard
// snapshot). It is the single full-table row reader the write-path WriteGuards
// share with List, so a guard sees exactly the rows a List would — but read under
// the write lock, atomically with the write it gates.
func readResources(ctx context.Context, q queryer, table string) ([]Resource, error) {
	// The table name IS the Kind string (tableFor validated it upstream), so the
	// projection/scan pick up the automations execution_class column here too.
	kind := Kind(table)
	rows, err := q.QueryContext(ctx,
		`SELECT `+selectColumns(kind)+` FROM `+table+` ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: read %s: %w", table, err)
	}
	defer rows.Close()
	out := []Resource{}
	for rows.Next() {
		res, err := scanResource(kind, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, rows.Err()
}

// identityFieldName is the JSON key a row of this kind carries its identity under
// (preset_id for a preset-batch, the DAT-005 exception; id otherwise) — used to
// name the offending field in a duplicate-identity ValidationError.
func identityFieldName(kind Kind) string {
	if kind == KindPresetBatch {
		return "preset_id"
	}
	return "id"
}

// baselineColumns is the shared api/1 projection every resource table exposes, in
// the order scanResource reads them.
const baselineColumns = "id, revision, external_id, labels, scope_node, created_at, updated_at, body"

// selectColumns is the column projection to read a row of kind into a Resource. It
// is the shared baseline for every kind PLUS, for the automations table only (the
// sole kind carrying an execution_class column), that column — so a fetched
// automation retains the compile-gated edge/app classification recorded on its row
// rather than silently defaulting to empty on Get/List. scanResource(kind, …) reads
// exactly the projection this returns.
func selectColumns(kind Kind) string {
	if kind == KindAutomation {
		return baselineColumns + ", execution_class"
	}
	return baselineColumns
}

// scanResource reads one row — the selectColumns(kind) projection — into a Resource.
// For the automations kind the trailing execution_class column is scanned too, so
// the persisted classification survives a read (not just the Create/Update return).
func scanResource(kind Kind, rows *sql.Rows) (Resource, error) {
	var r Resource
	var labelsJSON, body string
	dest := []any{&r.ID, &r.Revision, &r.ExternalID, &labelsJSON, &r.ScopeNode, &r.CreatedAt, &r.UpdatedAt, &body}
	if kind == KindAutomation {
		dest = append(dest, &r.ExecutionClass)
	}
	if err := rows.Scan(dest...); err != nil {
		return Resource{}, fmt.Errorf("store: scan row: %w", err)
	}
	if labelsJSON != "" && labelsJSON != "{}" {
		if err := json.Unmarshal([]byte(labelsJSON), &r.Labels); err != nil {
			return Resource{}, fmt.Errorf("store: decode labels: %w", err)
		}
	}
	r.Body = json.RawMessage(body)
	return r, nil
}

// baseline is the api/1 resource baseline parsed out of a row's JSON body. Both
// id and preset_id are read so the store can resolve either identity convention
// (preset-batch rows key on preset_id, the sole DAT-005 baseline exception).
type baseline struct {
	ID         string            `json:"id"`
	PresetID   string            `json:"preset_id"`
	ExternalID string            `json:"external_id"`
	ScopeNode  string            `json:"scope_node"`
	Labels     map[string]string `json:"labels"`
	CreatedAt  int64             `json:"created_at"`
	Revision   int64             `json:"revision"`
}

func (b baseline) identity(kind Kind) string {
	if kind == KindPresetBatch {
		return b.PresetID
	}
	return b.ID
}

func parseBaseline(kind Kind, body json.RawMessage) (baseline, error) {
	var b baseline
	if err := json.Unmarshal(body, &b); err != nil {
		return baseline{}, fmt.Errorf("store: parse %s baseline: %w", kind, err)
	}
	return b, nil
}

func marshalLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}
	b, err := json.Marshal(labels)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// protectedKeys are the store-owned baseline fields a patch may never overwrite;
// the store re-derives them itself.
var protectedKeys = map[string]bool{
	"id": true, "preset_id": true, "revision": true, "created_at": true, "updated_at": true,
}

// injectBaseline sets the store-authoritative revision/created_at/updated_at keys
// on a row body, returning the canonical marshaled body.
func injectBaseline(body json.RawMessage, revision, createdAt, updatedAt int64) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("store: decode body for baseline injection: %w", err)
	}
	m["revision"] = jsonInt(revision)
	m["created_at"] = jsonInt(createdAt)
	m["updated_at"] = jsonInt(updatedAt)
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("store: re-marshal body: %w", err)
	}
	return out, nil
}

// mergeBody shallow-merges a partial patch object over an existing body, leaving
// the store-owned baseline keys untouched.
func mergeBody(existing, patch json.RawMessage) (json.RawMessage, error) {
	base := map[string]json.RawMessage{}
	if err := json.Unmarshal(existing, &base); err != nil {
		return nil, fmt.Errorf("store: decode existing body: %w", err)
	}
	if len(patch) > 0 {
		over := map[string]json.RawMessage{}
		if err := json.Unmarshal(patch, &over); err != nil {
			return nil, fmt.Errorf("store: decode patch: %w", err)
		}
		for k, v := range over {
			if protectedKeys[k] {
				continue
			}
			base[k] = v
		}
	}
	out, err := json.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("store: re-marshal merged body: %w", err)
	}
	return out, nil
}

func jsonInt(v int64) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
