package wire

import (
	"encoding/json"
	"fmt"
)

// LeaseContent is one player/1 Lease `content` array entry for a plain
// `image`/`video` item (PLY-083): `{type, asset_ref, url, expires_at}`,
// plus the additive `duration_ms` (PLY-083b). This differs from relay/1's
// own ContentRef (REL-061), which carries no `type` field — a relay
// assigns `type` when converting a verified screen-program's content
// reference into a player/1 Lease content item (SetServedProgram, per-item
// from the entry's own ContentType, REL-061a).
//
// DurationMS mirrors relay/1's ContentRef.DurationMS (REL-061a) verbatim —
// SetServedProgram carries it unmodified from the persisted entry onto this
// field, `omitempty` so an item that carries none (the pre-PLY-083b wire
// shape) marshals with no `duration_ms` key, byte-identical to every prior
// release (mirroring ContentRef's own omitempty doc, REL-061a).
type LeaseContent struct {
	Type       string `json:"type"`
	AssetRef   string `json:"asset_ref"`
	URL        string `json:"url"`
	ExpiresAt  int64  `json:"expires_at"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// Lease is player/1's Lease shape (PLY-090) minus `signature` — exactly
// the fields a Lease's `signature` covers. Field declaration order matches
// PLY-090's own shape `{lease_id, screen_id, program_revision, priority,
// display, content, issued_at, valid_until, signature}` up to `signature`
// itself, which a caller appends (internal/relay/playerserver.LeaseResponse
// embeds this struct and adds Signature as its own trailing field, so
// JSON marshal order matches PLY-090 exactly).
type Lease struct {
	LeaseID         string         `json:"lease_id"`
	ScreenID        string         `json:"screen_id"`
	ProgramRevision string         `json:"program_revision"`
	Priority        string         `json:"priority"`
	Display         string         `json:"display"`
	Content         []LeaseContent `json:"content"`
	IssuedAt        int64          `json:"issued_at"`
	ValidUntil      int64          `json:"valid_until"`
}

// LeaseSignedBytes marshals lease into THE canonical bytes a Lease's
// `signature` covers (PLY-090) — struct-declaration-order JSON marshal,
// the same canonicalization convention HashSections/SignedScopeBytes
// already establish for relay/1's own snapshot signature.
//
// Both the relay (which signs a Lease at issuance, internal/relay/
// playerserver) and a player (which must recompute the identical bytes to
// verify one against its pinned trust anchor, a later task) MUST call this
// function rather than each marshaling a Lease independently — sharing it
// here is what keeps the two sides from drifting apart on the signed
// scope's byte representation, exactly as SignedScopeBytes does for
// relay/1's snapshot signature. Do not reimplement this elsewhere.
func LeaseSignedBytes(lease Lease) ([]byte, error) {
	b, err := json.Marshal(lease)
	if err != nil {
		return nil, fmt.Errorf("wire: LeaseSignedBytes: marshal lease: %w", err)
	}
	return b, nil
}
