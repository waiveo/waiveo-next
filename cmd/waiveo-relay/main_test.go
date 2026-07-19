package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/automation"
	"github.com/maaxton/waiveo-next/internal/relay/clocktrust"
	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// TestRegisterClockHintReceivesWireHint proves the relay/1 clock.hint receiver
// (REL-133) is wired into the relay's own listener and reachable end-to-end: a
// POSTed clock.hint bounded by the relay's own cert not_after is applied to the
// runtime clock, and one past not_after+grace is declined.
func TestRegisterClockHintReceivesWireHint(t *testing.T) {
	notAfter := time.UnixMilli(1784073600000)
	certDER := selfSignedCertDER(t, notAfter)

	mux := http.NewServeMux()
	store, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctl := clocktrust.NewController(store, clocktrust.NewRuntimeClock(), nil)
	if err := registerClockHint(mux, certDER, ctl); err != nil {
		t.Fatalf("registerClockHint: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(wire string) (accepted bool, status int) {
		resp, err := http.Post(srv.URL+"/relay/v1/clock-hint", "application/json", bytes.NewBufferString(wire))
		if err != nil {
			t.Fatalf("POST clock.hint: %v", err)
		}
		defer resp.Body.Close()
		var body struct {
			Accepted bool `json:"accepted"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return body.Accepted, resp.StatusCode
	}

	// Within not_after + grace -> accepted.
	if accepted, status := post(`{"type":"clock.hint","relay_id":"01J8Z4K4N5P6Q7R8S9T0V1W3A1","body":{"ts":1752537600000}}`); !accepted || status != http.StatusOK {
		t.Errorf("in-bound clock.hint: accepted=%v status=%d, want accepted=true status=200", accepted, status)
	}
	// Past not_after + grace -> declined (still 200).
	if accepted, status := post(`{"type":"clock.hint","relay_id":"01J8Z4K4N5P6Q7R8S9T0V1W3A1","body":{"ts":1784074200001}}`); accepted || status != http.StatusOK {
		t.Errorf("out-of-bound clock.hint: accepted=%v status=%d, want accepted=false status=200", accepted, status)
	}
}

func selfSignedCertDER(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "relay"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return der
}

// TestOfflineServeFallback pins the REL-055/061 boot policy the relay applies
// when the app peer is unreachable at startup: a hello or Pull failure is
// survivable (degrade to serving the persisted last-applied snapshot offline)
// IFF a prior successful pull already persisted a last-applied generation;
// otherwise it stays fatal because there is nothing to serve. This is the
// regression guard for the defect where a boot-time app-peer failure downed the
// whole process even though a valid persisted {generation, hash,
// screen_programs} sat in the store — a restart during a disconnection that
// REL-055 exists to keep serving.
func TestOfflineServeFallback(t *testing.T) {
	bootErr := errors.New("app peer unreachable")

	// Persisted snapshot present: a boot-time failure must NOT be fatal — the
	// relay serves the persisted last-applied copy offline.
	if fatal := offlineServeFallback(bootErr, true); fatal != nil {
		t.Errorf("offlineServeFallback(err, hasPersisted=true) = %v, want nil (serve persisted offline, REL-055/061)", fatal)
	}

	// Nothing persisted: the same failure IS fatal — no program to serve.
	if fatal := offlineServeFallback(bootErr, false); fatal == nil {
		t.Error("offlineServeFallback(err, hasPersisted=false) = nil, want the original error (nothing to serve)")
	} else if !errors.Is(fatal, bootErr) {
		t.Errorf("offlineServeFallback returned %v, want the original bootErr unchanged", fatal)
	}

	// A nil bootErr is never fatal regardless of persisted state.
	if fatal := offlineServeFallback(nil, false); fatal != nil {
		t.Errorf("offlineServeFallback(nil, false) = %v, want nil", fatal)
	}
	if fatal := offlineServeFallback(nil, true); fatal != nil {
		t.Errorf("offlineServeFallback(nil, true) = %v, want nil", fatal)
	}
}

func TestLoadConfigDefaultsAreLoopback(t *testing.T) {
	cfg, err := loadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("loadConfig(defaults): %v", err)
	}
	if cfg.listen != "127.0.0.1:7421" {
		t.Errorf("default listen = %q, want 127.0.0.1:7421", cfg.listen)
	}
	if cfg.feederURL != "https://127.0.0.1:7420" {
		t.Errorf("default feederURL = %q, want https://127.0.0.1:7420", cfg.feederURL)
	}
	if cfg.pairHost != "127.0.0.1" {
		t.Errorf("default pairHost = %q, want 127.0.0.1", cfg.pairHost)
	}
	if cfg.pairPort != 7421 {
		t.Errorf("default pairPort = %d, want 7421", cfg.pairPort)
	}
}

func TestLoadConfigOnBoxOverride(t *testing.T) {
	// The on-box first-photon shape: bind LAN-reachable, tell the screen to dial
	// the box's LAN IP, keep the feeder loopback (co-located).
	env := map[string]string{
		"WAIVEO_RELAY_LISTEN":    "0.0.0.0:7421",
		"WAIVEO_RELAY_PAIR_HOST": "192.0.2.12",
		"WAIVEO_RELAY_PAIR_PORT": "7421",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig(override): %v", err)
	}
	if cfg.listen != "0.0.0.0:7421" {
		t.Errorf("listen = %q, want 0.0.0.0:7421", cfg.listen)
	}
	if cfg.pairHost != "192.0.2.12" {
		t.Errorf("pairHost = %q, want the LAN IP a screen must dial", cfg.pairHost)
	}
	if cfg.feederURL != "https://127.0.0.1:7420" {
		t.Errorf("feederURL = %q, want the loopback default (feeder is co-located)", cfg.feederURL)
	}
}

func TestLoadConfigRejectsNonIntegerPairPort(t *testing.T) {
	// A bad port must fail fast at startup, not silently emit a pairing code no
	// screen can dial.
	env := map[string]string{"WAIVEO_RELAY_PAIR_PORT": "not-a-port"}
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("loadConfig accepted a non-integer WAIVEO_RELAY_PAIR_PORT, want error")
	}
}

// newTestPlayerServer builds a real playerserver.Server plus the ed25519
// signing key its issued Leases are signed with and a redeemable pairing
// grant's selector — a byte-exact duplicate of
// internal/relay/schedulehost's own test helper of the same name (this
// codebase's established cross-package pattern for small test collaborators,
// e.g. demoScreenScopeNodeID's own doc comment there).
func newTestPlayerServer(t *testing.T) (*playerserver.Server, ed25519.PrivateKey, string) {
	t.Helper()
	certPEM, keyPEM := tlsboot.GenSelfSigned()

	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("GenSelfSigned key did not PEM-decode to a PRIVATE KEY block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("x509.ParsePKCS8PrivateKey: %v", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("parsed key is %T, want ed25519.PrivateKey", key)
	}

	const grantID = "grant-schedresolver-test-01"
	grant := wire.PairingGrant{
		GrantID:                grantID,
		Purpose:                "pairing",
		ResultingPrincipalKind: "screen",
		TTL:                    900,
		RedemptionMode:         "one-time",
		IssuedAt:               time.Now().UnixMilli(),
	}
	srv, err := playerserver.NewServer(certPEM, []wire.PairingGrant{grant})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, priv, grantID
}

// pairAndPull pairs a player against srv (redeeming grantID) and pulls its
// served program back over player/1's own pair -> program HTTP surface — the
// black-box way to observe what the boot path actually configured
// (SetProgram/SetServedProgram), not an internal field peek.
func pairAndPull(t *testing.T, srv *playerserver.Server, grantID string) playerserver.LeaseResponse {
	t.Helper()
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(apihttp.WithTraceID(mux))
	t.Cleanup(ts.Close)

	pairBody, err := json.Marshal(playerserver.PairingRequest{
		HardwareID:    "hw-schedresolver-0001",
		GrantSelector: grantID,
		Capabilities:  playerserver.Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal pairing request: %v", err)
	}
	pairResp, err := http.Post(ts.URL+"/player/v1/pair", "application/json", bytes.NewReader(pairBody))
	if err != nil {
		t.Fatalf("POST /player/v1/pair: %v", err)
	}
	defer pairResp.Body.Close()
	var pr playerserver.PairingResponse
	if err := json.NewDecoder(pairResp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode pairing response: %v", err)
	}
	if pr.ChannelToken == "" {
		t.Fatalf("pairing did not yield a channel_token: %+v", pr)
	}

	pullBody, err := json.Marshal(playerserver.ProgramPullRequest{
		Capabilities: playerserver.Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("marshal program pull: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/player/v1/program", bytes.NewReader(pullBody))
	if err != nil {
		t.Fatalf("build program request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+pr.ChannelToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /player/v1/program: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("program pull status = %d, want 200", resp.StatusCode)
	}
	var lease playerserver.LeaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		t.Fatalf("decode lease: %v", err)
	}
	return lease
}

// buildDemoAppliedForTest runs the real feeder Build path (the same demo
// schedule Task 2 authored, REL-065) and adapts its output into a
// desiredstate.Applied value — the exact shape bootScheduleResolver receives
// from a verified pull, without standing up a live feeder + desiredstate.Pull
// round trip (already covered by internal/relay/desiredstate's own tests).
func buildDemoAppliedForTest(t *testing.T) desiredstate.Applied {
	t.Helper()
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}
	snap, err := snapshot.Build([]byte("fixture-image-bytes"), "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	return desiredstate.Applied{
		Generation:      snap.Generation,
		ScreenID:        snap.Sections.ScreenPrograms[0].ScreenID,
		ProgramRevision: snap.Sections.ScreenPrograms[0].ProgramRevision,
		Priority:        snap.Sections.ScreenPrograms[0].Priority,
		Display:         snap.Sections.ScreenPrograms[0].Display,
		ScreenPrograms:  snap.Sections.ScreenPrograms,
		Schedule:        snap.Sections.Schedule,
	}
}

// fakeScheduleSink builds a real *automation.CommandSink over a loopback
// device-command surface — the SAME reused constructor
// (automation.NewCommandSink over a deviceplane.CommandSurface) preset
// firing dispatches through in the running binary; a no-op controller is
// enough here since these tests assert on the served Lease, not on
// dispatched device commands.
func fakeScheduleSink() *automation.CommandSink {
	surface := deviceplane.NewCommandSurface(
		loopbackController{},
		registry.FixtureRegistry{},
		func(entityID string) (string, string, bool) { return entityID + "-device", "media-player", true },
	)
	return automation.NewCommandSink(surface, "01J8ZTESTSCHEDSINKRELAYID1")
}

// demoContentHourInstant is a fixed Unix-ms instant inside the feeder's demo
// content daypart (06:00-22:00 America/Chicago, REL-065) — a deterministic
// "now" so the boot resolve lands on display:content regardless of wall-clock
// time when this test runs.
func demoContentHourInstant(t *testing.T) int64 {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return time.Date(2026, time.January, 15, 12, 0, 0, 0, loc).UnixMilli()
}

// TestBootScheduleResolverServesResolvedProgramForGovernedScreen is the
// Task-6 end-to-end proof: with the feeder's real demo schedule (Task 2)
// present, booting the schedule resolver at a content-hour instant resolves
// the governed screen to display:content and the player server's served
// Lease reflects the schedule-resolved program (its own programRevision,
// DAT-113) — not the raw app-authored one SetServedProgram configured
// moments before, exactly as main() configures it ahead of booting the
// resolver.
func TestBootScheduleResolverServesResolvedProgramForGovernedScreen(t *testing.T) {
	applied := buildDemoAppliedForTest(t)

	srv, priv, grantID := newTestPlayerServer(t)
	srv.SetServedProgram(applied.ScreenPrograms[0], priv)
	appAuthoredRevision := applied.ScreenPrograms[0].ProgramRevision

	sink := fakeScheduleSink()
	site := hello.SiteBinding{TZ: "America/Chicago", Lat: 41.8781, Long: -87.6298}

	resolvers := bootScheduleResolverAt(applied, srv, sink, site, priv, demoContentHourInstant(t))
	if len(resolvers) != 1 {
		t.Fatalf("bootScheduleResolverAt returned %d resolver(s), want 1 (the demo schedule governs exactly one screen)", len(resolvers))
	}

	lease := pairAndPull(t, srv, grantID)
	if lease.Display != "content" {
		t.Errorf("served display = %q, want content (the schedule-resolved program)", lease.Display)
	}
	if lease.Priority != "scheduled" {
		t.Errorf("served priority = %q, want scheduled", lease.Priority)
	}
	if lease.ProgramRevision == appAuthoredRevision {
		t.Errorf("served program_revision = %q, still the app-authored one — the schedule resolver did not take over serving", lease.ProgramRevision)
	}
	if len(lease.Content) != 1 {
		t.Fatalf("served content has %d item(s), want 1", len(lease.Content))
	}
}

// TestBootScheduleResolverEmptyScheduleLeavesAppAuthoredProgramUnchanged
// asserts the stated additive serving policy (Global Constraints): with an
// empty (never-carried) schedule section, bootScheduleResolver builds no
// resolvers and the app-authored screen_programs SetServedProgram already
// configured is served completely unchanged — today's first-photon
// behavior, preserved.
func TestBootScheduleResolverEmptyScheduleLeavesAppAuthoredProgramUnchanged(t *testing.T) {
	appAuthored := wire.ScreenProgram{
		ScreenID:        "screen-first-photon",
		ProgramRevision: "rev-1",
		Priority:        "scheduled",
		Display:         "content",
		Content:         []wire.ContentRef{{AssetRef: "sha256:deadbeef", URL: "https://origin.example/content/deadbeef"}},
	}
	srv, priv, grantID := newTestPlayerServer(t)
	srv.SetServedProgram(appAuthored, priv)

	applied := desiredstate.Applied{} // never-carried schedule (today's first-photon state)
	sink := fakeScheduleSink()

	resolvers := bootScheduleResolverAt(applied, srv, sink, hello.SiteBinding{}, priv, time.Now().UnixMilli())
	if len(resolvers) != 0 {
		t.Fatalf("bootScheduleResolverAt returned %d resolver(s) for an empty schedule, want 0", len(resolvers))
	}

	lease := pairAndPull(t, srv, grantID)
	if lease.ProgramRevision != appAuthored.ProgramRevision {
		t.Errorf("served program_revision = %q, want unchanged app-authored %q", lease.ProgramRevision, appAuthored.ProgramRevision)
	}
	if lease.Display != appAuthored.Display || lease.Priority != appAuthored.Priority {
		t.Errorf("served display/priority = %q/%q, want unchanged app-authored %q/%q", lease.Display, lease.Priority, appAuthored.Display, appAuthored.Priority)
	}
}

// recordingController records every physical dispatch it receives, in order —
// the same fake-controller pattern internal/relay/schedulehost's own tests use
// (recordController) — so a test can observe whether a boot-time preset batch
// actually reached the device plane.
type recordingController struct {
	mu   sync.Mutex
	seen []string // "entity/command", in dispatch order
}

func (c *recordingController) Dispatch(entityID, command string, params map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, entityID+"/"+command)
	return nil
}

func (c *recordingController) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.seen))
	copy(out, c.seen)
	return out
}

// recordingScheduleSink builds a real *automation.CommandSink — the SAME
// constructor the running binary and fakeScheduleSink both use — wired to a
// recordingController so a test can observe exactly which preset commands a
// boot resolve dispatched.
func recordingScheduleSink(ctrl *recordingController) *automation.CommandSink {
	surface := deviceplane.NewCommandSurface(
		ctrl,
		registry.FixtureRegistry{},
		func(entityID string) (string, string, bool) { return entityID + "-device", "media-player", true },
	)
	return automation.NewCommandSink(surface, "01J8ZTESTSCHEDSINKRECORD01")
}

// marshalRows marshals each value to json.RawMessage — building a
// wire.ScheduleSection's opaquely-carried row arrays by hand (REL-065),
// mirroring internal/feeder/snapshot's own unexported marshalEachRow helper
// (which this package cannot reach directly).
func marshalRows(t *testing.T, vs ...any) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(vs))
	for _, v := range vs {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal row %+v: %v", v, err)
		}
		out = append(out, b)
	}
	return out
}

// skipMisfire fixture constants: a single schedule + one all-day daypart
// (00:00:00-23:59:59, so ANY boot instant lands inside it — modeling a relay
// restart landing inside an already-active daypart's window) declaring
// misfire:"skip" and bound to a preset batch.
const (
	skipMisfireSiteID     = "01J8ZBOOTSKIPSITE0000001"
	skipMisfireScreenID   = "01J8ZBOOTSKIPSCREEN000001"
	skipMisfireScheduleID = "01J8ZBOOTSKIPSCHEDULE0001"
	skipMisfireDaypartID  = "01J8ZBOOTSKIPDAYPART00001"
	skipMisfirePresetID   = "01J8ZBOOTSKIPPRESET000001"
	skipMisfireEntity     = "01J8ZBOOTSKIPENTITY000001"
)

// buildSkipMisfireAppliedForTest builds a minimal desiredstate.Applied whose
// carried schedule section governs one screen with a single all-day daypart
// declaring misfire:"skip" and bound to a preset batch — the DAT-075/076/
// 094/121 boot-resume regression fixture for bootScheduleResolverAt itself
// (the exact call site the fix touches).
func buildSkipMisfireAppliedForTest(t *testing.T) desiredstate.Applied {
	t.Helper()
	tz := "America/Chicago"
	lat := 41.8781
	long := -87.6298
	orgBound := "01J8ZBOOTSKIPORGBOUND0001"
	siteParent := skipMisfireSiteID

	siteNode := datamodel.ScopeNode{ID: skipMisfireSiteID, Kind: "site", ParentID: &orgBound, Name: "Skip Misfire Site", TZ: &tz, Lat: &lat, Long: &long, Revision: 1, CreatedAt: 1, UpdatedAt: 1}
	screenNode := datamodel.ScopeNode{ID: skipMisfireScreenID, Kind: "screen", ParentID: &siteParent, Name: "Skip Misfire Screen", Revision: 1, CreatedAt: 1, UpdatedAt: 1}
	schedule := datamodel.Schedule{ID: skipMisfireScheduleID, ScopeNode: skipMisfireScreenID, Name: "Skip Misfire Schedule", Revision: 1, CreatedAt: 1, UpdatedAt: 1}
	daypart := datamodel.Daypart{
		ID: skipMisfireDaypartID, ScheduleID: skipMisfireScheduleID, ScopeNode: skipMisfireScreenID,
		DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6}, StartTime: "00:00:00", EndTime: "23:59:59",
		DisplayPower: "on", PresetBatchID: skipMisfirePresetID, Misfire: "skip", Name: "All Day",
		Revision: 1, CreatedAt: 1, UpdatedAt: 1,
	}
	presetBatch := datamodel.PresetBatch{
		PresetID: skipMisfirePresetID, ScopeNode: skipMisfireScreenID, Name: "Skip Misfire Preset",
		Commands: []datamodel.PresetCommand{{EntityID: skipMisfireEntity, Command: "home"}},
		Revision: 1, CreatedAt: 1, UpdatedAt: 1,
	}

	sec := wire.ScheduleSection{
		ScopeNodes:    marshalRows(t, siteNode, screenNode),
		Schedules:     marshalRows(t, schedule),
		Dayparts:      marshalRows(t, daypart),
		PresetBatches: marshalRows(t, presetBatch),
	}.Normalized()

	return desiredstate.Applied{Schedule: sec}
}

// TestBootScheduleResolverSkipMisfireDoesNotResumeFire is the Task-6
// DAT-075/076/094/121 regression at the exact call site the fix touches
// (bootScheduleResolverAt): a daypart declaring misfire:"skip" MUST NOT
// re-dispatch its bound preset batch's device commands on a boot resolve that
// lands inside its already-active window — a site declares "skip" precisely
// so a relay restart (redeploy, crash-loop, box reboot) never re-toggles a
// device. The resolver is still built and governs the screen (the STATE
// projection, DAT-119, is unaffected); only the resume-edge preset dispatch
// is suppressed.
func TestBootScheduleResolverSkipMisfireDoesNotResumeFire(t *testing.T) {
	applied := buildSkipMisfireAppliedForTest(t)
	srv, priv, _ := newTestPlayerServer(t)
	ctrl := &recordingController{}
	sink := recordingScheduleSink(ctrl)
	site := hello.SiteBinding{TZ: "America/Chicago", Lat: 41.8781, Long: -87.6298}

	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	nowMs := time.Date(2026, time.January, 15, 12, 0, 0, 0, loc).UnixMilli() // inside the all-day daypart

	resolvers := bootScheduleResolverAt(applied, srv, sink, site, priv, nowMs)
	if len(resolvers) != 1 {
		t.Fatalf("bootScheduleResolverAt returned %d resolver(s), want 1 (the fixture schedule governs exactly one screen)", len(resolvers))
	}

	if calls := ctrl.calls(); len(calls) != 0 {
		t.Fatalf("boot resolve with misfire:skip dispatched %v to the device plane, want nothing (DAT-075/076/121: a skip misfire suppresses the boot resume-edge fire)", calls)
	}
}
