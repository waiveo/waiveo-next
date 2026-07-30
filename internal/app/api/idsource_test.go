package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// TestInjectedIDSourceMintsBothCreateAndJobIDs proves the api server's newID
// seam (api.go, mirroring the already-injected nowMs clock) is genuinely
// upstream of BOTH id-minting paths that used to call the package-level
// ulid.New() independently: a plain resource create (api.go's ensureID) and a
// bulk-enable Job (automations.go's bulkEnableExec). A fixed, deterministic
// newID sequence lets both outcomes be asserted exactly, proving neither path
// mints its own id from a generator of its own anymore.
func TestInjectedIDSourceMintsBothCreateAndJobIDs(t *testing.T) {
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clock := func() int64 { return fixedNowMs }
	idem := apihttp.NewIdempotencyStore(clock, 0)
	content := origin.New()

	minted := []string{
		"01J8Z9SEAM0F1RSTM1NTED1D01",
		"01J8Z9SEAM0AVT0F1XTVREM1D0",
		"01J8Z9SEAM0SEC0NDM1NTED020",
	}
	next := 0
	newID := func() string {
		id := minted[next]
		next++
		return id
	}

	fixture := newAuthFixture(t)
	jobs := api.NewJobRunner()
	ts := httptest.NewServer(api.New(st, idem, clock, newID, content, testContentBase, fixture.Auth,
		api.WithJobRunner(jobs)))
	t.Cleanup(ts.Close)
	e := &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture, jobs: jobs}

	// The org root the site below hangs off, written DIRECTLY THROUGH THE STORE
	// under the id boundaryOrgID names. Through the api it would consume an id
	// from the injected sequence this whole test is counting, and it could not
	// carry that id at all (a resource's own id is exclusively server-assigned,
	// rejectClientSuppliedID) — the store has no such rule, which is what lets
	// the constant be the real row. It has to be a real row: the store holds the
	// FULL tree, where an unresolvable parent_id is a DAT-002 violation.
	seedOrgRootThroughStore(t, st)

	// A create body with NO id: ensureID must mint from the injected source,
	// never a package-level generator of its own.
	body := map[string]any{
		"kind":      "site",
		"name":      "Seam Test Site",
		"parent_id": boundaryOrgID,
		"tz":        siteTZ,
		"lat":       siteLat,
		"long":      siteLong,
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, body), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", resp.StatusCode, raw)
	}
	if got := decodeID(t, raw); got != minted[0] {
		t.Fatalf("created resource id = %q, want the FIRST injected id %q", got, minted[0])
	}

	// Seed a compile-clean edge automation with no client-supplied id (id is
	// exclusively server-assigned, rejectClientSuppliedID) so bulk-enable has a
	// real matched target — its create consumes the SECOND injected id — then
	// prove the Job mints its own id from the SAME injected source, THIRD in
	// sequence after the two creates above.
	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations", edgeAutomationBody("", autoScopeNode, map[string]string{"env": "prod"}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create automation status = %d, body %s", resp.StatusCode, raw)
	}
	if got := decodeID(t, raw); got != minted[1] {
		t.Fatalf("created automation id = %q, want the SECOND injected id %q", got, minted[1])
	}

	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable",
		mustJSON(t, map[string]any{"selector": "env=prod", "enabled": true}), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("bulk-enable status = %d, body %s", resp.StatusCode, raw)
	}
	if got := decodeID(t, raw); got != minted[2] {
		t.Fatalf("Job id = %q, want the THIRD injected id %q", got, minted[2])
	}
}
