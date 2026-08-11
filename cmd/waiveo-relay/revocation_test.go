package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
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

// TestRevocationSurvivesARelayRestart is the half REL-123 actually turns on:
// "a synced one MUST be enforced regardless of connectivity", and a process
// restart is the connectivity gap an in-memory-only copy cannot cross.
//
// The two tests above prove connection-down / process-UP. This one restarts the
// process. A relay that pulled a revoking generation and then rebooted during an
// app-peer outage used to come back serving its PERSISTED programs to its
// PERSISTED channel tokens with an EMPTY revocation set — the durable half of
// its own last-applied row disagreeing with itself about one generation — and
// stayed that way until a pull it could not make restated the set.
//
// Everything durable is real: one on-disk store, closed as a process exit
// closes it and reopened from the same file, with the SAME relay identity and
// the SAME channel token carried across. The second boot runs main's own
// offline path — installPersistedServingState over a ZERO desiredstate.Applied,
// which is exactly what main holds when its pull fails or never happened —
// followed by the same scheduleDriver.apply seam. Nothing simulates the outage:
// the second process has no app peer of any kind, because none is constructed.
//
// Note what this case does NOT cover, and where that lives: it seeds the
// durable row through identity.Store.ApplyGeneration, the same primitive
// desiredstate.VerifyAndApply commits with, rather than through the verify
// chain itself. That the chain persists what it verified is
// internal/relay/desiredstate's TestServedRevocationSurvivesTheStore, and the
// whole feeder→relay→player/1 chain is the REL-123 corpus case.
func TestRevocationSurvivesARelayRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "relay.db")
	store, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	certPEM, priv := newTestRelayIdentity(t)
	nowMs := demoContentHourInstant(t)

	// ---- first process: pair under generation 7, revoked empty ----
	srv, err := playerserver.NewServer(certPEM, []wire.PairingGrant{testPlayerServerGrant()}, playerserver.WallClockMs)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetSigningKey(priv)
	srv.EnablePersistence(store)

	host, err := automationhost.New(store, deviceclass.Builtin(), loopbackController{}, loopbackResolver, rePullTestRelayID)
	if err != nil {
		t.Fatalf("automationhost.New: %v", err)
	}
	driver := &scheduleDriver{
		srv:       srv,
		sink:      fakeScheduleSink(),
		site:      hello.SiteBinding{TZ: "America/Chicago", Lat: 41.8781, Long: -87.6298},
		tickEvery: scheduleResolverTickInterval,
	}

	gen7 := buildRePullContentApplied(t, 7, rePullAssetA)
	persistAppliedGeneration(t, store, gen7)
	booted, err := installPersistedServingState(store, srv, gen7)
	if err != nil {
		t.Fatalf("installPersistedServingState (first boot): %v", err)
	}
	driver.apply(ctx, booted, nowMs)

	ts := newPlayerHTTP(t, srv)
	token := pairForToken(t, ts, testPlayerServerGrantID)
	if status, code := programPullStatus(t, ts, token); status != http.StatusOK {
		t.Fatalf("pre-revocation program pull = %d/%q, want 200", status, code)
	}

	// ---- generation 8 revokes the screen, applied live and persisted ----
	gen8 := buildRePullContentApplied(t, 8, rePullAssetB)
	gen8.Revoked = []string{testPlayerServerScreenID}
	puller := &rePuller{
		pull: func(int64) (desiredstate.Applied, error) {
			// A real pull persists the verified generation atomically BEFORE
			// returning it (desiredstate.VerifyAndApply); the fake does the
			// same, or the restart below would have nothing to read.
			persistAppliedGeneration(t, store, gen8)
			return gen8, nil
		},
		driver:  driver,
		host:    host,
		nowFn:   func() int64 { return nowMs },
		lastGen: gen7.Generation,
	}
	if applied := puller.tick(ctx); !applied {
		t.Fatal("re-pull tick of a higher generation returned applied=false, want true")
	}
	if status, code := programPullStatus(t, ts, token); status != http.StatusUnauthorized || code != "CHANNEL_TOKEN_REVOKED" {
		t.Fatalf("program pull after the live revocation = %d/%q, want 401/CHANNEL_TOKEN_REVOKED", status, code)
	}

	// ---- restart, with no app peer in existence ----
	if err := store.Close(); err != nil {
		t.Fatalf("store.Close(): %v", err)
	}
	reopened, err := identity.Open(dbPath)
	if err != nil {
		t.Fatalf("identity.Open (reopen): %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	restarted, err := playerserver.NewServer(certPEM, nil, playerserver.WallClockMs)
	if err != nil {
		t.Fatalf("NewServer (restarted): %v", err)
	}
	restarted.SetSigningKey(priv)
	restarted.EnablePersistence(reopened)
	restartedDriver := &scheduleDriver{
		srv:       restarted,
		sink:      fakeScheduleSink(),
		site:      hello.SiteBinding{TZ: "America/Chicago", Lat: 41.8781, Long: -87.6298},
		tickEvery: scheduleResolverTickInterval,
	}

	// main's offline boot, verbatim: no connection, so `applied` is the zero
	// value, and everything served comes from the durable row.
	offline := desiredstate.Applied{}
	offline, err = installPersistedServingState(reopened, restarted, offline)
	if err != nil {
		t.Fatalf("installPersistedServingState (restart): %v", err)
	}
	// Asserted directly as well as through the wire below, because the two can
	// fail independently: this pins that the persisted set REACHES the apply
	// seam, without which the wire answer could be right for the wrong reason.
	if !reflect.DeepEqual(offline.Revoked, []string{testPlayerServerScreenID}) {
		t.Fatalf("offline boot carried revoked = %v, want %v — a relay that reboots mid-outage has only its own last-synced copy (REL-123)", offline.Revoked, []string{testPlayerServerScreenID})
	}
	restartedDriver.apply(ctx, offline, nowMs)

	restartedTS := newPlayerHTTP(t, restarted)
	status, code := programPullStatus(t, restartedTS, token)
	if status != http.StatusUnauthorized || code != "CHANNEL_TOKEN_REVOKED" {
		t.Fatalf("program pull on the SAME durable token after a restart = %d/%q, want 401/CHANNEL_TOKEN_REVOKED — the relay served a credential its app peer had revoked, purely because the process restarted", status, code)
	}
}

// persistAppliedGeneration commits applied to the durable store exactly as
// desiredstate.VerifyAndApply does once a snapshot has verified: one atomic
// last-applied row carrying
// {generation, hash, screen_programs, revoked, device_inventory}
// (identity.Store.ApplyGeneration, REL-055/056).
func persistAppliedGeneration(t *testing.T, store *identity.Store, applied desiredstate.Applied) {
	t.Helper()
	programsJSON, err := json.Marshal(applied.ScreenPrograms)
	if err != nil {
		t.Fatalf("marshal screen_programs: %v", err)
	}
	revokedJSON, err := json.Marshal(applied.Revoked)
	if err != nil {
		t.Fatalf("marshal revoked: %v", err)
	}
	inventoryJSON, err := json.Marshal(applied.DeviceInventory.Normalized())
	if err != nil {
		t.Fatalf("marshal device_inventory: %v", err)
	}
	if err := store.ApplyGeneration(applied.Generation, fmt.Sprintf("sha256:gen%d", applied.Generation), programsJSON, revokedJSON, inventoryJSON); err != nil {
		t.Fatalf("ApplyGeneration(%d): %v", applied.Generation, err)
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
