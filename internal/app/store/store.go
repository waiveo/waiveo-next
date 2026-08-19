// Package store is the app-side persistence layer for the authoring loop: a
// pure-Go SQLite store (modernc.org/sqlite, the same cgo-free driver the relay's
// identity store uses, keeping CGO_ENABLED=0 building) holding the scope-node
// tree and the six data-model/1 scheduling-core row kinds, each carrying the
// api/1 resource baseline (id/revision/external_id/labels/scope_node/timestamps)
// plus its full JSON body, behind a store-level monotonic `generation`.
//
// Every write is validated before commit — a scheduling-core write through
// datamodel.ValidateRows, a scope-node write through datamodel.BuildFullScopeTree —
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
	"github.com/maaxton/waiveo-next/internal/shared/secretfile"
	_ "modernc.org/sqlite"
)

// Kind names one resource table. The string value IS the table name; the closed
// set below is the only accepted input (tableFor rejects anything else, so a
// Kind never reaches a non-parameterizable table-name interpolation unchecked).
type Kind string

const (
	KindScopeNode Kind = "scope_nodes"
	KindPlaylist  Kind = "playlists"
	// KindCast is the authored slidecast (data-model/1 DAT-043): a named,
	// ordered set of slides an operator builds once and references from a
	// playlist item (`source: "cast"`). It is validated with the scheduling-core
	// kinds (schedulingKinds, below) through datamodel.ValidateRows, which is
	// also what makes deleting a cast a playlist still plays a refusal rather
	// than a silently shortened rotation.
	KindCast           Kind = "casts"
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
	// KindWebhookEndpoint is the events/1 outbound-webhook registration kind
	// (EVT-150: "a management-plane resource this contract does not define").
	// The row carries only the endpoint's public configuration — its URL, the
	// schemas it subscribes to, its placement, its labels. Its SIGNING SECRET
	// and its delivery progress are deliberately NOT in this body: the body is
	// served back verbatim by the generic read handlers, so a secret stored here
	// would be a secret handed to every reader. Both live in the private
	// webhook_delivery_state table instead (webhooks.go).
	KindWebhookEndpoint Kind = "webhook_endpoints"
	// KindScreen is the screen's own identity row (data-model/1 DAT-004/DAT-004a):
	// the row a `screen_id` NAMES. It is deliberately not the `screen`-kind scope
	// node — that is a placement classification (DAT-001), and DAT-004 lets a
	// screen row sit under a node of any kind, so two screens may share one node
	// and a screen may sit directly under a site. Its writes are validated through
	// datamodel.ValidateIdentityRows (identityKinds, below).
	KindScreen Kind = "screens"
	// KindAdoptedDevice is the device row (DAT-004/DAT-004a): the row a `device_id`
	// names, carrying the members relay/1 REL-063's `device_inventory` entry is
	// defined as. It is an ADOPTION RECORD, not a copy of the relay's live view —
	// the relay reports discovered-but-unadopted candidates upward (REL-110/111)
	// and the app peer ships the adoption decision back down — which is why it is
	// a durable, authored resource here while the relay's own current report stays
	// the in-memory read model internal/app/devices serves.
	KindAdoptedDevice Kind = "adopted_devices"
	// KindVariable is the data-model/1 Variables kind (DAT-130): a named,
	// scope-placed JSON scalar. It is the api/1 resource baseline plus `name`
	// and `value`, and it is the row a `rules/1` `variable` condition reads
	// (RUL-150) and a `variable_write` action writes (RUL-220).
	//
	// Its writes are validated neither through datamodel.ValidateRows (it is not
	// a scheduling-core row and participates in no cascade) nor through
	// ValidateIdentityRows (it names nothing and nothing references it by id) —
	// they run the per-kind `validate` hook for the name grammar and the scalar
	// rule (DAT-131a/132/133) plus one WriteGuard for name uniqueness within a
	// scope node (DAT-131). See variables.go.
	KindVariable Kind = "variables"
)

// allKinds is every resource table in schema order (scope_nodes first, then the
// six scheduling-core kinds and the cast rows they reference, then automations,
// webhook endpoints, the two identity kinds, and variables). schedulingKinds is
// the subset validated through datamodel.ValidateRows; identityKinds the subset
// validated through datamodel.ValidateIdentityRows; automations are compile-gated
// (compile.Compile) instead — see automations.go; variables run their own
// per-kind validate hook and name-uniqueness guard — see variables.go.
var allKinds = []Kind{
	KindScopeNode, KindPlaylist, KindCast, KindSchedule, KindDaypart,
	KindValidityWindow, KindFallback, KindPresetBatch, KindAutomation,
	KindWebhookEndpoint, KindScreen, KindAdoptedDevice, KindVariable,
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

// DeleteGuard is WriteGuard's counterpart for the one verb that carries no body:
// a caller-supplied invariant check run INSIDE a Delete write transaction, before
// the row is removed, under the SAME write lock the removal commits under.
// Returning a non-nil error rolls the transaction back with nothing deleted and
// is propagated to the caller VERBATIM, exactly as a WriteGuard's is, so the api
// layer can supply a guard that returns its own typed refusal and recognize it on
// the way out.
//
// It is handed a DeleteView rather than the deleted kind's own rows, which is the
// whole reason it is a separate type: what a delete has to answer is not "is this
// row well-formed" — there is no row being written — but "does anything else
// still point at it", and the answer lives in OTHER tables. data-model/1's
// scope-node deletion rules (DAT-020/021) are exactly that question.
type DeleteGuard func(v DeleteView) error

// DeleteView is the read surface a DeleteGuard sees: this store, mid-transaction,
// under the write lock the delete holds. A guard MUST read through this view and
// never through a Store method — every public read takes the read lock, which the
// held write lock would deadlock against — and what it reads is the state the
// delete is about to commit against, with no window for another writer to move it.
type DeleteView interface {
	// Rows returns every row of kind, ordered by id ascending — the same
	// projection List serves, read inside the delete's transaction.
	Rows(kind Kind) ([]Resource, error)
	// PlacedAt returns one row whose own scope_node column names scopeNode, and
	// whether any exists at all, across every table that carries a placement
	// (placementTables). Which one it returns among several is the first match in
	// placement-table order; the question it answers is existence.
	PlacedAt(scopeNode string) (Placement, bool, error)
}

// Store is the app-side SQLite store. Safe for concurrent use: writes serialize
// under mu (write lock) and reads take mu (read lock), so a revision/generation
// bump is atomic against any concurrent read.
type Store struct {
	db *sql.DB
	mu sync.RWMutex
	// nowMs is the clock the OPENER chose (Open's required argument): every
	// stamp this store writes reads it, and nothing in this package reaches for
	// time.Now beside it.
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

	// required is the deployment's required-pack roster (marketplace/1
	// MKT-093a), wired by SetRequiredPacks. Nil — the default — makes no pack
	// required. It is read under mu: the RequiredFloor accessor takes the read
	// lock, and the enforcing check (packsfloor.go) runs inside a write
	// transaction that already holds the write lock.
	required RequiredPacks
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

// WallClockMs reads the host wall clock in epoch milliseconds.
//
// It is exported for the callers that genuinely want the HOST's reading: tests,
// and read-only diagnostics that run outside a serving process (the feeder's
// -store-check, which opens the store, reports and writes nothing). Naming it
// makes that a deliberate choice at the call site rather than an unremarkable
// `func() int64 { return time.Now().UnixMilli() }` nobody reads twice.
//
// A DEPLOYMENT does not pass this. It passes the app's persisted monotonic clock
// floor reading (internal/app/auth.ClockFloor.Now), so a row's created_at, the
// credential rows written in the same request and the audit event describing
// both come from ONE clock.
func WallClockMs() int64 { return time.Now().UnixMilli() }

// Open opens (creating if necessary) the SQLite store at dsn, migrating the
// schema. Pass ":memory:" for an ephemeral, test-only store, or a filesystem
// path for the feeder's persistent store (its parent dir is created 0700). A
// file-backed store runs under WAL + synchronous=FULL; every store uses
// _txlock=immediate so a write transaction takes its write lock up front (no
// read→write upgrade deadlock), reinforcing the serialized write path.
//
// nowMs is the injected clock every stamp this store writes comes from — a
// resource's created_at/updated_at baseline, a job's timestamps, a pack's
// install record, a pairing grant's expiry sweep, a webhook's delivery state.
// It is REQUIRED and positional, exactly as internal/app/auth.Open's is, and for
// the same reason: this package used to hardcode time.Now with no seam at all,
// so the app store stamped rows from the bare host clock while the credential
// store beside it ran on the app's persisted monotonic clock floor (SEC-066).
// The two agree only while the floor is 0. The day a time source advances the
// floor they diverge, and a store baseline written behind the credential row
// created in the same request is a discrepancy nobody would think to look for.
//
// There is deliberately NO variadic option with a wall-clock default. A default
// that silently reads the host clock is the defect this argument exists to
// remove, and a seam nobody is forced to fill is a seam that stays unfilled —
// which is how the hardcoded one survived as long as it did. A caller that
// genuinely wants the host's reading passes WallClockMs and says so.
func Open(dsn string, nowMs func() int64) (*Store, error) {
	if nowMs == nil {
		return nil, errors.New("store: Open requires a clock (a deployment passes auth.ClockFloor.Now; a test or a read-only diagnostic passes store.WallClockMs)")
	}
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

	// ARC-041/104: decide the newer-epoch refusal from the file's own recorded
	// epoch BEFORE any migration runs. Running this build's (older) DDL over a
	// file a newer build wrote is exactly the downgrade-open the refusal forbids,
	// so the check is the first thing that touches the database. A pre-marker
	// file reads 0 and is upgraded below rather than refused.
	onDiskEpoch, err := readSchemaEpoch(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if onDiskEpoch > PlatformSchemaEpoch {
		_ = db.Close()
		return nil, &EpochTooNewError{OnDisk: onDiskEpoch, Understood: PlatformSchemaEpoch}
	}

	// Additive-column convergence, BEFORE this build's DDL runs (schemamigrate.go).
	//
	// The order is load-bearing in both directions. It must run AFTER the epoch
	// gate, because converging a file a newer build wrote is the downgrade-open
	// ARC-041 forbids. And it must run BEFORE applySchemaDDL, because that DDL
	// carries `CREATE INDEX IF NOT EXISTS` statements: an index over a column the
	// on-disk table is missing does not no-op, it FAILS, which would turn a
	// silently-drifted store into one that cannot be opened at all. Adding the
	// columns first is what makes this build's own DDL a no-op rather than an
	// error.
	//
	// A conforming store — every fresh install, and every box from the second
	// boot after an upgrade — opens no transaction here at all.
	schemaChanges, err := migrateSchemaColumns(context.Background(), db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	reportSchemaMigration(dsn, schemaChanges)

	if err := applySchemaDDL(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	// CREATE TABLE IF NOT EXISTS is a no-op against a table an earlier build
	// already created, so a column added since then needs its own step
	// (migratePairingGrantsSchema's own doc). migrateSchemaColumns above now
	// converges EVERY such column generically, which makes these three a no-op
	// on any store it has already run over.
	//
	// They are kept rather than deleted in the same change that adds their
	// replacement: the generic pass has to prove itself against real boxes
	// before the hand-written path it supersedes is removed, and each of these
	// is idempotent by its own PRAGMA read, so running both costs three column
	// lookups and can never double-apply. Deleting them is a follow-up.
	if err := migratePairingGrantsSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate pairing grants: %w", err)
	}
	if err := migratePacksSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate packs: %w", err)
	}
	if err := migratePackInstallsSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate pack installs: %w", err)
	}

	// Every migration above has run, so the file now holds this build's shape:
	// stamp its epoch. A fresh file (read 0) and a pre-marker legacy file (also
	// 0) both advance to the current epoch here; a file already at this epoch is
	// re-stamped idempotently. A file at a NEWER epoch never reaches this line —
	// it was refused before any migration.
	if err := writeSchemaEpoch(db, PlatformSchemaEpoch); err != nil {
		_ = db.Close()
		return nil, err
	}

	// This database is secret material at rest, and was sitting at the umask
	// default. It holds the sealed webhook signing secrets, the durable audit
	// log, and the install records naming which key vouched for the code that is
	// running — none of which belongs to anyone but the process that opened it.
	// The sidecars go with it: WAL mode writes recently-changed page images to
	// `<db>-wal`, so leaving that readable leaks exactly what the database mode
	// is protecting.
	if dsn != ":memory:" {
		if err := os.Chmod(dsn, 0o600); err != nil && !os.IsNotExist(err) {
			_ = db.Close()
			return nil, fmt.Errorf("store: chmod %s: %w", dsn, err)
		}
		if err := secretfile.TightenSQLiteSidecars(dsn, 0o600); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: %w", err)
		}
	}

	s := &Store{db: db, nowMs: nowMs}
	// Seed the first-seen ledger from the column it took over, if this is the
	// upgrade that introduces it (devicefirstseen.go). It runs HERE rather than
	// in applySchemaDDL because it has to judge candidate values against the
	// app's own clock, which only exists once the store is built — and after the
	// column migration above, since the rows it reads must be the migrated shape.
	if err := s.backfillDeviceFirstSeen(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	// The store's own integrity report, once, at boot. A store carrying rows an
	// older build accepted and this build's validators do not is a legitimate
	// state — writes to it still work, because a write is judged on what IT
	// introduced (priorfaults.go) — but it is not a state anyone should have to
	// discover from a puzzling write refusal. A clean store logs nothing.
	s.reportStoredFaults(context.Background())
	return s, nil
}

// ErrNoStoreAtPath is returned by OpenReadOnly when there is no store file at
// the path. A read-only open cannot create one, and a caller that reports on a
// store an operator named needs to say "there is nothing here yet" rather than
// invent one — see reportStoreIDs, which is what a fresh path used to do.
var ErrNoStoreAtPath = errors.New("store: no store file at this path")

// OpenReadOnly opens an EXISTING store for inspection and cannot write to it —
// not the columns, not the epoch marker, not the file mode, not a sidecar.
//
// It exists because `waiveo-feeder -store-check` is documented as a dry run and
// was not one. Open MIGRATES: it adds missing columns, stamps the schema epoch,
// chmods the file to 0600 and leaves -wal/-shm sidecars beside it. So a check
// that opened the store to report on it was, in the same breath, applying the
// change it had just described in the future tense — and pointed at the one
// artefact an operator most wants to read without touching (a pre-deploy backup,
// or the live store under a running feeder), it silently converted that artefact
// to this build's schema. Asked twice, it gave two different answers, and the
// second one said the drift had never been there.
//
// So the reporting path gets a handle that CANNOT do any of that. The
// difference from Open is only what is left out: no migration, no DDL, no epoch
// stamp, no chmod, no fault report. The epoch gate is kept and applied first,
// because a workspace a newer build wrote is one this build must not interpret,
// let alone describe (ARC-041/104).
//
// Writes through the returned Store fail with SQLite's own read-only refusal.
// That is the intended behaviour and not a check this package repeats: the
// database is opened `mode=ro`, so the guarantee is enforced by the file handle
// rather than by remembering to test a flag at every call site.
//
// One honest caveat, stated rather than papered over: SQLite cannot read a
// WAL-mode database without its shared-memory index, so opening one whose
// sidecars are absent CREATES a `<db>-shm` and a zero-length `<db>-wal` beside
// it. The database file itself is not touched — not a byte, not its mode — and
// an empty write-ahead log holds no frames, so nothing about the store's content
// changes. They are not removed on close: deleting a `-shm` that another process
// has mapped is how a live database gets corrupted, and a check must never take
// that risk with a box that is serving.
func OpenReadOnly(dsn string, nowMs func() int64) (*Store, error) {
	if nowMs == nil {
		return nil, errors.New("store: OpenReadOnly requires a clock (a read-only diagnostic passes store.WallClockMs)")
	}
	if dsn == ":memory:" {
		return nil, errors.New("store: OpenReadOnly needs a file; an in-memory store has nothing to inspect")
	}
	if _, err := os.Stat(dsn); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNoStoreAtPath, dsn)
		}
		return nil, fmt.Errorf("store: stat %s: %w", dsn, err)
	}

	db, err := sql.Open("sqlite", "file:"+dsn+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("store: open %s read-only: %w", dsn, err)
	}
	db.SetMaxOpenConns(1)

	onDiskEpoch, err := readSchemaEpoch(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if onDiskEpoch > PlatformSchemaEpoch {
		_ = db.Close()
		return nil, &EpochTooNewError{OnDisk: onDiskEpoch, Understood: PlatformSchemaEpoch}
	}
	return &Store{db: db, nowMs: nowMs}, nil
}

// applySchemaDDL creates every table and index this build declares, over a
// database that may already hold some, all or none of them. It is the store's
// ONE inventory of its own shape.
//
// Extracted out of Open — where these statements used to sit inline — because
// something else now has to run them: schemamigrate.go builds a throwaway
// in-memory database with this exact function and reads the resulting shape
// back out to learn what a fresh store's columns ARE. That is
// what makes the expected column set incapable of drifting from the DDL: there
// is no second list to keep in step, and SQLite rather than a parser of ours
// decides what the DDL declares. A table created anywhere but here is invisible
// to that model, which TestDeclaredSchemaMatchesAFreshStore fails on.
//
// Every statement is `IF NOT EXISTS`, so this is safe to run at every open and
// on every store. What it deliberately CANNOT do is change a table that already
// exists — that is the whole of the defect schemamigrate.go exists to close, and
// the reason the additive pass runs before this does.
func applySchemaDDL(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("store: migrate meta: %w", err)
	}
	// The automations table carries the baseline PLUS an execution_class column,
	// so it is created from its own DDL first; the shared-baseline loop below then
	// no-ops over it (CREATE TABLE IF NOT EXISTS).
	if _, err := db.Exec(automationsTableDDL); err != nil {
		return fmt.Errorf("store: migrate automations: %w", err)
	}
	// The declarative-packs tables (packs + pack_files + pack_rows) are a
	// self-contained subsystem with their own dedicated CRUD (packs.go), not a
	// generic resource Kind, so they are migrated from their own DDL here.
	if _, err := db.Exec(automationVersionsSchema); err != nil {
		return fmt.Errorf("store: create automation_versions: %w", err)
	}
	if _, err := db.Exec(packsSchema); err != nil {
		return fmt.Errorf("store: migrate packs: %w", err)
	}
	// The api/1 Job tables (jobs + job_targets) are likewise their own
	// subsystem with their own CRUD (jobs.go): a Job is an execution record
	// over other rows, not a resource Kind, so it carries no revision,
	// external_id or scope_node of its own and never joins the desired-state
	// projection.
	if _, err := db.Exec(jobsSchema); err != nil {
		return fmt.Errorf("store: migrate jobs: %w", err)
	}
	// The events/1 durable event log (events + event_log_meta) is likewise its
	// own subsystem with its own reader/writer (eventlog.go): an event is an
	// immutable record, never updated after it is written, carrying no revision
	// and no place in the desired-state projection. It rides this store — rather
	// than a database of its own — so a box has ONE file to back up and one to
	// restore, and so the audit trail cannot be separated from the authored rows
	// it describes.
	if _, err := db.Exec(eventsSchema); err != nil {
		return fmt.Errorf("store: migrate events: %w", err)
	}
	// The outbound-webhook delivery state (webhook_delivery_state) is its own
	// subsystem for the same reason jobs are: it is execution state ABOUT a
	// resource row rather than a resource row itself, and it holds the sealed
	// signing secrets that must never appear in a served representation
	// (webhooks.go).
	if _, err := db.Exec(webhooksSchema); err != nil {
		return fmt.Errorf("store: migrate webhook delivery state: %w", err)
	}
	// The pending pairing-grant records (pairing_grants) are their own
	// subsystem too: a grant is minted and consumed, never PATCHed, and its
	// rows join the desired-state projection as relay/1's `pairing_grants`
	// section rather than as an api/1 resource family (pairinggrants.go).
	if _, err := db.Exec(pairingGrantsSchema); err != nil {
		return fmt.Errorf("store: migrate pairing grants: %w", err)
	}
	// The durable mirror of the relays' device.candidates reports
	// (discovered_devices). Its own table rather than a resource Kind: nothing
	// authors a sighting, it carries no revision to condition a write on, and a
	// relay's report replaces its rows wholesale — none of which the generic CRUD
	// machinery models (discovereddevices.go).
	if _, err := db.Exec(discoveredDevicesSchema); err != nil {
		return fmt.Errorf("store: migrate discovered devices: %w", err)
	}
	// The operator's IGNORE decisions (ignored_devices). Its own table beside the
	// mirror rather than a resource Kind for the same reasons: nothing authors it
	// into desired-state, it carries no revision, and — unlike adoption — it never
	// reaches a relay, so it must not bump the generation (ignoreddevices.go).
	if _, err := db.Exec(ignoredDevicesSchema); err != nil {
		return fmt.Errorf("store: migrate ignored devices: %w", err)
	}
	// When this site first saw each device (device_first_seen). Beside the mirror
	// rather than inside it for the reason the ignore decisions are: a relay's
	// report REPLACES that relay's mirror rows wholesale, so a fact that must
	// outlive every report — and every relay restart, which re-mints the relay's
	// idea of "first" from nothing — cannot live in a row the next report deletes
	// (devicefirstseen.go).
	if _, err := db.Exec(deviceFirstSeenSchema); err != nil {
		return fmt.Errorf("store: migrate device first seen: %w", err)
	}
	// Revoked screens and relay certificates (api/1 API-140, revocations.go).
	// Their own table rather than a column, so a revocation outlives the row it
	// concerns — the store hard-deletes, and a revocation lost to a tidy-up is
	// a relay that stops refusing a credential it should still refuse.
	if _, err := db.Exec(revocationsDDL); err != nil {
		return fmt.Errorf("store: migrate revocations: %w", err)
	}
	for _, k := range allKinds {
		if _, err := db.Exec(fmt.Sprintf(resourceTableDDL, string(k))); err != nil {
			return fmt.Errorf("store: migrate %s: %w", k, err)
		}
	}
	return nil
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
// (BuildFullScopeTree for scope nodes, ValidateRows for scheduling kinds) before
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
	// …and its opposite: a member the platform DERIVES is dropped rather than
	// persisted (derivedmembers.go). Beside the completion rather than anywhere
	// else, because both answer the same question — what the stored
	// representation of this row is, as against what a writer happened to send.
	body = stripDerivedMembers(kind, body)
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
		// The faults the store ALREADY carries for this kind, read before the
		// insert so the post-write validation can judge this create on what it
		// introduced rather than on what it inherited (priorfaults.go).
		prior, err := capturePriorFaults(ctx, tx, kind)
		if err != nil {
			return err
		}
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
		return validateAfterWrite(ctx, tx, kind, bf.ScopeNode, prior)
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
		// Before anything is merged or written: the faults this kind's rows
		// already carry, so the post-write validation judges this patch on what
		// it introduced rather than on what it inherited (priorfaults.go).
		prior, err := capturePriorFaults(ctx, tx, kind)
		if err != nil {
			return err
		}
		var curRev int64
		var curBody string
		err = tx.QueryRowContext(ctx, `SELECT revision, body FROM `+table+` WHERE id = ?`, id).Scan(&curRev, &curBody)
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
		// Over the EFFECTIVE post-merge body, for the reason the completion runs
		// there too: a patch is where a derived member most easily arrives, since
		// a console read-modify-write echoes back the whole document it was
		// served.
		merged = stripDerivedMembers(kind, merged)
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
		// RUL-394: the definition this write REPLACED, captured in the same
		// transaction. Outside it, the one write that mattered is the one whose
		// version can be missing.
		if err := recordAutomationVersion(ctx, tx, kind, id, curRev, curBody, now); err != nil {
			return err
		}
		if err := bumpGeneration(ctx, tx); err != nil {
			return err
		}
		if err := validateAfterWrite(ctx, tx, kind, bf.ScopeNode, prior); err != nil {
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
//
// That post-write revalidation is deliberately NOT the whole of referential
// integrity, and a caller must not read it as such. It re-runs the kind's own
// validator over what remains, and no validator sees a reference INTO the
// scope-node tree: datamodel.ValidateRows checks a row's references to other
// ROWS, never that a row's own scope_node names a node that exists, so removing
// a node rows are placed at revalidates clean (DAT-021). Rules a remaining-row-set
// check cannot see are the caller's to supply as guards, which is what
// DeleteGuard is for.
//
// One rule is now covered TWICE, and the guard order is what decides which
// answer a caller gets. Removing an interior scope node leaves its children's
// parent_id unresolvable, which validateAfterWrite's strict tree
// (datamodel.BuildFullScopeTree) rejects — but as a 422 VALIDATION_FAILED
// carrying SCOPE_NODE_PARENT_INVALID, which is the wrong shape for a deletion
// refusal. DAT-020 publishes SCOPE_NODE_NOT_EMPTY for it. The guards run BEFORE
// the removal, so the published refusal answers first and the strict tree is
// only the backstop underneath it.
func (s *Store) Delete(ctx context.Context, kind Kind, id string, rev int64, guards ...DeleteGuard) error {
	table, err := tableFor(kind)
	if err != nil {
		return err
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		// The faults already standing before this removal. A DELETE is the repair
		// path — the ONE operation that must stay available on a store carrying a
		// row no current validator accepts — so it is judged strictly on what
		// removing this row breaks, never on what it leaves untouched
		// (priorfaults.go).
		prior, err := capturePriorFaults(ctx, tx, kind)
		if err != nil {
			return err
		}
		var curRev int64
		err = tx.QueryRowContext(ctx, `SELECT revision FROM `+table+` WHERE id = ?`, id).Scan(&curRev)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: delete read %s: %w", kind, err)
		}
		if curRev != rev {
			return &RevisionMismatchError{Expected: rev, Current: curRev}
		}
		// The guards run against the store as it still stands — before the row is
		// removed, inside the transaction that would remove it. A guard that
		// refuses rolls the whole thing back with nothing written, and its error
		// reaches the caller verbatim.
		for _, g := range guards {
			if err := g(txDeleteView{ctx: ctx, tx: tx}); err != nil {
				return err
			}
		}
		// RUL-394: a deleted rule's versions go with it, on the same terms its
		// own rows do — restoring a version must not be a way to resurrect a
		// rule somebody removed.
		if err := deleteAutomationVersions(ctx, tx, kind, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE id = ?`, id); err != nil {
			return fmt.Errorf("store: delete %s: %w", kind, err)
		}
		if err := bumpGeneration(ctx, tx); err != nil {
			return err
		}
		// No placement to resolve: a delete removes a reference, never adds one.
		return validateAfterWrite(ctx, tx, kind, "", prior)
	})
}

// txDeleteView is the DeleteView a Delete hands its guards: the live transaction,
// nothing else. Every read a guard makes therefore goes through the same handle
// the removal will, sees the store exactly as the removal will find it, and can
// never reach for a Store method that would deadlock on the held write lock.
type txDeleteView struct {
	ctx context.Context
	tx  *sql.Tx
}

func (v txDeleteView) Rows(kind Kind) ([]Resource, error) {
	table, err := tableFor(kind)
	if err != nil {
		return nil, err
	}
	return readResources(v.ctx, v.tx, table)
}

func (v txDeleteView) PlacedAt(scopeNode string) (Placement, bool, error) {
	return placedAt(v.ctx, v.tx, scopeNode)
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

// AdvanceGeneration bumps the desired-state generation with NO row change, and
// fires the OnCommit hook exactly as a resource write does.
//
// It exists for one situation, and should not acquire a second without a reason
// as specific: a screen's program override has a TTL (data-model/1 DAT-004c's
// `expires_at`), so the desired state a screen resolves to CHANGES AT AN INSTANT
// with nothing written. Every other desired-state change is caused by a write
// and rides that write's generation bump; this one is caused by the clock.
//
// The generation is the whole delivery mechanism: a relay pulls only when nudged
// and answers `state.unchanged` for a generation it already holds (REL-050/051),
// so a re-derived snapshot at an unchanged generation reaches nobody. Advancing
// it is what makes the lapse arrive at the screen.
//
// This does NOT retire the override, and DAT-004d's "no write required to retire
// it" is not weakened by it: the row is untouched, the `override` member is
// still there, and it is `ScreenOverride.Applies` at resolution time — not this
// call — that stops it applying. This only publishes the fact.
func (s *Store) AdvanceGeneration(ctx context.Context) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error { return bumpGeneration(ctx, tx) })
}
