package eventsse

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/events"
)

// ws_test.go drives events/1's WebSocket binding (EVT-090–096) through the real
// handler over a real socket: a real RFC 6455 handshake, real frames, real close
// codes. Nothing here calls into the binding's internals.
//
// Two conventions carried over from the SSE tests, for the same reasons:
// negative assertions end on a real signal (a sentinel event, a close frame, an
// EOF) rather than on a timer, and every duration passed to a read is a hang
// guard rather than a timing assumption. Where timing IS the behavior under test
// — the EVT-095 keepalive — the cadence is INJECTED (WithHeartbeat) and the test
// still waits on the real signal it produces.

// wsSubscriber is a driven events/1 WS client: the socket plus the small amount
// of frame handling a subscriber does.
type wsSubscriber struct {
	conn *websocket.Conn
}

// wsFrame is any server→client frame, decoded into the union of the fields the
// four server frame types carry (EVT-092/093/094/095).
type wsFrame struct {
	Type         string          `json:"type"`
	ResumeResult string          `json:"resume_result"`
	Event        events.Envelope `json:"event"`
	FromID       *string         `json:"from_id"`
	ToID         string          `json:"to_id"`
	Reason       string          `json:"reason"`
}

// wsURL is the ws:// form of an httptest server's /events/v1.
func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/events/v1"
}

// dialWS opens an events/1 WS connection as cred, offering the contract's
// subprotocol (EVT-090) and authenticating with the session cookie (EVT-110).
// It does NOT send the hello — a test that drives frame zero itself needs the
// raw connection.
func dialWS(t *testing.T, srv *httptest.Server, cred authtest.Credential) *wsSubscriber {
	t.Helper()
	header := http.Header{}
	header.Set("Cookie", auth.SessionCookieName+"="+cred.Token)
	conn, _, err := websocket.Dial(t.Context(), wsURL(srv), &websocket.DialOptions{
		Subprotocols: []string{events.Subprotocol},
		HTTPHeader:   header,
	})
	if err != nil {
		t.Fatalf("dialing the WS binding: %v", err)
	}
	if got := conn.Subprotocol(); got != events.Subprotocol {
		t.Fatalf("the handshake must negotiate %q (EVT-090); got %q", events.Subprotocol, got)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return &wsSubscriber{conn: conn}
}

// openWS dials and completes the handshake: it sends hello and returns the
// connection together with the hello-ack's resume_result (EVT-091/092).
func openWS(t *testing.T, srv *httptest.Server, cred authtest.Credential, hello events.HelloFrame) (*wsSubscriber, string) {
	t.Helper()
	c := dialWS(t, srv, cred)
	hello.Type = events.FrameTypeHello
	c.send(t, hello)
	ack := c.next(t, 2*time.Second)
	if ack.Type != events.FrameTypeHelloAck {
		t.Fatalf("the server's first frame must be a hello-ack (EVT-092); got %q", ack.Type)
	}
	return c, ack.ResumeResult
}

// send writes one client→server frame.
func (c *wsSubscriber) send(t *testing.T, frame any) {
	t.Helper()
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal client frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("writing a client frame: %v", err)
	}
}

// next reads one server frame, failing the test if none arrives within d — so a
// missing push is a hard failure, never a hang.
func (c *wsSubscriber) next(t *testing.T, d time.Duration) wsFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		t.Fatalf("reading a server frame: %v", err)
	}
	var f wsFrame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("a server frame must be UTF-8 JSON (EVT-002); got %v data=%s", err, data)
	}
	return f
}

// expectClose reads until the connection ends and asserts the server named
// wantReason in its close (EVT-096). Frames that arrive first are drained, so a
// close that follows legitimate delivery is still observed.
func (c *wsSubscriber) expectClose(t *testing.T, wantReason string, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	for {
		_, data, err := c.conn.Read(ctx)
		if err == nil {
			var f struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(data, &f)
			if f.Type == events.FrameTypeEvent || f.Type == events.FrameTypeGap ||
				f.Type == events.FrameTypeHelloAck || f.Type == events.FrameTypePing {
				continue // pre-close traffic
			}
			t.Fatalf("expected the connection to close naming %s (EVT-096); got frame %s", wantReason, data)
		}
		var ce websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("expected a WS close naming %s (EVT-096); got a transport error: %v", wantReason, err)
		}
		if ce.Reason != wantReason {
			t.Fatalf("a server-initiated close must name the error-taxonomy code %s (EVT-096); got reason %q (status %d)",
				wantReason, ce.Reason, ce.Code)
		}
		return
	}
}

// upgradeRequest builds a WS upgrade request by hand — enough for the handler to
// select the WS binding (EVT-001) and refuse before any upgrade happens, which
// is the only thing the pre-upgrade refusal tests need.
func upgradeRequest(target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return req
}

// problemFrom decodes an api/1 Problem body and returns its code.
func problemFrom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("a pre-upgrade refusal must be an api/1 Problem (EVT-113); got Content-Type %q body=%s", ct, rec.Body.String())
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode Problem: %v", err)
	}
	return problem.Code
}

// TestWS_HandshakeRequiresTheContractSubprotocol drives EVT-090: "A WS connection
// to /events/v1 MUST negotiate the subprotocol events.v1+json; a client offering
// no subprotocol or a different one MUST be refused at the WS handshake, before
// any application-level frame is exchanged."
//
// The refusal is asserted at the HTTP level rather than by a failing Dial,
// because "before any application-level frame is exchanged" is a statement about
// WHERE the refusal happens: a 101 followed by a close frame would satisfy a
// dial-fails assertion while violating the requirement.
func TestWS_HandshakeRequiresTheContractSubprotocol(t *testing.T) {
	h := newTestServer(NewHub(events.NewEventLog(0)))
	for _, c := range []struct {
		name    string
		offered string
	}{
		{"no subprotocol offered", ""},
		{"a different subprotocol", "chat, superchat"},
		{"a near-miss", "events.v2+json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := upgradeRequest("/events/v1")
			if c.offered != "" {
				req.Header.Set("Sec-WebSocket-Protocol", c.offered)
			}
			testAuth().Authorize(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code == http.StatusSwitchingProtocols {
				t.Fatalf("the handshake must be refused before the upgrade (EVT-090); got 101")
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("a WS upgrade with no events/1 subprotocol must be refused 400; got %d body=%s", rec.Code, rec.Body.String())
			}
			if code := problemFrom(t, rec); code != codeRequestInvalid {
				t.Fatalf("the refusal code must be the published %s; got %q", codeRequestInvalid, code)
			}
		})
	}
}

// TestWS_UnauthenticatedUpgradeRefusedBeforeUpgrade drives EVT-113 on the WS
// binding: the refusal is an HTTP Problem with AUTH_REQUIRED, "never with a
// WS/SSE-level frame, since no session has been established yet to frame one
// over" — and it happens BEFORE the upgrade, so there is no frame channel to
// have used.
func TestWS_UnauthenticatedUpgradeRefusedBeforeUpgrade(t *testing.T) {
	h := newTestServer(NewHub(events.NewEventLog(0)))

	req := upgradeRequest("/events/v1")
	req.Header.Set("Sec-WebSocket-Protocol", events.Subprotocol)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated WS upgrade must be 401 (EVT-113); got %d body=%s", rec.Code, rec.Body.String())
	}
	if code := problemFrom(t, rec); code != "AUTH_REQUIRED" {
		t.Fatalf("code = %q, want AUTH_REQUIRED (EVT-113)", code)
	}
}

// TestWS_CredentialInQueryStringRejected drives EVT-112 on the WS binding: a
// credential MUST NOT be accepted as a query-string parameter on EITHER binding.
// The token presented is a real, live one, so the only reason the upgrade is
// refused is where it was carried.
func TestWS_CredentialInQueryStringRejected(t *testing.T) {
	h := newTestServer(NewHub(events.NewEventLog(0)))
	for _, param := range []string{"token", "api_key", "access_token", "session"} {
		t.Run(param, func(t *testing.T) {
			req := upgradeRequest("/events/v1?" + param + "=" + testAuth().Token)
			req.Header.Set("Sec-WebSocket-Protocol", events.Subprotocol)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("a live credential in the %q query parameter must still be refused on the WS binding (EVT-112); got %d", param, rec.Code)
			}
		})
	}
}

// TestWS_FirstFrameMustBeHello drives EVT-091: "The first client-to-server WS
// message on a newly opened connection MUST be a hello frame ... A connection
// that sends any other frame first MUST be closed."
func TestWS_FirstFrameMustBeHello(t *testing.T) {
	for _, c := range []struct {
		name  string
		frame any
	}{
		{"a ping before the hello", events.Ping()},
		{"a frame of an unknown type", map[string]string{"type": "subscribe"}},
		{"a frame with no type at all", map[string]string{"resume_from": idA}},
		{"not a frame at all", "hello"},
	} {
		t.Run(c.name, func(t *testing.T) {
			hub := NewHub(events.NewEventLog(0))
			srv := httptest.NewServer(newTestServer(hub))
			defer srv.Close()

			conn := dialWS(t, srv, testAuth().Credential())
			conn.send(t, c.frame)
			conn.expectClose(t, events.CloseInternal, 2*time.Second)
		})
	}
}

// TestWS_FreshSubscribeAcksFreshAndStreamsOnlyLiveAppends drives EVT-091/092/132
// end to end: a hello with no resume_from is acked `fresh`, the pre-existing
// backlog is NOT replayed, and a live append arrives as an EVT-093 event frame
// carrying the envelope intact.
func TestWS_FreshSubscribeAcksFreshAndStreamsOnlyLiveAppends(t *testing.T) {
	log := events.NewEventLog(0)
	log.Append(autoEnv(idA)) // pre-existing: a fresh subscribe must not replay it
	hub := NewHub(log)
	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	conn, result := openWS(t, srv, testAuth().Credential(), events.HelloFrame{})
	if result != events.ResumeResultFresh {
		t.Fatalf("a hello with no resume_from must ack resume_result fresh (EVT-092/132); got %q", result)
	}

	hub.Append(autoEnv(idB))
	f := conn.next(t, 2*time.Second)
	if f.Type != events.FrameTypeEvent {
		t.Fatalf("a delivered event must be a type:event frame (EVT-093); got %q", f.Type)
	}
	if f.Event.ID != idB {
		t.Fatalf("a fresh subscribe must stream only the live append %s, never the backlog (EVT-132); got %q", idB, f.Event.ID)
	}
	if f.Event.Schema != events.SchemaAutomationRun || f.Event.Origin != "relay" {
		t.Fatalf("the event frame must carry the whole envelope (EVT-093/010); got schema=%q origin=%q", f.Event.Schema, f.Event.Origin)
	}
}

// TestWS_ResumeAcksResumedAndReplaysTheBacklog drives EVT-092/133: a resume_from
// at a retained id acks `resumed` and delivers the backlog strictly after it, in
// id order, then continues live.
func TestWS_ResumeAcksResumedAndReplaysTheBacklog(t *testing.T) {
	log := events.NewEventLog(0)
	log.Append(autoEnv(idA))
	log.Append(autoEnv(idB))
	log.Append(autoEnv(idC))
	hub := NewHub(log)
	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	conn, result := openWS(t, srv, testAuth().Credential(), events.HelloFrame{ResumeFrom: idA})
	if result != events.ResumeResultResumed {
		t.Fatalf("a resume from a retained id must ack resume_result resumed (EVT-092/133); got %q", result)
	}
	for _, want := range []string{idB, idC} {
		if f := conn.next(t, 2*time.Second); f.Type != events.FrameTypeEvent || f.Event.ID != want {
			t.Fatalf("the resumed backlog must be delivered in id order; want %s got type=%q id=%q", want, f.Type, f.Event.ID)
		}
	}
	hub.Append(autoEnv(idD))
	if f := conn.next(t, 2*time.Second); f.Event.ID != idD {
		t.Fatalf("delivery must continue live after the backlog; want %s got %q", idD, f.Event.ID)
	}
}

// TestWS_AgedOutResumeAcksGapThenSendsTheGapFrame drives EVT-092/094/140/141/143:
// a resume_from past the retention horizon acks `gap`, "an explicit gap frame
// immediately follows", and delivery then resumes AT to_id inclusive.
func TestWS_AgedOutResumeAcksGapThenSendsTheGapFrame(t *testing.T) {
	log := events.NewEventLog(2) // bounded: idA ages out when idC lands
	log.Append(autoEnv(idA))
	log.Append(autoEnv(idB))
	log.Append(autoEnv(idC))
	hub := NewHub(log)
	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	conn, result := openWS(t, srv, testAuth().Credential(), events.HelloFrame{ResumeFrom: idA})
	if result != events.ResumeResultGap {
		t.Fatalf("a resume past the retention horizon must ack resume_result gap (EVT-092/141); got %q", result)
	}

	g := conn.next(t, 2*time.Second)
	if g.Type != events.FrameTypeGap {
		t.Fatalf("an explicit gap frame must IMMEDIATELY follow a gap ack (EVT-094); got %q", g.Type)
	}
	if g.FromID == nil || *g.FromID != idA || g.ToID != idB || g.Reason != events.ReasonRetentionExpired {
		t.Fatalf("the gap frame must be {from_id:%s,to_id:%s,reason:retention_expired} (EVT-140/141); got %+v", idA, idB, g)
	}
	// Delivery resumes AT to_id inclusive — never a silent hole (EVT-143).
	for _, want := range []string{idB, idC} {
		if f := conn.next(t, 2*time.Second); f.Type != events.FrameTypeEvent || f.Event.ID != want {
			t.Fatalf("delivery must resume at to_id inclusive; want %s got type=%q id=%q", want, f.Type, f.Event.ID)
		}
	}
}

// TestWS_MalformedResumeFromClosesRatherThanStartingFresh drives EVT-134 on the
// binding where the refusal cannot be an HTTP Problem: the upgrade has already
// happened, so the refusal is a close naming RESUME_FROM_INVALID (EVT-096) —
// and it happens BEFORE any hello-ack or event, so the cursor is never silently
// treated as an omitted one.
func TestWS_MalformedResumeFromClosesRatherThanStartingFresh(t *testing.T) {
	for _, c := range []struct {
		name       string
		resumeFrom string
	}{
		{"malformed", "not_a_ulid"},
		{"well-formed but never recorded", idPrefix + "ZZ"},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := events.NewEventLog(0)
			log.Append(autoEnv(idA))
			hub := NewHub(log)
			srv := httptest.NewServer(newTestServer(hub))
			defer srv.Close()

			conn := dialWS(t, srv, testAuth().Credential())
			conn.send(t, events.HelloFrame{Type: events.FrameTypeHello, ResumeFrom: c.resumeFrom})

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			_, data, err := conn.conn.Read(ctx)
			if err == nil {
				t.Fatalf("a rejected resume_from must deliver NOTHING — not a hello-ack, not an event (EVT-134); got %s", data)
			}
			var ce websocket.CloseError
			if !errors.As(err, &ce) {
				t.Fatalf("expected a close naming %s; got %v", events.CloseResumeFromInvalid, err)
			}
			if ce.Reason != events.CloseResumeFromInvalid {
				t.Fatalf("the close must name %s (EVT-096/134); got %q", events.CloseResumeFromInvalid, ce.Reason)
			}
		})
	}
}

// TestWS_MalformedSelectorCloses drives EVT-121 + EVT-096 together: a hello
// whose selector does not parse under api/1's grammar is refused, and because
// the upgrade has already happened the refusal is a close naming the taxonomy's
// own SELECTOR_INVALID — not the unclassified INTERNAL, which would tell a
// client to retry the same broken selector with backoff.
func TestWS_MalformedSelectorCloses(t *testing.T) {
	hub := NewHub(events.NewEventLog(0))
	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	conn := dialWS(t, srv, testAuth().Credential())
	conn.send(t, events.HelloFrame{Type: events.FrameTypeHello, Selector: "kind = = screen"})
	conn.expectClose(t, events.CloseSelectorInvalid, 2*time.Second)
}

// TestWS_ServerAnswersAClientPingWithAPong drives the receiving half of EVT-095:
// "Either peer MAY send a ping frame ... the receiving peer MUST respond with
// pong".
func TestWS_ServerAnswersAClientPingWithAPong(t *testing.T) {
	hub := NewHub(events.NewEventLog(0))
	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	conn, _ := openWS(t, srv, testAuth().Credential(), events.HelloFrame{})
	conn.send(t, events.Ping())
	if f := conn.next(t, 2*time.Second); f.Type != events.FrameTypePong {
		t.Fatalf("a client ping must be answered with a pong (EVT-095); got %q", f.Type)
	}

	// And the connection is still a live stream afterwards.
	hub.Append(autoEnv(idA))
	if f := conn.next(t, 2*time.Second); f.Event.ID != idA {
		t.Fatalf("the connection must stay live after a keepalive exchange; got %+v", f)
	}
}

// TestWS_UnansweredPingClosesIdleTimeout drives the sending half of EVT-095: the
// server pings an idle connection and, when no pong arrives within the
// round-trip bound, "MUST treat the connection as dead and close it ... MUST use
// the IDLE_TIMEOUT code".
//
// The cadence is INJECTED (WithHeartbeat), which is what makes this drivable at
// all without waiting out 30 seconds — and the assertions still wait on the real
// signals the cadence produces (the ping frame, then the close), never on a
// sleep.
func TestWS_UnansweredPingClosesIdleTimeout(t *testing.T) {
	hub := NewHub(events.NewEventLog(0))
	srv := httptest.NewServer(New(hub, testAuth().Auth, emptyScopeTree,
		WithHeartbeat(20*time.Millisecond, 40*time.Millisecond)))
	defer srv.Close()

	conn, _ := openWS(t, srv, testAuth().Credential(), events.HelloFrame{})

	// The server pings the idle connection...
	if f := conn.next(t, 2*time.Second); f.Type != events.FrameTypePing {
		t.Fatalf("the server must ping a connection idle past the keepalive interval (EVT-095); got %q", f.Type)
	}
	// ...and this client never answers, so the connection is closed as dead.
	conn.expectClose(t, events.CloseIdleTimeout, 2*time.Second)
}

// TestWS_AnsweredPingKeepsTheConnectionAlive is the false-positive guard for the
// keepalive: a client that DOES pong is not disconnected, and the stream keeps
// delivering. Without it, a server that closed every connection after one
// interval would pass the test above.
func TestWS_AnsweredPingKeepsTheConnectionAlive(t *testing.T) {
	hub := NewHub(events.NewEventLog(0))
	srv := httptest.NewServer(New(hub, testAuth().Auth, emptyScopeTree,
		WithHeartbeat(20*time.Millisecond, 2*time.Second)))
	defer srv.Close()

	conn, _ := openWS(t, srv, testAuth().Credential(), events.HelloFrame{})

	// Answer two consecutive keepalive round trips, so this cannot pass by
	// merely surviving one.
	for i := 0; i < 2; i++ {
		if f := conn.next(t, 2*time.Second); f.Type != events.FrameTypePing {
			t.Fatalf("round %d: expected a server ping; got %q", i, f.Type)
		}
		conn.send(t, events.Pong())
	}

	hub.Append(autoEnv(idA))
	for {
		f := conn.next(t, 2*time.Second)
		if f.Type == events.FrameTypePing {
			conn.send(t, events.Pong())
			continue
		}
		if f.Type != events.FrameTypeEvent || f.Event.ID != idA {
			t.Fatalf("a ponged connection must keep delivering (EVT-095); got %+v", f)
		}
		return
	}
}

// TestWS_RevocationTearsDownAnOpenStream drives EVT-114 on the WS binding: "A
// session or API key credential's revocation MUST terminate every open events/1
// connection authenticated by it within a bounded delay, not merely block future
// connections."
//
// This is the same property already proven for SSE, and the same distinction is
// the whole test: the stream is proven live FIRST, then the session is revoked,
// and the already-open socket must end — with the close naming AUTH_REQUIRED, so
// a client reconnects with a fresh credential rather than retrying the dead one.
func TestWS_RevocationTearsDownAnOpenStream(t *testing.T) {
	fixture, err := authtest.New(authtest.Config{})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	defer fixture.Close()

	hub := NewHub(events.NewEventLog(0))
	srv := httptest.NewServer(New(hub, fixture.Auth, emptyScopeTree))
	defer srv.Close()

	conn, _ := openWS(t, srv, fixture.Credential(), events.HelloFrame{})
	hub.Append(autoEnv(idA))
	if f := conn.next(t, 2*time.Second); f.Event.ID != idA {
		t.Fatalf("the stream must be live before revocation; got %+v", f)
	}
	if n := fixture.Revocations.Watching(fixture.SessionID); n != 1 {
		t.Fatalf("the open WS stream must be registered for its session's revocation; watching=%d", n)
	}

	if err := fixture.Store.RevokeSession(t.Context(), fixture.SessionID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	conn.expectClose(t, events.CloseAuthRequired, 2*time.Second)

	// ...and the credential is dead for a fresh upgrade too.
	header := http.Header{}
	header.Set("Cookie", auth.SessionCookieName+"="+fixture.Token)
	if _, resp, err := websocket.Dial(t.Context(), wsURL(srv), &websocket.DialOptions{
		Subprotocols: []string{events.Subprotocol},
		HTTPHeader:   header,
	}); err == nil {
		t.Fatal("a revoked credential must not open a new WS connection")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked credential's upgrade must be refused 401; got resp=%v err=%v", resp, err)
	}
}

// TestWS_StreamDeregistersOnDisconnect pins that a normally-disconnecting WS
// subscriber releases both of the registrations it holds — the Hub's fan-out
// entry and the revocation watcher — so a long-lived server does not accumulate
// one dead entry per connection that ever opened.
func TestWS_StreamDeregistersOnDisconnect(t *testing.T) {
	fixture, err := authtest.New(authtest.Config{})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	defer fixture.Close()

	hub := NewHub(events.NewEventLog(0))
	srv := httptest.NewServer(New(hub, fixture.Auth, emptyScopeTree))
	defer srv.Close()

	conn, _ := openWS(t, srv, fixture.Credential(), events.HelloFrame{})
	hub.Append(autoEnv(idA))
	conn.next(t, 2*time.Second)
	if got := hub.subscriberCount(); got != 1 {
		t.Fatalf("a live WS subscriber must hold a Hub registration; got %d", got)
	}

	_ = conn.conn.Close(websocket.StatusNormalClosure, "")

	// Poll a real condition (the registrations dropping) rather than sleeping a
	// settle time; the deadline is a hang guard.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.subscriberCount() == 0 && fixture.Revocations.Watching(fixture.SessionID) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("a disconnected WS subscriber must release its Hub registration and its revocation watcher; subs=%d watching=%d",
		hub.subscriberCount(), fixture.Revocations.Watching(fixture.SessionID))
}

// TestWS_HubCloseEndsTheConnection: a graceful server shutdown ends every open WS
// connection, naming UNAVAILABLE — the taxonomy's "temporarily unable to serve,
// retry with backoff", which is what a restarting server is.
func TestWS_HubCloseEndsTheConnection(t *testing.T) {
	hub := NewHub(events.NewEventLog(0))
	srv := httptest.NewServer(newTestServer(hub))
	defer srv.Close()

	conn, _ := openWS(t, srv, testAuth().Credential(), events.HelloFrame{})
	hub.Append(autoEnv(idA))
	if f := conn.next(t, 2*time.Second); f.Event.ID != idA {
		t.Fatalf("the stream must be live before shutdown; got %+v", f)
	}

	hub.Close()
	conn.expectClose(t, events.CloseUnavailable, 2*time.Second)
}

// TestWS_ResumeAcrossAFeederRestartDeliversTheBacklog is the SSE durability
// property (persistence_test.go) driven over the WS binding: the log is the
// PERSISTENT one, the whole stack is torn down and rebuilt from the same file,
// and a client reconnecting with a cursor it held across the restart resumes
// with every event recorded after it (EVT-133) — proving the durable substrate
// is reached by this binding too, not just by SSE.
func TestWS_ResumeAcrossAFeederRestartDeliversTheBacklog(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	clock := int64(1_752_537_600_000)
	now := func() int64 { return clock }

	first := bootFeederStack(t, dsn, now)
	first.hub.Append(classed(t, autoEnv(idA)))
	first.hub.Append(classed(t, autoEnv(idB)))
	first.hub.Append(classed(t, autoEnv(idC)))
	first.close()

	second := bootFeederStack(t, dsn, now)
	defer second.close()

	conn, result := openWS(t, second.srv, testAuth().Credential(), events.HelloFrame{ResumeFrom: idA})
	if result != events.ResumeResultResumed {
		t.Fatalf("a cursor held across a restart must resume cleanly (EVT-133); got resume_result %q", result)
	}
	for _, want := range []string{idB, idC} {
		if f := conn.next(t, 2*time.Second); f.Type != events.FrameTypeEvent || f.Event.ID != want {
			t.Fatalf("the restarted process must replay the backlog after the cursor; want %s got type=%q id=%q", want, f.Type, f.Event.ID)
		}
	}
	second.hub.Append(classed(t, autoEnv(idD)))
	if f := conn.next(t, 2*time.Second); f.Event.ID != idD {
		t.Fatalf("the restarted process must still be a LIVE stream; want %s got %q", idD, f.Event.ID)
	}
}
