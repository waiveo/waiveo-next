package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	// A real screen pairs, ACCEPTS a Lease, then pulls twice more and REFUSES the
	// last — so every observation this projection carries has a non-zero value to
	// lose, on every axis the entry has.
	//
	// That matters more than it looks: `want` below is derived from the snapshot's
	// own fields, so a field the projection drops is only caught when the snapshot
	// value differs from the zero one. Two unacknowledged pulls make unacked_pulls
	// 2 on one side and 0 on the other if the mapping forgets it; an acceptance
	// and a refusal do the same for the six fields that say what the SCREEN made
	// of what it was handed, which are the ones a console renders as "now
	// playing" and "why is this wall wrong".
	ts := newPlayerHTTP(t, srv)
	token := pairForToken(t, ts, "grant-offline-"+msScreenRowA)
	accepted := pullProgram(t, ts, token)
	ackLease(t, ts, token, playerserver.LeaseAckRequest{LeaseID: accepted.LeaseID, Accepted: true})
	pullProgram(t, ts, token)
	refused := pullProgram(t, ts, token)
	ackLease(t, ts, token, playerserver.LeaseAckRequest{
		LeaseID: refused.LeaseID, Accepted: false,
		Reason: "cast item 1 slide layer 3 (image): content fetch failed: HTTP 403 Forbidden",
	})

	statuses := srv.ScreenStatuses()
	if len(statuses) != 1 {
		t.Fatalf("%d observed screen(s), want 1", len(statuses))
	}
	st := statuses[0]

	// The complete expected projection, built from the observation snapshot BY
	// FIELD NAME rather than spelled out here.
	//
	// It used to be a hand-written literal, and that is a guard with the failure
	// it guards against built into it: adding a field to both types and
	// forgetting the one line in main.go leaves this literal missing it too, so
	// the two omissions agree and the test stays green. Six fields were added to
	// this entry in one change on 2026-08-11; a list maintained by the same hand
	// that maintains the mapping cannot catch a mapping that hand forgot.
	//
	// Derived from the wire type instead: every field the entry declares must be
	// carried, and a field with no same-named source on the snapshot is itself a
	// failure (a wire field nothing observes cannot be populated by anything).
	want := wantEntryFromSnapshot(t, st)

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
	// The two halves of what the SCREEN said, named literally. Every one of these
	// is zero-valued if the mapping omits it, and a zero here is not a neutral
	// omission at the console: an empty acked set reads as a screen that has
	// confirmed nothing, and a false `rejected` reads as one with no complaint.
	if captured[0].AckedProgramRevision != "rev-a" || captured[0].AckedDisplay != "content" || captured[0].AckedContentCount != 2 {
		t.Errorf("projected acceptance = %q/%q/%d, want rev-a/content/2 — this is the set a console renders as what the wall is showing",
			captured[0].AckedProgramRevision, captured[0].AckedDisplay, captured[0].AckedContentCount)
	}
	if !captured[0].Rejected || captured[0].RejectedProgramRevision != "rev-a" || captured[0].RejectReason == "" {
		t.Errorf("projected refusal = %v/%q/%q, want the refusal, its program and the player's reason",
			captured[0].Rejected, captured[0].RejectedProgramRevision, captured[0].RejectReason)
	}
	// Stated separately from the DeepEqual, because a value of 0 on BOTH sides
	// would satisfy that comparison while telling the app peer that a screen
	// which has abandoned two Leases is up to date — and the app peer reads this
	// field to decide whether `fetching` still applies.
	//
	if captured[0].UnackedPulls != 2 {
		t.Errorf("projected unacked_pulls = %d after two unacknowledged pulls, want 2 — the app peer's `fetching` judgement is bounded on this count, and a zero here restores the state that captured a broken wall forever",
			captured[0].UnackedPulls)
	}
}

// ackLease posts a PLY-091 acknowledgement — accepted or refused — for a Lease
// this screen was issued.
func ackLease(t *testing.T, ts *httptest.Server, token string, body playerserver.LeaseAckRequest) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal lease ack: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/player/v1/lease/ack", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build lease ack request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /player/v1/lease/ack: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lease ack status = %d, want 200", resp.StatusCode)
	}
}

// wantEntryFromSnapshot builds the wire entry the projection must produce for
// st, copying every field the wire type declares from the identically named
// field on the observation snapshot.
//
// The two types are deliberately separate — one is this relay's own record, the
// other is the frame — and this is the statement that their overlap is total:
// every fact the frame can carry is one the snapshot holds, under the same name.
// A wire field with no source fails here loudly rather than riding upstream as a
// permanent zero, which at the console is indistinguishable from a screen that
// never reported it.
func wantEntryFromSnapshot(t *testing.T, st playerserver.ScreenStatus) wire.ScreenStatusEntry {
	t.Helper()
	var want wire.ScreenStatusEntry
	wv := reflect.ValueOf(&want).Elem()
	sv := reflect.ValueOf(st)
	for i := 0; i < wv.NumField(); i++ {
		name := wv.Type().Field(i).Name
		src := sv.FieldByName(name)
		if !src.IsValid() {
			t.Fatalf("wire.ScreenStatusEntry declares %s and playerserver.ScreenStatus has no such field: the frame carries a fact nothing observes, so it can only ever be reported as a zero value", name)
		}
		if src.Type() != wv.Field(i).Type() {
			t.Fatalf("%s is %s on the wire and %s in the observation record", name, wv.Field(i).Type(), src.Type())
		}
		wv.Field(i).Set(src)
	}
	return want
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
