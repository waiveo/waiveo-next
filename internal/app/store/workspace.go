package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// workspace.go is the store half of api/1's two data-subject operations
// (API-120–123): the consistent snapshot an export carries, and the row
// destruction a delete performs.
//
// Neither operation is a resource-family CRUD call, so neither goes through the
// generic Create/Update/Delete path: an export READS the whole database as one
// file, and a delete removes every row of every kind at once. Both are given
// their own dedicated entry points here for the same reason the packs and jobs
// subsystems have theirs — they act on the store as a whole rather than on one
// row of one kind.
//
// # What a "workspace" comprises
//
// `archive/1`'s own Definitions fix this, and this file follows them rather than
// guessing: a Workspace is "the complete owned state this format exports: a
// relational store plus a content-addressed asset store, together with the
// installed packs operating on them." The relational store is this SQLite file —
// every resource table, the packs tables, and the api/1 Job tables. The asset
// store is the content origin, which lives outside this package (the api layer
// purges it alongside this call). The installed packs are rows in this same
// file, so they ride the snapshot and the purge with everything else.

// PlatformSchemaEpoch is the platform's own schema epoch: the integer an
// `archive/1` manifest records as `platform_schema_epoch` (ARC-040) and a
// restore refuses to open above (ARC-041).
//
// It is deliberately a single constant on the store rather than a per-table
// version: ARC-040 asks for "the source workspace's own platform schema epoch",
// one number describing the relational shape a restorer must understand, and the
// shape this store creates is migrated as one unit (Open runs every table's DDL
// together). It starts at 1 and MUST be incremented by any change that makes an
// older reader unable to open a newer file — which is exactly the refusal
// ARC-041 defines it to drive.
const PlatformSchemaEpoch = 1

// orgNodeKind is the scope-node `kind` value the workspace's own identity row
// carries (data-model/1 DAT-010's vocabulary, validated in
// internal/datamodel/scopetree.go).
const orgNodeKind = "org"

// ErrNoWorkspace is returned when an operation scoped to the workspace as a
// whole (API-120) cannot find the org-kind scope node that IS the workspace's
// identity.
//
// The org node is that identity because data-model/1 says so: DAT-010 makes
// `account_state` an org-node-only column, DAT-014 fixes a conformant tree at
// exactly one org node, and DAT-012 names the org node as the row that reaches
// `purged` "once [the data-subject delete operation] has run". A deployment with
// no org node has no workspace for either operation to act on.
var ErrNoWorkspace = errors.New("store: no org-kind scope node exists")

// WorkspaceRoot returns the id of the single org-kind scope node — the
// workspace's own identity (see ErrNoWorkspace) — and its current
// `account_state`.
//
// If more than one org node exists (a tree DAT-014 does not admit), the
// lowest-id one is returned rather than an error: the caller is an operation
// that must name ONE target, and picking deterministically is more useful than
// refusing over a tree shape the create path already rejects.
func (s *Store) WorkspaceRoot(ctx context.Context) (id string, accountState string, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, `SELECT id, body FROM scope_nodes ORDER BY id ASC`)
	if err != nil {
		return "", "", fmt.Errorf("store: read scope nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rowID string
		var body []byte
		if err := rows.Scan(&rowID, &body); err != nil {
			return "", "", fmt.Errorf("store: scan scope node: %w", err)
		}
		var node struct {
			Kind         string `json:"kind"`
			AccountState string `json:"account_state"`
		}
		if err := json.Unmarshal(body, &node); err != nil {
			continue
		}
		if node.Kind == orgNodeKind {
			return rowID, node.AccountState, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", fmt.Errorf("store: read scope nodes: %w", err)
	}
	return "", "", ErrNoWorkspace
}

// SnapshotInto writes a consistent point-in-time copy of the whole relational
// store to path — the bytes an `archive/1` container carries as its
// `workspace.sqlite` tar entry (ARC-085).
//
// It uses SQLite's own `VACUUM INTO`, which is exactly what ARC-083 demands:
// "the workspace's relational store MUST enter the archive only via a
// consistent-snapshot mechanism (an online backup or an equivalent
// atomic-snapshot operation) — never a raw filesystem copy of a store still open
// for live writes, which risks capturing a torn, inconsistent image." Copying
// the file off disk would be that forbidden raw copy; `VACUUM INTO` reads
// through the same connection under a read transaction and materializes a
// self-consistent database.
//
// The read lock is held for the duration, so no write commits into the middle of
// the snapshot. path MUST NOT already exist — SQLite refuses to overwrite, and
// that refusal is left to surface rather than papered over with a pre-delete,
// since an export writing over an existing file is a caller bug worth seeing.
func (s *Store) SnapshotInto(ctx context.Context, path string) error {
	if path == "" {
		return errors.New("store: snapshot: empty destination path")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("store: snapshot: create dir %s: %w", dir, err)
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	// The destination is interpolated as a SQL string literal because VACUUM INTO
	// does not accept a bound parameter for its target. Single quotes are doubled
	// so a path containing one cannot terminate the literal early.
	quoted := "'" + escapeSQLiteLiteral(path) + "'"
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO `+quoted); err != nil {
		return fmt.Errorf("store: snapshot into %s: %w", path, err)
	}
	return nil
}

// escapeSQLiteLiteral doubles every single quote, the whole of SQLite's own
// escaping rule for a string literal.
func escapeSQLiteLiteral(s string) string {
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// PurgeWorkspace destroys the workspace's relational data in ONE transaction:
// every row of every resource kind, every installed pack and its bundled files
// and collection rows. It is the relational half of the delete operation
// api/1 API-122 routes to `security-model.md` SEC-121's destruction path.
//
// Two things deliberately SURVIVE, and both are load-bearing:
//
//   - The org-kind scope node, whose `account_state` is set to `purged` instead
//     of being deleted. data-model/1 DAT-012 states the outcome directly:
//     `purged` is "the terminal member of its own vocabulary a workspace's org
//     node reaches once [the delete operation] has run". A row that reaches a
//     terminal state has to still exist to be in it, so the org node is a
//     tombstone recording that the destruction ran rather than a row the
//     destruction removes. Every OTHER scope node — sites, groups, screens — is
//     deleted with everything else.
//   - The api/1 Job tables. The Job this destruction is executing under is the
//     ONLY completion signal API-123 offers the client ("a Job resource a client
//     polls for completion"), so deleting the record mid-run would make the
//     operation's own outcome unobservable by construction. The jobs tables are
//     an execution log over rows, not workspace content, and they carry no
//     resource data of their own once those rows are gone.
//
// The generation is bumped and the post-commit hook fires, exactly as any other
// write does: every connected relay's desired state just became empty, and that
// is a change it must learn about rather than the one write the feeder is not
// told of.
func (s *Store) PurgeWorkspace(ctx context.Context) error {
	orgID, _, err := s.WorkspaceRoot(ctx)
	if err != nil {
		return err
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, kind := range allKinds {
			table, err := tableFor(kind)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
				return fmt.Errorf("store: purge %s: %w", table, err)
			}
		}
		for _, table := range []string{"pack_rows", "pack_files", "packs"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
				return fmt.Errorf("store: purge %s: %w", table, err)
			}
		}
		if err := reinsertPurgedOrg(ctx, tx, orgID, s.nowMs()); err != nil {
			return err
		}
		return bumpGeneration(ctx, tx)
	})
}

// reinsertPurgedOrg writes the org-node tombstone DAT-012 requires back into the
// emptied scope_nodes table: the same id, an `account_state` of `purged`, and
// nothing else carried forward from the row that was just destroyed.
//
// Nothing is carried forward on purpose. The node's former name, labels,
// external_id, timezone and coordinates are workspace data — a site's street
// address is exactly the kind of thing a data-subject delete exists to erase —
// so the tombstone is built fresh from the id and the terminal state alone. The
// name is a fixed, content-free literal because data-model/1 requires a
// non-empty name on every scope node.
func reinsertPurgedOrg(ctx context.Context, tx *sql.Tx, orgID string, now int64) error {
	body, err := json.Marshal(map[string]any{
		"id":            orgID,
		"kind":          orgNodeKind,
		"name":          "Purged Workspace",
		"parent_id":     nil,
		"account_state": "purged",
		"revision":      1,
		"created_at":    now,
		"updated_at":    now,
	})
	if err != nil {
		return fmt.Errorf("store: purge: build org tombstone: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO scope_nodes (id, revision, external_id, labels, scope_node, created_at, updated_at, body)
		 VALUES (?, 1, '', '{}', '', ?, ?, ?)`,
		orgID, now, now, string(body),
	); err != nil {
		return fmt.Errorf("store: purge: insert org tombstone: %w", err)
	}
	return nil
}
