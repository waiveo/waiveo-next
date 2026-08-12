package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/rules/compile"
)

// automationsTableDDL creates the automations table: the shared api/1 resource
// baseline (id/revision/external_id/labels/scope_node/timestamps + body) PLUS an
// execution_class column carrying the rules compiler's edge/app classification
// (compile.Classify). It is created from its own DDL (rather than the shared
// resourceTableDDL) precisely because of that extra column; Open runs it before
// the shared-baseline loop, which then no-ops over the already-present table.
const automationsTableDDL = `
CREATE TABLE IF NOT EXISTS automations (
	id              TEXT PRIMARY KEY,
	revision        INTEGER NOT NULL,
	external_id     TEXT NOT NULL DEFAULT '',
	labels          TEXT NOT NULL DEFAULT '{}',
	scope_node      TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL,
	body            TEXT NOT NULL,
	execution_class TEXT NOT NULL DEFAULT ''
);
`

// Execution-class column values (compile.Classify's ExecutionClass). Only edge
// rules ride the relay's edge_rules section (REL-062); app rules are stored and
// validated but their execution is deferred to an app-side runtime.
const (
	executionClassEdge = "edge"
	executionClassApp  = "app"
)

// rulesMinorVersion names the rules/1 minor an authored automation body is
// compiled against — the value the feeder carries as edge_rules.rules_minor_version
// (REL-062). Store-owned so the feeder derives it from EdgeRuleBodies rather than
// hardcoding its own copy.
const rulesMinorVersion = "1.0"

// compileGate runs the rules compiler over an automation body before it is
// committed, returning the compiler's edge/app classification. For any other kind
// it is a no-op (no execution class). A non-compiling rule returns the compiler's
// typed *compile.CompileError verbatim so the api layer can surface it as a 422
// VALIDATION_FAILED carrying the compiler's field/message — the compiler is the
// single validator, never re-implemented here. Injected baseline keys
// (revision/created_at/updated_at/…) are ignored by the rule parser, so gating the
// stored body is equivalent to gating the authored rule.
func compileGate(kind Kind, body json.RawMessage) (string, error) {
	if kind != KindAutomation {
		return "", nil
	}
	entry, cerr := compile.Compile(body)
	if cerr != nil {
		// Return the concrete *compile.CompileError (never a nil interface wrapping
		// a typed nil): cerr is non-nil in this branch.
		return "", cerr
	}
	return entry.ExecutionClass, nil
}

// setExecutionClass records an automation row's compiled execution_class, inside
// the caller's write transaction so it commits atomically with the row write and
// the generation bump. A no-op for kinds without an execution class.
func setExecutionClass(ctx context.Context, tx *sql.Tx, kind Kind, id, executionClass string) error {
	if kind != KindAutomation {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE automations SET execution_class = ? WHERE id = ?`, executionClass, id); err != nil {
		return fmt.Errorf("store: set execution_class: %w", err)
	}
	return nil
}

// readEdgeRuleBodies loads every ENABLED edge-classified automation body (ordered
// by id), unlocked — the query core EdgeRuleBodies and DesiredState each run inside
// their OWN read-lock section. Kept as a single unlocked helper (rather than two
// separately-locked public methods composed by a caller) is precisely what lets
// DesiredState fold this read into the SAME critical section as its scope-node/
// scheduling-row/generation reads: see DesiredState's doc comment. The bodies
// slice is never nil (an empty store yields an empty, non-nil slice, so the
// section marshals as `[]`, not `null`, per REL-060).
//
// The `enabled` predicate honors the Automation resource envelope's first-class
// active gate (openapi Automation.enabled, REQUIRED): the rule compiler ignores
// `enabled` (it is not rule vocabulary, so it never affects compile/classify),
// so it is enforced HERE on the carry path — a disabled automation stays stored
// and edge-classified but is NOT sent to the relay's edge_rules (REL-062) and
// therefore never fires.
//
// `enabled` lives in the JSON body, not a column, so it is read with
// json_extract. There is no COALESCE default: every stored automation carries an
// explicit flag, because Create materializes one when the write left it absent
// (declaredmembers.go), and json_extract over a missing member yields NULL —
// which `<> 0` does not satisfy, so a row that somehow lacks the flag is NOT
// carried. That is deliberately the same direction the create default takes: an
// automation nobody said was on does not act on real screens, and "absent" means
// "off" in exactly one direction everywhere it is read.
func readEdgeRuleBodies(ctx context.Context, q queryer) ([]json.RawMessage, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT body FROM automations
		 WHERE execution_class = ? AND json_extract(body, '$.enabled') <> 0
		 ORDER BY id ASC`, executionClassEdge)
	if err != nil {
		return nil, fmt.Errorf("store: read edge rule bodies: %w", err)
	}
	defer rows.Close()

	bodies := []json.RawMessage{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, fmt.Errorf("store: scan edge rule body: %w", err)
		}
		bodies = append(bodies, json.RawMessage(body))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read edge rule bodies: %w", err)
	}
	return bodies, nil
}

// EdgeRuleBodies returns the stored edge-classified automation bodies (ordered by
// id), the rules/1 minor they were compiled against, and the store generation the
// read was taken at — the inputs the feeder derives the relay's edge_rules section
// from (REL-062). ONLY edge rules are returned: an app-classified rule is stored
// and validated but not carried to the relay, so it never appears here. The three
// values are read under one read lock, so they form a consistent snapshot at the
// returned generation.
func (s *Store) EdgeRuleBodies(ctx context.Context) ([]json.RawMessage, string, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	bodies, err := readEdgeRuleBodies(ctx, s.db)
	if err != nil {
		return nil, "", 0, err
	}
	generation, err := readGeneration(ctx, s.db)
	if err != nil {
		return nil, "", 0, err
	}
	return bodies, rulesMinorVersion, generation, nil
}

// automationVersionsSchema retains an automation's superseded definitions
// (rules/1 RUL-394).
//
// Keyed by (automation id, the revision it was superseded AT) so the sequence an
// operator reads is the sequence that happened, and so a replay of the same
// revision cannot produce two rows claiming to be the same moment.
//
// ON DELETE is not expressed as a foreign key because the resource tables are
// generated per-kind and carry none; deleteAutomationVersions is called from the
// delete path instead. The rule it enforces is RUL-394's: the versions of a
// deleted rule go with it, on the same terms its own rows do.
const automationVersionsSchema = `
CREATE TABLE IF NOT EXISTS automation_versions (
	automation_id  TEXT NOT NULL,
	revision       INTEGER NOT NULL,
	body           TEXT NOT NULL,
	superseded_at  INTEGER NOT NULL,
	PRIMARY KEY (automation_id, revision)
);
CREATE INDEX IF NOT EXISTS automation_versions_by_id ON automation_versions(automation_id);
`

// AutomationVersion is one superseded definition.
type AutomationVersion struct {
	// Revision the definition was superseded AT — the revision it HELD, not the
	// one that replaced it. An operator restoring "revision 4" gets the rule as
	// it read at revision 4.
	Revision     int64
	Body         json.RawMessage
	SupersededAt int64
}

// recordAutomationVersion snapshots the definition an update is about to
// replace, inside that update's own transaction (RUL-394).
//
// In the transaction and not beside it, because a version captured outside can
// be missing for exactly the write that mattered: the process dies between the
// UPDATE and the snapshot, and the definition an operator wants back is the one
// that was never recorded.
//
// A no-op for every other kind — only automations are versioned here, because
// only they are a thing an operator AUTHORS and can break in a way that is hard
// to retype.
func recordAutomationVersion(ctx context.Context, tx *sql.Tx, kind Kind, id string, priorRevision int64, priorBody string, nowMs int64) error {
	if kind != KindAutomation {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO automation_versions (automation_id, revision, body, superseded_at)
		 VALUES (?, ?, ?, ?)`,
		id, priorRevision, priorBody, nowMs); err != nil {
		return fmt.Errorf("store: record automation version: %w", err)
	}
	return nil
}

// deleteAutomationVersions removes a deleted automation's history (RUL-394).
func deleteAutomationVersions(ctx context.Context, tx *sql.Tx, kind Kind, id string) error {
	if kind != KindAutomation {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM automation_versions WHERE automation_id = ?`, id); err != nil {
		return fmt.Errorf("store: delete automation versions: %w", err)
	}
	return nil
}

// ListAutomationVersions returns id's superseded definitions, newest first.
func (s *Store) ListAutomationVersions(ctx context.Context, id string) ([]AutomationVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx,
		`SELECT revision, body, superseded_at FROM automation_versions
		  WHERE automation_id = ? ORDER BY revision DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("store: list automation versions: %w", err)
	}
	defer rows.Close()
	out := []AutomationVersion{}
	for rows.Next() {
		var v AutomationVersion
		var body string
		if err := rows.Scan(&v.Revision, &body, &v.SupersededAt); err != nil {
			return nil, fmt.Errorf("store: scan automation version: %w", err)
		}
		v.Body = json.RawMessage(body)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate automation versions: %w", err)
	}
	return out, nil
}
