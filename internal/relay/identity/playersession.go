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

// playerSessionSchema creates the two tables this file's accessors read and
// write: a minted channel token's own record — keyed by the token's OWN
// SHA-256 digest, NEVER the raw token itself (see HashToken's doc) — and a
// one-time pairing grant's redemption marker, keyed by grant_id. Neither can
// hold asset/media bytes (`#52` gateway posture): a token-hash digest, a
// screen_id, an expiry, and a grant_id are the only values either table's
// columns can ever carry.
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
