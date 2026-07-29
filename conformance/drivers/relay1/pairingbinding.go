package relay1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// pairingbinding.go drives REL-121b — the relay binding that makes one-time
// pairing-grant redemption at-most-once SITE-wide rather than per relay.
//
// It is driven with TWO relays enrolled against the LIVE feeder, because a
// single-relay case cannot see this defect at all: consumption lives entirely
// in the redeeming relay's own store, `pairing_grants` rides ONE signed
// snapshot to every relay of the site (REL-067), and REL-122 makes each of them
// able to redeem for the grant's whole ttl with no app peer reachable. So the
// two identities the case needs are real enrollment-issued ones (REL-014), each
// relay's certificate carrying its own `relay_id` as the subject CommonName —
// exactly the value the app peer authenticates it by (REL-041/150) and the
// value REL-121b's comparison is against.
//
// The redemption itself goes through the committed player/1 surface
// (internal/relay/playerserver mounted on its real routes), never a
// re-implementation of the check.

// rel121bInput is the corpus input: the relay labels, the pairing_grants
// section BOTH relays apply, and the ordered redemption attempts.
type rel121bInput struct {
	Relays  []string            `json:"relays"`
	Grants  []rel121bGrant      `json:"snapshot_pairing_grants"`
	Attempt []rel121bAttemptDef `json:"attempts"`
}

// rel121bGrant is one pairing_grants entry as the corpus freezes it. relay_id
// is a `$relays[N]` reference and issued_at a `now`/`expired` marker: both are
// resolved against this run's own enrolled identities and clock, because a
// frozen relay_id could never match a runtime-issued enrollment identity and a
// frozen issued_at would make the ttl assertion depend on the wall clock.
type rel121bGrant struct {
	GrantID                string `json:"grant_id"`
	Purpose                string `json:"purpose"`
	ResultingPrincipalKind string `json:"resulting_principal_kind"`
	ScreenID               string `json:"screen_id"`
	RelayID                string `json:"relay_id"`
	TTL                    int64  `json:"ttl"`
	RedemptionMode         string `json:"redemption_mode"`
	IssuedAt               string `json:"issued_at"`
}

type rel121bAttemptDef struct {
	AtRelay       string `json:"at_relay"`
	GrantSelector string `json:"grant_selector"`
}

// rel121bExpected is the oracle: the per-attempt outcome, plus the two
// site-wide invariants the whole requirement exists for.
type rel121bExpected struct {
	Attempts []struct {
		PairingStatus      string `json:"pairing_status"`
		ScreenID           string `json:"screen_id"`
		ChannelTokenMinted bool   `json:"channel_token_minted"`
		ScreenIDDisclosed  bool   `json:"screen_id_disclosed"`
		Code               string `json:"code"`
	} `json:"attempts"`
	SiteWideRedemptionCount       int `json:"site_wide_redemption_count"`
	DistinctScreenIDsCredentialed int `json:"distinct_screen_ids_credentialed"`
}

// driveREL121b enrolls two relays, hands BOTH the identical pairing_grants
// section, and replays the corpus's attempts against their real player/1
// pairing surfaces.
func driveREL121b(rep *report.Report, client RelayClient, feeder Feeder, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-121b")
	if !ok {
		rep.Fail("REL-121b", contract, "case not found in frozen corpus")
		return
	}

	var in rel121bInput
	if err := decodeInto(&in, c.Input, "input"); err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	var exp rel121bExpected
	if err := decodeInto(&exp, c.Expected, "expected"); err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	if len(in.Relays) != 2 {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("the case declares %d relay(s); this defect is only visible with two", len(in.Relays)))
		return
	}
	if len(in.Attempt) != len(exp.Attempts) {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("%d attempts declared but %d expected outcomes", len(in.Attempt), len(exp.Attempts)))
		return
	}

	// Two REAL enrollments against the live feeder: distinct relay_ids, each
	// certified under its own certificate's CommonName (REL-014/041).
	idents := map[string]identity.RelayIdentity{}
	for _, label := range in.Relays {
		store, err := enrolledStore(client, feeder)
		if err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("enroll relay %s: %v", label, err))
			return
		}
		defer store.Close()
		id, ok, err := store.Identity()
		if err != nil || !ok {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("read relay %s identity: ok=%v err=%v", label, ok, err))
			return
		}
		idents[label] = id
	}
	if idents[in.Relays[0]].RelayID == idents[in.Relays[1]].RelayID {
		rep.Fail(c.CaseID, contract, "both enrollments returned one relay_id — this case cannot separate the two relays")
		return
	}

	grants, err := resolveREL121bGrants(in, idents)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}

	// ONE snapshot's pairing_grants, applied by BOTH relays — the arrangement
	// the defect lives in. Each relay's own player/1 surface is built from its
	// own enrollment certificate, which is where its enrolled identity comes
	// from (playerserver.NewServer).
	servers := map[string]*httptest.Server{}
	for label, id := range idents {
		srv, err := playerserver.NewServer(id.CertPEM, grants, playerserver.WallClockMs)
		if err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("build relay %s player/1 server: %v", label, err))
			return
		}
		mux := http.NewServeMux()
		srv.Register(mux)
		ts := httptest.NewServer(apihttp.WithTraceID(mux))
		defer ts.Close()
		servers[label] = ts
	}

	var diffs []report.Diff
	redemptions := 0
	credentialedScreens := map[string]bool{}

	for i, attempt := range in.Attempt {
		ts, ok := servers[attempt.AtRelay]
		if !ok {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("attempts[%d].at_relay", i), Expected: in.Relays, Actual: attempt.AtRelay})
			continue
		}
		status, body, err := postPair(ts.URL, attempt.GrantSelector)
		if err != nil {
			rep.Fail(c.CaseID, contract, fmt.Sprintf("attempts[%d]: POST /player/v1/pair: %v", i, err), diffs...)
			return
		}

		want := exp.Attempts[i]
		_, tokenMinted := body["channel_token"]
		screenID, screenDisclosed := body["screen_id"]

		if tokenMinted != want.ChannelTokenMinted {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("attempts[%d].channel_token_minted", i), Expected: want.ChannelTokenMinted, Actual: tokenMinted})
		}
		if want.Code != "" {
			if status < 400 {
				diffs = append(diffs, report.Diff{Field: fmt.Sprintf("attempts[%d] status", i), Expected: "4xx refusal", Actual: status})
			}
			// REL-121b: the SAME typed rejection an unresolvable selector
			// draws. A distinguishable code would make every relay of a site an
			// oracle for the grants held at its siblings.
			if got := jsonString(body["code"]); got != want.Code {
				diffs = append(diffs, report.Diff{Field: fmt.Sprintf("attempts[%d].code", i), Expected: want.Code, Actual: got})
			}
			if screenDisclosed != want.ScreenIDDisclosed {
				diffs = append(diffs, report.Diff{Field: fmt.Sprintf("attempts[%d].screen_id_disclosed", i), Expected: want.ScreenIDDisclosed, Actual: screenDisclosed})
			}
			continue
		}

		if status != http.StatusOK {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("attempts[%d] status", i), Expected: 200, Actual: status})
			continue
		}
		if got := jsonString(body["pairing_status"]); got != want.PairingStatus {
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("attempts[%d].pairing_status", i), Expected: want.PairingStatus, Actual: got})
		}
		if got := jsonString(screenID); got != want.ScreenID {
			// REL-121a: the redeemed result carries the grant's own screen_id,
			// which is exactly why a second relay redeeming the same grant
			// would credential the SAME screen row.
			diffs = append(diffs, report.Diff{Field: fmt.Sprintf("attempts[%d].screen_id", i), Expected: want.ScreenID, Actual: got})
		}
		redemptions++
		credentialedScreens[jsonString(screenID)] = true
	}

	// The two invariants the requirement exists for, asserted over the whole
	// run rather than per attempt.
	if redemptions != exp.SiteWideRedemptionCount {
		diffs = append(diffs, report.Diff{Field: "site_wide_redemption_count (REL-121/REL-121b)", Expected: exp.SiteWideRedemptionCount, Actual: redemptions})
	}
	if len(credentialedScreens) != exp.DistinctScreenIDsCredentialed {
		diffs = append(diffs, report.Diff{Field: "distinct_screen_ids_credentialed", Expected: exp.DistinctScreenIDsCredentialed, Actual: len(credentialedScreens)})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "site-wide one-time redemption diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"relay_id: runtime-issued by the live feeder's own enrollment (REL-010/014), so the corpus's `$relays[N]` reference is resolved to this run's identities rather than asserted byte-equal to a frozen value.",
		"issued_at: resolved from the run's own clock (`now`/`expired` markers) — a frozen instant would make the ttl assertion depend on when the corpus was written.",
		"channel_token: an opaque value player/1 gives no grammar; asserted present/absent, not byte-equal.")
}

// resolveREL121bGrants turns the corpus's frozen records into wire grants,
// resolving the `$relays[N]` binding reference and the issued_at marker.
func resolveREL121bGrants(in rel121bInput, idents map[string]identity.RelayIdentity) ([]wire.PairingGrant, error) {
	now := time.Now().UnixMilli()
	out := make([]wire.PairingGrant, 0, len(in.Grants))
	for _, g := range in.Grants {
		relayID := ""
		switch g.RelayID {
		case "$relays[0]":
			relayID = idents[in.Relays[0]].RelayID
		case "$relays[1]":
			relayID = idents[in.Relays[1]].RelayID
		case "":
			// An unbound record: REL-121's baseline shape, redeemable anywhere.
		default:
			return nil, fmt.Errorf("grant %s: relay_id %q is neither empty nor a $relays[N] reference", g.GrantID, g.RelayID)
		}
		issuedAt := now
		switch g.IssuedAt {
		case "now":
		case "expired":
			issuedAt = now - (g.TTL+60)*1000
		default:
			return nil, fmt.Errorf("grant %s: issued_at %q is neither \"now\" nor \"expired\"", g.GrantID, g.IssuedAt)
		}
		out = append(out, wire.PairingGrant{
			GrantID:                g.GrantID,
			Purpose:                g.Purpose,
			ResultingPrincipalKind: g.ResultingPrincipalKind,
			ScreenID:               g.ScreenID,
			RelayID:                relayID,
			TTL:                    g.TTL,
			RedemptionMode:         g.RedemptionMode,
			IssuedAt:               issuedAt,
		})
	}
	return out, nil
}

// postPair drives one real POST /player/v1/pair and returns its status and
// decoded body.
func postPair(baseURL, selector string) (int, map[string]json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"hardware_id":    "conformance-rel121b",
		"grant_selector": selector,
		"capabilities":   map[string]any{"content_types": []string{"image"}, "player_version": "1.0.0"},
	})
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.Post(baseURL+"/player/v1/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	var decoded map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("decode pairing response: %w", err)
	}
	return resp.StatusCode, decoded, nil
}

// jsonString unquotes a raw JSON string field, returning "" for an absent or
// non-string value.
func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
