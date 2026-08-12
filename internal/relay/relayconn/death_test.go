package relayconn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// The death-path tests. HV-22 was diagnosed against a client whose ONLY
// death detector was the read loop: a failed write returned its error to
// the caller and told the connection nothing, so one-way reporters wrote to
// a dead socket indefinitely and the supervisor above never learned. These
// pin the property that closed it — every half that can notice death marks
// the connection dead, exactly once, with the first cause, and releases the
// socket.
//
// They drive a REAL websocket against a real server, not a hand-built
// Client: the bug lived in how the transport's halves interact, and a
// struct literal with a nil *websocket.Conn cannot exercise that (fail
// calls CloseNow on it). newLiveClient is the seam.

// wsEcho is a bare relay/1-shaped server: it upgrades, then hands the raw
// connection to fn. It does no handshake — these tests need a live socket
// with a read loop on it, not an authenticated peer.
func wsEcho(t *testing.T, fn func(ctx context.Context, ws *websocket.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{wire.Subprotocol},
		})
		if err != nil {
			return
		}
		fn(r.Context(), ws)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newClient dials srv and returns a Client wired as Dial wires one, minus
// the authenticated handshake. The heartbeat is never started, so a test
// about the write path is not racing a 20s pinger.
func newClient(t *testing.T, srv *httptest.Server, readLoop bool) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{
		Subprotocols: []string{wire.Subprotocol},
	})
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	c := &Client{
		ws:      ws,
		relayID: "relay-test",
		pending: map[string]chan wire.Frame{},
		done:    make(chan struct{}),
		nudgeCh: make(chan struct{}, 1),
	}
	if readLoop {
		go c.readLoop()
	}
	t.Cleanup(func() { _ = c.ws.CloseNow() })
	return c
}

// newLiveClient is the ordinary shape: read loop running, as Dial leaves it.
func newLiveClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return newClient(t, srv, true)
}

// newWriteOnlyClient leaves the read loop OFF, so the write half is the ONLY
// thing that can notice death.
//
// That is not an artificial configuration — it is HV-22's, exactly: a read
// that stays silent while writes fail. With both halves live, "which half
// recorded the cause" is a race no test can assert (a failed write makes the
// transport close its own conn, and the read loop then trips over that and
// may well get there first). Removing one half is what makes the other's
// wiring observable.
func newWriteOnlyClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return newClient(t, srv, false)
}

// writeUntilFailure drives frames at c until one fails, bounded. The first
// write to a hard-closed peer often lands in the socket buffer and only the
// next reports EPIPE, so a single attempt is not a reliable observation.
func writeUntilFailure(t *testing.T, c *Client) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f, err := wire.NewFrame(wire.FrameTypeScreenStatus, "id", c.relayID, wire.ScreenStatusBody{Screens: []wire.ScreenStatusEntry{}})
		if err != nil {
			t.Fatalf("build frame: %v", err)
		}
		if err := c.send(f); err != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no write ever failed against a hard-closed peer; the test cannot observe what it is about")
	return nil
}

func waitDone(t *testing.T, c *Client, within time.Duration, what string) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(within):
		t.Fatalf("%s: Done never closed within %s — the connection was never marked dead", what, within)
	}
}

// TestWriteFailureMarksTheConnectionDead is HV-22's asymmetry, stated as a
// test: a frame that cannot be written must make the CONNECTION dead, not
// merely return an error to whoever tried to write it.
//
// The read loop is deliberately NOT running. With it running this test
// passes no matter what sendCtx does — the read loop notices the same dead
// peer and closes done itself — which would make it a test that cannot fail
// for the reason it claims. (Confirmed by mutation: deleting the write-side
// fail left the read-loop version green.) Silent reads beside failing
// writes is also HV-22's own shape, so the isolation is the realistic case,
// not a contrivance.
func TestWriteFailureMarksTheConnectionDead(t *testing.T) {
	closed := make(chan struct{})
	srv := wsEcho(t, func(_ context.Context, ws *websocket.Conn) {
		_ = ws.CloseNow() // peer gone: no reader, no close handshake
		close(closed)
	})
	c := newWriteOnlyClient(t, srv)
	<-closed

	writeUntilFailure(t, c)

	waitDone(t, c, 2*time.Second, "after a failed write with no read loop running")
	if err := c.Err(); err == nil {
		t.Fatal("Err() is nil after a failed write; the connection does not know it is dead")
	}
}

// TestErrNamesTheHalfThatNoticed: Err must say WHICH half of the transport
// failed. "The connection is down" without that is the report HV-22 gave an
// operator for two and a half hours, and read-failed and write-failed send
// you to different places.
func TestErrNamesTheHalfThatNoticed(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		srv := wsEcho(t, func(_ context.Context, ws *websocket.Conn) {
			_ = ws.Close(websocket.StatusNormalClosure, "")
		})
		c := newLiveClient(t, srv)
		waitDone(t, c, 5*time.Second, "peer closed the connection")
		if got := c.Err(); got == nil || !strings.Contains(got.Error(), "on the read side") {
			t.Fatalf("Err() = %v, want an error naming the READ side", got)
		}
	})

	t.Run("write", func(t *testing.T) {
		// HV-22's own configuration: the read half silent (here, absent)
		// while writes fail. It is also the only way to observe this label
		// deterministically — see newWriteOnlyClient.
		closed := make(chan struct{})
		srv := wsEcho(t, func(_ context.Context, ws *websocket.Conn) {
			_ = ws.CloseNow()
			close(closed)
		})
		c := newWriteOnlyClient(t, srv)
		<-closed

		writeUntilFailure(t, c)
		waitDone(t, c, 2*time.Second, "after a failed write with no read loop running")
		if got := c.Err(); got == nil || !strings.Contains(got.Error(), "on the write side") {
			t.Fatalf("Err() = %v, want an error naming the WRITE side", got)
		}
	})

	t.Run("heartbeat", func(t *testing.T) {
		// The REAL loop, not a direct fail() call. An earlier version of
		// this subtest called c.fail(sideHeartbeat, …) itself, which tested
		// fail's labelling and said nothing about whether the heartbeat is
		// wired to it — mutation confirmed that: reverting the heartbeat to
		// a bare CloseNow survived.
		//
		// The peer is up but never READS, so it never answers a ping: the
		// pong times out, which is the half-open case this loop exists for
		// (Config.PingInterval's doc). No read loop, so the heartbeat is
		// the only half that can notice.
		c := newWriteOnlyClient(t, wsEcho(t, func(ctx context.Context, ws *websocket.Conn) { <-ctx.Done() }))
		c.startHeartbeat(10*time.Millisecond, 30*time.Millisecond)

		waitDone(t, c, 5*time.Second, "after the ping round-trip failed")
		if got := c.Err(); got == nil || !strings.Contains(got.Error(), "on the heartbeat side") {
			t.Fatalf("Err() = %v, want an error naming the HEARTBEAT side", got)
		}
	})
}

// TestFirstCauseWins: a later half's failure must not overwrite the cause
// already recorded. A peer that exits fails a read and a write within
// microseconds of each other, and whichever goroutine is scheduled LAST
// would otherwise be the one an operator reads.
//
// Stated sequentially on purpose. "Which goroutine won a race" is not a
// property a test can assert; "a second failure does not overwrite the
// first" is, and it is the property that matters.
func TestFirstCauseWins(t *testing.T) {
	c := newLiveClient(t, wsEcho(t, func(ctx context.Context, ws *websocket.Conn) { <-ctx.Done() }))

	first := errors.New("the first cause")
	c.fail(sideRead, first)
	c.fail(sideWrite, errors.New("a later cause"))
	c.fail(sideHeartbeat, errors.New("a later cause still"))

	got := c.Err()
	if got == nil || !errors.Is(got, first) {
		t.Fatalf("Err() = %v, want the FIRST cause (%v) preserved", got, first)
	}
	if !strings.Contains(got.Error(), "on the read side") {
		t.Fatalf("Err() = %v, want the first cause's own side label, not a later one's", got)
	}
}

// TestConcurrentFailuresCloseDoneExactlyOnce: many halves failing at once is
// the NORMAL case, and a second close(done) panics — which would take the
// relay down over precisely the race the death path exists to handle.
//
// It asserts what IS knowable under a race: no panic, done closed, Err()
// non-nil and one of the causes actually offered (never a torn or zero
// value). Which one wins is scheduling, and this test deliberately does not
// claim otherwise.
func TestConcurrentFailuresCloseDoneExactlyOnce(t *testing.T) {
	c := newLiveClient(t, wsEcho(t, func(ctx context.Context, ws *websocket.Conn) { <-ctx.Done() }))

	readCause := errors.New("read cause")
	writeCause := errors.New("write cause")
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				c.fail(sideRead, readCause)
				return
			}
			c.fail(sideWrite, writeCause)
		}(i)
	}
	close(start)
	wg.Wait()

	select {
	case <-c.Done():
	default:
		t.Fatal("Done did not close after 32 concurrent failures")
	}
	got := c.Err()
	if got == nil {
		t.Fatal("Err() is nil while Done is closed: the two are supposed to be one atomic fact")
	}
	if !errors.Is(got, readCause) && !errors.Is(got, writeCause) {
		t.Fatalf("Err() = %v, want one of the two causes actually offered", got)
	}
}

// TestDeathReleasesTheSocket: the read loop used to close done and leave the
// underlying socket open forever — measured on a real relay as one fd stuck
// in CLOSE_WAIT per app-peer restart, holding the dead 4-tuple for the life
// of the process. A relay whose peer flaps is then on a slow path to fd
// exhaustion.
//
// Observed from the SERVER side, which is the only place the release is
// visible without reaching into the fd table: the accepted connection's
// read returns once the client's socket is really gone.
func TestDeathReleasesTheSocket(t *testing.T) {
	gone := make(chan struct{})
	srv := wsEcho(t, func(_ context.Context, ws *websocket.Conn) {
		// Read until the client's socket goes away.
		for {
			if _, _, err := ws.Read(context.Background()); err != nil {
				close(gone)
				return
			}
		}
	})
	c := newLiveClient(t, srv)

	c.fail(sideWrite, errors.New("write failed"))

	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the client's socket close: a dead connection is leaking its fd")
	}
}

// TestDeathUnblocksAnInFlightPull: fail drains pending, so a Pull waiting on
// a reply returns at once with the real cause instead of waiting out
// requestTimeout. Without the drain a relay that lost its peer mid-pull sits
// blocked for ten seconds per attempt with nothing to show for it.
func TestDeathUnblocksAnInFlightPull(t *testing.T) {
	c := newLiveClient(t, wsEcho(t, func(ctx context.Context, ws *websocket.Conn) {
		for {
			if _, _, err := ws.Read(context.Background()); err != nil {
				return
			}
		}
	}))

	type result struct {
		err     error
		elapsed time.Duration
	}
	res := make(chan result, 1)
	go func() {
		start := time.Now()
		_, err := c.Pull("", nil)
		res <- result{err, time.Since(start)}
	}()

	// Let the pull register itself before killing the connection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.pending)
		c.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	c.fail(sideRead, errors.New("peer vanished"))

	select {
	case got := <-res:
		if got.err == nil {
			t.Fatal("Pull returned no error after the connection died")
		}
		if got.elapsed >= requestTimeout {
			t.Fatalf("Pull took %v to notice a dead connection; it waited out requestTimeout instead of being drained", got.elapsed)
		}
	case <-time.After(requestTimeout + 2*time.Second):
		t.Fatal("Pull never returned after the connection died")
	}
}

// TestEncodeFailureDoesNotKillALiveConnection is the mirror the write-death
// change needs: an unencodable frame is this side's own bug and says
// nothing about the transport — nothing left the socket. Tearing the
// connection down over it would turn a local programming error into a fleet
// disconnect.
func TestEncodeFailureDoesNotKillALiveConnection(t *testing.T) {
	c := newLiveClient(t, wsEcho(t, func(ctx context.Context, ws *websocket.Conn) { <-ctx.Done() }))

	// A frame json.Marshal refuses: json.RawMessage re-validates on the way
	// out, so a Body holding invalid JSON fails to encode.
	unencodable := wire.Frame{Type: wire.FrameTypeScreenStatus, ID: "id", Body: json.RawMessage("{not json")}

	if err := c.send(unencodable); err == nil {
		t.Fatal("an unencodable frame was accepted; the test cannot observe what it is about")
	}
	select {
	case <-c.Done():
		t.Fatal("an ENCODE failure killed the connection; only a failed WRITE means the transport is dead")
	case <-time.After(200 * time.Millisecond):
	}
	if err := c.Err(); err != nil {
		t.Fatalf("Err() = %v after a local encode failure, want nil (the connection is fine)", err)
	}
}

// TestPullRefusesOnceTheConnectionIsDead: the connErr gate is checked under
// the same mutex fail sets it under, so a Pull can never register a pending
// entry that nothing will ever drain.
func TestPullRefusesOnceTheConnectionIsDead(t *testing.T) {
	c := newLiveClient(t, wsEcho(t, func(ctx context.Context, ws *websocket.Conn) { <-ctx.Done() }))
	c.fail(sideRead, errors.New("peer vanished"))

	if _, err := c.Pull("", nil); err == nil || !strings.Contains(err.Error(), "connection is down") {
		t.Fatalf("Pull on a dead connection = %v, want a connection-is-down refusal", err)
	}
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("Pull left %d pending entr(ies) on a dead connection; nothing will ever drain them", n)
	}
}

// TestFailIgnoresANilCause: fail(nil) must not mark a live connection dead.
// heartbeat.Run returns nil on an orderly shutdown, and a caller that passed
// that through would kill every connection at teardown.
func TestFailIgnoresANilCause(t *testing.T) {
	c := newLiveClient(t, wsEcho(t, func(ctx context.Context, ws *websocket.Conn) { <-ctx.Done() }))
	c.fail(sideHeartbeat, nil)
	select {
	case <-c.Done():
		t.Fatal("fail(nil) killed a live connection")
	default:
	}
	if err := c.Err(); err != nil {
		t.Fatalf("Err() = %v after fail(nil), want nil", err)
	}
}
