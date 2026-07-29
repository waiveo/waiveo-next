package virtualplayer_test

import (
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	relayenroll "github.com/maaxton/waiveo-next/internal/relay/enroll"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
	"github.com/maaxton/waiveo-next/internal/virtualplayer"
)

// bootPreemptRelay boots a fresh relay against the feeder at feederBaseURL,
// exactly like photon_test.go's bootTestRelay, EXCEPT the served program is
// preempt-priority (SetProgram priority "preempt") — the takeover-lease source
// this task's interrupt-now flow pulls (PLY-100/108). It returns the relay's
// dial host/port, its TLS leaf DER, the verified Applied desired state, and
// the live *playerserver.Server itself, so the test can read the relay-side
// ack / render-report records the player posts (PLY-091/110/111).
func bootPreemptRelay(t *testing.T, feederBaseURL string) (host string, port int, certDER []byte, applied desiredstate.Applied, srv *playerserver.Server) {
	t.Helper()

	store, err := identity.Open(":memory:")
	if err != nil {
		t.Fatalf("identity.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := relayenroll.Run(feederBaseURL, store); err != nil {
		t.Fatalf("relayenroll.Run: %v", err)
	}
	applied = pullDesiredState(t, feederBaseURL, store)

	relayID, ok, err := store.Identity()
	if err != nil {
		t.Fatalf("store.Identity: %v", err)
	}
	if !ok {
		t.Fatalf("store.Identity: no identity persisted after enrollment")
	}

	cert, der, err := relayTLSCertificateForTest(relayID.CertPEM, relayID.PrivateKey)
	if err != nil {
		t.Fatalf("relayTLSCertificateForTest: %v", err)
	}
	certDER = der

	srv, err = playerserver.NewServer(relayID.CertPEM, applied.PairingGrants, playerserver.WallClockMs)
	if err != nil {
		t.Fatalf("playerserver.NewServer: %v", err)
	}
	srv.SetSigningKey(relayID.PrivateKey)
	// The takeover: a preempt-priority program carrying the feeder's image
	// (PLY-100/101/108 — priority mirrors the screen-program, unmodified).
	srv.SetProgram(applied.Generation, applied.ScreenID, applied.ProgramRevision, "preempt", "content", []wire.LeaseContent{{
		Type:      "image",
		AssetRef:  applied.Image.AssetRef,
		URL:       applied.Image.URL,
		ExpiresAt: applied.Image.ExpiresAt,
	}})

	mux := http.NewServeMux()
	srv.Register(mux)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	httpSrv := &http.Server{
		Handler:   apihttp.WithTraceID(mux),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		ErrorLog:  quietErrorLog,
	}
	go func() { _ = httpSrv.ServeTLS(lis, "", "") }()
	t.Cleanup(func() { _ = httpSrv.Close() })

	h, p, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		t.Fatalf("net.SplitHostPort: %v", err)
	}
	port, err = strconv.Atoi(p)
	if err != nil {
		t.Fatalf("strconv.Atoi(%q): %v", p, err)
	}
	return h, port, certDER, applied, srv
}

// ply101Case mirrors exactly the fields conformance/corpora/player-1/
// PLY-101-valid-lease-preemption-interrupt-now.json declares — the frozen
// oracle for lease preemption / interrupt-now (PLY-090/091/094/100/101/107/
// 110/111/112). The test parses that file directly rather than hard-coding
// its numbers, so a corpus edit either keeps this test honest or breaks it
// loudly.
type ply101Case struct {
	Input struct {
		ActiveLeaseBefore struct {
			LeaseID         string `json:"lease_id"`
			Priority        string `json:"priority"`
			ProgramRevision string `json:"program_revision"`
		} `json:"active_lease_before"`
		CurrentlyRendering struct {
			AssetRef      string `json:"asset_ref"`
			RenderStartTS int64  `json:"render_start_ts"`
		} `json:"currently_rendering"`
		NewLease struct {
			LeaseID         string              `json:"lease_id"`
			ScreenID        string              `json:"screen_id"`
			ProgramRevision string              `json:"program_revision"`
			Priority        string              `json:"priority"`
			Display         string              `json:"display"`
			Content         []wire.LeaseContent `json:"content"`
			IssuedAt        int64               `json:"issued_at"`
			ValidUntil      int64               `json:"valid_until"`
		} `json:"new_lease"`
		DeliveryTime int64 `json:"delivery_time"`
	} `json:"input"`
	Expected struct {
		LeaseAck struct {
			LeaseID  string `json:"lease_id"`
			Accepted bool   `json:"accepted"`
		} `json:"lease_ack"`
		InterruptedImmediately bool `json:"interrupted_immediately"`
		WaitedForNaturalEnd    bool `json:"waited_for_natural_end"`
		PreviousAssetRenderEnd struct {
			AssetRef   string `json:"asset_ref"`
			TEnd       int64  `json:"t_end"`
			Cause      string `json:"cause"`
			Completion string `json:"completion"`
		} `json:"previous_asset_render_end"`
		TakeoverRenderStart struct {
			LeaseID  string `json:"lease_id"`
			AssetRef string `json:"asset_ref"`
			TS       int64  `json:"ts"`
		} `json:"takeover_render_start"`
		TakeoverRenderEndCause string `json:"takeover_render_end_cause"`
		ActiveLeaseAfter       string `json:"active_lease_after"`
	} `json:"expected"`
}

// loadPLY101 reads the frozen PLY-101 corpus case relative to THIS source
// file (so the test finds it regardless of the working directory).
func loadPLY101(t *testing.T) ply101Case {
	t.Helper()
	_, self, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(self), "..", "..", "conformance", "corpora", "player-1", "PLY-101-valid-lease-preemption-interrupt-now.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read PLY-101 corpus case: %v", err)
	}
	var c ply101Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("unmarshal PLY-101 corpus case: %v", err)
	}
	return c
}

// TestAdoptionOfPLY101 drives virtualplayer.AdoptionOf against the frozen
// PLY-101 oracle: a player mid-render of a scheduled asset, handed a
// preempt-priority Lease, MUST ack the new lease, interrupt-now (not wait for
// the current asset's natural end), report the interrupted asset's render/end
// with cause=scheduled/completion=interrupted at the delivery instant, report
// the takeover asset's render/start under the new lease, classify the takeover
// render as preempted, and make the new lease its sole active lease
// (PLY-091/094/100/101/107/110/111/112).
func TestAdoptionOfPLY101(t *testing.T) {
	c := loadPLY101(t)

	active := virtualplayer.ActiveRender{
		LeaseID:         c.Input.ActiveLeaseBefore.LeaseID,
		Priority:        c.Input.ActiveLeaseBefore.Priority,
		ProgramRevision: c.Input.ActiveLeaseBefore.ProgramRevision,
		AssetRef:        c.Input.CurrentlyRendering.AssetRef,
		RenderStartTS:   c.Input.CurrentlyRendering.RenderStartTS,
	}
	next := wire.Lease{
		LeaseID:         c.Input.NewLease.LeaseID,
		ScreenID:        c.Input.NewLease.ScreenID,
		ProgramRevision: c.Input.NewLease.ProgramRevision,
		Priority:        c.Input.NewLease.Priority,
		Display:         c.Input.NewLease.Display,
		Content:         c.Input.NewLease.Content,
		IssuedAt:        c.Input.NewLease.IssuedAt,
		ValidUntil:      c.Input.NewLease.ValidUntil,
	}

	got := virtualplayer.AdoptionOf(active, next, c.Input.DeliveryTime)

	if got.AckLeaseID != c.Expected.LeaseAck.LeaseID {
		t.Errorf("ack lease_id = %q, want %q", got.AckLeaseID, c.Expected.LeaseAck.LeaseID)
	}
	if got.AckAccepted != c.Expected.LeaseAck.Accepted {
		t.Errorf("ack accepted = %v, want %v", got.AckAccepted, c.Expected.LeaseAck.Accepted)
	}
	if got.InterruptedImmediately != c.Expected.InterruptedImmediately {
		t.Errorf("interrupted_immediately = %v, want %v (PLY-101)", got.InterruptedImmediately, c.Expected.InterruptedImmediately)
	}
	if got.WaitedForNaturalEnd != c.Expected.WaitedForNaturalEnd {
		t.Errorf("waited_for_natural_end = %v, want %v (PLY-101)", got.WaitedForNaturalEnd, c.Expected.WaitedForNaturalEnd)
	}

	wantPrev := virtualplayer.RenderEndReport{
		AssetRef:   c.Expected.PreviousAssetRenderEnd.AssetRef,
		TEnd:       c.Expected.PreviousAssetRenderEnd.TEnd,
		Cause:      c.Expected.PreviousAssetRenderEnd.Cause,
		Completion: c.Expected.PreviousAssetRenderEnd.Completion,
	}
	if got.PreviousRenderEnd != wantPrev {
		t.Errorf("previous_asset_render_end = %+v, want %+v (PLY-107/111/112)", got.PreviousRenderEnd, wantPrev)
	}

	wantStart := virtualplayer.RenderStartReport{
		LeaseID:  c.Expected.TakeoverRenderStart.LeaseID,
		AssetRef: c.Expected.TakeoverRenderStart.AssetRef,
		TS:       c.Expected.TakeoverRenderStart.TS,
	}
	if got.TakeoverRenderStart != wantStart {
		t.Errorf("takeover_render_start = %+v, want %+v (PLY-110)", got.TakeoverRenderStart, wantStart)
	}

	if got.TakeoverCause != c.Expected.TakeoverRenderEndCause {
		t.Errorf("takeover render cause = %q, want %q (PLY-107)", got.TakeoverCause, c.Expected.TakeoverRenderEndCause)
	}
	if got.ActiveLeaseAfter != c.Expected.ActiveLeaseAfter {
		t.Errorf("active_lease_after = %q, want %q (PLY-094)", got.ActiveLeaseAfter, c.Expected.ActiveLeaseAfter)
	}
}

// TestPreemptInterruptNowEndToEnd drives virtualplayer.Preempt end to end
// against a live in-process feeder+relay whose served program is
// preempt-priority: the player pairs, pulls the preempt Lease, verifies its
// signature, acks it, reports the interrupted-asset render/end and the
// takeover render/start to the relay, and fetches + verifies the takeover
// asset's bytes DIRECT from the feeder (PLY-084). The relay-side records prove
// the ack and render telemetry actually crossed the wire (PLY-091/110/111).
func TestPreemptInterruptNowEndToEnd(t *testing.T) {
	feederBaseURL, img := bootTestFeeder(t)
	host, port, certDER, applied, relayServer := bootPreemptRelay(t, feederBaseURL)

	if len(applied.PairingGrants) == 0 {
		t.Fatalf("applied desired state carried no pairing_grants")
	}
	code, err := playerserver.FormPairingCode(host, port, applied.PairingGrants[0], certDER)
	if err != nil {
		t.Fatalf("FormPairingCode: %v", err)
	}

	// The prior scheduled render the preempt Lease interrupts (the corpus's
	// active_lease_before / currently_rendering, generalized).
	active := virtualplayer.ActiveRender{
		LeaseID:         "01J8Z3K4N5P6Q7R8S9T0V1W2ZF",
		Priority:        "scheduled",
		Display:         "content",
		ProgramRevision: "rev-17",
		AssetRef:        "sha256:" + "aa" + "00",
		RenderStartTS:   1752537960000,
	}
	const deliveryTime int64 = 1752538000500

	adopt, err := virtualplayer.Preempt(code, active, deliveryTime)
	if err != nil {
		t.Fatalf("Preempt: unexpected error: %v", err)
	}

	if !adopt.InterruptedImmediately || adopt.WaitedForNaturalEnd {
		t.Errorf("preempt lease not interrupt-now: interrupted=%v waited=%v (PLY-101)", adopt.InterruptedImmediately, adopt.WaitedForNaturalEnd)
	}
	if !adopt.AckAccepted || adopt.AckLeaseID == "" {
		t.Errorf("preempt lease not acked: %+v (PLY-091)", adopt)
	}
	if adopt.ActiveLeaseAfter != adopt.AckLeaseID {
		t.Errorf("active_lease_after %q != acked lease %q (PLY-094)", adopt.ActiveLeaseAfter, adopt.AckLeaseID)
	}
	if adopt.TakeoverCause != "preempted" {
		t.Errorf("takeover cause = %q, want %q (PLY-107)", adopt.TakeoverCause, "preempted")
	}
	if got, want := signhash.ContentID(adopt.TakeoverBytes), signhash.ContentID(img); got != want {
		t.Errorf("takeover bytes content id = %q, want the feeder's served image %q", got, want)
	}

	// PLY-091/110/111: the ack and both render reports must have crossed the
	// wire, observed on the relay's own records.
	if rec, ok := relayServer.LeaseAck(adopt.AckLeaseID); !ok || !rec.Accepted {
		t.Errorf("relay did not record an accepted ack for %q (PLY-091)", adopt.AckLeaseID)
	}
	ends := relayServer.RenderEnds()
	if len(ends) != 1 {
		t.Fatalf("relay recorded %d render/end reports, want 1 (the interrupted asset, PLY-111)", len(ends))
	}
	if ends[0].Cause != "scheduled" || ends[0].Completion != "interrupted" {
		t.Errorf("interrupted render/end = cause %q completion %q, want scheduled/interrupted (PLY-107/112)", ends[0].Cause, ends[0].Completion)
	}
	if ends[0].TEnd != deliveryTime {
		t.Errorf("interrupted render/end t_end = %d, want the delivery instant %d (PLY-101)", ends[0].TEnd, deliveryTime)
	}
	starts := relayServer.RenderStarts()
	if len(starts) != 1 {
		t.Fatalf("relay recorded %d render/start reports, want 1 (the takeover asset, PLY-110)", len(starts))
	}
	if starts[0].LeaseID != adopt.AckLeaseID {
		t.Errorf("takeover render/start lease_id = %q, want the takeover lease %q (PLY-110/114)", starts[0].LeaseID, adopt.AckLeaseID)
	}
	if starts[0].TS != deliveryTime+virtualplayer.TakeoverStartLatencyMillis {
		t.Errorf("takeover render/start ts = %d, want delivery+latency %d (PLY-110)", starts[0].TS, deliveryTime+virtualplayer.TakeoverStartLatencyMillis)
	}
}

// ply155Case mirrors the fields conformance/corpora/player-1/
// PLY-155-valid-power-schedule-interaction.json declares that this task's
// interrupt-now takeover consumes: the screen's currently active (blank) lease
// and the incoming preempt lease, plus the expected off-air outcome. The test
// parses that frozen file directly rather than hard-coding its values.
type ply155Case struct {
	Input struct {
		ActiveLease struct {
			LeaseID  string              `json:"lease_id"`
			Priority string              `json:"priority"`
			Display  string              `json:"display"`
			Content  []wire.LeaseContent `json:"content"`
		} `json:"active_lease"`
		IncomingPreemptLease struct {
			LeaseID  string              `json:"lease_id"`
			Priority string              `json:"priority"`
			Display  string              `json:"display"`
			Content  []wire.LeaseContent `json:"content"`
		} `json:"incoming_preempt_lease"`
	} `json:"input"`
	Expected struct {
		PreemptLeaseAccepted bool   `json:"preempt_lease_accepted"`
		DisplayRemains       string `json:"display_remains"`
		PreemptForcesVisible bool   `json:"preempt_forces_visible"`
	} `json:"expected"`
}

// loadPLY155 reads the frozen PLY-155 corpus case relative to THIS source file.
func loadPLY155(t *testing.T) ply155Case {
	t.Helper()
	_, self, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(self), "..", "..", "conformance", "corpora", "player-1", "PLY-155-valid-power-schedule-interaction.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read PLY-155 corpus case: %v", err)
	}
	var c ply155Case
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("unmarshal PLY-155 corpus case: %v", err)
	}
	return c
}

// TestAdoptionOfPLY104BlankSuppressesTakeover drives virtualplayer.AdoptionOf
// against the frozen PLY-155 oracle: the active lease is display:blank (an
// intentional off-air state) and the incoming lease is preempt-priority carrying
// display:content. PLY-104 forbids the preemption from forcing the screen out of
// blank — the new lease is accepted and becomes the sole active lease
// (PLY-091/094) but NO takeover render/start is emitted and NO interrupted
// render/end is reported, because nothing was rendering in the foreground and
// nothing starts. This is the case the corpus-passing suite did not previously
// exercise through AdoptionOf, and it MUST agree with the relay-side
// AdoptPreemptDisplay (playerserver, PLY-104).
func TestAdoptionOfPLY104BlankSuppressesTakeover(t *testing.T) {
	c := loadPLY155(t)

	active := virtualplayer.ActiveRender{
		LeaseID:  c.Input.ActiveLease.LeaseID,
		Priority: c.Input.ActiveLease.Priority,
		Display:  c.Input.ActiveLease.Display,
	}
	next := wire.Lease{
		LeaseID:  c.Input.IncomingPreemptLease.LeaseID,
		Priority: c.Input.IncomingPreemptLease.Priority,
		Display:  c.Input.IncomingPreemptLease.Display,
		Content:  c.Input.IncomingPreemptLease.Content,
	}
	const deliveryTime int64 = 1752538000500

	got := virtualplayer.AdoptionOf(active, next, deliveryTime)

	// The new preempt lease is still accepted and becomes the sole active
	// lease (PLY-091/094).
	if !got.AckAccepted || got.AckLeaseID != next.LeaseID {
		t.Errorf("preempt lease not accepted: accepted=%v ack=%q, want true/%q (PLY-091)", got.AckAccepted, got.AckLeaseID, next.LeaseID)
	}
	if got.ActiveLeaseAfter != next.LeaseID {
		t.Errorf("active_lease_after = %q, want %q (PLY-094)", got.ActiveLeaseAfter, next.LeaseID)
	}

	// PLY-104 off-air rule: the preemption MUST NOT force the screen out of
	// blank, so no takeover render/start is emitted (Preempt would otherwise
	// POST render/start + fetch the takeover bytes, driving the screen visible).
	if got.TakeoverRenderStart != (virtualplayer.RenderStartReport{}) {
		t.Errorf("takeover_render_start = %+v, want zero — a preempt MUST NOT force a blanked screen visible (PLY-104)", got.TakeoverRenderStart)
	}
	// Nothing was rendering in the foreground, so no interrupted render/end.
	if got.PreviousRenderEnd != (virtualplayer.RenderEndReport{}) {
		t.Errorf("previous_asset_render_end = %+v, want zero — an intentional blank has no foreground render to interrupt (PLY-104/155)", got.PreviousRenderEnd)
	}
	if got.InterruptedImmediately {
		t.Errorf("interrupted_immediately = true, want false — there was no foreground render to interrupt (PLY-104)")
	}

	// Cross-check the frozen expected off-air outcome.
	if !c.Expected.PreemptLeaseAccepted || c.Expected.DisplayRemains != "blank" || c.Expected.PreemptForcesVisible {
		t.Fatalf("PLY-155 corpus expected block changed: accepted=%v display_remains=%q forces_visible=%v (want true/blank/false)", c.Expected.PreemptLeaseAccepted, c.Expected.DisplayRemains, c.Expected.PreemptForcesVisible)
	}
}

// TestPreemptIntoBlankHonorsPLY104EndToEnd drives virtualplayer.Preempt end to
// end against a live relay serving a preempt-priority content program, but with
// the screen's active render being display:blank (an intentional off-air state,
// PLY-155). PLY-104 forbids the preemption from forcing the screen visible, so —
// unlike TestPreemptInterruptNowEndToEnd — NO render/start and NO render/end may
// cross the wire and no takeover bytes are fetched, even though the pulled
// preempt lease itself carries display:content. The relay-side records prove the
// player did not drive the screen out of blank.
func TestPreemptIntoBlankHonorsPLY104EndToEnd(t *testing.T) {
	feederBaseURL, _ := bootTestFeeder(t)
	host, port, certDER, applied, relayServer := bootPreemptRelay(t, feederBaseURL)

	if len(applied.PairingGrants) == 0 {
		t.Fatalf("applied desired state carried no pairing_grants")
	}
	code, err := playerserver.FormPairingCode(host, port, applied.PairingGrants[0], certDER)
	if err != nil {
		t.Fatalf("FormPairingCode: %v", err)
	}

	// The screen is intentionally off-air: its active lease is display:blank
	// with nothing rendering in the foreground (PLY-155).
	active := virtualplayer.ActiveRender{
		LeaseID:  "01J8Z3K4N5P6Q7R8S9T0V1W2ZK",
		Priority: "scheduled",
		Display:  "blank",
	}
	const deliveryTime int64 = 1752538000500

	adopt, err := virtualplayer.Preempt(code, active, deliveryTime)
	if err != nil {
		t.Fatalf("Preempt: unexpected error: %v", err)
	}

	// The preempt lease is still acked and becomes active (PLY-091/094)...
	if !adopt.AckAccepted || adopt.AckLeaseID == "" {
		t.Errorf("preempt lease not acked: %+v (PLY-091)", adopt)
	}
	if adopt.ActiveLeaseAfter != adopt.AckLeaseID {
		t.Errorf("active_lease_after %q != acked lease %q (PLY-094)", adopt.ActiveLeaseAfter, adopt.AckLeaseID)
	}
	// ...but the screen stays blank: no takeover render, no bytes fetched (PLY-104).
	if adopt.TakeoverRenderStart != (virtualplayer.RenderStartReport{}) {
		t.Errorf("takeover_render_start = %+v, want zero (PLY-104)", adopt.TakeoverRenderStart)
	}
	if adopt.TakeoverBytes != nil {
		t.Errorf("takeover bytes fetched (%d bytes), want none — a preempt MUST NOT force a blanked screen visible (PLY-104)", len(adopt.TakeoverBytes))
	}

	// The wire is the real oracle: the ack crossed it, but neither render report did.
	if rec, ok := relayServer.LeaseAck(adopt.AckLeaseID); !ok || !rec.Accepted {
		t.Errorf("relay did not record an accepted ack for %q (PLY-091)", adopt.AckLeaseID)
	}
	if starts := relayServer.RenderStarts(); len(starts) != 0 {
		t.Errorf("relay recorded %d render/start reports, want 0 — the screen must stay blank (PLY-104)", len(starts))
	}
	if ends := relayServer.RenderEnds(); len(ends) != 0 {
		t.Errorf("relay recorded %d render/end reports, want 0 — nothing was rendering to interrupt (PLY-104/155)", len(ends))
	}
}
