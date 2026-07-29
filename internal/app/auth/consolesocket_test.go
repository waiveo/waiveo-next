package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// consolesocket_test.go covers the console binding's transport (SEC-070-074) —
// the half no conformance case can reach from a process that is not root, plus
// the two failure modes that are only visible from inside the package.
//
// The frozen corpus drives the REFUSAL direction over a real socket
// (SEC-072a) and the ADMISSION rule with an injected peer uid (SEC-072). What
// is left, and what lives here, is the ADMITTED direction over a real socket —
// which needs the unexported peer-uid seam, because a `go test` process is not
// uid 0 and must never need to be.

// shortTempDir is t.TempDir with a short path.
//
// A Unix domain socket address holds ~104 bytes, and t.TempDir builds its
// directory name out of the TEST'S OWN NAME — so a descriptively-named test
// produces a path the kernel refuses to bind, with an "invalid argument" that
// says nothing about why. That is a property of the test harness, not of the
// listener, so the tests use a short scratch directory instead of shortening
// their names.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wvc")
	if err != nil {
		t.Fatalf("scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// dialConsole is one console conversation: connect, send req, read the whole
// response.
func dialConsole(t *testing.T, path string, req ConsoleRequest) []byte {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	if _, err := conn.Write(body); err != nil {
		// A peer refused at accept time may close before this write lands. That
		// is a legitimate outcome, not a test failure; the response read below is
		// what the assertions are made on.
		t.Logf("write to console socket: %v", err)
	}
	got, err := io.ReadAll(conn)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Logf("read from console socket: %v", err)
	}
	return got
}

// newConsoleListener binds a listener over st in a fresh directory. peerUID,
// when non-nil, replaces the real peer-credential read.
func newConsoleListener(t *testing.T, st *Store, auditor *Auditor, peerUID func(*net.UnixConn) (int, error)) *ConsoleListener {
	t.Helper()
	dir := shortTempDir(t)
	ln, err := ListenConsole(dir, NewConsole(st, nil, auditor), nil)
	if err != nil {
		t.Fatalf("ListenConsole: %v", err)
	}
	if peerUID != nil {
		ln.peerUID = peerUID
	}
	go ln.Serve()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// TestPeerUIDReadsTheConnectingProcessesUID is the one assertion in this file
// that is about the SYSCALL rather than about the policy above it, and it is
// deliberately uid-independent: whatever uid this test process runs as, the
// credential the kernel reports for a connection this process made must be that
// uid.
//
// It is the check that separates "the listener reads SO_PEERCRED" from "the
// listener returns a plausible number". A readPeerUID that returned 0
// unconditionally — the single worst bug this file could have, since it admits
// every peer — passes every other test here and fails this one on any non-root
// runner; one that returned a constant non-zero fails it on a root runner.
func TestPeerUIDReadsTheConnectingProcessesUID(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "probe.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	var (
		got    int
		readEr error
	)
	go func() {
		defer close(done)
		conn, err := ln.AcceptUnix()
		if err != nil {
			readEr = err
			return
		}
		defer conn.Close()
		got, readEr = readPeerUID(conn)
	}()

	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	<-done

	if readEr != nil {
		t.Fatalf("readPeerUID: %v", readEr)
	}
	if want := os.Geteuid(); got != want {
		t.Fatalf("readPeerUID = %d, want this process's own euid %d — the peer credential is not being read off the connection", got, want)
	}
}

// TestConsoleSocketRefusesANonRootPeerWithNoBody is SEC-072 over the real
// transport, in the direction a non-root test process can produce.
//
// Skipped, not silently passed, when the test runs as root: a root peer would be
// ADMITTED, and asserting a refusal that could not have occurred is worse than
// asserting nothing.
func TestConsoleSocketRefusesANonRootPeerWithNoBody(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test connects as its own uid and asserts the refusal; running as root it would be admitted")
	}
	st := newSecurityTestStore(t)
	sink := &recordingSink{}
	auditor := NewAuditor(sink, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", func() int64 { return 1752537600000 }, testIDs(), nil)
	ln := newConsoleListener(t, st, auditor, nil)

	got := dialConsole(t, ln.Path(), ConsoleRequest{Verb: ConsoleVerbServiceStatus})
	if len(got) != 0 {
		t.Fatalf("a non-root peer received %d response bytes (%q); SEC-072 refuses with no response body", len(got), got)
	}
	// SEC-077's record still fires, and names no verb: the refusal happened
	// before the request was read.
	records := sink.payloads(ActionConsoleVerb)
	if len(records) != 1 {
		t.Fatalf("console.verb records = %d, want exactly 1 (SEC-077 admits no exception)", len(records))
	}
	if target, _ := records[0]["target"].(string); strings.Contains(target, ConsoleVerbServiceStatus) {
		t.Fatalf("the refusal record names the verb (%q); SEC-072 refuses at accept time, before any request is read", target)
	}
	if result, _ := records[0]["result"].(string); result != "failure" {
		t.Fatalf("the refusal record's result = %q, want failure", result)
	}
}

// TestConsoleSocketServesAnAdmittedPeerEndToEnd is the ADMITTED direction over a
// real socket: bind, connect, read the peer credential (here, the seam), admit,
// decode the request, run the verb for real, encode the response.
//
// It executes `session.revoke` and then checks the session is actually gone,
// because "admitted" that does not perform the verb is the failure mode this
// whole task exists to remove.
func TestConsoleSocketServesAnAdmittedPeerEndToEnd(t *testing.T) {
	ctx := t.Context()
	st := newSecurityTestStore(t)
	sink := &recordingSink{}
	auditor := NewAuditor(sink, "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5", func() int64 { return 1752537600000 }, testIDs(), nil)
	ln := newConsoleListener(t, st, auditor, func(*net.UnixConn) (int, error) { return 0, nil })

	subject, err := st.CreatePrincipal(ctx, KindUser, "console-subject")
	if err != nil {
		t.Fatalf("create principal: %v", err)
	}
	minted, err := st.MintSession(ctx, subject.PrincipalID, TokenKindSession, "", AALStandard, nil)
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}

	const traceID = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z6"
	raw := dialConsole(t, ln.Path(), ConsoleRequest{
		Verb:    ConsoleVerbSessionRevoke,
		Params:  map[string]any{"session_id": minted.Session.SessionID},
		TraceID: traceID,
	})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	if body["revoked"] != true {
		t.Fatalf("response = %v, want the verb's own result", body)
	}
	// SEC-074's Trace-Id propagation, over a transport with no header channel.
	if body["trace_id"] != traceID {
		t.Fatalf("trace_id = %v, want the caller's own %q propagated unmodified", body["trace_id"], traceID)
	}
	if _, err := st.LookupSession(ctx, minted.Token); err == nil {
		t.Fatal("the console dispatch reported success and the session still resolves — the verb was admitted and not performed")
	}
}

// TestConsoleSocketRefusesAnUnlistedVerbAsAnAPIProblem is SEC-074: the refusal
// body is api/1's Problem document, not a shape of the console binding's own.
func TestConsoleSocketRefusesAnUnlistedVerbAsAnAPIProblem(t *testing.T) {
	st := newSecurityTestStore(t)
	ln := newConsoleListener(t, st, nil, func(*net.UnixConn) (int, error) { return 0, nil })

	raw := dialConsole(t, ln.Path(), ConsoleRequest{Verb: "screens.list", TraceID: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z6"})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	for _, member := range []string{"type", "title", "status", "code", "trace_id"} {
		if _, ok := body[member]; !ok {
			t.Errorf("the refusal body omits api/1's Problem member %q: %v", member, body)
		}
	}
	if body["code"] != ErrCodeConsoleVerbNotAllowed {
		t.Errorf("code = %v, want %q", body["code"], ErrCodeConsoleVerbNotAllowed)
	}
	if body["type"] != "about:blank" {
		t.Errorf("type = %v, want api/1's about:blank (API-016)", body["type"])
	}
}

// TestConsoleSocketIsMode0700InsideA0700Directory is SEC-071's filesystem half,
// plus the directory that closes the bind-to-chmod window the socket's own mode
// cannot close by itself.
func TestConsoleSocketIsMode0700InsideA0700Directory(t *testing.T) {
	st := newSecurityTestStore(t)
	dir := shortTempDir(t)
	// A directory that already exists with a permissive mode: the listener must
	// tighten it rather than accept it, since t.TempDir and a real deployment
	// both hand it a directory somebody else created.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("loosen the directory: %v", err)
	}
	ln, err := ListenConsole(dir, NewConsole(st, nil, nil), nil)
	if err != nil {
		t.Fatalf("ListenConsole: %v", err)
	}
	defer ln.Close()

	fi, err := os.Lstat(ln.Path())
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket (mode %v)", ln.Path(), fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("socket mode = %04o, want 0700 (SEC-071)", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat the directory: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("directory mode = %04o, want 0700 — the socket's own mode is set AFTER bind, and this directory is what closes that window", perm)
	}
}

// TestListenConsoleRefusesToReplaceANonSocket is the destructive-unlink guard.
//
// ListenConsole runs as the app's own uid — root, on a real box — against a path
// built from configuration. "Unlink whatever is at this path" would turn a
// misconfigured auth directory into data loss on boot, so a regular file there
// is a refusal, and this is the test that says so.
func TestListenConsoleRefusesToReplaceANonSocket(t *testing.T) {
	st := newSecurityTestStore(t)
	dir := shortTempDir(t)
	path := filepath.Join(dir, ConsoleSocketName)
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("seed the path: %v", err)
	}
	if _, err := ListenConsole(dir, NewConsole(st, nil, nil), nil); err == nil {
		t.Fatal("ListenConsole replaced a regular file at the socket path")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the regular file at the socket path was removed: %v", err)
	}
}

// TestListenConsoleReplacesAStaleSocket is the other side of the same guard: a
// socket left behind by a process that did not shut down cleanly must not stop
// the next boot from binding, or one unclean stop costs the box its recovery
// path permanently.
func TestListenConsoleReplacesAStaleSocket(t *testing.T) {
	st := newSecurityTestStore(t)
	dir := shortTempDir(t)
	path := filepath.Join(dir, ConsoleSocketName)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("bind the stale socket: %v", err)
	}
	stale.SetUnlinkOnClose(false)
	_ = stale.Close()

	ln, err := ListenConsole(dir, NewConsole(st, nil, nil), nil)
	if err != nil {
		t.Fatalf("ListenConsole over a stale socket: %v", err)
	}
	defer ln.Close()
}

// TestConsoleSocketRefusesWhenThePeerCredentialCannotBeRead is the fail-closed
// branch: no credential, no admission. A listener that treated an unreadable
// credential as uid 0 would admit everyone on any platform where the syscall is
// unavailable, which is exactly what peercred_unsupported.go exists to prevent.
func TestConsoleSocketRefusesWhenThePeerCredentialCannotBeRead(t *testing.T) {
	st := newSecurityTestStore(t)
	ln := newConsoleListener(t, st, nil, func(*net.UnixConn) (int, error) {
		return -1, errors.New("no peer-credential mechanism")
	})
	if got := dialConsole(t, ln.Path(), ConsoleRequest{Verb: ConsoleVerbServiceStatus}); len(got) != 0 {
		t.Fatalf("a peer whose credential could not be read received %d response bytes (%q)", len(got), got)
	}
}

// TestConsoleGrantIssueMintsACredentialResetGrant drives the `grant.issue` verb
// over the real socket and confirms the grant it reports was actually persisted
// — with `issued_via: console`, which is the field SEC-034's record exists to
// carry and the reason the console path is distinguishable from the api one.
func TestConsoleGrantIssueMintsACredentialResetGrant(t *testing.T) {
	ctx := t.Context()
	sink := &recordingSink{}
	st, auditor := newAuditedTestStore(t, sink)
	ln := newConsoleListener(t, st, auditor, func(*net.UnixConn) (int, error) { return 0, nil })

	target, err := st.CreatePrincipal(ctx, KindUser, "target")
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := st.PutPasswordCredential(ctx, target.PrincipalID, "target@example.invalid", "the-password-being-replaced"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	raw := dialConsole(t, ln.Path(), ConsoleRequest{
		Verb: ConsoleVerbGrantIssue,
		Params: map[string]any{
			"purpose":             PurposeCredentialReset,
			"target_principal_id": target.PrincipalID,
			"base_url":            "https://box.example.invalid",
		},
	})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	grantID, _ := body["grant_id"].(string)
	code, _ := body["code"].(string)
	if grantID == "" || code == "" {
		t.Fatalf("grant.issue returned no grant: %v", body)
	}
	grant, err := st.Grant(ctx, grantID)
	if err != nil {
		t.Fatalf("the reported grant is not in the store: %v", err)
	}
	if grant.Purpose != PurposeCredentialReset {
		t.Errorf("persisted purpose = %q, want %q", grant.Purpose, PurposeCredentialReset)
	}
	if grant.IssuedVia != IssuedViaConsole {
		t.Errorf("persisted issued_via = %q, want %q (SEC-030)", grant.IssuedVia, IssuedViaConsole)
	}
	// SEC-034: the mint's own record, carrying both fields.
	created := sink.payloads(ActionGrantCreated)
	if len(created) != 1 {
		t.Fatalf("grant.created records = %d, want 1 (SEC-034)", len(created))
	}
	if created[0]["purpose"] != PurposeCredentialReset || created[0]["issued_via"] != IssuedViaConsole {
		t.Errorf("the record carries purpose=%v issued_via=%v", created[0]["purpose"], created[0]["issued_via"])
	}
	// SEC-051: the code is in the response and nowhere else.
	for _, rec := range sink.snapshot() {
		blob, _ := json.Marshal(rec)
		if strings.Contains(string(blob), code) {
			t.Fatalf("an audit record carries the one-time code: %s", blob)
		}
	}
	// And the grant it redeems really is redeemable by the target, so the verb
	// produced a working handoff rather than a well-shaped one.
	if _, err := st.RedeemCredentialResetGrant(ctx, code, "the-passphrase-the-target-chooses", ""); err != nil {
		t.Fatalf("the console-issued code did not redeem: %v", err)
	}
}

// TestConsoleGrantIssueRefusesARecoveryPurpose is the SEC-063 guard.
//
// A console-issued `recovery` grant carries an unconditional obligation to
// notify every owner and raise a persistent banner. This build has neither, so
// issuing one would produce a root-issued recovery credential nobody is told
// about — the exact artifact SEC-063 exists to make impossible. The refusal is
// UNIMPLEMENTED, distinct from CONSOLE_VERB_NOT_ALLOWED, because the verb is
// inside SEC-075's set and it is this build that cannot serve it.
func TestConsoleGrantIssueRefusesARecoveryPurpose(t *testing.T) {
	ctx := t.Context()
	st := newSecurityTestStore(t)
	ln := newConsoleListener(t, st, nil, func(*net.UnixConn) (int, error) { return 0, nil })

	target, err := st.CreatePrincipal(ctx, KindUser, "target")
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := st.PutPasswordCredential(ctx, target.PrincipalID, "target@example.invalid", "pw"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	raw := dialConsole(t, ln.Path(), ConsoleRequest{
		Verb:   ConsoleVerbGrantIssue,
		Params: map[string]any{"purpose": PurposeRecovery, "target_principal_id": target.PrincipalID},
	})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	if body["code"] != ErrCodeUnimplemented {
		t.Fatalf("code = %v, want %q", body["code"], ErrCodeUnimplemented)
	}
	// Nothing was minted.
	grants, err := st.CountGrants(ctx, PurposeRecovery)
	if err != nil {
		t.Fatalf("count recovery grants: %v", err)
	}
	if grants != 0 {
		t.Fatalf("a refused recovery issuance minted %d grant(s)", grants)
	}
}

// TestConsoleGrantIssueRejectsANonBooleanOptOut is SEC-053's opt-out being
// honoured or refused, never silently ignored: a caller who sends
// `"keep_existing_sessions": "true"` (a string) must be told, because the
// silently-ignored reading of that value evicts every session the target holds.
func TestConsoleGrantIssueRejectsANonBooleanOptOut(t *testing.T) {
	ctx := t.Context()
	st := newSecurityTestStore(t)
	ln := newConsoleListener(t, st, nil, func(*net.UnixConn) (int, error) { return 0, nil })

	target, err := st.CreatePrincipal(ctx, KindUser, "target")
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := st.PutPasswordCredential(ctx, target.PrincipalID, "target@example.invalid", "pw"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	raw := dialConsole(t, ln.Path(), ConsoleRequest{
		Verb: ConsoleVerbGrantIssue,
		Params: map[string]any{
			"purpose":                PurposeCredentialReset,
			"target_principal_id":    target.PrincipalID,
			"keep_existing_sessions": "true",
		},
	})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	if body["code"] != ErrCodeValidationFailed {
		t.Fatalf("code = %v, want %q", body["code"], ErrCodeValidationFailed)
	}
}

// TestConsoleGrantRedeemIsNotServed pins the deliberate gap. `grant.redeem` is
// inside SEC-075's verb set and this build does not serve it; the distinction
// that matters is UNIMPLEMENTED rather than CONSOLE_VERB_NOT_ALLOWED, so the
// audit trail never reads "the contract forbids this" where the truth is "this
// build cannot do it".
func TestConsoleGrantRedeemIsNotServed(t *testing.T) {
	st := newSecurityTestStore(t)
	ln := newConsoleListener(t, st, nil, func(*net.UnixConn) (int, error) { return 0, nil })
	raw := dialConsole(t, ln.Path(), ConsoleRequest{Verb: ConsoleVerbGrantRedeem})
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	if body["code"] != ErrCodeUnimplemented {
		t.Fatalf("code = %v, want %q", body["code"], ErrCodeUnimplemented)
	}
}
