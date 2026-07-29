package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
)

// revocation_test.go covers the WIRING relay/1 REL-123 depends on: a generation
// apply installs that generation's `revocation_and_site.revoked` (REL-066) into
// the player/1 server, at boot and live alike. The enforcement itself is
// internal/relay/playerserver's; what these cases prove is that the list a
// verified snapshot carried actually reaches it — the half that was missing,
// since the field was decoded off the wire and then dropped.

// TestApplyInstallsRevocationLive is the live-apply oracle. A screen serving
// happily under generation N appears in generation N+1's `revoked`; the very
// next request on its existing, unexpired channel token must be refused
// CHANNEL_TOKEN_REVOKED (PLY-072).
//
// Driven through rePuller.tick rather than driver.apply directly, because tick
// is what a state.changed nudge and a pull-on-reconnect actually call: a
// revocation installed only on some path tick does not take is a revocation
// that never fires in the running binary.
func TestApplyInstallsRevocationLive(t *testing.T) {
	driver, host, srv, grantID, nowMs := newRePullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appliedA := buildRePullContentApplied(t, 7, rePullAssetA)
	driver.apply(ctx, appliedA, nowMs)

	// Paired and serving under generation 7, with nothing revoked — so the
	// token is known-good before anything revokes it, and the refusal below is
	// attributable to the revocation rather than to a token that never worked.
	ts := newPlayerHTTP(t, srv)
	token := pairForToken(t, ts, grantID)
	if status, code := programPullStatus(t, ts, token); status != http.StatusOK {
		t.Fatalf("pre-revocation program pull = %d/%q, want 200", status, code)
	}

	appliedB := buildRePullContentApplied(t, 8, rePullAssetB)
	appliedB.Revoked = []string{testPlayerServerScreenID}

	puller := &rePuller{
		pull:    func(int64) (desiredstate.Applied, error) { return appliedB, nil },
		driver:  driver,
		host:    host,
		nowFn:   func() int64 { return nowMs },
		lastGen: appliedA.Generation,
	}
	if applied := puller.tick(ctx); !applied {
		t.Fatal("re-pull tick of a higher generation returned applied=false, want true (REL-056 apply)")
	}

	status, code := programPullStatus(t, ts, token)
	if status != http.StatusUnauthorized || code != "CHANNEL_TOKEN_REVOKED" {
		t.Fatalf("program pull after the screen was revoked live = %d/%q, want 401/CHANNEL_TOKEN_REVOKED (REL-066/REL-123, PLY-072)", status, code)
	}
}

// TestApplyWithdrawsRevocationLive is the other direction, and the reason the
// installed primitive is a set-replace: a generation that no longer names the
// screen un-revokes it. Without that, a revocation entered once could never be
// taken back — REL-066 carries no negative entry — so an operator restoring a
// screen would have no way to say so.
func TestApplyWithdrawsRevocationLive(t *testing.T) {
	driver, host, srv, grantID, nowMs := newRePullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appliedA := buildRePullContentApplied(t, 7, rePullAssetA)
	appliedA.Revoked = []string{testPlayerServerScreenID}
	driver.apply(ctx, appliedA, nowMs)

	// Revoked at boot: REL-123 is enforced at channel-token ISSUANCE too, so
	// the screen cannot even pair.
	if status := attemptPair(t, srv, grantID); status != http.StatusBadRequest {
		t.Fatalf("pairing a boot-revoked screen = %d, want 400 PAIRING_CODE_INVALID (REL-123 at issuance)", status)
	}

	appliedB := buildRePullContentApplied(t, 8, rePullAssetB)
	appliedB.Revoked = nil // generation 8 withdraws it

	puller := &rePuller{
		pull:    func(int64) (desiredstate.Applied, error) { return appliedB, nil },
		driver:  driver,
		host:    host,
		nowFn:   func() int64 { return nowMs },
		lastGen: appliedA.Generation,
	}
	if applied := puller.tick(ctx); !applied {
		t.Fatal("re-pull tick of a higher generation returned applied=false, want true")
	}

	// The refused pairing attempt must not have consumed the one-time grant,
	// or withdrawing the revocation would restore nothing.
	lease := pairAndPull(t, srv, grantID)
	if len(lease.Content) != 1 || lease.Content[0].AssetRef != rePullAssetB {
		t.Fatalf("served content after the revocation was withdrawn = %+v, want one item asset %s", lease.Content, rePullAssetB)
	}
}

// TestApplyInstallsRevocationAtBoot pins the boot half explicitly. main applies
// its persisted last-applied generation through this same driver.apply before
// pairingSrv.Register mounts a single route, so a screen revoked in that
// generation is refused from the first request the process ever serves — never
// briefly credentialed while the binary catches up.
func TestApplyInstallsRevocationAtBoot(t *testing.T) {
	driver, _, srv, grantID, nowMs := newRePullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	applied := buildRePullContentApplied(t, 7, rePullAssetA)
	applied.Revoked = []string{testPlayerServerScreenID}
	driver.apply(ctx, applied, nowMs)

	if status := attemptPair(t, srv, grantID); status != http.StatusBadRequest {
		t.Fatalf("pairing a screen revoked by the booted generation = %d, want 400 PAIRING_CODE_INVALID", status)
	}
}

// programPullStatus pulls the program with token and returns the HTTP status
// plus the Problem `code` on a refusal ("" on success) — the two values PLY-072
// makes a player act differently on. multiscreen_test.go's own pullProgram is
// the success-path counterpart; it fatals on any non-200, which is exactly the
// response these cases exist to inspect.
func programPullStatus(t *testing.T, ts *httptest.Server, token string) (int, string) {
	t.Helper()

	body, err := json.Marshal(playerserver.ProgramPullRequest{
		Capabilities: playerserver.Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal program pull: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/player/v1/program", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build program request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /player/v1/program: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, ""
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	return resp.StatusCode, problem.Code
}
