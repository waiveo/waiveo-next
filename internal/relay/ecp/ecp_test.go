package ecp

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
)

// seenRequest is what the fake ECP server records about the one request it
// expects to see for a single Dispatch call.
type seenRequest struct {
	method string
	path   string
}

// newFakeECP starts an httptest.Server that records the method+path of every
// request it receives and answers with status (default 200 if status is 0
// when passed via newFakeECPStatus). recv receives one seenRequest per call.
func newFakeECP(t *testing.T, status int) (*httptest.Server, <-chan seenRequest) {
	t.Helper()
	ch := make(chan seenRequest, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath (not the decoded URL.Path) preserves the percent-encoding
		// exactly as sent on the wire, so the keypress-escaping case can assert
		// the request actually carried an escaped key.
		ch <- seenRequest{method: r.Method, path: r.URL.EscapedPath()}
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

// targetFromServer derives the Target (host, port) Dispatch must hit to reach
// srv.
func targetFromServer(t *testing.T, srv *httptest.Server) Target {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing httptest server URL %q: %v", srv.URL, err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("splitting host:port from %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing port %q: %v", portStr, err)
	}
	return Target{Host: host, Port: port}
}

// TestDispatchVocabCommands is the table-driven case: each media-player
// vocabulary command (REG-066) dispatched with valid params must reach the
// ECP server as the exact method+path REG-066/the ECP command mapping
// requires, and Dispatch must return nil (the surface treats that as
// {ok:true}, REL-112).
func TestDispatchVocabCommands(t *testing.T) {
	cases := []struct {
		name       string
		command    string
		params     map[string]any
		wantMethod string
		wantPath   string
	}{
		{
			name:       "launch",
			command:    "launch",
			params:     map[string]any{"channel": "12345"},
			wantMethod: http.MethodPost,
			wantPath:   "/launch/12345",
		},
		{
			name:       "home",
			command:    "home",
			params:     nil,
			wantMethod: http.MethodPost,
			wantPath:   "/keypress/Home",
		},
		{
			name:       "keypress escaping",
			command:    "keypress",
			params:     map[string]any{"key": "Lit_%20"},
			wantMethod: http.MethodPost,
			wantPath:   "/keypress/Lit_%2520",
		},
		{
			name:       "power on",
			command:    "power",
			params:     map[string]any{"state": "on"},
			wantMethod: http.MethodPost,
			wantPath:   "/keypress/PowerOn",
		},
		{
			name:       "power off",
			command:    "power",
			params:     map[string]any{"state": "off"},
			wantMethod: http.MethodPost,
			wantPath:   "/keypress/PowerOff",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, recv := newFakeECP(t, http.StatusOK)
			target := targetFromServer(t, srv)
			c := New(map[string]Target{"entity-1": target})

			if err := c.Dispatch("entity-1", tc.command, tc.params); err != nil {
				t.Fatalf("Dispatch(%q) = %v, want nil", tc.command, err)
			}

			select {
			case got := <-recv:
				if got.method != tc.wantMethod || got.path != tc.wantPath {
					t.Errorf("server saw %s %s, want %s %s", got.method, got.path, tc.wantMethod, tc.wantPath)
				}
			default:
				t.Fatal("server never received a request")
			}
		})
	}
}

// TestDispatchLaunchMissingChannel: launch with no/empty channel must fail
// closed with COMMAND_UNRESOLVED without ever hitting the device (REL-113 —
// this controller re-validates rather than trusting the surface's own check).
func TestDispatchLaunchMissingChannel(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"absent", map[string]any{}},
		{"empty string", map[string]any{"channel": ""}},
		{"wrong type", map[string]any{"channel": 12345}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, recv := newFakeECP(t, http.StatusOK)
			target := targetFromServer(t, srv)
			c := New(map[string]Target{"entity-1": target})

			err := c.Dispatch("entity-1", "launch", tc.params)
			assertControllerError(t, err, "COMMAND_UNRESOLVED")

			select {
			case got := <-recv:
				t.Fatalf("server received a request (%+v) for an invalid launch — must fail closed before dispatch", got)
			default:
			}
		})
	}
}

// TestDispatchPowerBadState: power with a state outside {"on","off"} fails
// closed COMMAND_UNRESOLVED without touching the device.
func TestDispatchPowerBadState(t *testing.T) {
	srv, recv := newFakeECP(t, http.StatusOK)
	target := targetFromServer(t, srv)
	c := New(map[string]Target{"entity-1": target})

	err := c.Dispatch("entity-1", "power", map[string]any{"state": "standby"})
	assertControllerError(t, err, "COMMAND_UNRESOLVED")

	select {
	case got := <-recv:
		t.Fatalf("server received a request (%+v) for a bad power state — must fail closed before dispatch", got)
	default:
	}
}

// TestDispatchKeypressMissingKey mirrors the launch/channel case for
// keypress's required key param.
func TestDispatchKeypressMissingKey(t *testing.T) {
	srv, _ := newFakeECP(t, http.StatusOK)
	target := targetFromServer(t, srv)
	c := New(map[string]Target{"entity-1": target})

	err := c.Dispatch("entity-1", "keypress", map[string]any{})
	assertControllerError(t, err, "COMMAND_UNRESOLVED")
}

// TestDispatchUnknownEntity: an entityID absent from the Controller's own
// targets map has nowhere to dispatch to, so Dispatch fails closed
// COMMAND_UNRESOLVED (I2: a config/wiring gap — this controller's own address
// book has no Target for the entity — not a device that was reached and
// failed, so it must not share COMMAND_TARGET_UNREACHABLE with that case).
// Distinct from the device-command surface's own entity resolution
// (REL-112/113), which this controller sits behind and uses the identical
// code for.
func TestDispatchUnknownEntity(t *testing.T) {
	c := New(map[string]Target{})
	err := c.Dispatch("ghost", "home", nil)
	assertControllerError(t, err, "COMMAND_UNRESOLVED")
}

// TestDispatchUnknownCommand: a command name outside REG-066's media-player
// vocabulary fails closed COMMAND_UNRESOLVED.
func TestDispatchUnknownCommand(t *testing.T) {
	srv, recv := newFakeECP(t, http.StatusOK)
	target := targetFromServer(t, srv)
	c := New(map[string]Target{"entity-1": target})

	err := c.Dispatch("entity-1", "blast", nil)
	assertControllerError(t, err, "COMMAND_UNRESOLVED")

	select {
	case got := <-recv:
		t.Fatalf("server received a request (%+v) for an unknown command — must fail closed before dispatch", got)
	default:
	}
}

// TestDispatchNon2xxResponse: the device responding non-2xx to a
// well-formed request is a dispatch failure — the error must name the status
// and the command — but not a taxonomy-coded ControllerError (no code names
// "device rejected the command"; the surface buckets a plain error INTERNAL).
func TestDispatchNon2xxResponse(t *testing.T) {
	srv, _ := newFakeECP(t, http.StatusInternalServerError)
	target := targetFromServer(t, srv)
	c := New(map[string]Target{"entity-1": target})

	err := c.Dispatch("entity-1", "home", nil)
	if err == nil {
		t.Fatal("Dispatch = nil, want an error for a non-2xx ECP response")
	}
	var ce *deviceplane.ControllerError
	if errors.As(err, &ce) {
		t.Fatalf("Dispatch returned a ControllerError (%+v); want a plain error bucketed INTERNAL by the surface", ce)
	}
	msg := err.Error()
	if !strings.Contains(msg, "500") || !strings.Contains(msg, "home") {
		t.Errorf("error message = %q, want it to mention the status (500) and the command (home)", msg)
	}
}

// TestDispatchConnectionRefused: a Target whose ECP port refuses connections
// (device off/unreachable) fails closed COMMAND_TARGET_UNREACHABLE — the
// relay could not reach the device to attempt the command at all.
func TestDispatchConnectionRefused(t *testing.T) {
	srv, _ := newFakeECP(t, http.StatusOK)
	target := targetFromServer(t, srv)
	srv.Close() // now nothing listens on target's port

	c := New(map[string]Target{"entity-1": target})
	err := c.Dispatch("entity-1", "home", nil)
	assertControllerError(t, err, "COMMAND_TARGET_UNREACHABLE")
}

// TestDefaultPortZero: a Target with Port 0 dispatches to ECP's well-known
// default port 8060, not port 0.
func TestDefaultPortZero(t *testing.T) {
	if got := (Target{Host: "10.0.0.5"}).addr(); got != "10.0.0.5:8060" {
		t.Errorf("addr() = %q, want 10.0.0.5:8060", got)
	}
}

// TestAddrBracketsIPv6 asserts M1's fix: an IPv6 literal host is correctly
// bracketed in the rendered authority rather than producing a malformed one
// (e.g. "::1:8060", which is not parseable as host "::1" port "8060").
func TestAddrBracketsIPv6(t *testing.T) {
	if got := (Target{Host: "::1", Port: 8060}).addr(); got != "[::1]:8060" {
		t.Errorf("addr() = %q, want [::1]:8060", got)
	}
}

// TestWithHTTPClientNilIgnored asserts M2's fix: WithHTTPClient(nil) must not
// zero out Controller.client (which would panic the next Dispatch inside a
// nil *http.Client.Do) — New's constructed default is retained instead,
// matching ecppoll.WithHTTPClient's own nil-guard convention.
func TestWithHTTPClientNilIgnored(t *testing.T) {
	srv, recv := newFakeECP(t, http.StatusOK)
	target := targetFromServer(t, srv)
	c := New(map[string]Target{"entity-1": target}, WithHTTPClient(nil))

	if err := c.Dispatch("entity-1", "home", nil); err != nil {
		t.Fatalf("Dispatch with WithHTTPClient(nil) = %v, want nil (nil client must be ignored)", err)
	}
	select {
	case <-recv:
	default:
		t.Fatal("server never received a request")
	}
}

// countingListener wraps a net.Listener and counts every accepted TCP
// connection, used by TestDispatchDrainsBodyForConnectionReuse to detect
// whether Dispatch's http.Client.Transport was able to reuse a connection
// across sequential requests.
type countingListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return conn, err
}

// TestDispatchDrainsBodyForConnectionReuse asserts M3's fix: Dispatch must
// drain a response body before Close()ing it so http.Client's Transport can
// return the underlying TCP connection to its keep-alive pool. A non-2xx
// response with a non-trivial body (large enough that an undrained Close
// forces the Transport to tear down the connection rather than reuse it) is
// dispatched several times in a row against the same server; if the body is
// drained, every dispatch after the first should reuse the one accepted
// connection instead of opening a fresh one each time.
func TestDispatchDrainsBodyForConnectionReuse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/keypress/Home", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(strings.Repeat("x", 8192)))
	})
	srv := httptest.NewUnstartedServer(mux)
	cl := &countingListener{Listener: srv.Listener}
	srv.Listener = cl
	srv.Start()
	t.Cleanup(srv.Close)

	target := targetFromServer(t, srv)
	c := New(map[string]Target{"entity-1": target})

	for i := 0; i < 5; i++ {
		if err := c.Dispatch("entity-1", "home", nil); err == nil {
			t.Fatal("Dispatch = nil, want an error for a non-2xx ECP response")
		}
	}

	if got := cl.accepted.Load(); got > 1 {
		t.Errorf("server accepted %d TCP connections across 5 sequential Dispatch calls, want 1 (response body must be drained before Close so the client's Transport can reuse the connection)", got)
	}
}

// assertControllerError fails t unless err is a *deviceplane.ControllerError
// carrying wantCode.
func assertControllerError(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Dispatch = nil, want a ControllerError %s", wantCode)
	}
	var ce *deviceplane.ControllerError
	if !errors.As(err, &ce) {
		t.Fatalf("Dispatch error = %v (%T), want a *deviceplane.ControllerError", err, err)
	}
	if ce.Code != wantCode {
		t.Errorf("ControllerError.Code = %q, want %q", ce.Code, wantCode)
	}
}
