package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// pairinggrants.go is the store's pairing-grant subsystem: the pending
// pairing-grant records (security-model/1 SEC-030, projected onto relay/1
// REL-121/REL-121a's wire subset) that ride the signed desired-state
// snapshot's `pairing_grants` section (REL-067) to the relay, whose
// player-credential authority redeems them on a screen's behalf.
//
// It lives in the AUTHORING store — not the auth store — deliberately. A
// pairing grant is desired state: it must reach the relay inside a signed,
// generation-numbered snapshot, and a relay only re-pulls when the generation
// advances (REL-057). Every write here therefore rides writeTx and bumps the
// generation, exactly as a screen row or a playlist does; a grant minted in a
// store whose writes never advance the generation (the auth store, by design —
// a login must not look like an authored change) would sit undelivered until
// some unrelated edit happened to nudge the relays.
//
// Like jobs and webhook delivery state, this is a dedicated subsystem table
// rather than a generic resource Kind: a grant is created and consumed, never
// PATCHed, carries no revision to condition a write on, and its one-time code
// semantics (SEC-031) have no CRUD analog.
const pairingGrantsSchema = `
CREATE TABLE IF NOT EXISTS pairing_grants (
	grant_id                 TEXT PRIMARY KEY,
	purpose                  TEXT NOT NULL,
	resulting_principal_kind TEXT NOT NULL,
	screen_id                TEXT NOT NULL,
	scope_node               TEXT NOT NULL,
	ttl_seconds              INTEGER NOT NULL,
	redemption_mode          TEXT NOT NULL,
	issued_via               TEXT NOT NULL,
	issued_at                INTEGER NOT NULL
);
`

// ErrPairingGrantScreenUnknown reports an AddPairingGrant whose grant names a
// screen identity row this store does not hold — the caller's 404, minted
// before anything is written, so a grant can never be issued for a screen
// that does not exist (REL-121a's binding would join nothing).
var ErrPairingGrantScreenUnknown = errors.New("store: pairing grant names an unknown screen row")

// ErrPairingGrantScreenMoved reports an AddPairingGrant whose screen row still
// exists but no longer sits at the scope node the caller authorized against —
// the row was moved between the caller's authorization read and this mint.
// Refused rather than persisted: the caller's write authority was established
// at the OLD placement, and a grant recorded (and audited, SEC-034) under a
// node the row has left would be authorized by nothing. The caller retries,
// which re-runs authorization against the row's current placement.
var ErrPairingGrantScreenMoved = errors.New("store: pairing grant's screen row moved after authorization")

// AddPairingGrant persists one screen-bound pairing-grant record and bumps the
// desired-state generation, so the very next snapshot build carries it to the
// relay (REL-067) and the post-commit hook nudges every live relay connection
// (REL-057). The same transaction also retires this store's already-expired
// grant rows — self-cleaning at the only moment the table grows.
//
// g MUST be screen-bound (REL-121a): a grant with no ScreenID is refused here
// outright rather than persisted, because a store-minted grant always results
// in one of this store's own screen rows — the unbound REL-121 baseline shape
// exists for wire compatibility, not as something this app authors.
//
// scopeNode and issuedVia are the SEC-030 record fields the wire subset does
// not carry: the screen row's own placement, and the issuance channel
// ("api"/"console"), persisted so the stored record stays the canonical
// SEC-030 shape's superset of what rides the wire.
func (s *Store) AddPairingGrant(ctx context.Context, g wire.PairingGrant, scopeNode, issuedVia string) error {
	if g.GrantID == "" {
		return fmt.Errorf("store: AddPairingGrant: grant_id must not be empty")
	}
	if g.ScreenID == "" {
		return fmt.Errorf("store: AddPairingGrant: a store-minted pairing grant must be screen-bound (REL-121a)")
	}
	if g.TTL <= 0 {
		return fmt.Errorf("store: AddPairingGrant: ttl must be positive (SEC-032)")
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		// The binding must join a real row at mint time, checked inside the
		// same transaction that persists the grant so a concurrent screen
		// delete cannot slip between check and insert — and the row must still
		// sit at the scope node the caller AUTHORIZED against, re-read in the
		// same transaction so a concurrent move cannot slip between the
		// caller's authorization and this mint either. Without the second
		// check, a grant (and its SEC-034 audit record) would be filed under a
		// placement the row has left, on authority the caller may not hold at
		// the row's new node.
		var rowNode string
		err := tx.QueryRowContext(ctx,
			`SELECT scope_node FROM `+string(KindScreen)+` WHERE id = ?`, g.ScreenID).Scan(&rowNode)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPairingGrantScreenUnknown
		}
		if err != nil {
			return fmt.Errorf("store: AddPairingGrant: resolve screen row: %w", err)
		}
		if rowNode != scopeNode {
			return ErrPairingGrantScreenMoved
		}

		// Retire rows already past their ttl. Snapshot derivation filters
		// them anyway (BuildFromStore), so this is table hygiene, not the
		// expiry mechanism — the relay enforces ttl itself (REL-121/122).
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM pairing_grants WHERE issued_at + ttl_seconds * 1000 <= ?`, s.nowMs()); err != nil {
			return fmt.Errorf("store: AddPairingGrant: retire expired grants: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO pairing_grants (grant_id, purpose, resulting_principal_kind, screen_id,
			                             scope_node, ttl_seconds, redemption_mode, issued_via, issued_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			g.GrantID, g.Purpose, g.ResultingPrincipalKind, g.ScreenID,
			scopeNode, g.TTL, g.RedemptionMode, issuedVia, g.IssuedAt); err != nil {
			return fmt.Errorf("store: AddPairingGrant: insert: %w", err)
		}
		return bumpGeneration(ctx, tx)
	})
}

// readPairingGrants reads every stored pairing-grant record as its REL-121a
// wire shape, ordered by (issued_at, grant_id) so the derived section — and
// therefore the snapshot hash (REL-053) — is a deterministic function of the
// rows. Expiry is NOT applied here: derivation is per-instant, so the filter
// belongs beside the nowMs the snapshot is built at (BuildFromStore), not in
// a read with no instant of its own.
func readPairingGrants(ctx context.Context, q queryer) ([]wire.PairingGrant, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT grant_id, purpose, resulting_principal_kind, screen_id, ttl_seconds, redemption_mode, issued_at
		 FROM pairing_grants ORDER BY issued_at, grant_id`)
	if err != nil {
		return nil, fmt.Errorf("store: read pairing grants: %w", err)
	}
	defer rows.Close()

	out := []wire.PairingGrant{}
	for rows.Next() {
		var g wire.PairingGrant
		if err := rows.Scan(&g.GrantID, &g.Purpose, &g.ResultingPrincipalKind, &g.ScreenID,
			&g.TTL, &g.RedemptionMode, &g.IssuedAt); err != nil {
			return nil, fmt.Errorf("store: scan pairing grant: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
