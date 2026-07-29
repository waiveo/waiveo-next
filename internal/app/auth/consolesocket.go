package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// consolesocket.go is the console binding's TRANSPORT — security-model/1
// SEC-070 (a Unix domain socket, host-local, never the network), SEC-071 (the
// socket file is mode 0700), SEC-072 (the peer's effective uid is read off the
// live connection and must be 0), SEC-074 (request and response bodies reuse
// api/1's own conventions) and SEC-078 (it does not depend on TLS, because TLS
// is one of the things it exists to recover from).
//
// It is the half console.go's own doc said was missing. That file owns the
// DECISION made from a peer credential and the verb policy applied after it;
// this file owns reading the credential, which is a per-OS syscall
// (peercred_linux.go / peercred_darwin.go), and moving bytes.
//
// # Nothing here decides policy
//
// The split is deliberate and load-bearing for conformance: Console.Dispatch
// still takes the peer uid as a PARAMETER, exactly as it did when nothing called
// it, so the frozen corpus case that drives the admission rule with an injected
// `peer_uid` keeps driving the same code this socket drives. This file adds a
// caller; it does not add a second copy of the rule.
//
// # What actually gates admission, in order
//
//  1. The socket lives in a directory this process creates 0700 and refuses to
//     use if it is not 0700 and owned by this process's own euid. That gate is
//     established BEFORE the socket exists.
//  2. The socket file itself is chmod'd 0700 before the accept loop starts
//     (SEC-071).
//  3. Every accepted connection's peer credential is read and compared against
//     uid 0 (SEC-072) before a single byte of the request is read.
//
// (1) is what closes the window (2) would otherwise leave open. `net.Listen`
// creates a socket file whose mode is `0777 &^ umask` — typically 0755, briefly
// world-connectable — and there is no way to pass a mode to bind(2) from Go.
// Chmod-after-bind is therefore a real TOCTOU, and it is closed by the
// containing directory rather than by winning the race: a process that cannot
// traverse a 0700 directory cannot reach the socket inside it whatever the
// socket's own mode says. (3) is the application-level check SEC-071 calls the
// other half of "a defense-in-depth pair, neither alone this contract's sole
// control", and it is the one that does not depend on filesystem permissions at
// all.

// ConsoleSocketName is the console binding's socket file, inside the deployment's
// auth state directory (DefaultAuthDir).
const ConsoleSocketName = "console.sock"

// DefaultAuthDir is where a self-hosted deployment keeps its security-model/1
// state: the principal/credential/session/role/grant database, the first-boot
// setup code, the clock floor, and the console binding's socket.
//
// It is declared here, in the package that owns all four, so a second component
// that needs to find the console socket resolves it from the same constant the
// app serves it from rather than from a copy of the string.
const DefaultAuthDir = ".dev/feeder-auth"

// consoleConnDeadline bounds one console conversation end to end.
//
// It is a wall-clock I/O deadline, not a policy window, which is why it does not
// read the app's clock floor: it exists so a peer that connects and then says
// nothing cannot pin a goroutine and a file descriptor forever. Nothing about
// authorization or expiry is decided from it.
const consoleConnDeadline = 10 * time.Second

// consoleMaxRequestBytes bounds one request body. A console request is a verb, a
// small params object and a trace id; anything larger is a mistake or an attempt
// to make the app allocate, and it is refused by running out of reader rather
// than by trusting a length field the sender supplied.
const consoleMaxRequestBytes = 64 << 10

// ConsoleListener serves Console over a Unix domain socket (SEC-070).
type ConsoleListener struct {
	console *Console
	ln      *net.UnixListener
	path    string
	logf    func(format string, args ...any)

	// peerUID reads the connected peer's effective uid. It is a field, and
	// unexported, so this package's own tests can drive the full socket path
	// including the uid-0 branch — which no test process that is not root can
	// otherwise reach — while no other package, and no deployment, can substitute
	// anything for the real syscall. ListenConsole always installs readPeerUID.
	peerUID func(*net.UnixConn) (int, error)

	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    chan struct{}
}

// ListenConsole opens the console binding's socket inside dir and returns a
// listener that is bound and correctly permissioned but not yet accepting; call
// Serve to start it and Close to stop.
//
// Bind-then-permission-then-accept is the order, and Serve being separate is
// what makes it enforceable: nothing is served until the caller has a listener
// whose socket mode has already been set and verified.
func ListenConsole(dir string, c *Console, logf func(format string, args ...any)) (*ConsoleListener, error) {
	if c == nil {
		return nil, errors.New("auth: the console binding needs a dispatcher")
	}
	if dir == "" {
		return nil, errors.New("auth: the console binding needs a state directory")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := prepareConsoleDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ConsoleSocketName)
	if err := checkConsoleSocketPathLength(path); err != nil {
		return nil, err
	}
	if err := clearStaleConsoleSocket(path); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("auth: bind the console binding at %s: %w", path, err)
	}
	// Go removes the socket file when the listener created it and is closed
	// normally; stated rather than assumed, because a leftover socket is what
	// clearStaleConsoleSocket has to reason about on the next boot.
	ln.SetUnlinkOnClose(true)
	if err := chmodAndVerify(path, 0o700); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &ConsoleListener{
		console: c, ln: ln, path: path, logf: logf,
		peerUID: readPeerUID,
		closed:  make(chan struct{}),
	}, nil
}

// prepareConsoleDir establishes the directory gate described in this file's own
// doc: 0700, owned by this process's effective uid, checked rather than assumed.
//
// An existing directory is re-chmod'd rather than trusted, and then RE-STATTED,
// because "I called chmod and it returned nil" is not the same claim as "the
// directory is 0700" on a path that could be a symlink to somewhere else.
func prepareConsoleDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth: create the console binding's directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("auth: set mode 0700 on %s: %w", dir, err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("auth: stat %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("auth: %s is not a directory", dir)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("auth: %s is mode %04o, not 0700; the console binding's socket would be reachable by another uid (SEC-071)", dir, perm)
	}
	// Ownership matters independently of mode: a 0700 directory owned by someone
	// else is one they can empty and repopulate, socket file included.
	if owner, ok := fileOwnerUID(fi); ok {
		if euid := os.Geteuid(); owner != euid {
			return fmt.Errorf("auth: %s is owned by uid %d, not by this process's uid %d; the console binding refuses to serve from a directory another uid controls", dir, owner, euid)
		}
	}
	return nil
}

// maxConsoleSocketPath is the shortest `sockaddr_un.sun_path` any platform this
// builds for provides: 104 bytes on Darwin, 108 on Linux, both including the
// terminating NUL. The smaller figure is used so a path that binds on the
// development machine cannot fail to bind on the deployment target or the other
// way round.
const maxConsoleSocketPath = 104

// checkConsoleSocketPathLength refuses an over-long socket path with a message
// that says what is wrong.
//
// bind(2) reports `EINVAL` for this, which surfaces from Go as "invalid
// argument" and names neither the limit nor the length — an operator whose
// state directory is one level too deep would see the app refuse to start with
// no indication that a path length is the reason. On a recovery path of last
// resort (SEC-078) that is the difference between a five-minute fix and an
// afternoon.
func checkConsoleSocketPathLength(path string) error {
	if len(path)+1 > maxConsoleSocketPath {
		return fmt.Errorf("auth: the console binding's socket path is %d bytes (%s); a Unix domain socket address holds at most %d including its terminating NUL — use a shorter state directory",
			len(path), path, maxConsoleSocketPath)
	}
	return nil
}

// clearStaleConsoleSocket removes a socket file left behind by a process that
// did not shut down cleanly, and REFUSES to touch anything that is not a socket.
//
// The refusal is the point. This function runs as the app's own uid — root, on a
// real box — against a path assembled from configuration, and "unlink whatever
// is here" is how a misconfigured directory turns a boot into data loss. A
// regular file, a directory or a symlink at this path is a deployment error to
// report, not something to delete.
func clearStaleConsoleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("auth: inspect %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("auth: %s exists and is not a socket (mode %v); refusing to replace it", path, fi.Mode())
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("auth: remove the stale console socket %s: %w", path, err)
	}
	return nil
}

// chmodAndVerify sets perm on path and then reads the mode back.
//
// Reading back is not paranoia about chmod: it is the only way to catch a
// filesystem that silently ignores permission bits, which is exactly the case
// where SEC-071's filesystem-level half of the defense-in-depth pair is not
// actually present. A deployment on such a filesystem should fail to start
// rather than run believing it has a gate it does not have.
func chmodAndVerify(path string, perm os.FileMode) error {
	if err := os.Chmod(path, perm); err != nil {
		return fmt.Errorf("auth: set mode %04o on %s: %w", perm, path, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("auth: stat %s: %w", path, err)
	}
	if got := fi.Mode().Perm(); got != perm {
		return fmt.Errorf("auth: %s is mode %04o after chmod, not %04o (SEC-071)", path, got, perm)
	}
	return nil
}

// Path is the socket file this listener is bound to.
func (l *ConsoleListener) Path() string { return l.path }

// Serve accepts connections until Close. It blocks; run it in a goroutine.
func (l *ConsoleListener) Serve() {
	for {
		conn, err := l.ln.AcceptUnix()
		if err != nil {
			select {
			case <-l.closed:
				return
			default:
			}
			// A transient accept failure must not silently end the recovery path
			// of last resort (SEC-078). It is reported and the loop continues; a
			// permanently broken listener will keep reporting, which is visible,
			// whereas a returned goroutine is not.
			l.logf("console binding: accept: %v", err)
			continue
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.serveConn(conn)
		}()
	}
}

// Close stops accepting, unlinks the socket, and waits for in-flight
// conversations to finish.
func (l *ConsoleListener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		close(l.closed)
		err = l.ln.Close()
	})
	l.wg.Wait()
	return err
}

// serveConn handles one console conversation: read the peer credential, admit or
// refuse, then (if admitted) one request and one response.
//
// ORDER IS THE REQUIREMENT. SEC-072: a connection from any uid other than 0
// "MUST be refused at accept time with no response body ... logged, not surfaced
// to the rejected peer beyond connection closure". So the credential is read
// before the request is, the refusal writes nothing at all, and the peer learns
// only that the connection closed. A non-root peer never has a byte of its input
// parsed by this process, which is a stronger property than refusing after
// decoding would give and is why the request is not read first to obtain a verb
// for the audit record.
func (l *ConsoleListener) serveConn(conn *net.UnixConn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(consoleConnDeadline))

	ctx := context.Background()
	uid, err := l.peerUID(conn)
	if err != nil {
		// The peer credential could not be read, so the admission rule has no
		// input. Fail closed: refuse exactly as a non-root peer is refused. A
		// negative uid can never equal consoleRootUID, so the refusal (and its
		// audit record) goes through the same one branch rather than a second
		// path that could drift from it.
		l.logf("console binding: refusing a connection whose peer credential could not be read: %v", err)
		l.console.Dispatch(ctx, -1, ConsoleRequest{})
		return
	}
	if uid != consoleRootUID {
		// SEC-072's own words: logged here, recorded by Dispatch, and nothing is
		// written back.
		l.logf("console binding: refused a connection from uid %d (only uid 0 is admitted)", uid)
		l.console.Dispatch(ctx, uid, ConsoleRequest{})
		return
	}

	var req ConsoleRequest
	if err := json.NewDecoder(io.LimitReader(conn, consoleMaxRequestBytes)).Decode(&req); err != nil {
		l.write(conn, EncodeConsoleResponse(ConsoleResponse{
			Code:   ErrCodeValidationFailed,
			Detail: "The request body is not a JSON ConsoleRequest object.",
		}, req.TraceID))
		return
	}
	resp := l.console.Dispatch(ctx, uid, req)
	// SEC-074's Trace-Id propagation: the id the CALLER supplied is echoed
	// unmodified, since a console request has no header channel to carry it.
	l.write(conn, EncodeConsoleResponse(resp, req.TraceID))
}

func (l *ConsoleListener) write(conn *net.UnixConn, body []byte) {
	if _, err := conn.Write(body); err != nil {
		l.logf("console binding: write response: %v", err)
	}
}

// EncodeConsoleResponse renders one dispatch outcome as the console binding's
// response body (SEC-074).
//
// There is no second error shape here and no second body format: a refusal is
// api/1's Problem document, built by apihttp.ProblemBody — the same function
// every /api/v1 Problem is built by — and a success is an `application/json`
// object carrying the verb's own result plus the trace id. The console binding
// has no header channel, so `trace_id` rides in the body on both paths, which is
// where the Problem shape already puts it.
//
// A SEC-072 refusal is never encoded: that refusal carries no response body at
// all, and serveConn writes nothing for it. Passing one here returns nil rather
// than inventing a body for it.
func EncodeConsoleResponse(resp ConsoleResponse, traceID string) []byte {
	if resp.Code == ErrCodeConsolePeerNotRoot {
		return nil
	}
	var body any
	if resp.Code != "" {
		status := consoleProblemStatus(resp.Code)
		body = apihttp.ProblemBody(traceID, status, resp.Code, http.StatusText(status), resp.Detail, "", nil)
	} else {
		out := map[string]any{"trace_id": traceID}
		for k, v := range resp.Body {
			// A verb's own result never overwrites the envelope's trace id; the
			// verb set is closed and none produces a `trace_id`, and this is what
			// keeps that true if one ever does.
			if k == "trace_id" {
				continue
			}
			out[k] = v
		}
		body = out
	}
	raw, err := json.Marshal(body)
	if err != nil {
		// Unreachable for the shapes above (strings, numbers, bools, maps and
		// slices of them), and still not a panic: the console binding is a
		// recovery path, and taking the process down because a response would not
		// marshal is the worst possible failure mode for one.
		return []byte(`{"type":"about:blank","title":"Internal Server Error","status":500,"code":"INTERNAL"}`)
	}
	return append(raw, '\n')
}

// consoleProblemStatus maps a console error code onto the HTTP status api/1's
// Problem shape carries. The console binding is not HTTP and does not send a
// status LINE — but the Problem document has a `status` member (API-010), and
// filling it with anything other than what the same refusal would carry over
// /api/v1 would be a second error shape in all but name (SEC-074).
func consoleProblemStatus(code string) int {
	switch code {
	case ErrCodeValidationFailed:
		return http.StatusUnprocessableEntity // API-013a
	case ErrCodeNotFound:
		return http.StatusNotFound
	case ErrCodeConsoleVerbNotAllowed:
		return http.StatusForbidden
	case ErrCodeUnimplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}
