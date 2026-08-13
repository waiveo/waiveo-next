package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// The tier-grant ceremony (SEC-037): how an installed pack's process gets an
// identity to call the API with.
//
// # Why a grant and not a credential
//
// SEC-003a refuses an api-key for a `pack-service` principal, and none of the
// other five credential kinds describes a local child process. That refusal is
// the design, not an obstacle: a pack never holds a long-lived credential at
// all. The host mints a one-time, short-lived code, hands it to the process it
// has just started, and the process exchanges it for a SESSION. A leaked code
// is already spent; a session dies with the process that earned it.
//
// The pack's authority therefore lasts exactly as long as its process, which is
// the same lifetime the supervisor already manages. Nothing has to be revoked
// on shutdown, and nothing survives a crash.
//
// # Why the principal outlives the session
//
// The PRINCIPAL is created once and reused across restarts; only the session is
// per-start. A principal minted per launch would accumulate one row per restart
// forever and — worse — scatter a pack's audit trail across hundreds of
// identities, so "what did this extension do" would stop being answerable,
// which is the one thing the audit trail exists for.

// PurposeTierGrant is SEC-037's grant purpose. Declared here beside the ceremony
// it names rather than in grant.go's list, because the constant and the rules
// that make it safe should not be able to drift apart.
const PurposeTierGrant = "tier-grant"

// DefaultTierGrantTTLMs is the ttl SEC-037 calls for: this code is redeemed by a
// process the host started microseconds ago, not by a human who has to be given
// time to type it. Sixty seconds is generous for a process start and short
// enough that a code recovered from a crash dump is worthless.
const DefaultTierGrantTTLMs int64 = 60_000

// labelPackID carries the pack's manifest id through the grant to its
// redemption. The grant is the only thing the redeeming process presents, so
// the pack identity has to ride on it — the process cannot be trusted to
// SAY which pack it is, which is exactly the impersonation this closes.
const labelPackID = "pack_id"

// ErrTierGrantNoPack is a tier grant that carries no pack id — a grant that
// cannot say who it is for, which must never redeem to anything.
var ErrTierGrantNoPack = errors.New("auth: tier grant carries no pack_id label")

// ErrTierGrantNoScope is a tier grant with no scope node. A pack-service
// principal's authority is its role binding, and a binding has to be placed.
var ErrTierGrantNoScope = errors.New("auth: tier grant carries no scope node")

// PackSession is what a redeemed tier grant yields: the identity the pack now
// holds, and the bearer token it authenticates with.
type PackSession struct {
	PrincipalID string
	PackID      string
	Role        Role
	ScopeNode   string
	SessionID   string
	// Token is the bearer credential. Returned exactly once, at redemption, and
	// never stored in retrievable form — the same discipline every other session
	// token on this surface follows.
	Token string
}

// MintTierGrant issues the one-time code the host hands a pack process at start
// (SEC-037).
//
// role is the caller's choice, not this function's: "tier" is the pack's trust
// channel, and the mapping from channel to authority is deployment policy that
// the auth store has no business deciding. What is fixed here is the ceremony —
// one-time, pack-service, short-lived.
func (s *Store) MintTierGrant(ctx context.Context, packID, scopeNode string, role Role, opts ...MintGrantOption) (MintedGrant, error) {
	if packID == "" {
		return MintedGrant{}, ErrTierGrantNoPack
	}
	// Refused here rather than discovered at redemption. A pack-service
	// principal's authority IS its role binding, and a binding needs a scope
	// node — so a tier grant without one can never redeem. Minting it anyway
	// would hand the host a code that fails at the worst moment, in a child
	// process, with the pack already started.
	if scopeNode == "" {
		return MintedGrant{}, ErrTierGrantNoScope
	}
	o := MintGrantOptions{
		Purpose:                PurposeTierGrant,
		ResultingPrincipalKind: KindPackService,
		ScopeNode:              scopeNode,
		Labels:                 map[string]string{labelPackID: packID},
		Role:                   role,
		TTLMs:                  DefaultTierGrantTTLMs,
		// Stated rather than left to the default: SEC-037 REQUIRES one-time, and
		// a reader should not have to know SEC-031's default to see that.
		RedemptionMode: RedemptionOneTime,
		IssuedVia:      IssuedViaAPI,
	}
	for _, fn := range opts {
		fn(&o)
	}
	// Re-asserted after the options run. An option that changed the purpose or
	// the kind would produce a grant that is not a tier grant while claiming to
	// be one, and the redemption below would then mint a pack-service session
	// for something else entirely.
	o.Purpose = PurposeTierGrant
	o.ResultingPrincipalKind = KindPackService
	o.Labels[labelPackID] = packID
	return s.MintGrant(ctx, o)
}

// MintGrantOption tunes a tier grant — overriding the ttl, or attributing the
// mint to a principal for the SEC-034 record, are the intended uses. Purpose,
// principal kind and the pack id are re-asserted after options run and cannot be
// overridden.
//
// No named helpers are exported for those yet: the host-side mint that would
// call them does not exist, and a constructor with no caller is the shape the
// deadcode gate exists to refuse. They come back with their caller.
type MintGrantOption func(*MintGrantOptions)

// RedeemTierGrant exchanges a start-time code for the pack's session.
//
// The pack id comes off the GRANT, never off the request: a process that could
// name its own pack could claim any pack's identity, data and audit trail by
// presenting a code it happened to obtain. Everything about who this process is
// was decided by the host when it minted the grant.
//
// The principal is found-or-created under a stable identifier derived from the
// pack id, so a restart re-uses the identity rather than minting a new one.
func (s *Store) RedeemTierGrant(ctx context.Context, code string, device map[string]string) (PackSession, error) {
	var out PackSession

	_, err := s.RedeemGrant(ctx, code, PurposeTierGrant, func(tx *sql.Tx, g GrantRow) error {
		packID := g.Labels[labelPackID]
		if packID == "" {
			return ErrTierGrantNoPack
		}
		// Defence in depth: RedeemGrant already matched the purpose, but a grant
		// whose purpose is tier-grant and whose resulting kind is something else
		// would mint a session for the wrong kind of principal entirely.
		if g.ResultingPrincipalKind != KindPackService {
			return fmt.Errorf("auth: tier grant for %s resolves principal kind %q, want %s (SEC-037)",
				packID, g.ResultingPrincipalKind, KindPackService)
		}

		p, err := s.findOrCreatePackPrincipalTx(ctx, tx, packID)
		if err != nil {
			return err
		}
		if _, err := s.PutRoleBindingTx(ctx, tx, p.PrincipalID, g.ScopeNode, g.Role); err != nil {
			return err
		}
		out = PackSession{
			PrincipalID: p.PrincipalID, PackID: packID,
			Role: g.Role, ScopeNode: g.ScopeNode,
		}
		return nil
	})
	if err != nil {
		return PackSession{}, err
	}

	// Minted OUTSIDE the redemption transaction, deliberately. MintSession opens
	// its own write transaction, and nesting one inside the redemption's would
	// deadlock against the store's single write lock. The ordering is safe
	// because the grant is already spent: if this fails, the code cannot be
	// replayed and the host's remedy is to start the pack again with a fresh
	// grant — which is a retry it can always perform, unlike a human-typed code.
	minted, err := s.MintSession(ctx, out.PrincipalID, TokenKindSession, "", AALStandard, device)
	if err != nil {
		return PackSession{}, err
	}
	out.SessionID = minted.Session.SessionID
	out.Token = minted.Token
	return out, nil
}

// packPrincipalName is the stable display name a pack's principal is found by.
// Prefixed so it cannot collide with a human identifier: principals are looked
// up by this string, and an operator who happened to be called
// `waiveo/slidecast` must not be resolvable as one.
func packPrincipalName(packID string) string { return "pack:" + packID }

// findOrCreatePackPrincipalTx resolves the pack's one principal, creating it on
// first redemption.
//
// The lookup is by kind AND name together. By name alone, a pack could adopt an
// identically named principal of another kind; by kind alone there is no
// identity at all.
func (s *Store) findOrCreatePackPrincipalTx(ctx context.Context, tx *sql.Tx, packID string) (PrincipalRow, error) {
	name := packPrincipalName(packID)
	var p PrincipalRow
	err := tx.QueryRowContext(ctx,
		`SELECT principal_id, kind, display_name FROM principals WHERE kind = ? AND display_name = ?`,
		KindPackService, name,
	).Scan(&p.PrincipalID, &p.Kind, &p.DisplayName)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PrincipalRow{}, fmt.Errorf("auth: resolve pack principal %s: %w", packID, err)
	}
	return s.CreatePrincipalTx(ctx, tx, KindPackService, name)
}

// tierGrantRedeemRequest is POST /auth/tier-grant/redeem's body. The code is
// deliberately its ONLY member: the pack id rides on the grant, so there is
// nothing here a caller could use to name itself.
type tierGrantRedeemRequest struct {
	Code string `json:"code"`
}

// RedeemTierGrant is the HTTP half of SEC-037 — the route a pack process calls
// once, at start, to turn the code the host handed it into a session.
//
// Unauthenticated by necessity (API-090/091, `security: []`): the caller has no
// identity yet and acquiring one is the whole point, so requiring a session
// would make the operation unreachable by the only party permitted to perform
// it. What bounds it instead is SEC-033's attempt budget and SEC-036's atomic
// check-and-consume, exactly as for every other code-redemption route here.
func (h *Handlers) RedeemTierGrant(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)
	if r.Method != http.MethodPost {
		apihttp.WriteProblem(w, r, traceID, http.StatusMethodNotAllowed, "VALIDATION_FAILED", "Method Not Allowed")
		return
	}
	var req tierGrantRedeemRequest
	if !decodeBody(w, r, traceID, &req) {
		return
	}
	if req.Code == "" {
		writeValidationProblem(w, r, traceID, []fieldError{{"code", "required", "the one-time tier-grant code is required"}})
		return
	}

	store := h.auth.store
	// SEC-033, enforced BEFORE the lookup: the attack this bounds is guessing a
	// code that already exists, so the budget must refuse without checking the
	// guess. Keyed on this purpose so a sweep against pack codes cannot be
	// masked by reset or setup traffic, and vice versa.
	if !h.budget.Allow(PurposeTierGrant, RequestSource(r), store.Now()) {
		// The refused attempt is the only evidence a sweep leaves. It names the
		// SOURCE and never the code — the code is the secret being guessed at,
		// and an audit trail is a place secrets go to be read later.
		h.auth.auditor.Failure(traceID, "", ActionGrantRedeemed, "source:"+RequestSource(r))
		apihttp.WriteProblemExt(w, r, traceID, http.StatusTooManyRequests,
			"RATE_LIMITED", "Too Many Requests",
			"Too many tier-grant redemption attempts from this source; retry after a backoff.", nil)
		return
	}

	sess, err := store.RedeemTierGrant(r.Context(), req.Code, map[string]string{
		"user_agent": r.UserAgent(),
		"ip_class":   RequestIPClass(r),
	})
	if err != nil {
		h.auth.auditor.Failure(traceID, "", ActionGrantRedeemed, "source:"+RequestSource(r))
		h.writeGrantProblem(w, r, traceID, err, "The tier-grant code is not valid.")
		return
	}

	// Attributed to the PACK's principal, not to an anonymous source: from here
	// on every audit record this pack produces hangs off this id, and the record
	// that says how it got the identity has to be findable from the same place.
	h.auth.auditor.Success(traceID, sess.PrincipalID, ActionGrantRedeemed, "pack:"+sess.PackID)
	h.auth.auditor.Success(traceID, sess.PrincipalID, ActionSessionIssued, "session:"+sess.SessionID)

	// No cookie is set. A pack is not a browser: it presents the token as a
	// bearer credential, and a Set-Cookie here would be an ambient credential on
	// a client that has no cookie jar and no same-site protections to make one
	// safe.
	writeJSON(w, http.StatusCreated, map[string]any{
		"principal_id": sess.PrincipalID,
		"pack_id":      sess.PackID,
		"role":         string(sess.Role),
		"scope_node":   sess.ScopeNode,
		"session_id":   sess.SessionID,
		"token":        sess.Token,
	})
}
