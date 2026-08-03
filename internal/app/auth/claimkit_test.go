package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/emergencykit"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// ARC-110/113: a first-boot claim issues the emergency kit and returns it ONCE.
//
// The property that makes ARC-113 true of the API — rather than only of whatever
// renders it — is that there is no second way to get the passphrase. These drive
// that: the kit arrives in the claim response, the stored record cannot yield it
// back, and a claim on a server with no kit configured simply carries none.

// newClaimableHandlers stands up an UNCLAIMED workspace with a live setup
// grant, which is what the claim path needs and what no other harness here
// builds — the existing ones seed an owner, and a claim against a seeded owner
// exercises a different branch.
func newClaimableHandlers(t *testing.T) (*Handlers, string) {
	t.Helper()
	st, clock := newTestStore(t)
	minted, err := st.MintGrant(t.Context(), MintGrantOptions{
		Purpose:                PurposeSetup,
		ResultingPrincipalKind: KindUser,
		Role:                   RoleOwner,
		ScopeNode:              RootScopeNode,
		TTLMs:                  600_000,
		RedemptionMode:         RedemptionOneTime,
		IssuedVia:              IssuedViaConsole,
	})
	if err != nil {
		t.Fatalf("MintGrant: %v", err)
	}
	sink := &recordingSink{}
	revocations := NewRevocations()
	st.OnRevoke(revocations.Revoked)
	auditor := NewAuditor(sink, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", clock.now, ulid.New, nil)
	a := NewAuthenticator(st, auditor, NewLockout(3, 1000, 60_000), revocations)
	return NewHandlers(a, nil, RootScopeNode), minted.Code
}

// claimKitEnv stands up a claimable server with a kit directory.
func claimKitEnv(t *testing.T) (*Handlers, string, string) {
	t.Helper()
	kitDir := t.TempDir()
	h, code := newClaimableHandlers(t)
	return h.WithEmergencyKit(kitDir, "01WORKSPACEKEYID0000000000", func() string { return "01KITID000000000000000000" }), code, kitDir
}

func postClaim(t *testing.T, h *Handlers, code string) (*http.Response, []byte) {
	t.Helper()
	body := `{"code":"` + code + `","identifier":"owner@example.test","password":"a-sufficiently-long-passphrase"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Claim(rec, req)
	res := rec.Result()
	return res, rec.Body.Bytes()
}

func TestAClaimIssuesTheEmergencyKitAndReturnsItOnce(t *testing.T) {
	h, code, kitDir := claimKitEnv(t)

	res, raw := postClaim(t, h, code)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("claim = %d; body %s", res.StatusCode, raw)
	}
	var got struct {
		EmergencyKit *struct {
			KitID              string `json:"kit_id"`
			WorkspaceID        string `json:"workspace_id"`
			RecoveryPassphrase string `json:"recovery_passphrase"`
			Instructions       string `json:"instructions"`
		} `json:"emergency_kit"`
		EmergencyKitError string `json:"emergency_kit_error"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, raw)
	}
	if got.EmergencyKitError != "" {
		t.Fatalf("claim reported a kit failure: %s", got.EmergencyKitError)
	}
	if got.EmergencyKit == nil {
		t.Fatal("a claim on a kit-configured server returned no emergency kit — ARC-110 requires one at the point the " +
			"workspace's data key is established, and this is the only moment an operator is handed it")
	}
	// ARC-111: the kit must be sufficient on its own.
	if got.EmergencyKit.RecoveryPassphrase == "" {
		t.Error("the kit carries no recovery passphrase")
	}
	if got.EmergencyKit.WorkspaceID == "" {
		t.Error("the kit names no workspace, so an operator holding two printouts cannot tell what either recovers")
	}
	if got.EmergencyKit.Instructions == "" {
		t.Error("the kit carries no instructions — ARC-111 requires recovery to be completable from the kit alone, and " +
			"a kit that says 'see the manual' fails in exactly the situation it exists for")
	}

	// The passphrase really is the live one.
	ok, err := emergencykit.Verify(kitDir, got.EmergencyKit.RecoveryPassphrase)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Error("the passphrase handed to the operator does not verify against the stored record — the printed kit " +
			"would fail at the one moment it is needed")
	}
}

// TestThePassphraseIsNotRecoverableFromWhatTheClaimStored is the half that makes
// "returned once" mean something.
//
// A response that hands over a passphrase is only safe if nothing else can. This
// scans every byte the claim wrote to the kit directory for the passphrase in
// any form it could plausibly be stored, rather than checking the one file it
// expects — a future field that leaked it would be caught by the scan and missed
// by a check.
func TestThePassphraseIsNotRecoverableFromWhatTheClaimStored(t *testing.T) {
	h, code, kitDir := claimKitEnv(t)
	res, raw := postClaim(t, h, code)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("claim = %d; body %s", res.StatusCode, raw)
	}
	var got struct {
		EmergencyKit *struct {
			RecoveryPassphrase string `json:"recovery_passphrase"`
		} `json:"emergency_kit"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.EmergencyKit == nil {
		t.Fatalf("decode: %v (body %s)", err, raw)
	}
	pass := got.EmergencyKit.RecoveryPassphrase

	entries, err := os.ReadDir(kitDir)
	if err != nil {
		t.Fatalf("read kit dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the claim wrote nothing to the kit directory, so this scan proves nothing")
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(kitDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		body := string(b)
		for _, form := range []string{pass, strings.ToLower(pass), strings.ReplaceAll(pass, "-", ""), strings.ReplaceAll(pass, " ", "")} {
			if form == "" {
				continue
			}
			if strings.Contains(body, form) {
				t.Errorf("%s contains the recovery passphrase — it must store only a verifier the passphrase can be "+
					"CHECKED against, never anything it can be read back from, or regenerating a kit rotates a value "+
					"an attacker with disk access can simply read again (ARC-114)", e.Name())
			}
		}
	}
}

// TestAClaimWithoutAKitDirectoryCarriesNoKit pins the documented off-state, so a
// harness that stands up this server without a kit directory keeps working and
// the absence is a decision rather than an accident.
func TestAClaimWithoutAKitDirectoryCarriesNoKit(t *testing.T) {
	h, code := newClaimableHandlers(t)
	res, raw := postClaim(t, h, code)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("claim = %d; body %s", res.StatusCode, raw)
	}
	if strings.Contains(string(raw), "emergency_kit") {
		t.Errorf("a claim on a server with no kit directory carried an emergency_kit field: %s", raw)
	}
}
