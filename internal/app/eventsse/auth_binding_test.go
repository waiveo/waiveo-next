package eventsse

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/events"
)

// TestSSE_UnauthenticatedRequestRejectedBeforeStream drives EVT-113: "A request
// that fails authentication on either binding MUST be rejected before any
// upgrade or stream begins, with an HTTP error response in api/1's Problem shape
// (API-010) and the AUTH_REQUIRED code — never with a WS/SSE-level frame, since
// no session has been established yet to frame one over."
//
// The "before any stream begins" half is what the Content-Type assertion pins:
// a rejection that had already written the 200 + text/event-stream header block
// and then framed an error inside the stream would satisfy neither the status
// nor the shape this requirement names.
func TestSSE_UnauthenticatedRequestRejectedBeforeStream(t *testing.T) {
	h := newTestServer(NewHub(events.NewEventLog(0)))

	req := httptest.NewRequest(http.MethodGet, "/events/v1", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated SSE request must be 401 (EVT-113); got %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("the rejection must be an api/1 Problem (EVT-113); got Content-Type %q", ct)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode Problem: %v", err)
	}
	if problem.Code != "AUTH_REQUIRED" {
		t.Fatalf("code = %q, want AUTH_REQUIRED (EVT-113)", problem.Code)
	}
}

// TestSSE_AcceptsBothCredentialForms drives EVT-110/111: a connection is
// authenticated by "the platform session cookie, for a browser's native
// EventSource (which cannot set custom headers)" OR "an Authorization: Bearer
// <api-key> header, for a client library that supports one."
func TestSSE_AcceptsBothCredentialForms(t *testing.T) {
	fixture := testAuth()
	cases := []struct {
		name  string
		apply func(*http.Request)
	}{
		{"session cookie (browser EventSource)", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fixture.Token})
		}},
		{"Authorization: Bearer api key (client library)", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+fixture.APIKey)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub := NewHub(events.NewEventLog(0))
			srv := httptest.NewServer(newTestServer(hub))
			defer srv.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events/v1", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Accept", "text/event-stream")
			c.apply(req)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("an authenticated SSE request must be 200; got %d", resp.StatusCode)
			}

			// It is a real, live stream, not merely a 200: a fresh append
			// arrives.
			hub.Append(autoEnv(idA))
			br := bufio.NewReader(resp.Body)
			if f := readFrameWithin(t, br, 2*time.Second); f.id != idA {
				t.Fatalf("the authenticated stream must deliver a live append; got id %q", f.id)
			}
		})
	}
}

// TestSSE_CredentialInQueryStringRejected drives EVT-112: "An API key or session
// credential MUST NOT be accepted as a query-string parameter on either binding
// — a token that could ride a URL is a token that leaks into server access logs
// and intermediate proxies."
//
// The token presented here is a REAL, live one, so the only reason the request
// is refused is where it was carried.
func TestSSE_CredentialInQueryStringRejected(t *testing.T) {
	h := newTestServer(NewHub(events.NewEventLog(0)))
	for _, param := range []string{"token", "api_key", "access_token", "session"} {
		t.Run(param, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/events/v1?"+param+"="+testAuth().Token, nil)
			req.Header.Set("Accept", "text/event-stream")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("a live credential carried in the %q query parameter must still be refused (EVT-112); got %d", param, rec.Code)
			}
		})
	}
}

// TestSSE_RevocationTearsDownAnOpenStream drives EVT-114: "A session or API key
// credential's revocation MUST terminate every open events/1 connection
// authenticated by it within a bounded delay, not merely block future
// connections."
//
// The distinction is the whole test. It opens a stream, proves it is live by
// pushing an event through it, THEN revokes the session, and requires the
// already-open stream to end — which a "refuse the next connect" implementation
// would fail while looking perfectly correct from the outside.
//
// The delay achieved is immediate (revocation fires the store's OnRevoke hook
// in-process), so this waits on the connection closing rather than sleeping a
// fixed interval; the two-second budget is a timeout guarding a hang, not a
// timing assumption.
func TestSSE_RevocationTearsDownAnOpenStream(t *testing.T) {
	fixture, err := authtest.New(authtest.Config{})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	defer fixture.Close()

	hub := NewHub(events.NewEventLog(0))
	srv := httptest.NewServer(New(hub, fixture.Auth))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events/v1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fixture.Token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the stream must open; got %d", resp.StatusCode)
	}

	// The stream is genuinely live before revocation.
	br := bufio.NewReader(resp.Body)
	hub.Append(autoEnv(idA))
	if f := readFrameWithin(t, br, 2*time.Second); f.id != idA {
		t.Fatalf("the stream must be live before revocation; got id %q", f.id)
	}
	if n := fixture.Revocations.Watching(fixture.SessionID); n != 1 {
		t.Fatalf("the open stream must be registered for its session's revocation; watching=%d", n)
	}

	if err := fixture.Store.RevokeSession(context.Background(), fixture.SessionID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	// The already-open stream ends. A read on a torn-down SSE body returns an
	// error (or EOF) rather than another frame.
	done := make(chan error, 1)
	go func() {
		_, err := br.ReadString('\n')
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("revoking the session must END the open stream, not deliver another frame (EVT-114)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revoking the session did not terminate the open stream within the bounded delay (EVT-114) — refusing the NEXT connect is explicitly not sufficient")
	}

	// ...and the credential is dead for a fresh connect too.
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/events/v1", nil)
	req2.Header.Set("Accept", "text/event-stream")
	req2.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fixture.Token})
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked credential must also fail a fresh connect; got %d", resp2.StatusCode)
	}
}

// TestSSE_StreamDeregistersOnDisconnect pins that a normally-disconnecting
// stream removes its revocation watcher, so a long-lived server does not
// accumulate one dead entry per stream that ever connected.
func TestSSE_StreamDeregistersOnDisconnect(t *testing.T) {
	fixture, err := authtest.New(authtest.Config{})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	defer fixture.Close()

	hub := NewHub(events.NewEventLog(0))
	srv := httptest.NewServer(New(hub, fixture.Auth))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events/v1", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fixture.Token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	br := bufio.NewReader(resp.Body)
	hub.Append(autoEnv(idA))
	readFrameWithin(t, br, 2*time.Second)

	cancel()
	resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fixture.Revocations.Watching(fixture.SessionID) == 0 {
			return
		}
		// Yield to the server goroutine observing the closed connection. This
		// polls a condition rather than sleeping a fixed settle time.
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("a disconnected stream must deregister its revocation watcher; still watching %d",
		fixture.Revocations.Watching(fixture.SessionID))
}
