package playerserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

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
	grant := testGrant()

	srv, err := NewServer(certPEM, []wire.PairingGrant{grant})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// The persisted screen_programs entry, exactly the REL-061 offline-serve
	// shape desiredstate.ServedProgram hands back from the durable store —
	// preempt priority, content display, rev-99 — carrying signed content
	// pointers (never asset bytes, REL-140).
	served := wire.ScreenProgram{
		ScreenID:        "01J8Z3K4N5P6Q7R8S9T0V1W2X6",
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
