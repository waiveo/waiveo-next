package scanstatus

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

func TestLatestReportWins(t *testing.T) {
	r := New()
	r.ApplyDiscoveryScanStatus("relay-a", wire.DiscoveryScanStatusBody{
		State: wire.DiscoveryScanStateScanning, ScanID: "s1", StartedAt: 100, Candidates: 3,
	})
	r.ApplyDiscoveryScanStatus("relay-a", wire.DiscoveryScanStatusBody{
		State: wire.DiscoveryScanStateIdle, ScanID: "s1", StartedAt: 100, FinishedAt: 400, Candidates: 35,
	})

	got := r.Statuses()
	if len(got) != 1 {
		t.Fatalf("statuses = %d, want 1 — a report REPLACES the relay's previous state", len(got))
	}
	if got[0].State != wire.DiscoveryScanStateIdle || got[0].FinishedAt != 400 || got[0].Candidates != 35 {
		t.Errorf("status = %+v, want the later (finished) report", got[0])
	}
}

// TestNeverReportedRelayIsAbsent is the honesty rule: only a relay can assert it
// is idle, so a relay nothing has heard from must not be invented as idle.
func TestNeverReportedRelayIsAbsent(t *testing.T) {
	r := New()
	if got := r.Statuses(); len(got) != 0 {
		t.Fatalf("statuses = %+v on a fresh registry, want none", got)
	}
	r.ApplyDiscoveryScanStatus("relay-a", wire.DiscoveryScanStatusBody{State: wire.DiscoveryScanStateIdle})
	if got := r.Statuses(); len(got) != 1 || got[0].RelayID != "relay-a" {
		t.Fatalf("statuses = %+v, want only the relay that reported", got)
	}
}

func TestStatusesAreOrderedAndAttributed(t *testing.T) {
	r := New()
	for _, id := range []string{"relay-c", "relay-a", "relay-b"} {
		r.ApplyDiscoveryScanStatus(id, wire.DiscoveryScanStatusBody{State: wire.DiscoveryScanStateIdle})
	}
	got := r.Statuses()
	if len(got) != 3 || got[0].RelayID != "relay-a" || got[1].RelayID != "relay-b" || got[2].RelayID != "relay-c" {
		t.Fatalf("statuses = %+v, want relay-id order so a console does not reshuffle between polls", got)
	}
}

// TestEmptyRelayIDIsIgnored: the caller passes the AUTHENTICATED connection
// identity, so an empty one is a wiring defect — filing it would put a status on
// the console attributable to no relay.
func TestEmptyRelayIDIsIgnored(t *testing.T) {
	r := New()
	r.ApplyDiscoveryScanStatus("", wire.DiscoveryScanStatusBody{State: wire.DiscoveryScanStateScanning})
	if got := r.Statuses(); len(got) != 0 {
		t.Fatalf("statuses = %+v, want none — an unattributable report is dropped", got)
	}
}
