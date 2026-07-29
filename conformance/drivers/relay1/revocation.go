package relay1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// revocation.go drives REL-123 — the relay's enforcement of
// `revocation_and_site.revoked` (REL-066) against the credential decisions it
// makes, from its own last-synced copy, connected or not.
//
// REL-123 names three enforcement points. This relay performs two of them:
// channel-token ISSUANCE (playerserver.redeem) and per-connection credential
// VERIFICATION (the presented-token check in playerserver.handleProgram). The
// third, player-CERTIFICATE issuance, has no surface here at all — a redemption
// answers with trust anchors and a channel token, and this codebase issues no
// player certificate — so there is nothing for a differential oracle to observe
// and the case asserts nothing about it.
//
// Everything is real: a live in-process feeder signs each generation, a REAL
// enrollment supplies the relay identity, the snapshot goes through the client's
// own verify-and-apply (so `revoked` is only enforceable if it actually survives
// the wire → Applied carry), and every probe is an ordinary HTTP request to the
// committed player/1 routes.
//
// # Where the driver stands in for the binary, and why
//
// After each pull this stage installs the applied generation onto the player/1
// server itself — the revocation view, the grant set, the served program — which
// is what cmd/waiveo-relay's own apply seam does for the running binary. It has
// to: that seam lives in package main and is not importable. So this case proves
// the CONTRACT-visible chain (feeder signs → relay verifies → Applied carries →
// player/1 enforces); that the binary calls the seam on both its boot and its
// live-apply path is pinned by cmd/waiveo-relay's own tests, which drive
// rePuller.tick rather than the setter.

// rel123Input decodes the corpus's generation script and probe list.
type rel123Input struct {
	ScreenID    string             `json:"screen_id"`
	Generations []rel123Generation `json:"generations"`
	Probes      []rel123Probe      `json:"probes"`
}

// rel123Generation is one signed generation the feeder stages: its own
// `revoked` list, the grant labels its `pairing_grants` carries, and whether
// the app peer is still up once it has been applied.
type rel123Generation struct {
	Generation int64    `json:"generation"`
	Revoked    []string `json:"revoked"`
	Grants     []string `json:"grants"`
	AppPeer    string   `json:"app_peer"`
}

// rel123Probe is one ordinary player/1 request, made after a named generation
// has been applied. A `pair` probe redeems a grant label; a `program_pull`
// probe re-presents the token the most recent successful pairing minted.
type rel123Probe struct {
	AfterGeneration int64  `json:"after_generation"`
	Probe           string `json:"probe"`
	GrantSelector   string `json:"grant_selector"`
}

// rel123Expected is the oracle: a per-probe outcome, plus the two whole-run
// invariants — that the app peer really was gone for the final probes, and that
// every staged generation actually applied (a case whose later generations were
// silently refused would "prove" enforcement by never changing anything).
type rel123Expected struct {
	Probes []struct {
		Outcome            string `json:"outcome"`
		HTTPStatus         int    `json:"http_status"`
		Code               string `json:"code"`
		ChannelTokenMinted bool   `json:"channel_token_minted"`
		ScreenIDDisclosed  bool   `json:"screen_id_disclosed"`
	} `json:"probes"`
	AppPeerReachableDuringFinal bool    `json:"app_peer_reachable_during_final_two_probes"`
	GenerationsApplied          []int64 `json:"generations_applied"`
}

// rel123GrantTTL is how long each staged pairing grant lives. Comfortably
// longer than the run, since this case is about revocation and a grant that
// timed out mid-run would refuse with the same PAIRING_CODE_INVALID a
// revocation draws — a false pass.
const rel123GrantTTL = 3600

// driveREL123 replays the corpus's generation script against a really-enrolled
// relay and its really-served player/1 surface.
func driveREL123(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-123")
	if !ok {
		rep.Fail("REL-123", contract, "case not found in frozen corpus")
		return
	}

	var in rel123Input
	if err := decodeInto(&in, c.Input, "input"); err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	var exp rel123Expected
	if err := decodeInto(&exp, c.Expected, "expected"); err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	if len(in.Probes) != len(exp.Probes) {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("%d probes declared but %d expected outcomes", len(in.Probes), len(exp.Probes)))
		return
	}

	// The corpus names the screen by reference, never by a frozen literal: it
	// must be the screen the staged snapshot's own screen_programs entry is
	// for, or a paired player would be served the terminal default and a
	// `lease` probe would be asserting nothing about the program at all.
	if in.ScreenID != "$fixture_screen" {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("screen_id %q is not the $fixture_screen reference this stage resolves", in.ScreenID))
		return
	}
	screenID := snapshot.FixtureScreenID

	// Always restore the app peer, even on an early return: a case that left
	// the shared feeder down would break every stage that ran after it.
	defer feeder.SetAppPeerReachable(true)

	store, err := enrolledStore(client, feeder)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("enroll relay: %v", err))
		return
	}
	defer store.Close()
	relayID, ok, err := store.Identity()
	if err != nil || !ok {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("read relay identity: ok=%v err=%v", ok, err))
		return
	}

	srv, err := playerserver.NewServer(relayID.CertPEM, nil, playerserver.WallClockMs)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("build player/1 server: %v", err))
		return
	}
	// The relay's own Lease-signing identity (PLY-090) — a program pull that
	// succeeds has to produce a signed Lease, not a 500.
	srv.SetSigningKey(relayID.PrivateKey)
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(apihttp.WithTraceID(mux))
	defer ts.Close()

	var diffs []report.Diff
	applied := []int64{}
	probeIdx := 0
	token := "" // the channel token the most recent successful pairing minted

	for _, gen := range in.Generations {
		grants := rel123Grants(gen.Grants, screenID, relayID.RelayID)
		revoked, err := rel123Revoked(gen.Revoked, screenID)
		if err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("generation %d: %v", gen.Generation, err), diffs...)
			return
		}
		if err := feeder.StageRevokingSnapshot(gen.Generation, revoked, grants); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("stage generation %d: %v", gen.Generation, err), diffs...)
			return
		}
		got, err := client.Pull(feeder.EnrollBaseURL(), store)
		if err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("pull generation %d: %v", gen.Generation, err), diffs...)
			return
		}
		if got.Generation != gen.Generation {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("pulled generation %d, staged %d", got.Generation, gen.Generation), diffs...)
			return
		}
		applied = append(applied, got.Generation)

		// Install this generation onto the serving surface, exactly as the
		// binary's apply seam does. Revocation FIRST, for the same reason the
		// binary installs it first: the routes above are already serving.
		//
		// got.Revoked, never gen.Revoked — the whole point is whether the list
		// survived the signed wire and the verify chain. Reading the corpus
		// value here instead would make the case pass against an implementation
		// that dropped the field on the floor.
		srv.SetRevokedScreens(got.Generation, got.Revoked)
		srv.SetPairingGrants(got.Generation, got.PairingGrants)
		for _, sp := range got.ScreenPrograms {
			srv.SetServedProgram(got.Generation, sp)
		}

		if gen.AppPeer != "connected" {
			feeder.SetAppPeerReachable(false)
			// Prove the outage rather than assert it: with the app peer down, a
			// pull must FAIL. Without this the "while disconnected" probes below
			// would be indistinguishable from ordinary connected ones.
			if _, err := client.Pull(feeder.EnrollBaseURL(), store); err == nil {
				diffs = append(diffs, report.Diff{Field: "app_peer_reachable_during_final_two_probes", Expected: false, Actual: "a pull still succeeded — the app peer was not actually down"})
			}
		}

		for probeIdx < len(in.Probes) && in.Probes[probeIdx].AfterGeneration == gen.Generation {
			p := in.Probes[probeIdx]
			want := exp.Probes[probeIdx]

			switch p.Probe {
			case "pair":
				status, body, err := postPair(ts.URL, p.GrantSelector)
				if err != nil {
					rep.Fail(c.CaseID, contract, fmt.Sprintf("probes[%d]: POST /player/v1/pair: %v", probeIdx, err), diffs...)
					return
				}
				_, minted := body["channel_token"]
				_, disclosed := body["screen_id"]
				if minted != want.ChannelTokenMinted {
					diffs = append(diffs, report.Diff{Field: fmt.Sprintf("probes[%d].channel_token_minted", probeIdx), Expected: want.ChannelTokenMinted, Actual: minted})
				}
				if want.Outcome == "refused" {
					if status != want.HTTPStatus {
						diffs = append(diffs, report.Diff{Field: fmt.Sprintf("probes[%d] status", probeIdx), Expected: want.HTTPStatus, Actual: status})
					}
					// The same typed code an unresolvable selector draws: a
					// distinguishable one would make /pair an oracle for which
					// of a site's screens are revoked.
					if got := jsonString(body["code"]); got != want.Code {
						diffs = append(diffs, report.Diff{Field: fmt.Sprintf("probes[%d].code", probeIdx), Expected: want.Code, Actual: got})
					}
					if disclosed != want.ScreenIDDisclosed {
						diffs = append(diffs, report.Diff{Field: fmt.Sprintf("probes[%d].screen_id_disclosed", probeIdx), Expected: want.ScreenIDDisclosed, Actual: disclosed})
					}
					break
				}
				if status != http.StatusOK {
					diffs = append(diffs, report.Diff{Field: fmt.Sprintf("probes[%d] status", probeIdx), Expected: 200, Actual: status})
					break
				}
				if got := jsonString(body["screen_id"]); got != screenID {
					diffs = append(diffs, report.Diff{Field: fmt.Sprintf("probes[%d].screen_id", probeIdx), Expected: screenID, Actual: got})
				}
				token = jsonString(body["channel_token"])

			case "program_pull":
				if token == "" {
					rep.Fail(c.CaseID, contract, fmt.Sprintf("probes[%d]: no channel token held — an earlier pairing probe must mint one", probeIdx), diffs...)
					return
				}
				status, code, err := getProgram(ts.URL, token)
				if err != nil {
					rep.Fail(c.CaseID, contract, fmt.Sprintf("probes[%d]: GET /player/v1/program: %v", probeIdx, err), diffs...)
					return
				}
				if status != want.HTTPStatus {
					diffs = append(diffs, report.Diff{Field: fmt.Sprintf("probes[%d] status", probeIdx), Expected: want.HTTPStatus, Actual: status})
				}
				if code != want.Code {
					diffs = append(diffs, report.Diff{Field: fmt.Sprintf("probes[%d].code", probeIdx), Expected: want.Code, Actual: code})
				}

			default:
				rep.Fail(c.CaseID, contract, fmt.Sprintf("probes[%d]: unknown probe kind %q", probeIdx, p.Probe), diffs...)
				return
			}
			probeIdx++
		}
	}

	if probeIdx != len(in.Probes) {
		diffs = append(diffs, report.Diff{Field: "probes driven", Expected: len(in.Probes), Actual: probeIdx})
	}
	if !equalInt64s(applied, exp.GenerationsApplied) {
		diffs = append(diffs, report.Diff{Field: "generations_applied", Expected: exp.GenerationsApplied, Actual: applied})
	}
	if exp.AppPeerReachableDuringFinal {
		diffs = append(diffs, report.Diff{Field: "app_peer_reachable_during_final_two_probes", Expected: false, Actual: true})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "revocation enforcement diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"screen_id: the `$fixture_screen` reference resolved to the screen the staged snapshot's own screen_programs entry names — a frozen literal could not be the screen a program is actually served for.",
		"relay_id / grant relay binding (REL-121c): runtime-issued by the live feeder's own enrollment, so each grant is bound to THIS run's relay identity rather than to a frozen value.",
		"issued_at: the run's own clock; a frozen instant would make every grant's ttl depend on when the corpus was written.",
		"channel_token: an opaque value player/1 gives no grammar; asserted present/absent and re-presented, never byte-equal.",
		"player-certificate issuance, the third decision REL-123 names, is not asserted: this relay issues no player certificate, so there is no behavior to observe.")
}

// rel123Revoked resolves the corpus's `revoked` entries against this run's own
// screen. The corpus writes the reference `$fixture_screen`, never the id
// itself: the id has to be the screen the staged snapshot's `screen_programs`
// entry actually names, and a frozen literal that drifted from it would revoke
// a screen nothing serves — which reads on the wire exactly like a relay that
// ignores `revoked` altogether. An unresolvable entry is a case-authoring
// error, reported rather than passed through as an opaque id (REL-066 would
// happily carry it, and the case would silently assert nothing).
func rel123Revoked(entries []string, screenID string) ([]string, error) {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e != "$fixture_screen" {
			return nil, fmt.Errorf("revoked entry %q is not the $fixture_screen reference this stage resolves", e)
		}
		out = append(out, screenID)
	}
	return out, nil
}

// rel123Grants turns the corpus's grant labels into REL-121 records bound to
// this run's own screen (REL-121a) and relay (REL-121b/c). The label IS the
// grant_id, so the corpus's `grant_selector` resolves without a mapping table.
func rel123Grants(labels []string, screenID, relayID string) []wire.PairingGrant {
	now := time.Now().UnixMilli()
	out := make([]wire.PairingGrant, 0, len(labels))
	for _, label := range labels {
		out = append(out, wire.PairingGrant{
			GrantID:                label,
			Purpose:                "pairing",
			ResultingPrincipalKind: "screen",
			ScreenID:               screenID,
			RelayID:                relayID,
			TTL:                    rel123GrantTTL,
			RedemptionMode:         "one-time",
			IssuedAt:               now,
		})
	}
	return out
}

// getProgram drives one real GET /player/v1/program with token and returns its
// status plus the Problem `code` on a refusal ("" on success) — the two values
// PLY-072/073 make a player act differently on.
func getProgram(baseURL, token string) (int, string, error) {
	body, err := json.Marshal(map[string]any{
		"capabilities": map[string]any{"content_types": []string{"image"}, "player_version": "1.0.0"},
	})
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequest(http.MethodGet, baseURL+"/player/v1/program", bytesReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, "", nil
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		return resp.StatusCode, "", fmt.Errorf("decode problem body: %w", err)
	}
	return resp.StatusCode, problem.Code, nil
}

// equalInt64s reports whether two generation lists match element for element.
func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
