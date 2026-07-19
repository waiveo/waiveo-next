package playerserver

import (
	"testing"
	"time"
)

// TestProgramRejectsRevokedScreenToken confirms a channel token whose screen_id
// the relay has revoked (RevokeScreen — modeling relay/1 REL-066's
// revocation_and_site.revoked, checked against the relay's own last-synced copy)
// is refused with CHANNEL_TOKEN_REVOKED, a DISTINCT typed code from the
// CHANNEL_TOKEN_EXPIRED an expired-but-unrevoked token draws (PLY-072/073). The
// token itself is otherwise valid — unexpired and known — so only the
// revocation is under test.
func TestProgramRejectsRevokedScreenToken(t *testing.T) {
	srv, _, token := programTestServer(t)

	screenID, _, ok := srv.LookupChannelToken(token)
	if !ok {
		t.Fatalf("freshly redeemed token %q is not known to the server", token)
	}

	srv.RevokeScreen(screenID)

	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_REVOKED")
}

// TestProgramRevokedTakesPrecedenceOverExpired confirms revocation is terminal
// (PLY-073's re-pair path) and reported ahead of expiry (PLY-074's renew path):
// a token that is BOTH past its expires_at AND names a revoked screen is refused
// with CHANNEL_TOKEN_REVOKED, so a player is driven to re-pair rather than to a
// futile renewal of a credential whose screen no longer exists (PLY-075).
func TestProgramRevokedTakesPrecedenceOverExpired(t *testing.T) {
	srv, _, token := programTestServer(t)

	screenID, _, ok := srv.LookupChannelToken(token)
	if !ok {
		t.Fatalf("freshly redeemed token %q is not known to the server", token)
	}

	srv.mu.Lock()
	rec := srv.tokens[token]
	rec.ExpiresAt = time.Now().Add(-time.Minute).UnixMilli()
	srv.tokens[token] = rec
	srv.mu.Unlock()

	srv.RevokeScreen(screenID)

	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	assertTypedError(t, resp, raw, "CHANNEL_TOKEN_REVOKED")
}
