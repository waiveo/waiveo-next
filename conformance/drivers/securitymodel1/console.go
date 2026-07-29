package securitymodel1

import (
	"context"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/app/auth"
)

// driveConsoleAdmission drives SEC-072-valid-console-admission-uid0 against the
// LIVE console dispatcher (internal/app/auth.Console).
//
// The peer uid arrives as an INPUT, which is what the contract's own Conformance
// notes call for rather than a shortcut this driver invented: "SO_PEERCRED
// admission (SEC-072) ... conformance cases model them as a given input
// (`peer_uid: 0` or `peer_uid: 1000`) ... the driver harness that actually opens
// a Unix socket and checks SO_PEERCRED is a systemd-install-smoke-lane concern,
// not this static corpus's."
//
// Three things are asserted, and the third is the one that makes SEC-073's
// "without any further credential exchange" a fact rather than a claim: the
// attributed principal is looked up in the live store and asked how many
// credential rows it holds (zero), and an attempt to attach one is made and must
// be REFUSED. A synthetic principal that merely happens to have no credential
// today would satisfy the count and fail the refusal.
//
// The verb is executed for real — the corpus names `session.revoke` with a
// session id, so this driver mints a session at exactly that id (a pinned id
// source) and confirms the dispatch actually revoked it. Admission that
// "succeeds" without the verb running would otherwise pass every field the
// expected block names.
func driveConsoleAdmission(rep *report.Report, c corpus.Case) {
	k := newCheck(c)
	ctx := context.Background()

	peerUID, okUID := inputInt(c, "peer_uid")
	verb, okVerb := inputString(c, "verb")
	sessionID, okSession := inputString(c, "params.session_id")
	socketMode, _ := inputString(c, "socket_mode")
	if !okUID || !okVerb || !okSession {
		k.fail(rep, "the frozen input block is missing a field this case is driven from (peer_uid=%v verb=%v session_id=%v)", okUID, okVerb, okSession)
		return
	}
	if credential, present := digInput(c, "presented_credential"); present && credential != nil {
		k.fail(rep, "this case's premise is a peer presenting NO credential; the frozen input carries %v", credential)
		return
	}
	if socketMode != "" {
		k.note("socket_mode %q (SEC-071) is not asserted: this tree has no Unix-domain-socket listener for the console binding, only the admission rule and verb policy this dispatcher owns", socketMode)
	}

	// The session id the case names is minted for real: the id source hands the
	// store exactly that ULID when the session row is created, so the dispatch
	// below acts on the case's own subject rather than on a substitute.
	// The queue is positional: the first id the store draws is the subject
	// principal's, the second is the session's — which is the one the case
	// itself names.
	ids := deterministicIDs(idPrefix+"ZZ", sessionID)
	fixedNow := int64(1752537600000)
	st, err := auth.Open(":memory:", func() int64 { return fixedNow }, ids)
	if err != nil {
		k.fail(rep, "open auth store: %v", err)
		return
	}
	defer st.Close()

	subject, err := st.CreatePrincipal(ctx, auth.KindUser, "console-subject")
	if err != nil {
		k.fail(rep, "create the subject principal: %v", err)
		return
	}
	minted, err := st.MintSession(ctx, subject.PrincipalID, auth.TokenKindSession, "", auth.AALStandard, nil)
	if err != nil {
		k.fail(rep, "mint the subject session: %v", err)
		return
	}
	if minted.Session.SessionID != sessionID {
		k.fail(rep, "the fixture session id is %q, not the case's own %q — the pinned id source did not take", minted.Session.SessionID, sessionID)
		return
	}

	console := auth.NewConsole(st, nil, nil)
	resp := console.Dispatch(ctx, int(peerUID), auth.ConsoleRequest{Verb: verb, Params: map[string]any{"session_id": sessionID}})

	// SEC-002/073: the attributed principal carries no credential row, and one
	// cannot be attached.
	credentialRowRequired := false
	if resp.PrincipalID != "" {
		n, err := st.CredentialCount(ctx, resp.PrincipalID)
		if err != nil {
			k.fail(rep, "count the console principal's credentials: %v", err)
			return
		}
		if _, err := st.PutPasswordCredential(ctx, resp.PrincipalID, "console", "hunter2"); err == nil {
			// A credential was accepted onto the system-console principal, so the
			// binding's identity is no longer admission alone.
			credentialRowRequired = true
		}
		if n > 0 {
			credentialRowRequired = true
		}
	} else {
		credentialRowRequired = true
	}

	k.boolAt("admitted", resp.Admitted)
	k.stringAt("attributed_principal_kind", resp.PrincipalKind)
	k.boolAt("attributed_principal_credential_row_required", credentialRowRequired)
	k.nullAt("error", resp.Code)

	// The verb really ran: the named session no longer resolves. This is not in
	// the case's expected block — it is the precondition that makes `admitted`
	// mean "admitted and served" rather than "admitted and ignored" — so a
	// divergence is reported as its own diff rather than silently tolerated.
	if _, err := st.LookupSession(ctx, minted.Token); err == nil {
		k.diff("session.revoke actually executed", true, false)
	}
	k.finish(rep)
}

// driveConsoleVerbNotAllowed drives SEC-075-invalid-console-verb-not-allowed
// against the same live dispatcher: a peer that has ALREADY proven uid-0 names a
// general resource-data-access verb, and is refused.
//
// SEC-075's closure is the requirement — "general resource data access MUST NOT
// be exposed over this binding, even to a caller who has already proven uid-0" —
// so this case only means anything if the uid check has passed. The driver
// therefore asserts that the SAME uid admitted by SEC-072's case is used here,
// reading it from this case's own input rather than assuming it.
func driveConsoleVerbNotAllowed(rep *report.Report, c corpus.Case) {
	k := newCheck(c)
	ctx := context.Background()

	peerUID, okUID := inputInt(c, "peer_uid")
	verb, okVerb := inputString(c, "verb")
	if !okUID || !okVerb {
		k.fail(rep, "the frozen input block is missing a field this case is driven from (peer_uid=%v verb=%v)", okUID, okVerb)
		return
	}

	fixedNow := int64(1752537600000)
	st, err := auth.Open(":memory:", func() int64 { return fixedNow }, deterministicIDs())
	if err != nil {
		k.fail(rep, "open auth store: %v", err)
		return
	}
	defer st.Close()

	console := auth.NewConsole(st, nil, nil)

	// The uid this case supplies must be one the admission rule ACCEPTS,
	// otherwise the refusal below would be SEC-072's and the case would pass for
	// the wrong reason entirely. Proven, not assumed: the same uid is dispatched
	// with an ALLOWED verb first, and that dispatch must be admitted.
	probe := console.Dispatch(ctx, int(peerUID), auth.ConsoleRequest{Verb: auth.ConsoleVerbServiceStatus})
	if !probe.Admitted {
		k.fail(rep, "peer_uid %d was refused admission (%s) — this case's premise is a peer that HAS proven uid-0", peerUID, probe.Code)
		return
	}

	params, _ := digInput(c, "params")
	paramMap, _ := params.(map[string]any)
	resp := console.Dispatch(ctx, int(peerUID), auth.ConsoleRequest{Verb: verb, Params: paramMap})

	k.boolAt("admitted", resp.Admitted)
	k.boolAt("executed", resp.Executed)
	k.stringAt("error.code", resp.Code)
	k.finish(rep)
}
