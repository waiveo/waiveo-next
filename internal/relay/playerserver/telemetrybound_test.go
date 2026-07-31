package playerserver

import (
	"encoding/json"
	"net/http"
	"testing"
)

// telemetrybound_test.go pins that the relay's in-memory telemetry records stop
// growing.
//
// Every one of them arrives on an ordinary SUCCESS path — a player reports a
// render start and end per content item, and acknowledges a fresh lease every
// pull — so an unbounded record is not an edge case a relay might reach but the
// state it reaches by working. A relay runs on an appliance for months between
// touches with nobody watching its memory.

// TestRenderReportsStopGrowing drives well past the bound and requires the
// retained set to stop at it, on both report families.
func TestRenderReportsStopGrowing(t *testing.T) {
	srv, token := preemptProgramTestServer(t)
	ts := newPairingTestServer(t, srv)
	lease := leaseFor(t, srv, token)

	const over = telemetryHistory + 50
	for i := range over {
		start, _ := json.Marshal(RenderStartRequest{
			LeaseID: lease.LeaseID, AssetRef: "sha256:cccc", TS: int64(i),
		})
		resp := postPlayerJSON(t, ts, token, "/player/v1/render/start", start)
		resp.Body.Close()

		end, _ := json.Marshal(RenderEndRequest{
			ScreenID: testScreenIDA, AssetRef: "sha256:aaaa", ProgramRevision: "rev-18",
			TStart: int64(i), TEnd: int64(i) + 1, Cause: "scheduled", Completion: "completed",
		})
		resp = postPlayerJSON(t, ts, token, "/player/v1/render/end", end)
		resp.Body.Close()
	}

	if got := len(srv.RenderStarts()); got != telemetryHistory {
		t.Errorf("retained %d render/start reports after %d, want %d", got, over, telemetryHistory)
	}
	if got := len(srv.RenderEnds()); got != telemetryHistory {
		t.Errorf("retained %d render/end reports after %d, want %d", got, over, telemetryHistory)
	}
}

// TestTheOldestReportIsTheOneDropped: a bound that evicted arbitrarily would
// keep the count right and lose whichever report a reader was about to look at.
// Oldest-first is the only eviction order that leaves the RECENT ones — the ones
// an operator is asking about — intact.
func TestTheOldestReportIsTheOneDropped(t *testing.T) {
	srv, token := preemptProgramTestServer(t)
	ts := newPairingTestServer(t, srv)
	lease := leaseFor(t, srv, token)

	const over = telemetryHistory + 10
	for i := range over {
		body, _ := json.Marshal(RenderStartRequest{LeaseID: lease.LeaseID, AssetRef: "sha256:cccc", TS: int64(i)})
		postPlayerJSON(t, ts, token, "/player/v1/render/start", body).Body.Close()
	}

	kept := srv.RenderStarts()
	if kept[0].TS != int64(over-telemetryHistory) {
		t.Errorf("oldest retained TS = %d, want %d — eviction is not oldest-first",
			kept[0].TS, over-telemetryHistory)
	}
	if kept[len(kept)-1].TS != int64(over-1) {
		t.Errorf("newest retained TS = %d, want %d — the most recent report was dropped",
			kept[len(kept)-1].TS, over-1)
	}
}

// TestLeaseAcksStopGrowing covers the map, which needs its own eviction: a map
// has no order to trim by, so it carries an arrival list beside it. Without that
// the map would keep every lease_id ever acknowledged while the reports beside it
// stayed bounded — the leak surviving in the one record that looks fixed.
func TestLeaseAcksStopGrowing(t *testing.T) {
	srv, token := preemptProgramTestServer(t)
	ts := newPairingTestServer(t, srv)

	const over = telemetryHistory + 20
	for range over {
		// A fresh lease per ack, which is what a polling player produces: every
		// program pull mints a new lease_id (PLY-097).
		lease := leaseFor(t, srv, token)
		body, _ := json.Marshal(LeaseAckRequest{LeaseID: lease.LeaseID, Accepted: true})
		resp := postPlayerJSON(t, ts, token, "/player/v1/lease/ack", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("ack = %d, want 200", resp.StatusCode)
		}
		resp.Body.Close()
	}

	srv.mu.Lock()
	acks, order := len(srv.leaseAcks), len(srv.ackOrder)
	srv.mu.Unlock()

	if order != telemetryHistory {
		t.Errorf("ack order holds %d entries after %d acks, want %d", order, over, telemetryHistory)
	}
	if acks > telemetryHistory {
		t.Errorf("lease-ack map holds %d entries after %d acks, want at most %d — the map outgrew its order",
			acks, over, telemetryHistory)
	}
}
