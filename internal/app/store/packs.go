package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// packsSchema creates the three declarative-packs tables. They form a
// self-contained subsystem (installed via InstallPack, removed via
// UninstallPack) that rides the SAME store-level monotonic generation and the
// SAME serialized single-writer discipline every other kind does — an install
// or uninstall bumps the generation exactly once inside one transaction, so it
// is atomic against any concurrent read and never leaves a partially-installed
// pack behind.
//
//   - packs      — one row per installed pack: the id (<publisher>/<name>), a
//     store-owned monotonic revision (the api/1 resource baseline the ETag /
//     If-Match convention reads), the manifest version and its dataModel.version
//     (MAN-053 regression is checked against the latter), and the full manifest
//     body.
//   - pack_files — the pack's rendered content bundle as data: its ui-schema/1
//     page documents (file_kind='page', name = the ui.pages[].path) and its
//     locale catalogs (file_kind='locale', name = the locale, e.g. 'en'). Never
//     code — a pack is data end to end (declarative-only pre-ctx/1).
//   - pack_rows  — the universal-envelope rows of the pack's declared
//     collections (MAN-051): pack_id + collection + the seven envelope columns +
//     the declared-fields body. Created here; its row-level CRUD (the api/1
//     pack-data surface) lands with the data-collections task. Install writes no
//     rows into it (a pack ships no seed data); uninstall cascades it away.
const packsSchema = `
CREATE TABLE IF NOT EXISTS packs (
	id                 TEXT PRIMARY KEY,
	revision           INTEGER NOT NULL,
	version            TEXT NOT NULL,
	data_model_version INTEGER NOT NULL,
	manifest           TEXT NOT NULL,
	created_at         INTEGER NOT NULL,
	updated_at         INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS pack_files (
	pack_id   TEXT NOT NULL,
	file_kind TEXT NOT NULL,
	name      TEXT NOT NULL,
	body      TEXT NOT NULL,
	PRIMARY KEY (pack_id, file_kind, name)
);
CREATE TABLE IF NOT EXISTS pack_rows (
	pack_id         TEXT NOT NULL,
	collection      TEXT NOT NULL,
	entity_id       TEXT NOT NULL,
	revision        INTEGER NOT NULL,
	lifecycle_state TEXT NOT NULL,
	scope_node      TEXT NOT NULL,
	labels          TEXT NOT NULL DEFAULT '{}',
	template_ref    TEXT NOT NULL DEFAULT '',
	params          TEXT NOT NULL DEFAULT '',
	external_id     TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL,
	body            TEXT NOT NULL,
	PRIMARY KEY (pack_id, collection, entity_id)
);
`

// pack_files.file_kind values: a ui-schema/1 page document, or a locale catalog.
const (
	PackFilePage   = "page"
	PackFileLocale = "locale"
)

// Pack is one installed pack's persisted identity + baseline: the id, the
// store-owned revision (the ETag validator), the manifest version and its
// declared dataModel.version, and the full manifest body. The bundle content
// (pages, locale catalogs) and collection rows live in pack_files / pack_rows.
type Pack struct {
	ID               string
	Revision         int64
	Version          string
	DataModelVersion int
	Manifest         json.RawMessage
	CreatedAt        int64
	UpdatedAt        int64
}

// PackFile is one bundled file: its kind (page|locale), its name (a page path or
// a locale), and its JSON body.
type PackFile struct {
	Kind string
	Name string
	Body json.RawMessage
}

// PackInstall is the fully-parsed, already-manifest-validated install the packs
// pipeline hands the store to persist in one transaction. The store trusts it
// has passed manifest.Validate (the pipeline is the gate); the store owns only
// the revision/timestamp/generation baseline and the atomicity.
type PackInstall struct {
	ID               string
	Version          string
	DataModelVersion int
	Manifest         json.RawMessage
	Files            []PackFile
}

// InstallGuard is a caller-supplied precondition re-checked INSIDE the install
// transaction, against the prior installed pack row read under the SAME write
// lock the upsert commits under. It runs only when a prior row exists (a fresh
// install can never regress), and a non-nil return rolls the whole transaction
// back with the guard's error propagated VERBATIM (so the pipeline can return
// its own typed *packs.ManifestError and the api layer renders the same 422 it
// does on the sequential path).
//
// This closes the check-then-write (TOCTOU) race a pre-transaction snapshot
// leaves open: the pipeline reads the installed dataModel.version ONCE, outside
// this tx, to feed manifest.Validate — but between that read and this commit a
// concurrent install of the same pack id may have landed a HIGHER version, so
// MAN-053's version-regression gate would be skipped for both (each saw "not
// installed") and the lower version could silently win by committing last. The
// guard re-evaluates the regression here, atomically, so the store never ends
// at a dataModel.version below one already installed. It mirrors the WriteGuard
// pattern the generic Create/Update path uses for external_id uniqueness.
type InstallGuard func(prior Pack) error

// GetPack returns the installed pack with id, and whether it exists.
func (s *Store) GetPack(ctx context.Context, id string) (Pack, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return getPack(ctx, s.db, id)
}

// getPack reads one pack row via q (the read path or an in-transaction snapshot).
func getPack(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, id string) (Pack, bool, error) {
	var p Pack
	var body string
	err := q.QueryRowContext(ctx,
		`SELECT id, revision, version, data_model_version, manifest, created_at, updated_at
		 FROM packs WHERE id = ?`, id).
		Scan(&p.ID, &p.Revision, &p.Version, &p.DataModelVersion, &body, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Pack{}, false, nil
	}
	if err != nil {
		return Pack{}, false, fmt.Errorf("store: get pack: %w", err)
	}
	p.Manifest = json.RawMessage(body)
	return p, true, nil
}

// ListPacks returns every installed pack, ordered by id ascending (the keyset
// order the api/1 list convention pages over).
func (s *Store) ListPacks(ctx context.Context) ([]Pack, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, revision, version, data_model_version, manifest, created_at, updated_at
		 FROM packs ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list packs: %w", err)
	}
	defer rows.Close()

	out := []Pack{}
	for rows.Next() {
		var p Pack
		var body string
		if err := rows.Scan(&p.ID, &p.Revision, &p.Version, &p.DataModelVersion, &body, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan pack: %w", err)
		}
		p.Manifest = json.RawMessage(body)
		out = append(out, p)
	}
	return out, rows.Err()
}

// InstallPack persists a validated pack in ONE transaction: it upserts the packs
// row (revision 1 on a fresh install, current+1 on a reinstall), REPLACES the
// pack's entire pack_files bundle (so a reinstall never leaves an orphaned page
// or locale from the prior manifest), and bumps the store generation exactly
// once — all atomically. It returns the resulting Pack and whether the pack was
// newly created (a fresh install → 201) versus updated in place (a reinstall →
// 200). A reinstall deliberately leaves pack_rows untouched (the pack's authored
// data survives a pack update); uninstall is the only path that removes rows.
//
// The MAN-053 dataModel.version-regression refusal is enforced UPSTREAM by the
// pipeline's manifest.Validate (which surfaces the contract's typed error) for
// the sequential case; a caller additionally passes an InstallGuard so the same
// regression check is re-run against the prior row read INSIDE this transaction,
// closing the TOCTOU race two concurrent installs of one id would otherwise slip
// through (see InstallGuard). The store owns only the revision/generation
// baseline and the atomicity — the guard owns the typed refusal.
func (s *Store) InstallPack(ctx context.Context, spec PackInstall, guards ...InstallGuard) (Pack, bool, error) {
	var out Pack
	var created bool
	if err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMs()

		prior, found, err := getPack(ctx, tx, spec.ID)
		if err != nil {
			return err
		}

		var revision, createdAt int64
		if found {
			// Re-check every precondition against the prior row read under this
			// write lock BEFORE the upsert — the atomic snapshot that closes the
			// check-then-write race a pre-transaction read leaves open (e.g. a
			// concurrent install of this same id that landed a higher
			// dataModel.version). A guard's error rolls the whole tx back.
			for _, g := range guards {
				if err := g(prior); err != nil {
					return err
				}
			}
			revision = prior.Revision + 1
			createdAt = prior.CreatedAt
			if _, err := tx.ExecContext(ctx,
				`UPDATE packs SET revision = ?, version = ?, data_model_version = ?, manifest = ?, updated_at = ?
				 WHERE id = ?`,
				revision, spec.Version, spec.DataModelVersion, string(spec.Manifest), now, spec.ID,
			); err != nil {
				return fmt.Errorf("store: update pack: %w", err)
			}
		} else {
			revision = 1
			createdAt = now
			created = true
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO packs (id, revision, version, data_model_version, manifest, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				spec.ID, revision, spec.Version, spec.DataModelVersion, string(spec.Manifest), createdAt, now,
			); err != nil {
				return fmt.Errorf("store: insert pack: %w", err)
			}
		}

		// Replace the bundle wholesale: clear the prior files, then insert the new
		// set. On a fresh install the DELETE is a no-op.
		if _, err := tx.ExecContext(ctx, `DELETE FROM pack_files WHERE pack_id = ?`, spec.ID); err != nil {
			return fmt.Errorf("store: clear pack files: %w", err)
		}
		for _, f := range spec.Files {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO pack_files (pack_id, file_kind, name, body) VALUES (?, ?, ?, ?)`,
				spec.ID, f.Kind, f.Name, string(f.Body),
			); err != nil {
				return fmt.Errorf("store: insert pack file %s/%s: %w", f.Kind, f.Name, err)
			}
		}

		if err := bumpGeneration(ctx, tx); err != nil {
			return err
		}

		out = Pack{
			ID: spec.ID, Revision: revision, Version: spec.Version,
			DataModelVersion: spec.DataModelVersion, Manifest: spec.Manifest,
			CreatedAt: createdAt, UpdatedAt: now,
		}
		return nil
	}); err != nil {
		return Pack{}, false, err
	}
	return out, created, nil
}

// UninstallPack removes a pack and EVERYTHING it owns — its packs row, its
// pack_files bundle, and its pack_rows collection data — in ONE transaction,
// bumping the generation once. rev MUST equal the stored revision (the api/1
// If-Match precondition), else ErrRevisionMismatch and nothing is removed. A
// missing pack is ErrNotFound. This is the dev-POC's deliberately destructive
// uninstall: a pack's authored rows are removed with it (retention/export is a
// later concern), and every nav entry it contributed disappears with its files.
func (s *Store) UninstallPack(ctx context.Context, id string, rev int64) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var curRev int64
		err := tx.QueryRowContext(ctx, `SELECT revision FROM packs WHERE id = ?`, id).Scan(&curRev)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: uninstall read pack: %w", err)
		}
		if curRev != rev {
			return &RevisionMismatchError{Expected: rev, Current: curRev}
		}
		for _, stmt := range []string{
			`DELETE FROM pack_rows WHERE pack_id = ?`,
			`DELETE FROM pack_files WHERE pack_id = ?`,
			`DELETE FROM packs WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
				return fmt.Errorf("store: uninstall pack: %w", err)
			}
		}
		return bumpGeneration(ctx, tx)
	})
}

// GetPackFile returns one bundled file of a pack — a page document
// (kind=PackFilePage, name = the page path) or a locale catalog
// (kind=PackFileLocale, name = the locale) — and whether it exists.
func (s *Store) GetPackFile(ctx context.Context, packID, kind, name string) (json.RawMessage, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var body string
	err := s.db.QueryRowContext(ctx,
		`SELECT body FROM pack_files WHERE pack_id = ? AND file_kind = ? AND name = ?`,
		packID, kind, name).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: get pack file: %w", err)
	}
	return json.RawMessage(body), true, nil
}

// PackFileNames returns the names of a pack's files of the given kind, sorted —
// a test/introspection helper (e.g. asserting exactly the declared page paths
// and locale catalogs landed).
func (s *Store) PackFileNames(ctx context.Context, packID, kind string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT name FROM pack_files WHERE pack_id = ? AND file_kind = ? ORDER BY name ASC`,
		packID, kind)
	if err != nil {
		return nil, fmt.Errorf("store: list pack files: %w", err)
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("store: scan pack file name: %w", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}
