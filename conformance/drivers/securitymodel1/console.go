package securitymodel1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

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
		// This case drives the ADMISSION RULE with the injected peer uid the
		// contract's own Conformance notes call for, so it terminates no socket
		// and has no inode to stat. SEC-071's mode is asserted off a real socket
		// by SEC-072a, which drives the shipped listener; naming that here keeps a
		// reader from concluding the mode is unasserted anywhere.
		k.note("socket_mode %q (SEC-071) is not asserted by this case, which drives the admission rule with an injected peer_uid and terminates no socket; it is asserted off the real inode by SEC-072a-invalid-console-peer-not-root", socketMode)
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

// driveConsolePeerNotRoot drives SEC-072a-invalid-console-peer-not-root against
// the REAL console-binding transport: auth.ListenConsole's own Unix domain
// socket, the real per-OS peer-credential syscall, and the real refusal path.
//
// # Why this case exists beside the injected-uid one
//
// The contract's Conformance notes bless modelling `peer_uid` as an input, and
// SEC-072's case above does exactly that — but that arrangement could not
// distinguish a shipped app that reads SO_PEERCRED from one that has no socket
// at all, which is precisely the state this tree was in: "there is no Unix
// domain socket (SEC-070), no 0700 socket file (SEC-071), and nothing reads
// SO_PEERCRED (SEC-072)". This case closes that by connecting to a socket the
// shipped constructor bound and letting the shipped syscall read this test
// process's own credential.
//
// # Why it is the REFUSAL half specifically
//
// Because it is the half a non-root process can drive. The admitted branch needs
// the connecting process to be uid 0, which a conformance run is not (and must
// not require, or CI would have to run as root to be conformant). So the socket
// exercises the direction available to it, and the admitted direction stays on
// the injected-uid case the contract sanctions. A run that IS root cannot drive
// this case at all and says so as a PENDING rather than passing vacuously — a
// root peer would be ADMITTED, and reporting that as a refusal would be a lie.
//
// # What is asserted, and why each one has teeth
//
//   - `response_body_bytes: 0` — the client reads to EOF and must get nothing.
//     SEC-072's refusal "carries no response body"; a listener that answered
//     with a Problem naming CONSOLE_PEER_NOT_ROOT would fail here, which is
//     deliberate: that would tell an unprivileged prober that the binding exists
//     and what it refuses on.
//   - `audit_record_names_a_verb: false` — the refusal happened BEFORE the
//     request was read. A listener that decoded the body first and refused after
//     would record `console:service.status` and fail this, which is how "refused
//     at accept time" is distinguished from "refused eventually".
//   - `socket_file_mode` / `socket_directory_mode` — SEC-071's filesystem half,
//     read off the real inode, plus the directory that closes the bind-to-chmod
//     window.
func driveConsolePeerNotRoot(rep *report.Report, c corpus.Case) {
	k := newCheck(c)

	casePeerUID, okUID := inputInt(c, "peer_uid")
	verb, okVerb := inputString(c, "verb")
	if !okUID || !okVerb {
		k.fail(rep, "the frozen input block is missing a field this case is driven from (peer_uid=%v verb=%v)", okUID, okVerb)
		return
	}
	if casePeerUID == 0 {
		k.fail(rep, "this case's premise is a NON-root peer; its frozen input declares peer_uid 0")
		return
	}
	if os.Geteuid() == 0 {
		rep.Pending(c.CaseID, contract, fmt.Sprintf(
			"this case models a peer whose effective uid is not 0 (%d) connecting to the real console socket; this conformance process runs as uid 0, so the only connection it can make would be ADMITTED — driving it would assert a refusal that did not happen. Re-run as a non-root user.", casePeerUID))
		return
	}
	if verb != auth.ConsoleVerbServiceStatus {
		k.note("the case names verb %q; it is sent verbatim and must never be read, which is the point of the assertion below", verb)
	}

	dir, err := os.MkdirTemp("", "secmodel1-consolesock-")
	if err != nil {
		k.fail(rep, "scratch dir: %v", err)
		return
	}
	defer os.RemoveAll(dir)

	fixedNow := corpusInstantMs
	sink := &recordingSink{}
	ids := deterministicIDs()
	auditor := auth.NewAuditor(sink, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", func() int64 { return fixedNow }, ids, nil)
	st, err := auth.Open(dir+"/auth.db", func() int64 { return fixedNow }, ids, auth.WithAuditor(auditor))
	if err != nil {
		k.fail(rep, "open auth store: %v", err)
		return
	}
	defer st.Close()

	// The SHIPPED constructor, with the SHIPPED peer-credential reader. Nothing
	// about admission is injected here.
	ln, err := auth.ListenConsole(dir, auth.NewConsole(st, nil, auditor), nil)
	if err != nil {
		k.fail(rep, "bind the console binding: %v", err)
		return
	}
	go ln.Serve()
	defer ln.Close()

	// SEC-070/071, read off the real inode.
	fi, err := os.Lstat(ln.Path())
	if err != nil {
		k.fail(rep, "stat the console socket: %v", err)
		return
	}
	network := "not-a-socket"
	if fi.Mode()&os.ModeSocket != 0 {
		network = "unix"
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		k.fail(rep, "stat the console socket's directory: %v", err)
		return
	}
	k.stringAt("socket_network", network)
	k.stringAt("socket_file_mode", fmt.Sprintf("%04o", fi.Mode().Perm()))
	k.stringAt("socket_directory_mode", fmt.Sprintf("%04o", dirInfo.Mode().Perm()))

	// The connection: this process's own uid, which is not 0 (checked above).
	conn, err := net.Dial("unix", ln.Path())
	if err != nil {
		k.fail(rep, "dial the console socket: %v", err)
		return
	}
	defer conn.Close()
	req, err := json.Marshal(auth.ConsoleRequest{Verb: verb, TraceID: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z6"})
	if err != nil {
		k.fail(rep, "encode the console request: %v", err)
		return
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// A write may fail outright once the peer has closed, which is itself a
	// refusal; the assertion below is on what came BACK, so a write error is not
	// a driver failure.
	_, _ = conn.Write(req)
	body, readErr := io.ReadAll(conn)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		// A reset connection reads as an error on some platforms and as a clean
		// EOF on others. Either way the peer got no body, which is what SEC-072
		// requires; the byte count below is the assertion, and this only records
		// how the close presented itself.
		k.note("the refused connection closed with %v (no body either way)", readErr)
	}

	k.boolAt("admitted", len(body) > 0)
	k.intAt("response_body_bytes", int64(len(body)))

	// SEC-077's record, from the real auditor: the refusal is recorded, and the
	// record names no verb because the request was never read.
	verbNamed, result := consoleAuditVerbAndResult(sink, verb)
	k.boolAt("audit_record_names_a_verb", verbNamed)
	k.stringAt("audit_result", result)
	k.finish(rep)
}

// consoleAuditVerbAndResult reports whether the console-verb audit record names
// the verb the (refused) peer sent, and what result it carries.
//
// "Names a verb" is asked of the verb the CASE sent, not of any verb: a record
// whose target is the placeholder the refusal path writes is not naming one, and
// that difference is the whole assertion.
func consoleAuditVerbAndResult(sink *recordingSink, verb string) (bool, string) {
	for _, env := range sink.snapshot() {
		raw, err := json.Marshal(env)
		if err != nil {
			continue
		}
		var decoded struct {
			Payload struct {
				Action string `json:"action"`
				Target string `json:"target"`
				Result string `json:"result"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			continue
		}
		if decoded.Payload.Action != auth.ActionConsoleVerb {
			continue
		}
		return strings.Contains(decoded.Payload.Target, verb), decoded.Payload.Result
	}
	return false, "<no console.verb record emitted>"
}
