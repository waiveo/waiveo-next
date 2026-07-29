package relay1

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
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
	ScreenID     string             `json:"screen_id"`
	Generations  []rel123Generation `json:"generations"`
	Probes       []rel123Probe      `json:"probes"`
	RelayRestart rel123Restart      `json:"relay_restart"`
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

// rel123Restart is the corpus's relay-restart stage: after the generation
// script above, the relay PROCESS restarts with the app peer still gone, and
// the listed probes are re-made against the rebuilt relay.
type rel123Restart struct {
	AfterGeneration int64         `json:"after_generation"`
	AppPeer         string        `json:"app_peer"`
	Probes          []rel123Probe `json:"probes"`
}

// rel123ExpectedRestart is the restart stage's oracle: the per-probe outcome,
// what the relay read back out of durable storage to produce it, and that the
// app peer really was still absent while it did.
type rel123ExpectedRestart struct {
	Probes []struct {
		Outcome    string `json:"outcome"`
		HTTPStatus int    `json:"http_status"`
		Code       string `json:"code"`
	} `json:"probes"`
	RevokedReadBack        []string `json:"revoked_read_back_from_durable_store"`
	ScreenProgramsReadBack bool     `json:"screen_programs_read_back_from_durable_store"`
	AppPeerReachable       bool     `json:"app_peer_reachable_after_restart"`
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
	AppPeerReachableDuringFinal bool                  `json:"app_peer_reachable_during_final_two_probes"`
	GenerationsApplied          []int64               `json:"generations_applied"`
	RelayRestart                rel123ExpectedRestart `json:"relay_restart"`
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

	// A FILE-backed store, not the in-memory one every other stage opens: this
	// case restarts the relay, and a restart whose durable state never reached a
	// file is not a restart at all. The directory is this run's own and is
	// removed with it.
	dir, err := os.MkdirTemp("", "relay1-rel123-")
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("create store dir: %v", err))
		return
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "relay.db")

	store, err := enrolledStoreAt(client, feeder, dbPath)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("enroll relay: %v", err))
		return
	}
	storeOpen := true
	defer func() {
		if storeOpen {
			_ = store.Close()
		}
	}()
	relayID, ok, err := store.Identity()
	if err != nil || !ok {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("read relay identity: ok=%v err=%v", ok, err))
		return
	}

	srv, ts, err := rel123PlayerServer(relayID.CertPEM, relayID.PrivateKey, store)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("build player/1 server: %v", err))
		return
	}
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
		if err := srv.SetRevokedScreens(got.Generation, got.Revoked); err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("install generation %d's revocation view: %v", got.Generation, err), diffs...)
			return
		}
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

	// ---- the relay restarts, with the app peer still gone ----
	//
	// Everything above proves connection-down / process-UP. This stage is what
	// separates a revocation the relay REMEMBERS from one it merely happened to
	// be holding in RAM: a relay whose revoked set lives only in the running
	// process comes back after a reboot serving its persisted programs to its
	// persisted channel tokens with nothing revoked, and stays that way until a
	// pull it cannot make restates the set.
	// rel123Restarted takes ownership of the store — it closes it to restart —
	// so the outer defer must stand down BEFORE the call, not after it: a
	// failure inside would otherwise leave the handle to be closed twice or not
	// at all, depending on where it stopped.
	storeOpen = false
	if reason := rel123Restarted(client, feeder, store, dbPath, relayID, token, applied, in.RelayRestart, exp.RelayRestart, &diffs); reason != "" {
		rep.Fail(c.CaseID, contract, reason, diffs...)
		return
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
		"player-certificate issuance, the third decision REL-123 names, is not asserted: this relay issues no player certificate, so there is no behavior to observe.",
		"the restart stage re-probes only the presented-token decision, not issuance: a relay booting with no app peer has no pairing_grants at all (`pairing_grants` is a live section, not durable state), so a refused pairing attempt there would be attributable to the absent grant rather than to the revocation — an assertion that would pass whatever the relay does about `revoked`.")
}

// rel123PlayerServer builds one player/1 server over store and mounts it on a
// live HTTPS-free httptest listener — the same construction the driver uses for
// the first relay process and for the restarted one, so the restarted relay is
// assembled exactly like the original rather than like a convenient stub.
//
// The durable session tier is wired (EnablePersistence), because that is what
// cmd/waiveo-relay's own boot does and what makes a channel token minted before
// a restart resolvable after one. Without it the restart stage would find the
// token simply unknown, and its refusal would say nothing about `revoked`.
func rel123PlayerServer(certPEM []byte, priv ed25519.PrivateKey, store *identity.Store) (*playerserver.Server, *httptest.Server, error) {
	srv, err := playerserver.NewServer(certPEM, nil, playerserver.WallClockMs)
	if err != nil {
		return nil, nil, err
	}
	// The relay's own Lease-signing identity (PLY-090) — a program pull that
	// succeeds has to produce a signed Lease, not a 500.
	srv.SetSigningKey(priv)
	srv.EnablePersistence(store)
	mux := http.NewServeMux()
	srv.Register(mux)
	return srv, httptest.NewServer(apihttp.WithTraceID(mux)), nil
}

// rel123Restarted drives the corpus's relay-restart stage: it closes the
// durable store as a process exit would, reopens the SAME file, rebuilds the
// player/1 server from nothing, and installs the serving state that boot reads
// out of durable storage — the relay's persisted screen_programs
// (desiredstate.ServedProgram) and the persisted revocation set
// (desiredstate.ServedRevocation), which is precisely the pair
// cmd/waiveo-relay's own offline boot installs. Then it re-probes.
//
// It returns a non-empty reason when the stage could not be driven at all;
// probe divergences are appended to diffs instead. It takes ownership of store
// (it closes it to restart), so the caller must not close it again.
func rel123Restarted(
	client RelayClient,
	feeder Feeder,
	store *identity.Store,
	dbPath string,
	relayID identity.RelayIdentity,
	token string,
	applied []int64,
	in rel123Restart,
	exp rel123ExpectedRestart,
	diffs *[]report.Diff,
) string {
	// The corpus says WHEN the relay restarts. It has to be after the last
	// generation the script applied, or the stage would be probing a relay whose
	// durable row is not the one the probes above were written against.
	if len(applied) == 0 || in.AfterGeneration != applied[len(applied)-1] {
		return fmt.Sprintf("relay_restart.after_generation is %d but the script's last applied generation was %v", in.AfterGeneration, applied)
	}
	if in.AppPeer == "connected" {
		return fmt.Sprintf("relay_restart.app_peer is %q — this stage exists to probe a relay with no app peer at all", in.AppPeer)
	}

	if err := store.Close(); err != nil {
		return fmt.Sprintf("close the durable store to restart the relay: %v", err)
	}
	reopened, err := identity.Open(dbPath)
	if err != nil {
		return fmt.Sprintf("reopen the durable store after the restart: %v", err)
	}
	defer reopened.Close()

	// Prove the outage still holds rather than assume it carried over from
	// before the restart — the same discipline the generation loop applies. A
	// reopened store dials with the same persisted identity, so if the app peer
	// had come back up this pull would succeed and every probe below would be
	// an ordinary connected one wearing an offline label.
	if _, err := client.Pull(feeder.EnrollBaseURL(), reopened); err == nil {
		*diffs = append(*diffs, report.Diff{Field: "relay_restart.app_peer_reachable_after_restart", Expected: false, Actual: "a pull still succeeded — the app peer was not actually down"})
	}

	// What the restarted relay knows, and the ONLY thing it knows: whatever the
	// durable row holds. Read here rather than inferred from the wire answer
	// below, because the two can diverge — a refusal could be right for the
	// wrong reason (a token the relay simply lost), and this pins that the
	// revocation itself came back.
	revoked, err := desiredstate.ServedRevocation(reopened)
	if err != nil {
		return fmt.Sprintf("read the persisted revocation set after the restart: %v", err)
	}
	wantRevoked, err := rel123Revoked(exp.RevokedReadBack, snapshot.FixtureScreenID)
	if err != nil {
		return fmt.Sprintf("expected.relay_restart: %v", err)
	}
	if !equalStrings(revoked, wantRevoked) {
		*diffs = append(*diffs, report.Diff{Field: "relay_restart.revoked_read_back_from_durable_store", Expected: wantRevoked, Actual: revoked})
	}

	served, err := desiredstate.ServedProgram(reopened)
	if err != nil {
		return fmt.Sprintf("read the persisted screen_programs after the restart: %v", err)
	}
	// The vacuity guard: a relay that came back with NOTHING would refuse every
	// probe below for reasons having nothing to do with `revoked`.
	if gotPrograms := len(served) > 0; gotPrograms != exp.ScreenProgramsReadBack {
		*diffs = append(*diffs, report.Diff{Field: "relay_restart.screen_programs_read_back_from_durable_store", Expected: exp.ScreenProgramsReadBack, Actual: gotPrograms})
	}

	srv, ts, err := rel123PlayerServer(relayID.CertPEM, relayID.PrivateKey, reopened)
	if err != nil {
		return fmt.Sprintf("rebuild the player/1 server after the restart: %v", err)
	}
	defer ts.Close()

	// The boot install, in the order cmd/waiveo-relay installs it: the
	// revocation view first, so there is no instant at which a revoked screen
	// could pull the program the same generation carries.
	if err := srv.SetRevokedScreens(0, revoked); err != nil {
		return fmt.Sprintf("install the persisted revocation view after the restart: %v", err)
	}
	for _, sp := range served {
		srv.SetServedProgram(0, sp)
	}

	if len(in.Probes) != len(exp.Probes) {
		return fmt.Sprintf("relay_restart: %d probes declared but %d expected outcomes", len(in.Probes), len(exp.Probes))
	}
	for i, p := range in.Probes {
		want := exp.Probes[i]
		if p.Probe != "program_pull" {
			return fmt.Sprintf("relay_restart.probes[%d]: unsupported probe kind %q — a relay booting offline holds no pairing grant, so only the presented-token decision is observable here", i, p.Probe)
		}
		if token == "" {
			return fmt.Sprintf("relay_restart.probes[%d]: no channel token held across the restart", i)
		}
		status, code, err := getProgram(ts.URL, token)
		if err != nil {
			return fmt.Sprintf("relay_restart.probes[%d]: GET /player/v1/program: %v", i, err)
		}
		if status != want.HTTPStatus {
			*diffs = append(*diffs, report.Diff{Field: fmt.Sprintf("relay_restart.probes[%d] status", i), Expected: want.HTTPStatus, Actual: status})
		}
		if code != want.Code {
			*diffs = append(*diffs, report.Diff{Field: fmt.Sprintf("relay_restart.probes[%d].code", i), Expected: want.Code, Actual: code})
		}
	}

	if exp.AppPeerReachable {
		*diffs = append(*diffs, report.Diff{Field: "relay_restart.app_peer_reachable_after_restart", Expected: false, Actual: true})
	}
	return ""
}

// equalStrings reports whether two string lists match element for element.
func equalStrings(a, b []string) bool {
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
