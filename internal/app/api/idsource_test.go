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
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clock := func() int64 { return fixedNowMs }
	idem := apihttp.NewIdempotencyStore(clock, 0)
	content := origin.New()

	minted := []string{
		"01J8Z9SEAM0FIRSTMINTEDID01",
		"01J8Z9SEAM0SECONDMINTED02",
	}
	next := 0
	newID := func() string {
		id := minted[next]
		next++
		return id
	}

	ts := httptest.NewServer(api.New(st, idem, clock, newID, content, testContentBase))
	t.Cleanup(ts.Close)
	e := &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase}

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

	// Seed a compile-clean edge automation under a CLIENT-supplied id (the
	// create-minting path is not under test a second time here) so bulk-enable
	// has a real matched target, then prove the Job mints its own id from the
	// SAME injected source, in sequence after the create above.
	autoID := "01J8Z9SEAM0AUTOMATIONFIXTU"
	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations", edgeAutomationBody(autoID, autoScopeNode, map[string]string{"env": "prod"}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create automation status = %d, body %s", resp.StatusCode, raw)
	}

	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable",
		mustJSON(t, map[string]any{"selector": "env=prod", "enabled": true}), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("bulk-enable status = %d, body %s", resp.StatusCode, raw)
	}
	if got := decodeID(t, raw); got != minted[1] {
		t.Fatalf("Job id = %q, want the SECOND injected id %q", got, minted[1])
	}
}
