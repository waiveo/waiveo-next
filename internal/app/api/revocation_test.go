package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
)

// The revocation operation's four load-bearing properties (API-141-144).
//
// The one that matters most is the last: a revocation recorded through the API
// must REACH the enforcement half. Both halves of this control existed and were
// tested in isolation for weeks while being connected by nothing, so a test that
// stops at "the row was written" would reproduce exactly the gap this closes.

type revocationBody struct {
	SubjectKind         string `json:"subject_kind"`
	SubjectID           string `json:"subject_id"`
	ScreensAffected     int    `json:"screens_affected"`
	Revoked             bool   `json:"revoked"`
	AlreadyRevoked      bool   `json:"already_revoked"`
	CertificatesRevoked int    `json:"certificates_revoked"`
}

func postRevocation(t *testing.T, e *testEnv, kind, id string, confirm bool) (*http.Response, revocationBody) {
	t.Helper()
	body := map[string]any{"subject_kind": kind, "subject_id": id}
	if confirm {
		body["confirm"] = true
	}
	res, raw := e.do(t, http.MethodPost, "/api/v1/revocations", mustJSON(t, body), nil)
	var out revocationBody
	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode: %v (body %s)", err, raw)
		}
	}
	return res, out
}

// TestAnUnconfirmedRevocationChangesNothingAndReportsTheRadius is API-143.
//
// The default matters: a caller that FORGETS confirm learns the blast radius
// rather than causing it. A confirmation that defaulted to true would make the
// safe path the one an operator has to remember.
func TestAnUnconfirmedRevocationChangesNothingAndReportsTheRadius(t *testing.T) {
	e := newEnv(t)
	site := e.createNode(t, siteNode(""))
	screenID := decodeID(t, e.createOK(t, "/api/v1/screens", mustJSON(t, screenFixture(site, "Lobby", nil))))

	res, got := postRevocation(t, e, "screen", screenID, false)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unconfirmed revocation = %d, want 200", res.StatusCode)
	}
	if got.Revoked {
		t.Error("an UNCONFIRMED revocation reported itself as performed")
	}
	if got.ScreensAffected != 1 {
		t.Errorf("screens_affected = %d, want 1 — the radius is what makes the confirmation informed", got.ScreensAffected)
	}

	// Nothing changed: a second unconfirmed call still says not-already-revoked.
	_, again := postRevocation(t, e, "screen", screenID, false)
	if again.AlreadyRevoked {
		t.Error("an unconfirmed revocation recorded the revocation anyway")
	}
}

// TestAConfirmedRevocationIsRecordedAndIsTerminal covers API-142: recording it
// twice is a no-op, and the second call says so rather than pretending it acted.
func TestAConfirmedRevocationIsRecordedAndIsTerminal(t *testing.T) {
	e := newEnv(t)
	site := e.createNode(t, siteNode(""))
	screenID := decodeID(t, e.createOK(t, "/api/v1/screens", mustJSON(t, screenFixture(site, "Lobby", nil))))

	res, got := postRevocation(t, e, "screen", screenID, true)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("confirmed revocation = %d, want 200", res.StatusCode)
	}
	if !got.Revoked || got.AlreadyRevoked {
		t.Fatalf("first confirmed revocation: revoked=%v already=%v, want true/false", got.Revoked, got.AlreadyRevoked)
	}

	_, second := postRevocation(t, e, "screen", screenID, true)
	if !second.AlreadyRevoked {
		t.Error("a second revocation of the same subject did not report already_revoked — a caller cannot tell " +
			"'I did that' from 'that was already true', and revocation is terminal (API-142)")
	}
}

// TestRevokingAScreenReachesTheSignedSnapshot is the property the whole
// operation exists for.
//
// The enforcement half consults `revocation_and_site.revoked` on every channel
// token, including while disconnected (relay/1 REL-066, player/1 PLY-072). It
// was built, tested, and fed by nothing. A test that asserted only that a row
// was written would leave that exact gap in place.
func TestRevokingAScreenReachesTheSignedSnapshot(t *testing.T) {
	e := newEnv(t)
	site := e.createNode(t, siteNode(""))
	keep := decodeID(t, e.createOK(t, "/api/v1/screens", mustJSON(t, screenFixture(site, "Kept", nil))))
	drop := decodeID(t, e.createOK(t, "/api/v1/screens", mustJSON(t, screenFixture(site, "Revoked", nil))))

	before := revokedInSnapshot(t, e)
	if len(before) != 0 {
		t.Fatalf("a fresh workspace already carries revoked ids %v", before)
	}

	if _, got := postRevocation(t, e, "screen", drop, true); !got.Revoked {
		t.Fatal("the revocation was not recorded")
	}

	after := revokedInSnapshot(t, e)
	if len(after) != 1 || after[0] != drop {
		t.Fatalf("the signed snapshot carries revoked=%v, want exactly [%s] — a revocation the snapshot does not "+
			"carry is one the relay never learns about, which is the gap this operation closes", after, drop)
	}
	for _, id := range after {
		if id == keep {
			t.Error("a screen nobody revoked appears in the revoked list")
		}
	}
}

// revokedInSnapshot derives desired state and reads the section a relay enforces
// from — through snapshot.BuildFromStore rather than the store, so what is
// asserted is what actually rides the signed bytes.
func revokedInSnapshot(t *testing.T, e *testEnv) []string {
	t.Helper()
	ds, err := e.store.DesiredState(t.Context())
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	snap, _, err := snapshot.BuildFromStore(ds, contenturl.Signer{Base: testContentBase}, mustSigning(t), fixedNowMs)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}
	return snap.Sections.RevocationAndSite.Revoked
}

// TestAnUnknownSubjectKindIsRefused: the closed set is enforced at the request,
// so a typo becomes a 422 rather than a row nothing will ever read.
func TestAnUnknownSubjectKindIsRefused(t *testing.T) {
	e := newEnv(t)
	e.createNode(t, siteNode(""))
	for _, kind := range []string{"", "device", "Screen", "relays"} {
		res, _ := postRevocation(t, e, kind, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", true)
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("subject_kind %q = %d, want 422", kind, res.StatusCode)
		}
	}
}

// TestRevocationRequiresOwnerAtRoot is SEC-003f.
//
// This test exists because its absence let a mutation pass: deleting the
// authorization check entirely left every test here green. Stating a floor in a
// contract and driving only the happy path leaves the floor enforced by nothing
// an implementation change would disturb — which is the same enforcement-first,
// authoring-never shape this whole operation was filed to close, one level up.
//
// `admin` is the interesting refusal, not `viewer`: SEC-003f's whole content is
// that admin is NOT enough, because the blast radius exceeds SEC-020's
// credential revocation. A test that only refused a viewer would pass against an
// implementation whose floor was admin.
func TestRevocationRequiresOwnerAtRoot(t *testing.T) {
	e := newEnv(t)
	site := e.createNode(t, siteNode(""))
	screenID := decodeID(t, e.createOK(t, "/api/v1/screens", mustJSON(t, screenFixture(site, "Lobby", nil))))
	orgID, _, err := e.store.WorkspaceRoot(t.Context())
	if err != nil {
		t.Fatalf("WorkspaceRoot: %v", err)
	}

	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleOperator, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			who, err := e.auth.AddPrincipal(authtest.Config{Role: role, ScopeNode: orgID})
			if err != nil {
				t.Fatalf("AddPrincipal(%s): %v", role, err)
			}
			body := mustJSON(t, map[string]any{"subject_kind": "screen", "subject_id": screenID, "confirm": true})
			res, raw := e.doAsPrincipal(t, who, http.MethodPost, "/api/v1/revocations", body)
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("%s revoked a screen: %d (body %s) — the floor is owner-at-root because the act takes a "+
					"site dark and is terminal (SEC-003f)", role, res.StatusCode, raw)
			}
		})
	}

	// The control: the owner still can. Without it, a handler that refused
	// everyone would satisfy every assertion above.
	if _, got := postRevocation(t, e, "screen", screenID, true); !got.Revoked {
		t.Error("the owner could not revoke — the refusals above would then prove nothing")
	}
}

// TestRevokingARelayReachesTheEnrollmentAuthority is the relay half's
// equivalent of the snapshot test above: the record is not the revocation.
//
// A relay's certificates live in the enrollment authority, and the
// connection-time check reads that registry (relay/1 REL-016/041). An operation
// that wrote a row and reached nothing would be a second inert record beside an
// inert enforcement — which is the shape this whole issue reports.
func TestRevokingARelayReachesTheEnrollmentAuthority(t *testing.T) {
	var revokedFor []string
	e := newEnvWithOptions(t, api.WithRelayCertRevoker(func(relayID string) int {
		revokedFor = append(revokedFor, relayID)
		return 2 // a re-enrolled relay holding two issuances
	}))
	e.createNode(t, siteNode(""))

	const relayID = "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0"

	// The unconfirmed query must NOT reach the authority: it changes nothing.
	if _, got := postRevocation(t, e, "relay", relayID, false); got.Revoked {
		t.Fatal("an unconfirmed relay revocation reported itself performed")
	}
	if len(revokedFor) != 0 {
		t.Fatalf("an UNCONFIRMED revocation reached the enrollment authority for %v — the radius query must change "+
			"nothing, and revoking a certificate is a change", revokedFor)
	}

	_, got := postRevocation(t, e, "relay", relayID, true)
	if !got.Revoked {
		t.Fatal("the confirmed relay revocation was not recorded")
	}
	if len(revokedFor) != 1 || revokedFor[0] != relayID {
		t.Fatalf("the enrollment authority saw %v, want exactly [%s] — a revocation that reaches only the store "+
			"leaves the relay's own certificates valid, and it reconnects", revokedFor, relayID)
	}
	if got.CertificatesRevoked != 2 {
		t.Errorf("certificates_revoked = %d, want 2 — the count is how an operator sees that the act reached "+
			"something rather than silently reaching nothing", got.CertificatesRevoked)
	}
}

// TestRevokingARelayWithNoRevokerStillRecordsTheDecision pins the documented
// degraded answer: the record stands, and the zero count is what makes the gap
// visible rather than silent.
func TestRevokingARelayWithNoRevokerStillRecordsTheDecision(t *testing.T) {
	e := newEnv(t)
	e.createNode(t, siteNode(""))
	_, got := postRevocation(t, e, "relay", "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0", true)
	if !got.Revoked {
		t.Fatal("the revocation was not recorded")
	}
	if got.CertificatesRevoked != 0 {
		t.Errorf("certificates_revoked = %d with no revoker wired, want 0", got.CertificatesRevoked)
	}
}
