package playerserver

import (
	"encoding/json"
	"net/http"
	"testing"
)

// telemetryauth_test.go covers the three routes a channel token authorizes that
// used to ask for nothing: lease/ack, render/start and render/end. PLY-070 names
// "render acknowledgement and telemetry" in the set a channel token authorizes,
// and PLY-076 says that token is presentable as an Authorization: Bearer header
// on EVERY operation it authorizes — so a route in the set that reads no header
// is not a lenient route, it is an unauthenticated one.
//
// Each case asserts the PUBLISHED code rather than merely a 4xx: an ack can be
// refused for an absent credential, a revoked screen, an expired token, or a
// lease that is not this screen's, and an operator reading only a status line
// cannot tell a credential problem from a reference problem.

// postTelemetry is the table shape shared by every case below: the three routes,
// each with a body that is otherwise valid, so the refusal under test is the only
// thing wrong with the request.
type telemetryRoute struct {
	name string
	path string
	// body builds a well-formed body for screenID/leaseID. Both are supplied even
	// where a route ignores one, so a case can vary either without knowing which
	// route reads which.
	body func(screenID, leaseID string) []byte
}

func telemetryRoutes() []telemetryRoute {
	return []telemetryRoute{
		{"lease/ack", "/player/v1/lease/ack", func(_, leaseID string) []byte {
			b, _ := json.Marshal(LeaseAckRequest{LeaseID: leaseID, Accepted: true})
			return b
		}},
		{"render/start", "/player/v1/render/start", func(_, leaseID string) []byte {
			b, _ := json.Marshal(RenderStartRequest{LeaseID: leaseID, AssetRef: "sha256:cccc", TS: 1752538000600})
			return b
		}},
		{"render/end", "/player/v1/render/end", func(screenID, _ string) []byte {
			b, _ := json.Marshal(RenderEndRequest{
				ScreenID: screenID, AssetRef: "sha256:aaaa", ProgramRevision: "rev-17",
				TStart: 1752537960000, TEnd: 1752538000500, Cause: "scheduled", Completion: "completed",
			})
			return b
		}},
	}
}

// decodeProblem reads a Problem body for assertTypedError, which takes the
// decoded map rather than the response.
func decodeProblem(t *testing.T, resp *http.Response) map[string]json.RawMessage {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode Problem body: %v", err)
	}
	return raw
}

// TestTelemetryRoutesRefuseAnAbsentCredential is the core of the defect: before
// this, all three routes recorded whatever an unauthenticated caller posted.
// Anyone who could reach the relay's player port could acknowledge leases and
// file playback reports.
func TestTelemetryRoutesRefuseAnAbsentCredential(t *testing.T) {
	for _, rt := range telemetryRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			srv, token := preemptProgramTestServer(t)
			ts := newPairingTestServer(t, srv)
			lease := leaseFor(t, srv, token)

			// The empty token is postPlayerJSON's "send no Authorization header".
			resp := postPlayerJSON(t, ts, "", rt.path, rt.body(testScreenIDA, lease.LeaseID))
			defer resp.Body.Close()
			assertTypedError(t, resp, decodeProblem(t, resp), "CHANNEL_TOKEN_INVALID")

			assertNothingRecorded(t, srv)
		})
	}
}

// TestTelemetryRoutesRefuseAnUnknownCredential: a well-formed bearer that
// resolves to no session is the same refusal as none at all — the taxonomy's
// CHANNEL_TOKEN_INVALID covers "malformed or unknown" together.
func TestTelemetryRoutesRefuseAnUnknownCredential(t *testing.T) {
	for _, rt := range telemetryRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			srv, token := preemptProgramTestServer(t)
			ts := newPairingTestServer(t, srv)
			lease := leaseFor(t, srv, token)

			resp := postPlayerJSON(t, ts, "ct-not-a-real-token", rt.path, rt.body(testScreenIDA, lease.LeaseID))
			defer resp.Body.Close()
			assertTypedError(t, resp, decodeProblem(t, resp), "CHANNEL_TOKEN_INVALID")

			assertNothingRecorded(t, srv)
		})
	}
}

// TestTelemetryRoutesRefuseARevokedScreen is the reason this needed a credential
// at all rather than merely being untidy. Revocation is enforced at the points
// the relay performs, and it could not bite here: with no credential to inspect
// there was nothing on which to notice the screen was revoked, so a revoked
// screen kept full access to all three routes (REL-123, PLY-072).
func TestTelemetryRoutesRefuseARevokedScreen(t *testing.T) {
	for _, rt := range telemetryRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			srv, token := preemptProgramTestServer(t)
			ts := newPairingTestServer(t, srv)
			// The lease is issued BEFORE revocation, so the reference is legitimate
			// and the only thing that changed is the screen's standing.
			lease := leaseFor(t, srv, token)

			srv.SetRevokedScreens(2, []string{testScreenIDA})

			resp := postPlayerJSON(t, ts, token, rt.path, rt.body(testScreenIDA, lease.LeaseID))
			defer resp.Body.Close()
			assertTypedError(t, resp, decodeProblem(t, resp), "CHANNEL_TOKEN_REVOKED")

			assertNothingRecorded(t, srv)
		})
	}
}

// TestAckAndRenderStartRefuseALeaseThisScreenWasNotIssued: LEASE_UNKNOWN
// (PLY-114). An authenticated player may only speak about leases it holds; an
// invented lease_id is the case that let a caller record an acknowledgement for a
// Lease that never existed.
func TestAckAndRenderStartRefuseALeaseThisScreenWasNotIssued(t *testing.T) {
	for _, rt := range telemetryRoutes() {
		if rt.path == "/player/v1/render/end" {
			continue // PLY-111's body carries no lease_id; its own case is below.
		}
		t.Run(rt.name, func(t *testing.T) {
			srv, token := preemptProgramTestServer(t)
			ts := newPairingTestServer(t, srv)
			leaseFor(t, srv, token) // a real lease exists; the request names a different id

			resp := postPlayerJSON(t, ts, token, rt.path, rt.body(testScreenIDA, "01J8Z3K4N5P6Q7R8S9T0V1W2ZG"))
			defer resp.Body.Close()
			assertTypedError(t, resp, decodeProblem(t, resp), "LEASE_UNKNOWN")

			assertNothingRecorded(t, srv)
		})
	}
}

// TestAckAndRenderStartRefuseAnotherScreensLease is the cross-screen case
// PLY-070 forbids in as many words: a token authorizes "exactly the one screen_id
// it was issued to". Screen B's lease_id is real — it just is not A's — so this
// passes every check except the one that matters, and it is the case a
// non-empty-lease_id validation would let through.
func TestAckAndRenderStartRefuseAnotherScreensLease(t *testing.T) {
	for _, rt := range telemetryRoutes() {
		if rt.path == "/player/v1/render/end" {
			continue
		}
		t.Run(rt.name, func(t *testing.T) {
			srv, tokenA, tokenB := twoScreenServer(t)
			ts := newPairingTestServer(t, srv)
			leaseB := leaseFor(t, srv, tokenB)

			resp := postPlayerJSON(t, ts, tokenA, rt.path, rt.body(testScreenIDA, leaseB.LeaseID))
			defer resp.Body.Close()
			assertTypedError(t, resp, decodeProblem(t, resp), "LEASE_UNKNOWN")

			assertNothingRecorded(t, srv)

			// And screen B's own ack for its own lease still works, so the refusal is
			// about who presented it rather than about the lease being unusable.
			ok := postPlayerJSON(t, ts, tokenB, rt.path, rt.body(testScreenIDB, leaseB.LeaseID))
			defer ok.Body.Close()
			if ok.StatusCode != http.StatusOK {
				t.Fatalf("screen B's own %s = %d, want 200", rt.name, ok.StatusCode)
			}
		})
	}
}

// TestRenderEndRefusesAReportFiledAgainstAnotherScreen: render/end's body
// carries a CLIENT-SUPPLIED screen_id, because PLY-111 makes it events/1's
// content.played payload field for field — and that field is the subject of the
// record. An authenticated player naming a sibling screen there would file
// playback evidence against a screen it holds no credential for.
//
// This is the vector the reported symptom did not name: adding a credential alone
// closes the anonymous case and leaves this one, since the caller here is a fully
// authorized player.
func TestRenderEndRefusesAReportFiledAgainstAnotherScreen(t *testing.T) {
	srv, tokenA, _ := twoScreenServer(t)
	ts := newPairingTestServer(t, srv)

	rt := telemetryRoutes()[2]
	resp := postPlayerJSON(t, ts, tokenA, rt.path, rt.body(testScreenIDB, ""))
	defer resp.Body.Close()
	assertTypedError(t, resp, decodeProblem(t, resp), "VALIDATION_FAILED")
	assertNothingRecorded(t, srv)

	// Its own screen is accepted, so the refusal is about the mismatch.
	ok := postPlayerJSON(t, ts, tokenA, rt.path, rt.body(testScreenIDA, ""))
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("render/end for the token's own screen = %d, want 200", ok.StatusCode)
	}
	if len(srv.RenderEnds()) != 1 {
		t.Fatalf("recorded %d render/end reports, want 1", len(srv.RenderEnds()))
	}
}

// TestAnAcknowledgedLeaseSurvivesLaterPulls guards the bound this enforcement
// must not overreach past. Every program pull mints a fresh Lease (PLY-097), so a
// relay keeping only the newest would refuse a conformant player that pulled
// again while its acknowledgement was in flight — PLY-114 says "currently or
// MOST-RECENTLY active". This is the legitimate flow the refusals above must not
// catch.
func TestAnAcknowledgedLeaseSurvivesLaterPulls(t *testing.T) {
	srv, token := preemptProgramTestServer(t)
	ts := newPairingTestServer(t, srv)

	first := leaseFor(t, srv, token)
	leaseFor(t, srv, token) // the player pulls again before acknowledging

	rt := telemetryRoutes()[0]
	resp := postPlayerJSON(t, ts, token, rt.path, rt.body(testScreenIDA, first.LeaseID))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ack of the previous lease = %d, want 200 — a player that re-pulled before acking is conformant", resp.StatusCode)
	}

	// Past the retained history it IS refused, which is what keeps the record
	// bounded on a relay that runs for months.
	for range leaseHistoryPerScreen {
		leaseFor(t, srv, token)
	}
	stale := postPlayerJSON(t, ts, token, rt.path, rt.body(testScreenIDA, first.LeaseID))
	defer stale.Body.Close()
	assertTypedError(t, stale, decodeProblem(t, stale), "LEASE_UNKNOWN")
}

// TestIssuedLeaseHistoryIsBoundedPerScreen pins the memory property directly
// rather than inferring it from the refusal above: a relay is the component with
// no operator watching it, and every pull mints an id. A lease_id-keyed record
// would grow for as long as the process runs.
func TestIssuedLeaseHistoryIsBoundedPerScreen(t *testing.T) {
	srv, token := preemptProgramTestServer(t)

	for range leaseHistoryPerScreen * 5 {
		leaseFor(t, srv, token)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if got := len(srv.issuedLeases[testScreenIDA]); got != leaseHistoryPerScreen {
		t.Fatalf("retained %d issued lease_ids for one screen, want %d", got, leaseHistoryPerScreen)
	}
	if got := len(srv.issuedLeases); got != 1 {
		t.Fatalf("issuedLeases holds %d screens, want 1 — the map is keyed by screen, not by lease", got)
	}
}

// assertNothingRecorded: a refused request must not reach the durable record.
// Worth asserting separately every time, because the original handlers wrote to
// the map BEFORE answering, so a check bolted on after the write would return the
// right status and still poison the state.
func assertNothingRecorded(t *testing.T, srv *Server) {
	t.Helper()
	if n := len(srv.RenderStarts()); n != 0 {
		t.Errorf("a refused request recorded %d render/start reports, want 0", n)
	}
	if n := len(srv.RenderEnds()); n != 0 {
		t.Errorf("a refused request recorded %d render/end reports, want 0", n)
	}
	srv.mu.Lock()
	acks := len(srv.leaseAcks)
	srv.mu.Unlock()
	if acks != 0 {
		t.Errorf("a refused request recorded %d lease acks, want 0", acks)
	}
}
