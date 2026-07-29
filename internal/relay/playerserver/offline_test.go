package playerserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// offlineScreenID is the screen identity row (DAT-004a) these offline-serve
// cases both bind their pairing grant to and install their persisted
// screen_programs entry for. The two MUST be the same id: the relay serves a
// program per screen, so a grant redeeming into a different screen would be
// served the terminal default and these cases would assert nothing about the
// persisted entry.
const offlineScreenID = "01J8Z3K4N5P6Q7R8S9T0V1W2X6"

// TestSetServedProgramDeliversPersistedPreemptLease is Task 2's player-side
// half of REL-061: the relay sources its program-delivery lease from a
// PERSISTED screen_programs entry (a wire.ScreenProgram, as desiredstate's
// ServedProgram returns from the durable store) rather than from a live pull.
// SetServedProgram carries the entry's priority: preempt / display: content /
// program_revision: rev-99 and its content pointers UNMODIFIED onto the issued
// Lease (PLY-108/109, REL-061), so a preempt/content assignment reaches the
// screen through the relay's own offline continuity with no app peer live.
func TestSetServedProgramDeliversPersistedPreemptLease(t *testing.T) {
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grant := testGrantForScreen(offlineScreenID)

	srv, err := NewServer(certPEM, []wire.PairingGrant{grant})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// The persisted screen_programs entry, exactly the REL-061 offline-serve
	// shape desiredstate.ServedProgram hands back from the durable store —
	// preempt priority, content display, rev-99 — carrying signed content
	// pointers (never asset bytes, REL-140).
	served := wire.ScreenProgram{
		ScreenID:        offlineScreenID,
		ProgramRevision: "rev-99",
		Priority:        "preempt",
		Display:         "content",
		Content: []wire.ContentRef{{
			AssetRef:  "sha256:" + strings.Repeat("cc", 32),
			URL:       "https://198.51.100.20/cas/cccc0000",
			ExpiresAt: 1752545000000,
		}},
	}
	srv.SetServedProgram(1, served, priv)

	_, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})
	var pairResp PairingResponse
	remarshal(t, raw, &pairResp)
	if pairResp.ChannelToken == "" {
		t.Fatalf("pairing did not yield a channel_token: %+v", pairResp)
	}

	resp, body := doProgram(t, srv, pairResp.ChannelToken, []string{"image", "video"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
	}

	var lease LeaseResponse
	remarshal(t, body, &lease)

	if lease.ProgramRevision != "rev-99" {
		t.Errorf("program_revision = %q, want %q (from the persisted screen_program)", lease.ProgramRevision, "rev-99")
	}
	if lease.Priority != "preempt" {
		t.Errorf("priority = %q, want %q (PLY-108 / REL-061, unmodified from the persisted entry)", lease.Priority, "preempt")
	}
	if lease.Display != "content" {
		t.Errorf("display = %q, want %q (PLY-109 / REL-061, unmodified from the persisted entry)", lease.Display, "content")
	}
	if len(lease.Content) != 1 {
		t.Fatalf("content has %d items, want 1", len(lease.Content))
	}
	if lease.Content[0].Type != "image" {
		t.Errorf("content[0].type = %q, want %q", lease.Content[0].Type, "image")
	}
	if lease.Content[0].URL != served.Content[0].URL {
		t.Errorf("content[0].url = %q, want the persisted content-origin URL %q", lease.Content[0].URL, served.Content[0].URL)
	}
	if lease.Content[0].AssetRef != served.Content[0].AssetRef {
		t.Errorf("content[0].asset_ref = %q, want %q", lease.Content[0].AssetRef, served.Content[0].AssetRef)
	}
}

// TestSetServedProgramCarriesPerItemContentType asserts SetServedProgram
// (REL-061a) annotates EACH content item's Lease `type` (PLY-083) from that
// item's OWN ContentRef.ContentType, in order — not a blanket "image"
// constant — and that an item whose ContentType is "" (an older feeder
// predating the field) still defaults to `image`, preserving this
// codebase's historical single-image behavior byte-for-byte.
func TestSetServedProgramCarriesPerItemContentType(t *testing.T) {
	certPEM, _, priv, _ := testRelaySigningIdentity(t)
	grant := testGrantForScreen(offlineScreenID)

	srv, err := NewServer(certPEM, []wire.PairingGrant{grant})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	served := wire.ScreenProgram{
		ScreenID:        offlineScreenID,
		ProgramRevision: "rev-cast-1",
		Priority:        "scheduled",
		Display:         "content",
		Content: []wire.ContentRef{
			{
				AssetRef: "sha256:" + strings.Repeat("aa", 32),
				URL:      "https://198.51.100.20/cas/aaaa0000",
				// ContentType left unset — must default to "image".
			},
			{
				AssetRef:    "sha256:" + strings.Repeat("bb", 32),
				URL:         "https://198.51.100.20/cas/bbbb0000",
				ContentType: "video",
				DurationMS:  9000,
			},
		},
	}
	srv.SetServedProgram(1, served, priv)

	_, raw := doPair(t, srv, PairingRequest{
		HardwareID:    "hw-cast-0001",
		GrantSelector: grant.GrantID,
		Capabilities:  Capabilities{ContentTypes: []string{"image", "video"}, PlayerVersion: "1.0.0"},
	})
	var pairResp PairingResponse
	remarshal(t, raw, &pairResp)
	if pairResp.ChannelToken == "" {
		t.Fatalf("pairing did not yield a channel_token: %+v", pairResp)
	}

	resp, body := doProgram(t, srv, pairResp.ChannelToken, []string{"image", "video"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
	}

	var lease LeaseResponse
	remarshal(t, body, &lease)

	if len(lease.Content) != 2 {
		t.Fatalf("content has %d items, want 2", len(lease.Content))
	}
	if lease.Content[0].Type != "image" {
		t.Errorf("content[0].type = %q, want %q (ContentType-unset default, REL-061a)", lease.Content[0].Type, "image")
	}
	if lease.Content[1].Type != "video" {
		t.Errorf("content[1].type = %q, want %q (carried from the persisted entry's own ContentType)", lease.Content[1].Type, "video")
	}
	if lease.Content[0].AssetRef != served.Content[0].AssetRef {
		t.Errorf("content[0].asset_ref = %q, want %q", lease.Content[0].AssetRef, served.Content[0].AssetRef)
	}
	if lease.Content[1].AssetRef != served.Content[1].AssetRef {
		t.Errorf("content[1].asset_ref = %q, want %q — item 1's own digest must survive, not just item 0's", lease.Content[1].AssetRef, served.Content[1].AssetRef)
	}
	if lease.Content[0].DurationMS != 0 {
		t.Errorf("content[0].duration_ms = %d, want 0 (item carried none, PLY-083b/omitempty)", lease.Content[0].DurationMS)
	}
	if lease.Content[1].DurationMS != 9000 {
		t.Errorf("content[1].duration_ms = %d, want 9000 (carried from the persisted entry's own DurationMS, REL-061a -> PLY-083b)", lease.Content[1].DurationMS)
	}
}
