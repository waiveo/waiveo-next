package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// packInstallsSchema creates the two tables the marketplace install layer owns.
// They are deliberately asymmetric in lifetime, and that asymmetry is the whole
// security content of marketplace/1 MKT-094b:
//
//   - pack_installs — the append-only install-record history (MKT-094/094a/094b):
//     one row per SUCCESSFULLY APPLIED install, carrying MKT-094's pin
//     {pack_id, trust_channel, resolved_version, source, stale_source} plus
//     MKT-094a's provenance (the content digest and key id of the signature
//     envelope verification that admitted the artifact, and the resolved index
//     entry's own digest). A pack's records are PACK-OWNED state and are
//     removed with the pack by UninstallPack, exactly as its authored rows are.
//
//   - pack_channel_marks — the per-(pack_id, trust channel) resolved-version
//     high-water mark (MKT-050). This is NOT pack-owned: it is the verifier's
//     own memory of what it has EVER resolved for that pair, and it deliberately
//     SURVIVES an uninstall (as well as a restart, channel-index/1 CHI-062).
//     Dropping it with the pack would make uninstall-then-reinstall a downgrade
//     path around MKT-050 in which every individual step succeeds.
//
// Both are written inside the install transaction (InstallPack), never beside
// it: an install and its record land together or neither lands, so a record can
// never describe an install that did not happen and an install can never run
// without recording which key vouched for it.
const packInstallsSchema = `
CREATE TABLE IF NOT EXISTS pack_installs (
	record_id        TEXT PRIMARY KEY,
	pack_id          TEXT NOT NULL,
	resolved_version TEXT NOT NULL,
	trust_channel    TEXT NOT NULL DEFAULT '',
	source           TEXT NOT NULL,
	stale_source     INTEGER NOT NULL DEFAULT 0,
	content_digest   TEXT NOT NULL,
	key_id           TEXT NOT NULL,
	artifact_digest  TEXT NOT NULL DEFAULT '',
	installed_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS pack_installs_by_pack ON pack_installs (pack_id, record_id);
CREATE TABLE IF NOT EXISTS pack_channel_marks (
	pack_id          TEXT NOT NULL,
	trust_channel    TEXT NOT NULL,
	resolved_version TEXT NOT NULL,
	updated_at       INTEGER NOT NULL,
	PRIMARY KEY (pack_id, trust_channel)
);
`

// SourceDirect is the install record's `source` value for an install no registry
// source mediated — the direct, out-of-band artifact upload (marketplace/1
// MKT-023's locally-installed path, recorded per MKT-094a). It is a sentinel, not
// a registry-source id: a configured source may never take this name, so a
// direct install can never be read back as a registry-mediated one.
const SourceDirect = "direct"

// PackInstallRecord is one appended install record (marketplace/1 MKT-094 +
// MKT-094a): what version of which pack landed, where it was resolved from, and
// — the half MKT-094a adds — the provenance the install was actually accepted
// on.
//
// TrustChannel and ArtifactDigest are empty for a direct (non-registry) install:
// there is no channel pointer to auto-track and no index entry to name. An empty
// TrustChannel is therefore load-bearing, not a missing value — it is what says
// this install is not eligible for channel auto-tracking (MKT-090).
//
// ContentDigest and KeyID are NEVER optional. They come from the packsig
// envelope verification that admitted the artifact, and they are the only record
// anywhere on the box of which key vouched for the bytes that are running: the
// envelope itself is dropped at install and never persisted (MKT-009a), so once
// this is not written the answer is unreconstructable.
type PackInstallRecord struct {
	RecordID        string
	PackID          string
	ResolvedVersion string
	TrustChannel    string
	Source          string
	StaleSource     bool
	ContentDigest   string
	KeyID           string
	ArtifactDigest  string
	InstalledAt     int64
}

// ChannelMarkAdvance is the MKT-050 resolved-version high-water advance an
// install commits atomically with itself. Higher is the caller's version order
// (MKT-050a: component-wise numeric over MAJOR.MINOR.PATCH) — supplied rather
// than assumed, because the store has no business knowing a version grammar, and
// a store-side string comparison is exactly the lexicographic order MKT-050a
// forbids.
//
// The mark is advanced under the write lock and only ever UPWARD: a concurrent
// install that committed a higher version first is not walked back by this one.
type ChannelMarkAdvance struct {
	TrustChannel string
	Version      string
	Higher       func(candidate, current string) bool
}

// validate refuses a record that could not answer the question the record exists
// to answer. This is a hard precondition rather than a nil-tolerant write: a row
// with no verifying key id would attest to provenance nobody established, which
// is worse than no row at all (MKT-094a).
func (r PackInstallRecord) validate() error {
	switch {
	case r.PackID == "":
		return errors.New("store: install record carries no pack id")
	case r.ResolvedVersion == "":
		return errors.New("store: install record carries no resolved version")
	case r.Source == "":
		return errors.New("store: install record carries no source")
	case r.ContentDigest == "":
		return errors.New("store: install record carries no verified content digest (marketplace/1 MKT-094a)")
	case r.KeyID == "":
		return errors.New("store: install record carries no verifying key id (marketplace/1 MKT-094a)")
	}
	return nil
}

// installRecordID mints an install record's id. Monotonic, not plain New: two
// installs inside one millisecond (a test, a scripted batch) would otherwise
// have no defined relative order, and this history's whole shape — "the newest
// record is the current pin" (MKT-094), "the most recent version this install
// ever successfully applied" (MKT-050) — is read off that order. The closure is
// safe for concurrent use; every call also happens under the store write lock.
var installRecordID = ulid.Monotonic()

// appendInstallRecord writes one record inside the install transaction. The
// record id is a fresh monotonic ULID, so id order IS chronological order: the
// history pages in that order and the LAST row is MKT-094's current pin.
func appendInstallRecord(ctx context.Context, tx *sql.Tx, rec PackInstallRecord) error {
	if err := rec.validate(); err != nil {
		return err
	}
	if rec.RecordID == "" {
		rec.RecordID = installRecordID()
	}
	stale := 0
	if rec.StaleSource {
		stale = 1
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO pack_installs
		   (record_id, pack_id, resolved_version, trust_channel, source, stale_source, content_digest, key_id, artifact_digest, installed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.RecordID, rec.PackID, rec.ResolvedVersion, rec.TrustChannel, rec.Source, stale,
		rec.ContentDigest, rec.KeyID, rec.ArtifactDigest, rec.InstalledAt,
	); err != nil {
		return fmt.Errorf("store: append pack install record: %w", err)
	}
	return nil
}

// advanceChannelMark raises the (pack_id, trust channel) resolved-version
// high-water mark inside the install transaction, never lowering it. Called only
// for a registry-mediated install (a direct install pins no channel, so it has
// no pair to mark).
func advanceChannelMark(ctx context.Context, tx *sql.Tx, packID string, now int64, adv ChannelMarkAdvance) error {
	if adv.Higher == nil {
		return errors.New("store: channel-mark advance carries no version order (marketplace/1 MKT-050a)")
	}
	if adv.TrustChannel == "" || adv.Version == "" {
		return errors.New("store: channel-mark advance carries no trust channel or version")
	}
	current, found, err := readChannelMark(ctx, tx, packID, adv.TrustChannel)
	if err != nil {
		return err
	}
	if found && !adv.Higher(adv.Version, current) {
		return nil // an equal or lower version never walks the mark back
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO pack_channel_marks (pack_id, trust_channel, resolved_version, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (pack_id, trust_channel) DO UPDATE SET resolved_version = excluded.resolved_version, updated_at = excluded.updated_at`,
		packID, adv.TrustChannel, adv.Version, now,
	); err != nil {
		return fmt.Errorf("store: advance pack channel mark: %w", err)
	}
	return nil
}

// readChannelMark reads one (pack_id, trust channel) high-water mark via q.
func readChannelMark(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, packID, trustChannel string) (string, bool, error) {
	var v string
	err := q.QueryRowContext(ctx,
		`SELECT resolved_version FROM pack_channel_marks WHERE pack_id = ? AND trust_channel = ?`,
		packID, trustChannel).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read pack channel mark: %w", err)
	}
	return v, true, nil
}

// PackChannelMark returns the highest pack version this store has ever recorded
// an install of for (packID, trustChannel), and whether any exists (MKT-050).
// It survives an uninstall on purpose (MKT-094b) — a resolver checks it BEFORE
// deciding whether a channel pointer may be followed, and a mark cleared with
// the pack would make uninstall-then-reinstall a downgrade path.
func (s *Store) PackChannelMark(ctx context.Context, packID, trustChannel string) (string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readChannelMark(ctx, s.db, packID, trustChannel)
}

// ListPackInstalls returns a pack's install-record history, oldest first
// (record_id is a ULID, so id order is chronological order and the LAST row is
// MKT-094's current pin). An uninstalled pack has none: its records were removed
// with it (MKT-094b).
func (s *Store) ListPackInstalls(ctx context.Context, packID string) ([]PackInstallRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT record_id, pack_id, resolved_version, trust_channel, source, stale_source, content_digest, key_id, artifact_digest, installed_at
		   FROM pack_installs WHERE pack_id = ? ORDER BY record_id ASC`, packID)
	if err != nil {
		return nil, fmt.Errorf("store: list pack installs: %w", err)
	}
	defer rows.Close()

	out := []PackInstallRecord{}
	for rows.Next() {
		var r PackInstallRecord
		var stale int
		if err := rows.Scan(&r.RecordID, &r.PackID, &r.ResolvedVersion, &r.TrustChannel, &r.Source,
			&stale, &r.ContentDigest, &r.KeyID, &r.ArtifactDigest, &r.InstalledAt); err != nil {
			return nil, fmt.Errorf("store: scan pack install record: %w", err)
		}
		r.StaleSource = stale != 0
		out = append(out, r)
	}
	return out, rows.Err()
}
