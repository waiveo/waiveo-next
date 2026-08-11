package main

import (
	"reflect"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// screenstatusreport_test.go covers the relay binary's own half of parity row
// 5.8: the projection from the player server's observation snapshot onto the
// `screen.status` wire entries, and the offline no-op.
//
// The projection is the one place a field can be silently DROPPED on its way
// upstream, and a dropped field looks exactly like a screen that never reported
// it — a console showing "never rendered anything" for a wall that is rendering
// fine. It is asserted field-for-field rather than spot-checked, because the
// failure this guards is adding a field to ScreenStatus and forgetting one line
// here, which nothing else notices.
//
// The whole-stack proof that the report REACHES the app peer and becomes an
// api/1 row lives in internal/feeder/relayconn/screenstatus_e2e_test.go; this
// file is only the projection and the degrade.

// TestReportScreenStatusProjectsEveryObservedField pins the projection.
func TestReportScreenStatusProjectsEveryObservedField(t *testing.T) {
	srv := newPlayerServerWithGrants(t, twoScreenGrant(msScreenRowA))
	srv.SetProgram(1, msScreenRowA, "rev-a", playerserver.PriorityPreempt, "content", []wire.LeaseContent{
		{Type: "image", AssetRef: "sha256:aa", URL: "https://origin.example/content/aa"},
		{Type: "image", AssetRef: "sha256:bb", URL: "https://origin.example/content/bb"},
	})

	// A real screen pairs and pulls TWICE without acknowledging, so every
	// observation this projection carries has a non-zero value to lose.
	//
	// That matters more than it looks: `want` below is spelled from the snapshot's
	// own fields, so a field the projection drops is only caught when the snapshot
	// value differs from the zero one. Two unacknowledged pulls make unacked_pulls
	// 2 on one side and 0 on the other if the mapping forgets it — which is the
	// exact omission this test exists to catch, on the exact field the app peer's
	// `fetching` judgement now depends on.
	ts := newPlayerHTTP(t, srv)
	token := pairForToken(t, ts, "grant-offline-"+msScreenRowA)
	pullProgram(t, ts, token)
	pullProgram(t, ts, token)

	statuses := srv.ScreenStatuses()
	if len(statuses) != 1 {
		t.Fatalf("%d observed screen(s), want 1", len(statuses))
	}
	st := statuses[0]

	// The complete expected projection, spelled from the observation snapshot's
	// own fields. screenStatusEntries is the PRODUCTION mapping (main.go); this
	// is the independent statement of what it must produce, so a field the
	// producer forgets surfaces as an inequality below rather than as two copies
	// of the same omission agreeing with each other.
	want := wire.ScreenStatusEntry{
		ScreenID:             st.ScreenID,
		Paired:               st.Paired,
		LastPullAgeMs:        st.LastPullAgeMs,
		LastAckAgeMs:         st.LastAckAgeMs,
		LastRenderStartAgeMs: st.LastRenderStartAgeMs,
		UnackedPulls:         st.UnackedPulls,
		ProgramRevision:      st.ProgramRevision,
		Priority:             st.Priority,
		Display:              st.Display,
		ContentCount:         st.ContentCount,
		RenderAssetRef:       st.RenderAssetRef,
	}

	captured := screenStatusEntries(srv)
	if len(captured) != 1 {
		t.Fatalf("%d projected entr(ies), want 1", len(captured))
	}
	if !reflect.DeepEqual(captured[0], want) {
		t.Fatalf("projected entry =\n  %+v\nwant\n  %+v\n— a field dropped here looks exactly like a screen that never reported it", captured[0], want)
	}
	// And the substance, so the comparison above cannot be satisfied by two
	// equally empty values.
	if captured[0].ScreenID != msScreenRowA || captured[0].Priority != playerserver.PriorityPreempt || captured[0].ContentCount != 2 {
		t.Errorf("projected entry carries %q/%q/%d, want the screen's own preempt program with 2 items",
			captured[0].ScreenID, captured[0].Priority, captured[0].ContentCount)
	}
	// Stated separately from the DeepEqual, because a value of 0 on BOTH sides
	// would satisfy that comparison while telling the app peer that a screen
	// which has abandoned two Leases is up to date — and the app peer reads this
	// field to decide whether `fetching` still applies.
	if captured[0].UnackedPulls != 2 {
		t.Errorf("projected unacked_pulls = %d after two pulls and no ack, want 2 — the app peer's `fetching` judgement is bounded on this count, and a zero here restores the state that captured a broken wall forever",
			captured[0].UnackedPulls)
	}
}

// TestReportScreenStatusWithNoConnectionIsASilentNoOp: a relay is expected to
// run for long stretches with no app peer (REL-055), and its reporter tickers
// keep firing throughout. A nil client must be the ordinary offline case, never
// a panic that takes the whole reporting goroutine down for the rest of the
// process's life — the accumulated observations are still there and the next
// connection reports them.
func TestReportScreenStatusWithNoConnectionIsASilentNoOp(t *testing.T) {
	srv := newPlayerServerWithGrants(t)
	srv.SetProgram(1, msScreenRowA, "rev-a", playerserver.PriorityScheduled, "content", nil)
	reportScreenStatus(nil, srv) // must not panic
}
