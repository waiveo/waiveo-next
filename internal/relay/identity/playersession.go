// This file extends the relay's operational store (identity.go) with the
// player/1-facing credential state relay/1 REL-142 itself puts out of scope
// — "the wire messages a relay uses to issue a player its certificate and
// channel token ... player-facing credential issuance" is explicitly "a
// distinct contract this one only supplies inputs to (player/1)" — but
// which player/1 ITSELF requires live in exactly this durable tier:
// PLY-091 requires a relay persist Lease delivery/acknowledgement state
// "relay/1's own operational storage, mirroring the persistence relay/1
// REL-142 already requires of it", and PLY-105 extends that identical
// mirrored tier to a preempt Lease grant surviving "that relay's own
// disconnection from its app peer." A minted channel token (PLY-070/071)
// and a one-time pairing grant's own redemption (relay/1 REL-121) are the
// same class of relay-owned, player/1-defined credential state PLY-091/
// PLY-105 already carve this exception for — see
// internal/relay/playerserver's own EnablePersistence doc for the full
// contract analysis this file's existence rests on.
package identity

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
)

// playerSessionSchema creates the three tables this file's accessors read and
// write: a minted channel token's own record — keyed by the token's OWN
// SHA-256 digest, NEVER the raw token itself (see HashToken's doc) — a
// one-time pairing grant's redemption marker, keyed by grant_id, and the ledger
// of redemptions this relay owes its app peer upstream (relay/1 REL-124/124a,
// admitted into this durable tier by REL-142a). None can hold asset/media bytes
// (`#52` gateway posture): a token-hash digest, a screen_id, an expiry, a
// grant_id, and a timestamp are the only values any of their columns can ever
// carry.
//
// player_redemption_reports.seq is a plain rowid alias, not AUTOINCREMENT: a
// reported row is DELETED, so the ledger has no history for a reused rowid to
// collide with, and AUTOINCREMENT would add a hidden sqlite_sequence table to a
// store whose table set is deliberately pinned
// (TestOpenCreatesExactOperationalTableSet). Reuse cannot reorder the ledger
// either — SQLite assigns max(rowid)+1, so a fresh row always sorts after every
// row still pending.
const playerSessionSchema = `
CREATE TABLE IF NOT EXISTS player_channel_tokens (
	token_hash  TEXT PRIMARY KEY,
	screen_id   TEXT NOT NULL,
	expires_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS player_redeemed_grants (
	grant_id     TEXT PRIMARY KEY,
	redeemed_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS player_redemption_reports (
	seq          INTEGER PRIMARY KEY,
	grant_id     TEXT NOT NULL,
	redeemed_at  INTEGER NOT NULL
);
`

// HashToken returns the SHA-256 hex digest of a channel token — the value
// SetPlayerSession persists and PlayerSession looks up by, never the raw
// token itself. A channel token is bearer-authenticating entirely on its
// own (player/1 PLY-076: "Authorization: Bearer <token>"), so a raw copy
// sitting in this file's on-disk table would let anyone able to read that
// file — a stolen backup, a misdirected support bundle, an operator with
// filesystem access but no protocol access — present it exactly as though
// they were the screen it authorizes. Hashing before persisting gives a
// channel token the same one-way, verify-without-reveal property
// security-model/1 already requires of every OTHER stored credential (a
// password or an API key is never itself the row security-model/1 SEC-003
// stores): the store can confirm a PRESENTED token's digest matches, but
// reading the table alone never yields a token a screen — or an attacker —
// could present.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}

// SetPlayerSession durably persists (replacing any previously persisted
// record under the same tokenHash) the screen_id and expires_at a minted
// channel token resolves to, keyed by HashToken(token) — never the token
// itself (HashToken's own doc). This is what lets
// internal/relay/playerserver.Server.LookupChannelToken resolve a channel
// token minted in an EARLIER process lifetime: relay/1 is the sole issuer
// and sole verifier of a screen's per-connection credential (REL-120), and
// player/1 PLY-071's 24-hour bounded expiry presupposes a token remains
// valid for its issued lifetime regardless of how many times the relay
// process restarts in between.
func (s *Store) SetPlayerSession(tokenHash, screenID string, expiresAt int64) error {
	_, err := s.db.Exec(
		`INSERT INTO player_channel_tokens (token_hash, screen_id, expires_at) VALUES (?, ?, ?)
		 ON CONFLICT (token_hash) DO UPDATE SET screen_id = excluded.screen_id, expires_at = excluded.expires_at`,
		tokenHash, screenID, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("identity: SetPlayerSession: %w", err)
	}
	return nil
}

// PlayerSession returns the screen_id and expires_at persisted under
// tokenHash (HashToken(token)), and whether a record is persisted there at
// all — the read half SetPlayerSession's own doc describes.
func (s *Store) PlayerSession(tokenHash string) (screenID string, expiresAt int64, ok bool, err error) {
	err = s.db.QueryRow(`SELECT screen_id, expires_at FROM player_channel_tokens WHERE token_hash = ?`, tokenHash).
		Scan(&screenID, &expiresAt)
	if err == sql.ErrNoRows {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("identity: PlayerSession: %w", err)
	}
	return screenID, expiresAt, true, nil
}

// MarkPairingGrantRedeemed durably records that grantID's one-time pairing
// grant (relay/1 REL-121) has been redeemed, at redeemedAtMs. Idempotent: a
// second call for the same grantID leaves the persisted record unchanged
// (ON CONFLICT DO NOTHING) — it is the FACT of redemption, not the exact
// moment, that a later restart's redemption check (PairingGrantRedeemed)
// consults.
func (s *Store) MarkPairingGrantRedeemed(grantID string, redeemedAtMs int64) error {
	_, err := s.db.Exec(
		`INSERT INTO player_redeemed_grants (grant_id, redeemed_at) VALUES (?, ?)
		 ON CONFLICT (grant_id) DO NOTHING`,
		grantID, redeemedAtMs,
	)
	if err != nil {
		return fmt.Errorf("identity: MarkPairingGrantRedeemed: %w", err)
	}
	return nil
}

// PendingRedemptionReport is one redemption this relay performed and owes its
// app peer (relay/1 REL-124/REL-124a). Seq is the ledger position the row
// occupies — opaque to the caller except that it is the value
// MarkRedemptionReported clears the row by, so a report delivered on the wire
// retires exactly the row it came from and no other.
type PendingRedemptionReport struct {
	Seq        int64
	GrantID    string
	RedeemedAt int64
}

// RecordRedemptionReport durably enqueues one redemption for upstream report
// (REL-124/REL-124a, admitted into this durable tier by REL-142a). One row per
// REDEMPTION rather than per grant: REL-124 requires every redemption be
// reported, and a `multi` grant (REL-121's other redemption_mode) is redeemed
// more than once, so a grant-keyed ledger would silently collapse the second
// and later redemptions of one into the first's report.
//
// Rows survive a restart on purpose: "the next connection opportunity" may
// arrive after one, and a redemption performed while disconnected (REL-122) is
// exactly the case REL-124 exists for.
func (s *Store) RecordRedemptionReport(grantID string, redeemedAtMs int64) error {
	if _, err := s.db.Exec(
		`INSERT INTO player_redemption_reports (grant_id, redeemed_at) VALUES (?, ?)`,
		grantID, redeemedAtMs,
	); err != nil {
		return fmt.Errorf("identity: RecordRedemptionReport: %w", err)
	}
	return nil
}

// PendingRedemptionReports returns every not-yet-reported redemption in ledger
// order (oldest first) — the batch a connected relay drains onto the wire.
func (s *Store) PendingRedemptionReports() ([]PendingRedemptionReport, error) {
	rows, err := s.db.Query(`SELECT seq, grant_id, redeemed_at FROM player_redemption_reports ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("identity: PendingRedemptionReports: %w", err)
	}
	defer rows.Close()

	out := []PendingRedemptionReport{}
	for rows.Next() {
		var r PendingRedemptionReport
		if err := rows.Scan(&r.Seq, &r.GrantID, &r.RedeemedAt); err != nil {
			return nil, fmt.Errorf("identity: PendingRedemptionReports: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: PendingRedemptionReports: %w", err)
	}
	return out, nil
}

// MarkRedemptionReported clears the ledger row seq names, once its report has
// actually been written to the connection. Clearing by seq rather than by
// grant_id is what keeps a `multi` grant's OTHER outstanding redemptions
// pending: they are distinct rows and a delivered report for one says nothing
// about the rest.
func (s *Store) MarkRedemptionReported(seq int64) error {
	if _, err := s.db.Exec(`DELETE FROM player_redemption_reports WHERE seq = ?`, seq); err != nil {
		return fmt.Errorf("identity: MarkRedemptionReported: %w", err)
	}
	return nil
}

// PairingGrantRedeemed reports whether grantID's one-time pairing grant has
// already been durably marked redeemed (MarkPairingGrantRedeemed) — checked
// by internal/relay/playerserver.Server.redeem on every one-time grant
// selector it resolves, so a grant already consumed in an EARLIER process
// lifetime cannot be redeemed a second time purely because the relay
// process restarted in between (relay/1 REL-121: a one-time grant's
// redemption count MUST NOT exceed one regardless of concurrency — a
// restart is not an exception to that rule).
func (s *Store) PairingGrantRedeemed(grantID string) (bool, error) {
	var redeemedAt int64
	err := s.db.QueryRow(`SELECT redeemed_at FROM player_redeemed_grants WHERE grant_id = ?`, grantID).Scan(&redeemedAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("identity: PairingGrantRedeemed: %w", err)
	}
	return true, nil
}
