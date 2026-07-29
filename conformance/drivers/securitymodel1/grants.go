package securitymodel1

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// grantErrorCode maps a redemption failure onto the security-model/1 error code
// its own Error taxonomy names for it (SEC-035), for the cases that drive the
// STORE directly and so never see a wire response. It returns "" for a nil error
// so a caller reads "no error" as the taxonomy does, as the absence of a code.
//
// It is a SECOND copy of the mapping internal/app/auth/handlers.go's
// writeGrantProblem makes, and that is a real limit rather than a convenience: a
// shipped handler that refused correctly and typed the refusal wrongly would
// pass every case that reads its code from here. Nothing this function returns
// therefore decides SEC-035 — driveGrantRefusalsOnTheWire does, by reading each
// code out of the response body the mounted route actually writes.
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
//
// WHAT THIS CASE ACTUALLY PROVES is SEC-031's, not SEC-120's: replaying a
// consumed code proves a `one-time` grant is single-use, which is a property of
// every purpose, and it says nothing at all about whether an UNCLAIMED box is
// first-come-first-served — a handler that admitted the first caller with no
// code whatsoever, and refused everyone after, passes this case exactly as the
// shipped one does. SEC-120's own clause is driven by
// driveUnclaimedBoxWithoutCode below, and the traceability rows are attributed
// accordingly.
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

	h, err := newClaimHarness(dir, corpusInstantMs)
	if err != nil {
		k.fail(rep, "build the claim harness: %v", err)
		return
	}
	defer h.close()

	// The installer's stand-in: mint the one-time setup grant and persist its
	// code (SEC-120's "auto-generate ... and present it").
	boot, err := auth.EnsureClaimWindow(ctx, h.authStore, dir, auth.RootScopeNode)
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

// driveUnclaimedBoxWithoutCode drives
// SEC-120a-invalid-unclaimed-box-claimed-without-the-setup-code against the same
// LIVE, HTTP-mounted claim route, and it is the case that decides SEC-120.
//
// SEC-120's own clause is about an UNCLAIMED box: "the setup endpoint MUST be
// claimable only by redeeming this grant. An installed-but-unclaimed box MUST
// NOT be first-come-first-served to whoever reaches its setup endpoint first on
// a shared network." Every leg below therefore runs while the box is still
// unclaimed and its setup grant still live and unredeemed — the exact window a
// first-come-first-served handler would give away.
//
// Three legs, and the third is what stops this case being satisfiable by a
// handler that simply refuses everything:
//
//  1. a caller presenting NO code — the pure arrival-order attack;
//  2. a caller presenting a wrong but well-formed code;
//  3. the caller who holds the code the installer actually generated, who MUST
//     still be able to claim afterwards. That leg proves the two refusals were
//     the code check firing rather than the endpoint being shut, and it proves
//     the failed attempts did not burn the one-time grant the legitimate owner
//     is holding.
func driveUnclaimedBoxWithoutCode(rep *report.Report, c corpus.Case) {
	k := newCheck(c)
	ctx := context.Background()

	if state, _ := inputString(c, "box_claim_state"); state != "unclaimed" {
		k.fail(rep, "this case is about an UNCLAIMED box; its input declares box_claim_state %q", state)
		return
	}
	if endpoint, ok := inputString(c, "endpoint"); ok && endpoint != claimRoute {
		k.note("corpus input names endpoint %q; the shipped mux mounts the first-boot claim at %q — driven against the shipped route", endpoint, claimRoute)
	}
	attempts, okAttempts := digInput(c, "claim_attempts")
	attemptList, okList := attempts.([]any)
	if !okAttempts || !okList || len(attemptList) == 0 {
		k.fail(rep, "the frozen input block carries no claim_attempts array")
		return
	}

	dir, err := os.MkdirTemp("", "secmodel1-unclaimed-")
	if err != nil {
		k.fail(rep, "scratch dir: %v", err)
		return
	}
	defer os.RemoveAll(dir)

	h, err := newClaimHarness(dir, corpusInstantMs)
	if err != nil {
		k.fail(rep, "build the claim harness: %v", err)
		return
	}
	defer h.close()

	// The installer's stand-in, run exactly as the shipped feeder runs it at
	// boot: it mints the one-time setup grant and persists the code that is the
	// only key to this endpoint.
	boot, err := auth.EnsureClaimWindow(ctx, h.authStore, dir, auth.RootScopeNode)
	if err != nil {
		k.fail(rep, "EnsureClaimWindow: %v", err)
		return
	}
	if boot.Claimed || boot.Code == "" {
		k.fail(rep, "the fixture box reports claimed=%v code-present=%v; the case's unclaimed premise was not established", boot.Claimed, boot.Code != "")
		return
	}
	if want, present := digInput(c, "setup_grant.redeemed"); present && want != false {
		k.fail(rep, "this case requires an UNREDEEMED setup grant; its input declares setup_grant.redeemed=%v", want)
		return
	}
	// The marker table every `$`-prefixed presented_code in this case resolves
	// through. A code the driver could not resolve is a driver failure, never a
	// literal that gets sent as-is.
	//
	// `$setup_grant.code_one_nibble_flipped` is the SHAPE-VALID wrong code, and
	// it is derived from the real one rather than written into the case as a
	// literal. That is the difference between testing "does the handler reject
	// obvious garbage" and "does the handler require THE code": a literal like
	// "NOT-THE-GENERATED-SETUP-CODE" is not well-formed by any definition this
	// system uses, so an implementation that admits any correctly-SHAPED code it
	// cannot resolve would sail past it and claim the box. Flipping one nibble
	// of the minted code yields a string identical in length, alphabet and case
	// that no grant can resolve to.
	markers := map[string]string{
		"$setup_grant.code":                    boot.Code,
		"$setup_grant.code_one_nibble_flipped": flipOneNibble(boot.Code),
	}

	for i := range attemptList {
		base := fmt.Sprintf("claim_attempts.%d", i)
		code, okCode := inputString(c, base+".presented_code")
		identifier, okID := inputString(c, base+".identifier")
		password, okPW := inputString(c, base+".password")
		if !okCode || !okID || !okPW {
			k.fail(rep, "%s is missing a field this case is driven from (code=%v identifier=%v password=%v)", base, okCode, okID, okPW)
			return
		}
		resolved, ok := resolveMarker(code, markers)
		if !ok {
			k.fail(rep, "%s.presented_code names the marker %q, which this driver cannot resolve", base, code)
			return
		}
		ownersBefore, err := h.authStore.CountOwnerBindings(ctx)
		if err != nil {
			k.fail(rep, "count owner bindings before %s: %v", base, err)
			return
		}
		res := h.claim(resolved, identifier, password)
		ownersAfter, err := h.authStore.CountOwnerBindings(ctx)
		if err != nil {
			k.fail(rep, "count owner bindings after %s: %v", base, err)
			return
		}
		want := fmt.Sprintf("claim_attempts.%d", i)
		k.boolAt(want+".claimed", res.status == http.StatusCreated)
		k.boolAt(want+".new_owner_principal_created", ownersAfter > ownersBefore)
		k.stringAt(want+".error.code", problemCode(res))
	}

	ownersAfterAttempts, err := h.authStore.CountOwnerBindings(ctx)
	if err != nil {
		k.fail(rep, "count owner bindings after the attempts: %v", err)
		return
	}
	k.intAt("owner_bindings_after_attempts", int64(ownersAfterAttempts))
	k.stringAt("box_claim_state_after_attempts", claimState(ownersAfterAttempts))

	// Leg 3: the code holder. Anything other than a clean claim here means the
	// two refusals above proved nothing about the code check.
	holderCode, okCode := inputString(c, "then_the_code_holder_claims.presented_code")
	holderID, okID := inputString(c, "then_the_code_holder_claims.identifier")
	holderPW, okPW := inputString(c, "then_the_code_holder_claims.password")
	if !okCode || !okID || !okPW {
		k.fail(rep, "then_the_code_holder_claims is missing a field this case is driven from")
		return
	}
	resolved, ok := resolveMarker(holderCode, markers)
	if !ok {
		k.fail(rep, "then_the_code_holder_claims.presented_code names the marker %q, which this driver cannot resolve", holderCode)
		return
	}
	holder := h.claim(resolved, holderID, holderPW)
	ownersAfterHolder, err := h.authStore.CountOwnerBindings(ctx)
	if err != nil {
		k.fail(rep, "count owner bindings after the code holder's claim: %v", err)
		return
	}
	k.boolAt("code_holder_claimed", holder.status == http.StatusCreated)
	k.intAt("owner_bindings_after_code_holder", int64(ownersAfterHolder))
	k.finish(rep)
}

// driveGrantRefusalsOnTheWire drives
// SEC-035a-invalid-grant-refusals-on-the-redemption-endpoint against the LIVE,
// HTTP-mounted redemption endpoint, reading each refusal's code off the api/1
// Problem body the route actually writes.
//
// This is the case that lets SEC-035 be decided at all. SEC-035 is a statement
// about WIRE CODES — "MUST be refused with GRANT_EXPIRED ... GRANT_ALREADY_
// REDEEMED ... GRANT_PURPOSE_MISMATCH" — and the SEC-035 case above drives the
// store directly, so its `error.code` is produced by grantErrorCode in this same
// driver: a shipped handler that refused correctly but typed the refusal with
// the wrong code would keep that case green. Here the code is read out of the
// response body, so the mapping under test is the shipped one.
//
// Each of the three refusals gets its OWN harness, because each needs a
// different grant state and a different clock reading, and a fixture that shared
// one would have to sequence them into an order the contract does not require.
func driveGrantRefusalsOnTheWire(rep *report.Report, c corpus.Case) {
	k := newCheck(c)
	ctx := context.Background()

	endpointPurpose, okPurpose := inputString(c, "redemption_endpoint_purpose")
	if !okPurpose {
		k.fail(rep, "the frozen input block names no redemption_endpoint_purpose")
		return
	}
	if endpointPurpose != auth.PurposeSetup {
		// Honest rather than silently normalized: this tree mounts exactly one
		// grant-redemption route, the first-boot claim, and it redeems `setup`.
		k.fail(rep, "the only grant-redemption route this tree mounts redeems %q; the case names %q", auth.PurposeSetup, endpointPurpose)
		return
	}
	if endpoint, ok := inputString(c, "endpoint"); ok && endpoint != claimRoute {
		k.note("corpus input names endpoint %q; the shipped mux mounts the redemption route at %q — driven against the shipped route", endpoint, claimRoute)
	}
	attempts, okAttempts := digInput(c, "attempts")
	attemptList, okList := attempts.([]any)
	if !okAttempts || !okList || len(attemptList) == 0 {
		k.fail(rep, "the frozen input block carries no attempts array")
		return
	}

	for i := range attemptList {
		base := fmt.Sprintf("attempts.%d", i)
		purpose, okP := inputString(c, base+".grant.purpose")
		kind, okK := inputString(c, base+".grant.resulting_principal_kind")
		mode, okM := inputString(c, base+".grant.redemption_mode")
		issuedAt, okI := inputTimeMs(c, base+".grant.issued_at")
		ttlMs, okT := inputDurationMs(c, base+".grant.ttl")
		attemptAt, okA := inputTimeMs(c, base+".redemption_attempted_at")
		presented, okC := inputString(c, base+".presented_code")
		redeemedFirst, okR := digInput(c, base+".redeemed_before_this_attempt")
		if !okP || !okK || !okM || !okI || !okT || !okA || !okC || !okR {
			k.fail(rep, "%s is missing a field this case is driven from", base)
			return
		}

		dir, err := os.MkdirTemp("", "secmodel1-refusal-")
		if err != nil {
			k.fail(rep, "scratch dir: %v", err)
			return
		}
		// effect is what the refusal actually DID, measured on the live store —
		// not what it said. A handler that types the refusal correctly on the wire
		// and claims the box anyway satisfies every code assertion below while
		// handing an attacker an owner principal, a role binding and a session.
		// SEC-035 says "MUST be refused", which is two clauses: the label AND the
		// effect. Only asserting the label leaves exactly that seam.
		var effect struct{ ownersDelta, principalsDelta int }
		outcome, drvErr := func() (httpResult, error) {
			defer os.RemoveAll(dir)
			// The harness starts at the grant's own issued_at, so the grant row is
			// stamped with the instant the case declares rather than with whatever
			// the harness happened to be set to.
			h, err := newClaimHarness(dir, issuedAt)
			if err != nil {
				return httpResult{}, fmt.Errorf("build the claim harness: %w", err)
			}
			defer h.close()

			minted, err := h.authStore.MintGrant(ctx, auth.MintGrantOptions{
				Purpose:                purpose,
				ResultingPrincipalKind: kind,
				ScopeNode:              auth.RootScopeNode,
				TTLMs:                  ttlMs,
				RedemptionMode:         mode,
			})
			if err != nil {
				return httpResult{}, fmt.Errorf("mint the %s grant: %w", purpose, err)
			}
			resolved, ok := resolveMarker(presented, map[string]string{"$grant.code": minted.Code})
			if !ok {
				return httpResult{}, fmt.Errorf("presented_code names the marker %q, which this driver cannot resolve", presented)
			}

			if redeemedFirst == true {
				first := h.claim(resolved, "first-redeemer@example.invalid", "first-redeemer-passphrase")
				if first.status != http.StatusCreated {
					return httpResult{}, fmt.Errorf("the first redemption was refused (%d %s) — this attempt's already-redeemed premise could not be established", first.status, first.raw)
				}
			}

			// The clock moves to the instant of the attempt. No sleep: the whole
			// harness reads one injected variable.
			h.setNow(attemptAt)

			ownersBefore, err := h.authStore.CountOwnerBindings(ctx)
			if err != nil {
				return httpResult{}, fmt.Errorf("count owner bindings before the attempt: %w", err)
			}
			principalsBefore, err := h.authStore.CountPrincipals(ctx)
			if err != nil {
				return httpResult{}, fmt.Errorf("count principals before the attempt: %w", err)
			}
			res := h.claim(resolved, "attempting@example.invalid", "attempting-passphrase")
			ownersAfter, err := h.authStore.CountOwnerBindings(ctx)
			if err != nil {
				return httpResult{}, fmt.Errorf("count owner bindings after the attempt: %w", err)
			}
			principalsAfter, err := h.authStore.CountPrincipals(ctx)
			if err != nil {
				return httpResult{}, fmt.Errorf("count principals after the attempt: %w", err)
			}
			effect.ownersDelta = ownersAfter - ownersBefore
			effect.principalsDelta = principalsAfter - principalsBefore
			return res, nil
		}()
		if drvErr != nil {
			k.fail(rep, "%s: %v", base, drvErr)
			return
		}

		want := fmt.Sprintf("attempts.%d", i)
		// `redeemed` is now the OR of what the route said and what it did: a 201,
		// or any owner binding / principal the refusal created behind a refusal
		// status. A correctly-labelled refusal that still claimed the box reads as
		// redeemed here, and the case's expected `false` fails it.
		k.boolAt(want+".redeemed", outcome.status == http.StatusCreated || effect.ownersDelta > 0 || effect.principalsDelta > 0)
		k.intAt(want+".http_status", int64(outcome.status))
		k.stringAt(want+".error.code", problemCode(outcome))
	}
	k.finish(rep)
}

// resolveMarker resolves a `$`-prefixed corpus marker naming a value the LIVE
// fixture minted (a setup code, a grant code) against the table the driving
// function built. A non-marker string is its own value.
//
// The false return is deliberately fatal at every call site rather than a
// fallback to the literal: a marker sent verbatim would be a wrong code, which
// several of these cases expect to be refused anyway — so the case would still
// pass, for entirely the wrong reason.
func resolveMarker(raw string, markers map[string]string) (string, bool) {
	if !strings.HasPrefix(raw, "$") {
		return raw, true
	}
	v, ok := markers[raw]
	return v, ok
}

// claimRoute is where internal/app/api actually mounts the first-boot claim.
const claimRoute = "/api/v1/auth/setup"

// claimHarness mounts the SAME http.Handler production wires (api.New) over a
// real auth store on disk — on disk rather than in memory because
// EnsureClaimWindow persists the setup code into the same directory, and a
// fixture that split those two would not be the deployment shape SEC-120
// describes.
//
// Its clock is a VARIABLE the caller moves (nowMs), never a sleep: the same
// injectable-clock discipline the package doc states, extended to the HTTP
// surface so a wire-level expiry case (SEC-035a) can be driven at all.
type claimHarness struct {
	authStore *auth.Store
	appStore  *store.Store
	handler   http.Handler
	nowMs     *int64
}

// corpusInstantMs is the instant the frozen corpora pin to
// (2026-07-15T00:00:00Z as the corpus writes it), used where a case declares no
// instant of its own.
const corpusInstantMs int64 = 1752537600000

func newClaimHarness(dir string, startMs int64) (*claimHarness, error) {
	now := startMs
	clock := func() int64 { return now }
	ids := deterministicIDs()

	authStore, err := auth.Open(dir+"/auth.db", clock, ids)
	if err != nil {
		return nil, err
	}
	appStore, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		_ = authStore.Close()
		return nil, err
	}
	authn := auth.NewAuthenticator(authStore, nil, auth.NewDefaultLockout(), auth.NewRevocations())
	handler := api.New(appStore, apihttp.NewIdempotencyStore(clock, 0), clock, ids,
		origin.New(), "https://origin.example", authn, api.WithJobRunner(api.NewJobRunner()))
	return &claimHarness{authStore: authStore, appStore: appStore, handler: handler, nowMs: &now}, nil
}

// setNow moves the harness's injected clock. Every component built above shares
// the closure that reads it, so one assignment moves the store's row timestamps,
// the grant expiry check and the api layer together — a fixture whose halves
// disagreed about the time would prove nothing about a ttl.
func (h *claimHarness) setNow(ms int64) { *h.nowMs = ms }

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

// flipOneNibble returns code with its LAST hex digit changed to a different
// one, preserving length, alphabet and case exactly.
//
// It exists so a case can present a wrong code that is indistinguishable in
// SHAPE from a real one. A wrong code that looks wrong only proves the handler
// rejects malformed input; a wrong code that looks right is what proves the
// handler resolves it against an actual grant. The distinction is not
// theoretical: an implementation that admits any shape-valid code it fails to
// resolve — on an unclaimed box, first caller wins — is exactly the
// first-come-first-served takeover SEC-120 forbids, and it passes a case whose
// wrong code is obvious garbage.
//
// A non-hex or empty code is returned unchanged: the caller is then presenting
// something whose shape this helper cannot preserve, and silently substituting
// a different shape would be the very confusion this exists to avoid.
func flipOneNibble(code string) string {
	if code == "" {
		return code
	}
	b := []byte(code)
	last := b[len(b)-1]
	switch {
	case last >= '0' && last <= '8':
		b[len(b)-1] = last + 1
	case last == '9':
		b[len(b)-1] = 'a'
	case last >= 'a' && last <= 'e':
		b[len(b)-1] = last + 1
	case last == 'f':
		b[len(b)-1] = '0'
	default:
		return code // not lowercase hex — shape cannot be preserved
	}
	return string(b)
}
