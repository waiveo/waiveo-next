package securitymodel1

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// grantErrorCode maps a redemption failure onto the security-model/1 error code
// its own Error taxonomy names for it (SEC-035). It is deliberately the same
// three-way mapping internal/app/auth/handlers.go's writeGrantProblem makes for
// the HTTP surface, expressed here for the cases that drive the store directly —
// and it returns "" for a nil error so a caller reads "no error" as the taxonomy
// does, as the absence of a code.
func grantErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, auth.ErrGrantExpired):
		return "GRANT_EXPIRED"
	case errors.Is(err, auth.ErrGrantAlreadyRedeemed):
		return "GRANT_ALREADY_REDEEMED"
	case errors.Is(err, auth.ErrGrantPurposeMismatch):
		return "GRANT_PURPOSE_MISMATCH"
	default:
		return "UNEXPECTED:" + err.Error()
	}
}

// driveGrantExpired drives SEC-035-invalid-grant-expired-rejected against the
// LIVE grant store: a `recovery`-purpose grant is minted at the case's own
// `issued_at` with the case's own `ttl`, the INJECTED clock is then moved to the
// case's own `redemption_attempted_at`, and the redemption is attempted.
//
// The clock is a variable this function reassigns — never a sleep. The contract
// asks for exactly that ("grant `ttl` expiry ... exercised against an injectable
// clock in a driver harness, not wall-clock sleeps in a static corpus").
//
// `session_issued` is observed TWO ways, and both must agree that nothing was
// issued: the consume callback (which performs whatever the grant authorizes,
// and must never run for an expired code) sets a flag, and the store is then
// asked whether any session row exists for the target principal at all. A
// redemption that somehow issued a session without running consume would be
// caught by the second; one that ran consume without persisting would be caught
// by the first.
func driveGrantExpired(rep *report.Report, c corpus.Case) {
	k := newCheck(c)
	ctx := context.Background()

	purpose, okPurpose := inputString(c, "grant.purpose")
	mode, okMode := inputString(c, "grant.redemption_mode")
	issuedAt, okIssued := inputTimeMs(c, "grant.issued_at")
	ttlMs, okTTL := inputDurationMs(c, "grant.ttl")
	attemptAt, okAttempt := inputTimeMs(c, "redemption_attempted_at")
	if !okPurpose || !okMode || !okIssued || !okTTL || !okAttempt {
		k.fail(rep, "the frozen input block is missing a field this case is driven from (purpose=%v mode=%v issued_at=%v ttl=%v attempted_at=%v)",
			okPurpose, okMode, okIssued, okTTL, okAttempt)
		return
	}

	now := issuedAt
	st, err := auth.Open(":memory:", func() int64 { return now }, deterministicIDs())
	if err != nil {
		k.fail(rep, "open auth store: %v", err)
		return
	}
	defer st.Close()

	target, err := st.CreatePrincipal(ctx, auth.KindUser, "grant-expiry-target")
	if err != nil {
		k.fail(rep, "create target principal: %v", err)
		return
	}
	minted, err := st.MintGrant(ctx, auth.MintGrantOptions{
		Purpose:                purpose,
		ResultingPrincipalKind: auth.KindUser,
		TTLMs:                  ttlMs,
		RedemptionMode:         mode,
	})
	if err != nil {
		k.fail(rep, "mint the %s grant: %v", purpose, err)
		return
	}

	// The clock moves forward past the grant's ttl. No sleep, no wall clock.
	now = attemptAt

	consumeRan := false
	_, redeemErr := st.RedeemGrant(ctx, minted.Code, purpose, func(tx *sql.Tx, g auth.GrantRow) error {
		// What a recovery redemption authorizes: a session for the target. It
		// must never run for a code whose ttl has elapsed.
		consumeRan = true
		return nil
	})

	sessionIDs, err := st.ListSessionIDs(ctx, target.PrincipalID)
	if err != nil {
		k.fail(rep, "list target sessions: %v", err)
		return
	}

	k.boolAt("redeemed", redeemErr == nil)
	k.boolAt("session_issued", consumeRan || len(sessionIDs) > 0)
	k.stringAt("error.code", grantErrorCode(redeemErr))
	k.finish(rep)
}

// driveFirstBootClaimOutsideWindow drives
// SEC-120-invalid-first-boot-claim-outside-window against the LIVE, HTTP-mounted
// claim route — internal/app/api's own mux, the same handler a browser reaches.
//
// The case's premise is a box whose one-time setup grant has ALREADY been
// redeemed ("box_claim_state": "claimed"), so the driver produces that state the
// only way the shipped code can: it runs the real first-boot bootstrap
// (auth.EnsureClaimWindow, the installer's stand-in), claims the box once
// through the real route, and then presents the SAME code a second time.
//
// Replaying the original code — rather than inventing a wrong one — is what the
// case's own expectation forces. Its `presented_code` is written
// "WRONG-OR-REPLAYED-CODE" and its expected error is GRANT_ALREADY_REDEEMED; a
// merely wrong code resolves to no grant at all and is refused UNAUTHENTICATED
// (which is correct behavior and a different requirement). The replay is the leg
// that reaches the code the case pins.
func driveFirstBootClaimOutsideWindow(rep *report.Report, c corpus.Case) {
	k := newCheck(c)
	ctx := context.Background()

	endpoint, _ := inputString(c, "claim_attempt.endpoint")
	if endpoint != "" && endpoint != claimRoute {
		// A real, if minor, divergence between the frozen corpus and the shipped
		// surface, surfaced rather than quietly normalized: the case names
		// /api/v1/setup/claim, the app mounts the claim handler at
		// /api/v1/auth/setup (internal/app/api/api.go). The expected block
		// asserts nothing about the path, so the case is driven against the route
		// that actually exists.
		k.note("corpus input names endpoint %q; the shipped mux mounts the first-boot claim at %q — driven against the shipped route", endpoint, claimRoute)
	}

	dir, err := os.MkdirTemp("", "secmodel1-claim-")
	if err != nil {
		k.fail(rep, "scratch dir: %v", err)
		return
	}
	defer os.RemoveAll(dir)

	h, err := newClaimHarness(dir)
	if err != nil {
		k.fail(rep, "build the claim harness: %v", err)
		return
	}
	defer h.close()

	// The installer's stand-in: mint the one-time setup grant and persist its
	// code (SEC-120's "auto-generate ... and present it").
	boot, err := auth.EnsureClaimWindow(ctx, h.authStore, dir, auth.RootScopeNode, nil)
	if err != nil {
		k.fail(rep, "EnsureClaimWindow: %v", err)
		return
	}
	if boot.Claimed || boot.Code == "" {
		k.fail(rep, "the fixture box reports claimed=%v code-present=%v before any claim ran", boot.Claimed, boot.Code != "")
		return
	}

	// Leg 1: the legitimate first claim. It must succeed, or the case's premise
	// (a box already claimed) was never established and leg 2 would prove
	// nothing.
	first := h.claim(boot.Code, "owner@example.invalid", "first-owner-passphrase")
	if first.status != http.StatusCreated {
		k.fail(rep, "the first claim was refused (%d %s) — the case's premise (an already-claimed box) could not be established", first.status, first.raw)
		return
	}
	principalsAfterFirst, err := h.authStore.CountPrincipals(ctx)
	if err != nil {
		k.fail(rep, "count principals: %v", err)
		return
	}

	// Leg 2: the second claim attempt, replaying the same one-time code against
	// the same live route.
	second := h.claim(boot.Code, "usurper@example.invalid", "second-owner-passphrase")

	principalsAfterSecond, err := h.authStore.CountPrincipals(ctx)
	if err != nil {
		k.fail(rep, "count principals after the replay: %v", err)
		return
	}
	owners, err := h.authStore.CountOwnerBindings(ctx)
	if err != nil {
		k.fail(rep, "count owner bindings: %v", err)
		return
	}

	k.boolAt("claimed", second.status == http.StatusCreated)
	k.boolAt("new_owner_principal_created", principalsAfterSecond > principalsAfterFirst || owners > 1)
	k.stringAt("error.code", problemCode(second))
	k.finish(rep)
}

// claimRoute is where internal/app/api actually mounts the first-boot claim.
const claimRoute = "/api/v1/auth/setup"

// claimHarness mounts the SAME http.Handler production wires (api.New) over a
// real auth store on disk — on disk rather than in memory because
// EnsureClaimWindow persists the setup code into the same directory, and a
// fixture that split those two would not be the deployment shape SEC-120
// describes.
type claimHarness struct {
	authStore *auth.Store
	appStore  *store.Store
	handler   http.Handler
}

func newClaimHarness(dir string) (*claimHarness, error) {
	fixedNow := int64(1752537600000) // 2026-07-15T00:00:00Z, the instant the frozen corpora pin to
	clock := func() int64 { return fixedNow }
	ids := deterministicIDs()

	authStore, err := auth.Open(dir+"/auth.db", clock, ids)
	if err != nil {
		return nil, err
	}
	appStore, err := store.Open(":memory:")
	if err != nil {
		_ = authStore.Close()
		return nil, err
	}
	authn := auth.NewAuthenticator(authStore, nil, auth.NewDefaultLockout(), auth.NewRevocations())
	handler := api.New(appStore, apihttp.NewIdempotencyStore(clock, 0), clock, ids,
		origin.New(), "https://origin.example", authn, api.WithJobRunner(api.NewJobRunner()))
	return &claimHarness{authStore: authStore, appStore: appStore, handler: handler}, nil
}

func (h *claimHarness) close() {
	_ = h.authStore.Close()
	_ = h.appStore.Close()
}

// httpResult is one request's decoded outcome.
type httpResult struct {
	status int
	body   map[string]any
	raw    []byte
}

// claim drives one first-boot claim through the live mux.
func (h *claimHarness) claim(code, identifier, password string) httpResult {
	payload, _ := json.Marshal(map[string]string{"code": code, "identifier": identifier, "password": password})
	req := httptest.NewRequest(http.MethodPost, claimRoute, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return httpResult{status: rec.Code, body: decoded, raw: rec.Body.Bytes()}
}

// problemCode reads api/1's Problem `code` member out of a refusal body, or ""
// from a success.
func problemCode(r httpResult) string {
	if r.status < 400 {
		return ""
	}
	code, _ := r.body["code"].(string)
	return code
}
