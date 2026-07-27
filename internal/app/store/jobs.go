package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	apiv1 "github.com/maaxton/waiveo-next/api/gen/go"
	"github.com/maaxton/waiveo-next/internal/shared/apijob"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// jobs.go is the durable execution record behind api/1's Job resource
// (API-110-117): the persisted half of internal/shared/apijob, whose state
// machine this file stores and reloads rather than re-deriving.
//
// It exists because API-112 makes polling the ONLY way a client learns an async
// operation finished — "a client determines completion by reading the Job
// resource again and inspecting `state`, not by any signal delivered on the
// original request". A Job that lives only in the accepting handler's memory
// makes that sentence unsatisfiable: the 202 response is the first and last time
// anyone sees it. API-116 then requires the record itself to survive the
// process — "a server crash or restart MUST NOT lose a target's already-reached
// terminal state, and MUST resume any target left `running` rather than silently
// drop it". This file supplies the durability that requirement is stated over:
// every accepted Job and every subsequent per-target transition is committed
// before it is observable, so a restart reloads the exact target states last
// committed (apijob.Restore), never an all-pending reset.
//
// AdvanceJob is the seam the executor drives (the api layer's JobRunner, which
// walks an accepted job's targets and commits each transition here).
//
// The record holds both halves a resume needs: WHICH rows the job acts on
// (job_targets) and WHAT was being applied to them (the JobOperation persisted
// on the job row). InterruptedRuns below joins the two into the inventory a
// restarted process resumes from — one entry per job still holding a target
// left `running`, carrying the accepted operation so the executor has something
// to re-apply rather than a bare target list it can only stare at.
//
// The store persists the operation OPAQUELY: a kind string and a JSON payload,
// neither of which it interprets. The vocabulary belongs to the layer that
// accepted the operation and will re-apply it (the api layer), and a store that
// knew "automations.bulk-enable" would have to be edited every time a new async
// operation is added — which is exactly how a durable record and its executor
// drift apart.
//
// Two deliberate departures from the resource-table CRUD above:
//
//   - A Job is NOT a resource Kind. It carries no revision, no external_id, no
//     labels and no scope_node of its own; it is an execution record over OTHER
//     rows, and modelling it as a Kind would hand it an ETag/If-Match surface and
//     a place in the desired-state projection that api/1 gives it no meaning for.
//     It gets its own tables and its own dedicated CRUD, exactly as the packs
//     subsystem does.
//   - A job write does NOT bump the store generation, and so runs through
//     runWriteTx rather than writeTx: the generation is the feeder's
//     desired-state cursor (REL-052), and a target's progress through a job is
//     not desired state. Bumping it would nudge every connected relay to re-fetch
//     a snapshot that did not change, once per target transition.

// jobsSchema creates the two Job tables. The target set is a child table rather
// than a JSON blob on the job row so a single target's transition is a
// single-row UPDATE, and so `state` is queryable — which is what makes the
// interrupted-run inventory (InterruptedRuns) a query rather than a full scan
// and decode of every job that ever ran.
//
// seq preserves the target ORDER the accepting handler resolved (API-112's
// `targets` is an array, and the 202 body's order is the order a later poll must
// still show); (job_id, seq) is the primary key, with target_id unique per job
// enforced by its own index — apijob.New collapses duplicate ids on the way in,
// so two rows sharing one target_id would be a corrupt record, and the index
// makes the store refuse to hold one.
//
// op_kind/op_payload carry the accepted operation (JobOperation) on the JOB row
// rather than per target: one job is one operation applied to many rows, so
// storing it per target would be N copies of one fact and N places for them to
// disagree. Both default to empty, which is the record's honest way of saying
// "this job persisted no re-appliable operation" — see JobOperation.
const jobsSchema = `
CREATE TABLE IF NOT EXISTS jobs (
	id         TEXT PRIMARY KEY,
	created_by TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	op_kind    TEXT NOT NULL DEFAULT '',
	op_payload TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS job_targets (
	job_id     TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	target_id  TEXT NOT NULL,
	state      TEXT NOT NULL,
	err_code   TEXT NOT NULL DEFAULT '',
	err_detail TEXT NOT NULL DEFAULT '',
	scope_node TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (job_id, seq)
);
CREATE UNIQUE INDEX IF NOT EXISTS job_targets_unique_target ON job_targets (job_id, target_id);
`

// ErrJobExists is returned by CreateJob when the supplied Job id is already
// persisted. A server-minted ULID colliding is an internal fault, not a client
// error — the api layer surfaces it as INTERNAL, never as a validation problem
// the caller could act on.
var ErrJobExists = errors.New("store: job already exists")

// JobOperation is WHAT an accepted Job was applying to its targets, persisted
// alongside WHICH rows it applies to — the durable intent a restarted process
// re-applies to a target an interrupted run left `running` (API-116's "MUST
// resume any target left `running` rather than silently drop it").
//
// It is opaque to the store: Kind names the operation in the accepting layer's
// own vocabulary and Payload is that operation's arguments as JSON, and this
// package parses neither. It only guarantees they come back byte-for-byte as
// they went in.
//
// The ZERO value is meaningful and is not a defect: it records that the job
// carries NO re-appliable operation, which is the truthful entry for work whose
// arguments must not be persisted (a client passphrase) or whose execution
// cannot be safely repeated. Such a job's stranded targets are reconciled by
// the executor rather than resumed — the record's job is to make the difference
// legible, not to pretend every operation can be replayed.
type JobOperation struct {
	// Kind names the operation. Empty means the job persisted none.
	Kind string
	// Payload is the operation's arguments as JSON, or nil when it takes none.
	Payload []byte
}

// JobRecord is one persisted Job as read back: the shared api/1 state machine
// (apijob.Job — the same type the accepting handler built and the same one that
// renders the wire resource), plus the per-target scope-node placements a READ
// of the Job is scoped by.
//
// The placements are carried alongside rather than inside apijob because they
// are not part of API-112's Job shape and never appear on the wire: they exist
// only so the api layer can decide whether this caller may see this Job at all
// (a Job's `targets` enumerate rows by id, so showing one to a caller who cannot
// read those rows would leak exactly what a scoped list refuses to). They are
// recorded at ACCEPTANCE time — the placement each target held when the job was
// accepted over it — so a Job's readability does not drift as rows are later
// moved or deleted, and cannot be widened by moving a row.
type JobRecord struct {
	Job *apijob.Job
	// ScopeNodes maps a target id to the scope node it was placed at when the
	// Job was accepted. A target the accepting handler could not attribute to a
	// node maps to "", which no scope-bound principal can read.
	ScopeNodes map[string]string
}

// CreateJob persists a freshly-accepted Job (API-111): the job row plus one
// job_targets row per target, in the job's own target order, committed in ONE
// transaction — so a crash between the two can never leave a Job with a
// truncated target set, and the 202 the caller is about to receive names a Job
// that is already durable (API-116).
//
// scopeNodes supplies the placement each target held at acceptance (see
// JobRecord); a target absent from the map is stored with an empty placement,
// which is readable only by a principal whose authority reaches the workspace
// root.
//
// op is the operation being applied (JobOperation), committed in that SAME
// transaction as the targets it applies to. That is the point of putting it
// here rather than in a follow-up write: a job durable enough to be polled but
// not durable enough to say what it was doing is exactly the record that
// stranded a restarted process with a target list and nothing to do to it. The
// zero JobOperation is accepted and means the job persists no re-appliable
// operation.
//
// The Job's id MUST be a valid ULID (DAT-005a): every id the store persists is
// one, and the api layer draws this one from the same injected id source every
// other server-minted id comes from, so a malformed id here is a wiring fault
// worth refusing at the boundary rather than storing.
func (s *Store) CreateJob(ctx context.Context, j *apijob.Job, scopeNodes map[string]string, op JobOperation) error {
	if j == nil {
		return errors.New("store: create job: nil job")
	}
	if !ulid.Valid(j.ID) {
		return fmt.Errorf("store: create job: id %q is not a valid ULID", j.ID)
	}
	if j.CreatedBy == "" {
		return fmt.Errorf("store: create job %s: created_by is required (API-112)", j.ID)
	}
	now := s.nowMs()
	return s.runWriteTx(ctx, func(tx *sql.Tx) error {
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id = ?`, j.ID).Scan(&exists)
		if err == nil {
			return fmt.Errorf("%w: %s", ErrJobExists, j.ID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: create job read %s: %w", j.ID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO jobs (id, created_by, created_at, updated_at, op_kind, op_payload)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			j.ID, j.CreatedBy, j.CreatedAt.UnixMilli(), now, op.Kind, string(op.Payload),
		); err != nil {
			return fmt.Errorf("store: insert job %s: %w", j.ID, err)
		}
		return insertJobTargets(ctx, tx, j, scopeNodes)
	})
}

// GetJob reads the persisted Job with id and whether it exists, rebuilding the
// shared state machine from the committed per-target record (apijob.Restore) —
// so a target that reached a terminal state before a restart comes back in that
// state, and the job-level state is re-derived from the restored targets rather
// than stored separately and trusted (API-113: the job state is a function of
// its targets, never an independent field that could drift).
func (s *Store) GetJob(ctx context.Context, id string) (JobRecord, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readJob(ctx, s.db, id)
}

// AdvanceJob applies one or more state-machine transitions to the persisted Job
// with id and commits the result: it reloads the job under the write lock, hands
// the reconstructed apijob.Job to apply — which drives the SAME StartTarget /
// SucceedTarget / FailTarget / Cancel transitions any in-memory Job does — and
// writes back every target's resulting state in the same transaction.
//
// The reload-inside-the-transaction is the point. The write path serializes
// under Store.mu, so apply evaluates against the state the transition commits
// over, never a snapshot read in an earlier critical section that a concurrent
// advance could have moved underneath it.
//
// An illegal transition is REFUSED, not partially applied: apply's error rolls
// the transaction back before anything is written and is propagated verbatim, so
// the caller sees the state machine's own diagnosis (apijob's "target %q is %s,
// not running") and the persisted record is exactly what it was. A missing job
// is ErrNotFound.
func (s *Store) AdvanceJob(ctx context.Context, id string, apply func(*apijob.Job) error) (JobRecord, error) {
	if apply == nil {
		return JobRecord{}, errors.New("store: advance job: nil apply")
	}
	var out JobRecord
	err := s.runWriteTx(ctx, func(tx *sql.Tx) error {
		rec, found, err := readJob(ctx, tx, id)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		if err := apply(rec.Job); err != nil {
			return err
		}
		if err := updateJobTargets(ctx, tx, rec.Job, s.nowMs()); err != nil {
			return err
		}
		out = rec
		return nil
	})
	if err != nil {
		return JobRecord{}, err
	}
	return out, nil
}

// InterruptedRuns returns one entry per Job still holding at least one target in
// the `running` state — each carrying the operation that job accepted and the
// ids of its stranded targets, in the job's own target order. It is the
// inventory API-116's "MUST resume any target left `running` rather than
// silently drop it" is resumed FROM after a restart.
//
// A target is `running` only because the executor committed that transition
// BEFORE attempting its write, so every entry here names work whose outcome this
// process cannot observe: it may have completed, half-completed, or never
// started. That is precisely why the operation rides along — a resume needs
// something to re-apply, and re-applying is the only way to reach a state the
// record can honestly report.
//
// It deliberately reports rather than repairs. Rewriting these rows to pending,
// or failing them, would be the store answering an execution question it cannot
// answer: whether re-applying THIS operation is safe is a property of the
// operation, and only the layer that accepted it knows. The store's job is to
// still be holding the target, and the operation, when that layer asks.
//
// Jobs are returned in id order (which for ULIDs is acceptance order), so a
// resume replays the oldest interrupted work first.
func (s *Store) InterruptedRuns(ctx context.Context) ([]InterruptedRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.job_id, j.op_kind, j.op_payload, t.target_id
		 FROM job_targets t JOIN jobs j ON j.id = t.job_id
		 WHERE t.state = ? ORDER BY t.job_id ASC, t.seq ASC`,
		string(apiv1.JobTargetStateRunning))
	if err != nil {
		return nil, fmt.Errorf("store: list interrupted job runs: %w", err)
	}
	defer rows.Close()
	out := []InterruptedRun{}
	for rows.Next() {
		var jobID, opKind, opPayload, targetID string
		if err := rows.Scan(&jobID, &opKind, &opPayload, &targetID); err != nil {
			return nil, fmt.Errorf("store: scan interrupted job run: %w", err)
		}
		// The rows arrive grouped by job (job_id is the leading ORDER BY term), so
		// one entry accumulates until the id changes — no map, and target order is
		// the job's own seq order rather than whatever a map iteration produced.
		if n := len(out); n > 0 && out[n-1].JobID == jobID {
			out[n-1].TargetIDs = append(out[n-1].TargetIDs, targetID)
			continue
		}
		op := JobOperation{Kind: opKind}
		if opPayload != "" {
			op.Payload = []byte(opPayload)
		}
		out = append(out, InterruptedRun{JobID: jobID, Op: op, TargetIDs: []string{targetID}})
	}
	return out, rows.Err()
}

// InterruptedRun is one Job an earlier process left mid-flight: the operation it
// accepted, and the targets that process committed as `running` and never
// carried to a terminal state.
type InterruptedRun struct {
	JobID     string
	Op        JobOperation
	TargetIDs []string
}

// insertJobTargets writes a job's full target set in job order.
func insertJobTargets(ctx context.Context, tx *sql.Tx, j *apijob.Job, scopeNodes map[string]string) error {
	for i, t := range j.Targets() {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO job_targets (job_id, seq, target_id, state, err_code, err_detail, scope_node)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			j.ID, i, t.ID, string(t.State), t.ErrCode, t.ErrDetail, scopeNodes[t.ID],
		); err != nil {
			return fmt.Errorf("store: insert job target %s/%s: %w", j.ID, t.ID, err)
		}
	}
	return nil
}

// updateJobTargets writes back every target's post-transition state and touches
// the job's updated_at. The scope-node placement is untouched: it records where
// each target sat when the job was ACCEPTED, and no transition may rewrite it.
func updateJobTargets(ctx context.Context, tx *sql.Tx, j *apijob.Job, now int64) error {
	for _, t := range j.Targets() {
		if _, err := tx.ExecContext(ctx,
			`UPDATE job_targets SET state = ?, err_code = ?, err_detail = ? WHERE job_id = ? AND target_id = ?`,
			string(t.State), t.ErrCode, t.ErrDetail, j.ID, t.ID,
		); err != nil {
			return fmt.Errorf("store: update job target %s/%s: %w", j.ID, t.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET updated_at = ? WHERE id = ?`, now, j.ID); err != nil {
		return fmt.Errorf("store: touch job %s: %w", j.ID, err)
	}
	return nil
}

// readJob is the single Job reader both the read path (GetJob, over *sql.DB) and
// the write path (AdvanceJob, over its own *sql.Tx) share, so an advance decides
// its transition against exactly the record a read would return.
func readJob(ctx context.Context, q interface {
	queryer
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, id string) (JobRecord, bool, error) {
	var createdBy string
	var createdAt int64
	err := q.QueryRowContext(ctx, `SELECT created_by, created_at FROM jobs WHERE id = ?`, id).Scan(&createdBy, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return JobRecord{}, false, nil
	}
	if err != nil {
		return JobRecord{}, false, fmt.Errorf("store: get job %s: %w", id, err)
	}

	rows, err := q.QueryContext(ctx,
		`SELECT target_id, state, err_code, err_detail, scope_node
		 FROM job_targets WHERE job_id = ? ORDER BY seq ASC`, id)
	if err != nil {
		return JobRecord{}, false, fmt.Errorf("store: get job targets %s: %w", id, err)
	}
	defer rows.Close()

	var targets []apijob.Target
	scopeNodes := map[string]string{}
	for rows.Next() {
		var t apijob.Target
		var state, scopeNode string
		if err := rows.Scan(&t.ID, &state, &t.ErrCode, &t.ErrDetail, &scopeNode); err != nil {
			return JobRecord{}, false, fmt.Errorf("store: scan job target: %w", err)
		}
		t.State = apiv1.JobTargetState(state)
		targets = append(targets, t)
		scopeNodes[t.ID] = scopeNode
	}
	if err := rows.Err(); err != nil {
		return JobRecord{}, false, fmt.Errorf("store: get job targets %s: %w", id, err)
	}

	// Restore validates the persisted record against the state machine's own
	// closed vocabulary: a row holding a state or a failure code the live
	// transitions could not have produced is refused here rather than served as
	// if the machine had produced it.
	j, err := apijob.Restore(id, createdBy, time.UnixMilli(createdAt).UTC(), targets)
	if err != nil {
		return JobRecord{}, false, fmt.Errorf("store: restore job %s: %w", id, err)
	}
	return JobRecord{Job: j, ScopeNodes: scopeNodes}, true, nil
}
