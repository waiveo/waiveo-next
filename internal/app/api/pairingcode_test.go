package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/paircode"
	"github.com/maaxton/waiveo-next/internal/shared/tlsboot"
)

// pairingcode_test.go drives POST /screens/{screen_id}/pairing-code through
// the LIVE handler: the minted grant must actually land in the store's
// desired-state read (the section a relay pulls — never a 201 that mutated
// nothing), and the returned pairing code must decode, through the SAME
// shared codec a player uses, to exactly the connected relay's dial address,
// the minted grant's selector, and the commitment over the relay's
// trust-anchor SPKI.

const pairingRelayID = "01J8ZRELAYAAAAAAAAAAAAAAA1"

// pairingDirFixture builds a WithPairing directory advertising one connected
// relay and returns it with the relay's SPKI (for commitment assertions).
func pairingDirFixture(t *testing.T, addr string) (api.PairingRelayDirectory, []byte) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	dir := api.PairingRelayDirectory{
		ConnectedRelays: func() []api.PairingRelay {
			return []api.PairingRelay{{RelayID: pairingRelayID, AdvertisedAddress: addr}}
		},
		RelaySPKI: func(relayID string) ([]byte, bool) {
			if relayID != pairingRelayID {
				return nil, false
			}
			return spki, true
		},
	}
	return dir, spki
}

type pairingCodeResponse struct {
	GrantID               string `json:"grant_id"`
	ScreenID              string `json:"screen_id"`
	TTLSeconds            int64  `json:"ttl_seconds"`
	RedemptionMode        string `json:"redemption_mode"`
	IssuedAt              int64  `json:"issued_at"`
	ExpiresAt             int64  `json:"expires_at"`
	PairingCode           string `json:"pairing_code"`
	RelayID               string `json:"relay_id"`
	CodeUnavailableReason string `json:"code_unavailable_reason"`
}

// createScreenRow creates a screen identity row via the live handler and
// returns its server-minted id.
func createScreenRow(t *testing.T, e *testEnv, siteID string) string {
	t.Helper()
	return decodeID(t, e.createOK(t, "/api/v1/screens", mustJSON(t, screenFixture(siteID, "Lobby Screen", nil))))
}

// desiredStateOf is the store's desired-state read — the exact input the
// snapshot builder derives the `pairing_grants` section from.
func desiredStateOf(t *testing.T, e *testEnv) store.DesiredStateResult {
	t.Helper()
	ds, err := e.store.DesiredState(context.Background())
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	return ds
}

func issueCode(t *testing.T, e *testEnv, screenID string, headers map[string]string) (*http.Response, pairingCodeResponse) {
	t.Helper()
	resp, raw := e.do(t, http.MethodPost, "/api/v1/screens/"+screenID+"/pairing-code", nil, headers)
	var out pairingCodeResponse
	if len(raw) > 0 && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode response %s: %v", raw, err)
		}
	}
	return resp, out
}

// TestIssuePairingCodeMintsDeliverableGrant: the operation's 201 is backed by
// real work — the grant is in the store's desired-state read (what the next
// snapshot build carries to the relay), the generation advanced (what nudges
// live relays to re-pull), and the code decodes to the three REL-126
// components a player needs, byte-exact.
func TestIssuePairingCodeMintsDeliverableGrant(t *testing.T) {
	dir, spki := pairingDirFixture(t, "192.0.2.40:7443")
	e := newEnvWithOptions(t, api.WithPairing(dir))
	siteID := e.createNode(t, siteNode(""))
	screenID := createScreenRow(t, e, siteID)

	genBefore := e.generation(t)
	resp, out := issueCode(t, e, screenID, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue: %d, want 201", resp.StatusCode)
	}

	if out.ScreenID != screenID {
		t.Errorf("screen_id = %q, want %q", out.ScreenID, screenID)
	}
	if out.GrantID == "" || out.RedemptionMode != "one-time" || out.TTLSeconds <= 0 {
		t.Errorf("grant shape = %+v, want a one-time grant with a positive ttl", out)
	}
	if out.ExpiresAt != out.IssuedAt+out.TTLSeconds*1000 {
		t.Errorf("expires_at = %d, want issued_at + ttl (%d)", out.ExpiresAt, out.IssuedAt+out.TTLSeconds*1000)
	}
	if out.RelayID != pairingRelayID {
		t.Errorf("relay_id = %q, want %q", out.RelayID, pairingRelayID)
	}
	if out.CodeUnavailableReason != "" {
		t.Errorf("code_unavailable_reason = %q alongside a code", out.CodeUnavailableReason)
	}

	// The code is the shared codec's own packing of (relay dial address,
	// grant_selector, commitment-over-SPKI) — decoded here exactly as a
	// player decodes it (PLY-024/PLY-053).
	host, port, selector, commitment, err := paircode.Decode(out.PairingCode)
	if err != nil {
		t.Fatalf("paircode.Decode(%q): %v", out.PairingCode, err)
	}
	if host != "192.0.2.40" || port != 7443 {
		t.Errorf("code dials %s:%d, want 192.0.2.40:7443", host, port)
	}
	if selector != out.GrantID {
		t.Errorf("grant_selector = %q, want the minted grant_id %q", selector, out.GrantID)
	}
	if want := tlsboot.Commitment(spki); !bytes.Equal(commitment, want) {
		t.Errorf("commitment = %x, want %x (over the relay's trust-anchor SPKI, PLY-052)", commitment, want)
	}

	// The real path: the grant rides the store's desired-state read — the
	// exact input the snapshot builder derives `pairing_grants` from — and
	// the generation advanced so live relays get nudged (REL-057).
	if gen := e.generation(t); gen != genBefore+1 {
		t.Errorf("generation %d -> %d, want exactly one advance", genBefore, gen)
	}
	ds := desiredStateOf(t, e)
	found := false
	for _, g := range ds.PairingGrants {
		if g.GrantID == out.GrantID {
			found = true
			if g.ScreenID != screenID {
				t.Errorf("stored grant is bound to %q, want %q (REL-121a)", g.ScreenID, screenID)
			}
		}
	}
	if !found {
		t.Fatalf("the minted grant %q is not in the desired-state read — a 201 that delivered nothing", out.GrantID)
	}
}

// TestIssuePairingCodeWithNoRelayStillMintsTheGrant: no connected relay means
// no code can be formed — but the grant IS minted and delivered (it will ride
// the snapshot the moment a relay connects and pulls), and the response says
// exactly why the code is absent instead of failing or pretending.
func TestIssuePairingCodeWithNoRelayStillMintsTheGrant(t *testing.T) {
	dir := api.PairingRelayDirectory{
		ConnectedRelays: func() []api.PairingRelay { return nil },
		RelaySPKI:       func(string) ([]byte, bool) { return nil, false },
	}
	e := newEnvWithOptions(t, api.WithPairing(dir))
	siteID := e.createNode(t, siteNode(""))
	screenID := createScreenRow(t, e, siteID)

	resp, out := issueCode(t, e, screenID, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue: %d, want 201", resp.StatusCode)
	}
	if out.PairingCode != "" || out.RelayID != "" {
		t.Errorf("a code was formed with no relay connected: %+v", out)
	}
	if out.CodeUnavailableReason == "" {
		t.Error("code_unavailable_reason is empty — the absent code must be explained")
	}
	ds := desiredStateOf(t, e)
	if len(ds.PairingGrants) != 1 || ds.PairingGrants[0].GrantID != out.GrantID {
		t.Fatalf("desired state carries %+v, want exactly the minted grant", ds.PairingGrants)
	}
}

// TestIssuePairingCodeUnknownScreen404sAndMintsNothing: the binding must name
// a real screen row — an unknown id is refused and NO grant is persisted.
func TestIssuePairingCodeUnknownScreen404sAndMintsNothing(t *testing.T) {
	dir, _ := pairingDirFixture(t, "192.0.2.40:7443")
	e := newEnvWithOptions(t, api.WithPairing(dir))
	_ = e.createNode(t, siteNode(""))

	resp, _ := issueCode(t, e, "01J8Z9N0SUCHSCREENR0WXXXXX", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("issue against an unknown screen: %d, want 404", resp.StatusCode)
	}
	if ds := desiredStateOf(t, e); len(ds.PairingGrants) != 0 {
		t.Fatalf("a refused issue persisted %+v", ds.PairingGrants)
	}
}

// TestIssuePairingCodeIdempotencyKeyReplays: a keyed retry replays the FIRST
// mint verbatim (same grant_id, same code) rather than minting a second
// grant — the API-052 replay discipline on a non-create action route.
func TestIssuePairingCodeIdempotencyKeyReplays(t *testing.T) {
	dir, _ := pairingDirFixture(t, "192.0.2.40:7443")
	e := newEnvWithOptions(t, api.WithPairing(dir))
	siteID := e.createNode(t, siteNode(""))
	screenID := createScreenRow(t, e, siteID)

	key := map[string]string{"Idempotency-Key": "pairing-key-1"}
	resp1, out1 := issueCode(t, e, screenID, key)
	resp2, out2 := issueCode(t, e, screenID, key)
	if resp1.StatusCode != http.StatusCreated || resp2.StatusCode != http.StatusCreated {
		t.Fatalf("issue twice: %d, %d, want 201 both", resp1.StatusCode, resp2.StatusCode)
	}
	if out1.GrantID != out2.GrantID || out1.PairingCode != out2.PairingCode {
		t.Errorf("keyed retry minted a DIFFERENT grant: %q vs %q", out1.GrantID, out2.GrantID)
	}
	if ds := desiredStateOf(t, e); len(ds.PairingGrants) != 1 {
		t.Fatalf("desired state carries %d grants after a keyed replay, want 1", len(ds.PairingGrants))
	}
}
