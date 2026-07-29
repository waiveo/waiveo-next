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
	relay_id                 TEXT NOT NULL DEFAULT '',
	scope_node               TEXT NOT NULL,
	ttl_seconds              INTEGER NOT NULL,
	redemption_mode          TEXT NOT NULL,
	issued_via               TEXT NOT NULL,
	issued_at                INTEGER NOT NULL
);
`

// migratePairingGrantsSchema brings a pairing_grants table created by an
// earlier build up to the current column set. `CREATE TABLE IF NOT EXISTS` is a
// no-op against an existing table, so a store that has been running since
// before relay_id existed would otherwise keep a nine-column table and fail
// every insert — every pairing code unissuable. The check is a PRAGMA read
// rather than a blind `ALTER TABLE ... ADD COLUMN` whose "duplicate column"
// error is swallowed, mirroring migrateTelemetrySchema's own reasoning:
// swallowing an error class to make a statement idempotent also swallows the
// unrelated failures that share it.
//
// An existing row is left with relay_id = ” — an unbound grant, which
// REL-121c forbids an app peer from MINTING but which the relay still handles
// safely (REL-121b: an absent binding is the pre-existing any-relay behavior).
// Nothing back-fills a binding onto an already-issued grant, because nothing
// knows which relay its already-printed pairing code dials.
func migratePairingGrantsSchema(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(pairing_grants)`)
	if err != nil {
		return fmt.Errorf("inspect pairing_grants: %w", err)
	}
	defer rows.Close()

	hasRelayID := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan pairing_grants column: %w", err)
		}
		if name == "relay_id" {
			hasRelayID = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate pairing_grants columns: %w", err)
	}
	if hasRelayID {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE pairing_grants ADD COLUMN relay_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add pairing_grants.relay_id: %w", err)
	}
	return nil
}

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
	// REL-121c: this store is an app peer's own desired-state authoring tier,
	// and every grant written here rides `pairing_grants` to EVERY relay
	// enrolled to the site. An unbound one-time grant is therefore redeemable
	// once per relay — each relay's own consumption record correctly never
	// exceeding one, and every one of those redemptions resolving to the same
	// screen row (REL-121a). Refused at the storage boundary, not only at the
	// API handler, so no future caller can persist one by taking a different
	// route to this method.
	// Every grant this app peer mints must be one-time AND relay-bound.
	//
	// Guarding only `RedemptionMode == "one-time"` left the hole open by the
	// mode field: nothing in this repo validates RedemptionMode, so a caller
	// supplying anything else got an UNBOUND grant delivered to every relay —
	// and a non-one-time mode also skips consumption marking on the relay side,
	// making it redeemable repeatedly at every one of them. That is a worse
	// version of the defect the binding closes, reachable by the very route this
	// storage-boundary check exists to cover.
	if g.RedemptionMode != "one-time" {
		return fmt.Errorf("store: AddPairingGrant: redemption_mode must be %q, got %q — this app peer mints no other mode (REL-121c)", "one-time", g.RedemptionMode)
	}
	if g.RelayID == "" {
		return fmt.Errorf("store: AddPairingGrant: a one-time pairing grant must be relay-bound (REL-121b/REL-121c)")
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
			`INSERT INTO pairing_grants (grant_id, purpose, resulting_principal_kind, screen_id, relay_id,
			                             scope_node, ttl_seconds, redemption_mode, issued_via, issued_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			g.GrantID, g.Purpose, g.ResultingPrincipalKind, g.ScreenID, g.RelayID,
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
		`SELECT grant_id, purpose, resulting_principal_kind, screen_id, relay_id, ttl_seconds, redemption_mode, issued_at
		 FROM pairing_grants ORDER BY issued_at, grant_id`)
	if err != nil {
		return nil, fmt.Errorf("store: read pairing grants: %w", err)
	}
	defer rows.Close()

	out := []wire.PairingGrant{}
	for rows.Next() {
		var g wire.PairingGrant
		if err := rows.Scan(&g.GrantID, &g.Purpose, &g.ResultingPrincipalKind, &g.ScreenID, &g.RelayID,
			&g.TTL, &g.RedemptionMode, &g.IssuedAt); err != nil {
			return nil, fmt.Errorf("store: scan pairing grant: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ErrPairingGrantBoundElsewhere reports a redemption report (relay/1
// REL-124/REL-124b) whose grant exists but names a DIFFERENT relay than the one
// reporting it. Refused rather than honoured: a relay is untrusted input on that
// verb, and a report that could retire another relay's grant would let any
// enrolled relay cancel a pairing in progress at its sibling.
var ErrPairingGrantBoundElsewhere = errors.New("store: pairing grant is bound to a different relay")

// RetirePairingGrant deletes the pairing-grant row grantID names, on the report
// of the relay it is bound to (relay/1 REL-124/REL-124a), and bumps the
// desired-state generation so the grant stops riding later snapshots.
//
// relayID is the REPORTING connection's own authenticated identity, never a
// value the report asserts (REL-124b — the caller takes it from the mTLS
// client-certificate identity). The delete is conditioned on it, so this method
// cannot retire a grant belonging to any other relay no matter what a report
// claims.
//
// The return is consumption-only and idempotent, exactly as REL-124b requires:
//   - the grant exists and is bound to relayID -> deleted, retired=true;
//   - the grant is not on record at all (already retired, expired away, never
//     minted) -> retired=false, nil error. A relay re-sending an unacknowledged
//     report, or reporting one the app peer already retired, is behaving as
//     REL-124a's re-send rule requires and must not be refused for it;
//   - the grant exists but names another relay -> ErrPairingGrantBoundElsewhere
//     and nothing is written.
//
// It never creates a row, never revives one, and never marks anything redeemed:
// the retirement is a deletion of desired state, and the site-wide at-most-once
// property is REL-121b's binding, not this call (REL-124c).
func (s *Store) RetirePairingGrant(ctx context.Context, grantID, relayID string) (retired bool, err error) {
	if grantID == "" {
		return false, fmt.Errorf("store: RetirePairingGrant: grant_id must not be empty")
	}
	if relayID == "" {
		return false, fmt.Errorf("store: RetirePairingGrant: relay_id must not be empty — a report is attributed to an authenticated connection (REL-124b)")
	}
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		// Read the binding and delete under ONE transaction, so a concurrent
		// re-mint of the same grant_id cannot slip between the authorization
		// check and the delete it authorizes.
		var boundRelay string
		qerr := tx.QueryRowContext(ctx, `SELECT relay_id FROM pairing_grants WHERE grant_id = ?`, grantID).Scan(&boundRelay)
		if errors.Is(qerr, sql.ErrNoRows) {
			return nil // not on record: an idempotent no-op, not a refusal
		}
		if qerr != nil {
			return fmt.Errorf("store: RetirePairingGrant: resolve grant: %w", qerr)
		}
		// An UNBOUND row is a pre-binding grant this store migrated: its code was
		// printed before relay_id existed, so it is redeemable at any relay by
		// design (REL-121b's baseline shape). Refusing to retire it would make
		// every legitimate post-upgrade redemption report an authorization
		// failure the operator is told to investigate — crying wolf for the
		// whole of its remaining ttl, on the one path that is working correctly.
		if boundRelay != "" && boundRelay != relayID {
			return ErrPairingGrantBoundElsewhere
		}
		if _, derr := tx.ExecContext(ctx, `DELETE FROM pairing_grants WHERE grant_id = ?`, grantID); derr != nil {
			return fmt.Errorf("store: RetirePairingGrant: delete: %w", derr)
		}
		retired = true
		return bumpGeneration(ctx, tx)
	})
	if err != nil {
		return false, err
	}
	return retired, nil
}
