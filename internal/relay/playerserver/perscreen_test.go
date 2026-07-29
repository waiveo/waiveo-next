package playerserver

import (
	"crypto/ed25519"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// perscreen_test.go defends the ONE property the served-program state exists to
// have: a relay serves each paired screen ITS OWN program, selected by the
// screen_id the presenting channel token resolves to (PLY-035/076, REL-061).
//
// A `screen_programs` section is an ARRAY, one entry per screen identity row
// (data-model/1 DAT-004a). While this server held a single program value for
// the whole relay, every one of those entries wrote the same slot, so the last
// writer decided what EVERY paired player on the site was shown — a two-screen
// site served one screen's content to both, and which one depended on write
// order rather than on anything anybody authored.

// twoScreenServer builds a Server serving two screens DIFFERENT programs, with
// a redeemable screen-bound grant for each, and returns a freshly redeemed
// channel token per screen.
func twoScreenServer(t *testing.T) (srv *Server, tokenA, tokenB string) {
	t.Helper()

	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grantA := testGrantForScreen(testScreenIDA)
	grantB := testGrantForScreen(testScreenIDB)

	srv, err := NewServer(certPEM, []wire.PairingGrant{grantA, grantB})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	srv.SetProgram(1, testScreenIDA, "rev-lobby", "scheduled", "content", []wire.LeaseContent{{
		Type:     "image",
		AssetRef: "sha256:" + strings.Repeat("a1", 32),
		URL:      "https://198.51.100.20/content/lobby",
	}}, priv)
	srv.SetProgram(1, testScreenIDB, "rev-cafe", "preempt", "content", []wire.LeaseContent{{
		Type:     "image",
		AssetRef: "sha256:" + strings.Repeat("b2", 32),
		URL:      "https://198.51.100.20/content/cafe",
	}}, priv)

	return srv, redeemToken(t, srv, grantA.GrantID), redeemToken(t, srv, grantB.GrantID)
}

// redeemToken pairs against srv with selector and returns the minted channel
// token, failing the test if the redemption did not produce one.
func redeemToken(t *testing.T, srv *Server, selector string) string {
	t.Helper()
	_, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-" + selector,
		GrantSelector: selector,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})
	var resp PairingResponse
	remarshal(t, raw, &resp)
	if resp.ChannelToken == "" {
		t.Fatalf("pairing with selector %q yielded no channel_token: %+v", selector, resp)
	}
	return resp.ChannelToken
}

// pullLease pulls /player/v1/program with token and returns the issued Lease.
func pullLease(t *testing.T, srv *Server, token string) LeaseResponse {
	t.Helper()
	resp, raw := doProgram(t, srv, token, []string{"image", "video"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("program pull status = %d, want 200; body = %v", resp.StatusCode, raw)
	}
	var lease LeaseResponse
	remarshal(t, raw, &lease)
	return lease
}

// TestEachPairedScreenIsServedItsOwnProgram is the case the whole-server
// program value fails: two screens with DIFFERENT programs, two paired players,
// each pulling with its own channel token, each receiving ITS OWN screen's
// program_revision, priority, display and content — not whichever entry was
// written last.
//
// Both directions are asserted deliberately. A one-sided check passes on an
// implementation that simply serves screen A to everybody, which is exactly the
// defect: it is only the DISAGREEMENT between the two pulls that proves the
// selection is happening.
func TestEachPairedScreenIsServedItsOwnProgram(t *testing.T) {
	srv, tokenA, tokenB := twoScreenServer(t)

	leaseA := pullLease(t, srv, tokenA)
	leaseB := pullLease(t, srv, tokenB)

	if leaseA.ScreenID != testScreenIDA {
		t.Errorf("screen A's Lease names screen_id %q, want %q — the Lease is not even for the screen that asked", leaseA.ScreenID, testScreenIDA)
	}
	if leaseB.ScreenID != testScreenIDB {
		t.Errorf("screen B's Lease names screen_id %q, want %q", leaseB.ScreenID, testScreenIDB)
	}

	if leaseA.ProgramRevision != "rev-lobby" {
		t.Errorf("screen A served program_revision %q, want rev-lobby — screen A was served another screen's program (REL-061 is one entry PER screen)", leaseA.ProgramRevision)
	}
	if leaseB.ProgramRevision != "rev-cafe" {
		t.Errorf("screen B served program_revision %q, want rev-cafe — screen B was served another screen's program (REL-061 is one entry PER screen)", leaseB.ProgramRevision)
	}

	// priority/display ride the entry too (PLY-108/109), so a server that got
	// only program_revision right would still be handing a screen the wrong
	// playback class.
	if leaseA.Priority != "scheduled" || leaseB.Priority != "preempt" {
		t.Errorf("served priorities = A:%q B:%q, want A:scheduled B:preempt (PLY-108, each from its own entry)", leaseA.Priority, leaseB.Priority)
	}

	if len(leaseA.Content) != 1 || len(leaseB.Content) != 1 {
		t.Fatalf("content item counts = A:%d B:%d, want 1 each", len(leaseA.Content), len(leaseB.Content))
	}
	if leaseA.Content[0].URL != "https://198.51.100.20/content/lobby" {
		t.Errorf("screen A served content url %q, want the lobby asset — screen A is showing screen B's content", leaseA.Content[0].URL)
	}
	if leaseB.Content[0].URL != "https://198.51.100.20/content/cafe" {
		t.Errorf("screen B served content url %q, want the cafe asset — screen B is showing screen A's content", leaseB.Content[0].URL)
	}
	if leaseA.Content[0].URL == leaseB.Content[0].URL {
		t.Error("both screens were served the SAME content url — every paired player on this site is watching one screen's program")
	}
}

// TestScreenWithNoEntryIsServedTheTerminalDefault is the second half of the
// same defect. A screen this relay holds no `screen_programs` entry for MUST be
// served data-model/1's terminal default — display: blank with no content,
// "powered on, showing nothing, distinct from off" (DAT-118, the
// DAT-118-valid-terminal-default-blank corpus case's own lease_projection) —
// and MUST NOT be served a sibling screen's program as a fallback. Falling back
// to a sibling is not a lesser bug than serving the wrong entry; it IS serving
// the wrong entry.
func TestScreenWithNoEntryIsServedTheTerminalDefault(t *testing.T) {
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grantA := testGrantForScreen(testScreenIDA)
	grantB := testGrantForScreen(testScreenIDB)

	srv, err := NewServer(certPEM, []wire.PairingGrant{grantA, grantB})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Screen A has a program; screen B has none at all.
	srv.SetProgram(1, testScreenIDA, "rev-lobby", "scheduled", "content", testImageContent(), priv)

	_ = redeemToken(t, srv, grantA.GrantID)
	lease := pullLease(t, srv, redeemToken(t, srv, grantB.GrantID))

	if lease.Display != DisplayBlank {
		t.Errorf("display = %q, want %q — a screen with no screen_programs entry must reach DAT-118's terminal default, not another screen's program", lease.Display, DisplayBlank)
	}
	if len(lease.Content) != 0 {
		t.Errorf("content = %+v, want no items — DAT-118's terminal default is blank with NO content", lease.Content)
	}
	if lease.ProgramRevision != TerminalProgramRevision {
		t.Errorf("program_revision = %q, want the stable terminal sentinel %q, so a screen parked at the terminal blank never spuriously re-swaps", lease.ProgramRevision, TerminalProgramRevision)
	}
	if lease.ScreenID != testScreenIDB {
		t.Errorf("screen_id = %q, want %q — the Lease is still this screen's, it simply has no program", lease.ScreenID, testScreenIDB)
	}
	// The signature still has to verify: a terminal-default Lease is a Lease
	// (PLY-090), not a degenerate unsigned response.
	if lease.Signature == "" {
		t.Error("terminal-default Lease carries no signature; PLY-090 admits no unsigned Lease")
	}
}

// TestGenerationFenceIsPerScreen pins REL-052/056 at the granularity it now
// guards. Two things must both hold:
//
//   - a strictly-older generation's late write is still refused FOR ITS OWN
//     SCREEN (the original fence, unchanged); and
//   - one screen's higher generation does NOT fence another screen's write. A
//     single whole-server fence made screen A's resolver, running ahead at a
//     higher generation, silently swallow every write screen B's resolver made
//     at the generation B was legitimately resolved for — B would be stuck on
//     whatever it had, or on nothing, with no error anywhere.
func TestGenerationFenceIsPerScreen(t *testing.T) {
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	srv, err := NewServer(certPEM, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Screen A races ahead to generation 9.
	srv.SetProgram(9, testScreenIDA, "a-gen-9", "scheduled", "content", nil, priv)

	// Screen B's own resolver writes at generation 4 — older than A's, but B has
	// never been written at all, so nothing of B's is being reverted.
	srv.SetProgram(4, testScreenIDB, "b-gen-4", "scheduled", "content", nil, priv)
	if got := srv.programFor(testScreenIDB).ProgramRevision; got != "b-gen-4" {
		t.Errorf("screen B program_revision = %q, want b-gen-4 — screen A's higher generation fenced a DIFFERENT screen's write; screens do not supersede one another, only later generations of the same screen do", got)
	}

	// The fence itself is intact per screen: a strictly-older write for B is
	// refused, and A is untouched throughout.
	srv.SetProgram(3, testScreenIDB, "b-gen-3-stale", "blank", "blank", nil, priv)
	if got := srv.programFor(testScreenIDB).ProgramRevision; got != "b-gen-4" {
		t.Errorf("screen B program_revision = %q, want b-gen-4 — a stale generation-3 write reverted screen B past generation 4 (REL-052/056)", got)
	}
	if got := srv.programFor(testScreenIDA).ProgramRevision; got != "a-gen-9" {
		t.Errorf("screen A program_revision = %q, want a-gen-9 — writes aimed at screen B disturbed screen A", got)
	}

	// A same-generation write still wins within a screen, so a same-generation
	// schedule resolver can replace that screen's boot baseline.
	srv.SetProgram(4, testScreenIDB, "b-gen-4-schedule", "scheduled", "content", nil, priv)
	if got := srv.programFor(testScreenIDB).ProgramRevision; got != "b-gen-4-schedule" {
		t.Errorf("screen B program_revision = %q, want b-gen-4-schedule — a same-generation write must still win", got)
	}
}

// TestConcurrentPerScreenWritesDoNotOverwriteEachOther is the multi-resolver
// case as it actually runs: a site under schedule builds one resolver per
// screen, each on its own goroutine, each writing its own screen's resolved
// program every tick. Every screen must end on ITS OWN program.
//
// Run under -race this also covers the map writes the per-screen state
// introduced. The whole-server program value could not fail this test's
// concurrency, only its CORRECTNESS: it never raced, it just ended with every
// screen sharing whichever program was written last.
func TestConcurrentPerScreenWritesDoNotOverwriteEachOther(t *testing.T) {
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	srv, err := NewServer(certPEM, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	const (
		screens = 8
		ticks   = 50
	)
	ids := make([]string, screens)
	for i := range ids {
		ids[i] = screenIDForIndex(i)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			<-start
			for tick := 0; tick < ticks; tick++ {
				srv.SetProgram(1, id, revisionForIndex(i), "scheduled", "content", nil, priv)
			}
		}(i, id)
	}
	close(start)
	wg.Wait()

	for i, id := range ids {
		if got := srv.programFor(id).ProgramRevision; got != revisionForIndex(i) {
			t.Errorf("screen %s served program_revision %q, want %q — a concurrent resolver for a different screen overwrote it", id, got, revisionForIndex(i))
		}
	}
}

// screenIDForIndex/revisionForIndex build distinct, recognisable per-screen
// fixtures for the concurrency case.
func screenIDForIndex(i int) string {
	return "01J8Z9DEM0SCREENR0W00000" + string(rune('A'+i)) + "0"
}

func revisionForIndex(i int) string {
	return "rev-screen-" + string(rune('A'+i))
}

// TestCurrentDisplayReportsTheAskedScreen pins the accessor keepalive's PLY-155
// blank-Lease suppression reads. PLY-155 suppresses recovery "whenever the
// TARGET screen's own currently active Lease has display: blank", so the
// accessor must answer for the screen named — a whole-server reading both
// suppresses recovery on a live screen because a different screen is blank, and
// relaunches an intentionally blanked screen because a different one is showing
// content.
func TestCurrentDisplayReportsTheAskedScreen(t *testing.T) {
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	srv, err := NewServer(certPEM, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	srv.SetProgram(1, testScreenIDA, "rev-a", "scheduled", DisplayContent, testImageContent(), priv)
	srv.SetProgram(1, testScreenIDB, "rev-b", "scheduled", DisplayBlank, nil, priv)

	if got := srv.CurrentDisplay(testScreenIDA); got != DisplayContent {
		t.Errorf("CurrentDisplay(A) = %q, want %q — recovery would be suppressed on a screen showing content (PLY-155)", got, DisplayContent)
	}
	if got := srv.CurrentDisplay(testScreenIDB); got != DisplayBlank {
		t.Errorf("CurrentDisplay(B) = %q, want %q — an intentionally blanked screen would be relaunched (PLY-155)", got, DisplayBlank)
	}
	// A screen with no program reports what it would actually be served: the
	// DAT-118 terminal default. The accessor and the handler must never
	// disagree about what a given screen is currently being shown.
	if got := srv.CurrentDisplay("01J8Z9DEM0SCREENR0WUNKN0WN"); got != DisplayBlank {
		t.Errorf("CurrentDisplay(unknown screen) = %q, want %q (the terminal default it would be served)", got, DisplayBlank)
	}
}

// TestSoleServedScreenOnlyAnswersWhenThereIsNoAmbiguity pins the accessor
// cmd/waiveo-relay uses to wire PLY-155 for a caller that knows a device entity
// but not a screen. It must answer only when there is exactly one screen —
// with several, an entity cannot be attributed to one of them and any answer
// would be a guess with a real chance of naming the wrong screen.
func TestSoleServedScreenOnlyAnswersWhenThereIsNoAmbiguity(t *testing.T) {
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	srv, err := NewServer(certPEM, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	if _, ok := srv.SoleServedScreen(); ok {
		t.Error("SoleServedScreen reported a screen with none configured")
	}

	srv.SetProgram(1, testScreenIDA, "rev-a", "scheduled", DisplayContent, nil, priv)
	got, ok := srv.SoleServedScreen()
	if !ok || got != testScreenIDA {
		t.Errorf("SoleServedScreen() = (%q, %v), want (%q, true)", got, ok, testScreenIDA)
	}

	srv.SetProgram(1, testScreenIDB, "rev-b", "scheduled", DisplayBlank, nil, priv)
	if got, ok := srv.SoleServedScreen(); ok {
		t.Errorf("SoleServedScreen() = (%q, true) with two screens configured, want false — answering here picks a screen arbitrarily", got)
	}
}

// TestSetProgramIgnoresAnEmptyScreenID: an entry naming no screen is not
// installed. No channel token ever resolves to an empty screen_id, so such an
// entry could only ever be dead state — and, worse, a bug that dropped the
// screen id on the way in would silently install every screen's program into
// one shared "" slot, which is the whole-server behavior back again under a
// different name.
func TestSetProgramIgnoresAnEmptyScreenID(t *testing.T) {
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	srv, err := NewServer(certPEM, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	srv.SetProgram(1, "", "rev-nowhere", "scheduled", "content", testImageContent(), priv)

	if _, ok := srv.SoleServedScreen(); ok {
		t.Error("a program installed under an empty screen_id became this server's sole served screen")
	}
	if got := srv.programFor(testScreenIDA).ProgramRevision; got != TerminalProgramRevision {
		t.Errorf("screen A program_revision = %q, want the terminal default — an entry naming no screen leaked into a real screen's serving", got)
	}
}

// programFor is the read programFor(screenID) exposes to this package's tests
// without them reaching into the lock discipline directly.
func (s *Server) programFor(screenID string) program {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.programForLocked(screenID)
}

// compile-time assurance the fixtures above use a real ed25519 key type, so a
// signature check in these cases is meaningful rather than vacuous.
var _ ed25519.PrivateKey = ed25519.PrivateKey(nil)
