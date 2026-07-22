package eventsse

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/events"
)

// siteScope is the frozen corpus site scope-node ULID stamped onto the test
// envelopes' scope_node (matches the eventingest fixtures so the two packages
// exercise the same shared-log envelope shape).
const siteScope = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"

// idPrefix is 24 Crockford base32 characters (leading 0) — a 2-symbol counter
// suffix makes a full 26-char valid ULID that sorts in suffix order, so
// idA < idB < idC < idD is also chronological id order (EVT-011/140).
const idPrefix = "01J8Z3K4N5P6Q7R8S9T0V1W2"

var (
	idA = idPrefix + "Y5"
	idB = idPrefix + "Y6"
	idC = idPrefix + "Y7"
	idD = idPrefix + "Y8"
)

// corpusAutomationRunPayload is the frozen corpus automation.run payload
// (conformance/corpora/events-1) — a real, serializable event body so a streamed
// SSEEventLine data: field carries an intact automation.run (EVT-040/103).
func corpusAutomationRunPayload() json.RawMessage {
	return json.RawMessage(`{"rule_id":"01J8Z3K4N5P6Q7R8S9T0V1W2YC","rule_revision":4,"trigger_snapshot":{"kind":"state"},"condition_results":[{"passed":true}],"action_outcomes":[{"status":"ok"}],"mode_disposition":"ran","misfire_caught":false}`)
}

// autoEnv builds a valid automation.run durable-event envelope with id — the
// shape the ingest appends and the SSE server streams (EVT-010/103). The
// cost/retention class values are cosmetic here: SSEEventLine only serializes the
// envelope, it does not re-validate it (validation is the ingest's job, EVT-013).
func autoEnv(id string) events.Envelope {
	return events.Envelope{
		ID:             id,
		Schema:         events.SchemaAutomationRun,
		TS:             1752537000000,
		ScopeNode:      siteScope,
		TraceID:        idPrefix + "ZZ",
		CostClass:      "standard",
		RetentionClass: "operational",
		Origin:         "relay",
		Payload:        corpusAutomationRunPayload(),
	}
}

// dialSSE opens a streaming GET /events/v1 against srv with the given query and
// headers, asserts a 200 text/event-stream response, and returns a buffered
// reader over the live body plus a cancel that tears the connection down (firing
// the handler's request-context close path). The caller MUST call cancel.
func dialSSE(t *testing.T, srv *httptest.Server, query string, header http.Header) (*bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	url := srv.URL + "/events/v1"
	if query != "" {
		url += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("dialing SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("SSE request must respond 200 (EVT-100); got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		cancel()
		t.Fatalf("SSE Content-Type must be text/event-stream (EVT-100); got %q", ct)
	}
	br := bufio.NewReader(resp.Body)
	return br, func() { cancel(); resp.Body.Close() }
}

type sseFrame struct {
	event string
	id    string
	data  string
}

// readFrameWithin reads exactly one SSE frame (lines up to the blank-line
// terminator) from br, failing the test if none arrives within d — so a missing
// live push or a wrongly-suppressed backlog is a hard failure, never a hang.
func readFrameWithin(t *testing.T, br *bufio.Reader, d time.Duration) sseFrame {
	t.Helper()
	type result struct {
		f   sseFrame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var f sseFrame
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				ch <- result{f, err}
				return
			}
			if line == "\n" { // blank line terminates the frame (EVT-103)
				ch <- result{f, nil}
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("reading SSE frame: %v", r.err)
		}
		return r.f
	case <-time.After(d):
		t.Fatalf("timed out after %s waiting for an SSE frame", d)
		return sseFrame{}
	}
}

// TestSSE_FreshConnectionStreamsOnlyLiveAppends: a fresh SSE connection (no
// resume_from) does NOT replay the pre-existing backlog — it delivers only events
// recorded from connection time forward (EVT-132), pushing each new Append live
// (EVT-100/103).
func TestSSE_FreshConnectionStreamsOnlyLiveAppends(t *testing.T) {
	log := events.NewEventLog(0)
	log.Append(autoEnv(idA))
	log.Append(autoEnv(idB))
	notify := make(chan struct{}, 1)

	srv := httptest.NewServer(New(log, notify))
	defer srv.Close()

	br, closeConn := dialSSE(t, srv, "", nil)
	defer closeConn()

	// A live append after the fresh watermark is established must stream through.
	log.Append(autoEnv(idC))
	notify <- struct{}{}

	f := readFrameWithin(t, br, 2*time.Second)
	if f.event != "event" {
		t.Fatalf("live frame event field must be %q; got %q", "event", f.event)
	}
	if f.id != idC {
		t.Fatalf("fresh connection must stream only the live append %s, not the backlog; got id %q", idC, f.id)
	}
	var env events.Envelope
	if err := json.Unmarshal([]byte(f.data), &env); err != nil {
		t.Fatalf("frame data must be the envelope JSON (EVT-103); got %v data=%s", err, f.data)
	}
	if env.ID != idC || env.Origin != "relay" {
		t.Fatalf("streamed envelope must be the appended one (id %s, origin relay); got id=%q origin=%q", idC, env.ID, env.Origin)
	}
}

// TestSSE_ResumeFromStreamsBacklogThenLive: a resume_from at a retained id
// streams the backlog strictly after it, in id order, then continues live
// (EVT-102/133).
func TestSSE_ResumeFromStreamsBacklogThenLive(t *testing.T) {
	log := events.NewEventLog(0)
	log.Append(autoEnv(idA))
	log.Append(autoEnv(idB))
	log.Append(autoEnv(idC))
	notify := make(chan struct{}, 1)

	srv := httptest.NewServer(New(log, notify))
	defer srv.Close()

	br, closeConn := dialSSE(t, srv, "resume_from="+idA, nil)
	defer closeConn()

	// backlog after idA, in id order: idB then idC.
	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idB {
		t.Fatalf("first resumed frame must be event %s; got event=%q id=%q", idB, f.event, f.id)
	}
	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idC {
		t.Fatalf("second resumed frame must be event %s; got event=%q id=%q", idC, f.event, f.id)
	}

	// then a live append continues the same stream.
	log.Append(autoEnv(idD))
	notify <- struct{}{}
	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idD {
		t.Fatalf("live continuation frame must be event %s; got event=%q id=%q", idD, f.event, f.id)
	}
}

// TestSSE_LastEventIDTakesPrecedenceOverResumeFromQuery: a Last-Event-ID header
// overrides a resume_from query parameter (EVT-102) — the browser-native
// reconnect path.
func TestSSE_LastEventIDTakesPrecedenceOverResumeFromQuery(t *testing.T) {
	log := events.NewEventLog(0)
	log.Append(autoEnv(idA))
	log.Append(autoEnv(idB))
	log.Append(autoEnv(idC))
	notify := make(chan struct{}, 1)

	srv := httptest.NewServer(New(log, notify))
	defer srv.Close()

	// query says resume from idA (would replay idB, idC); Last-Event-ID says idB,
	// which must win — so only idC streams.
	h := http.Header{}
	h.Set("Last-Event-ID", idB)
	br, closeConn := dialSSE(t, srv, "resume_from="+idA, h)
	defer closeConn()

	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idC {
		t.Fatalf("Last-Event-ID must take precedence (EVT-102): expected only event %s; got event=%q id=%q", idC, f.event, f.id)
	}
}

// TestSSE_MalformedResumeFromRejectedBeforeStream: a malformed resume_from is
// rejected with a RESUME_FROM_INVALID Problem before any event is delivered
// (EVT-134) — not treated as an omitted (fresh) resume.
func TestSSE_MalformedResumeFromRejectedBeforeStream(t *testing.T) {
	log := events.NewEventLog(0)
	notify := make(chan struct{}, 1)
	h := New(log, notify)

	req := httptest.NewRequest(http.MethodGet, "/events/v1?resume_from=not_a_ulid", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a malformed resume_from must be a 400 Problem before the stream (EVT-134); got %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("rejection must be an api/1 Problem (application/problem+json); got %q", ct)
	}
	if strings.Contains(rec.Body.String(), "text/event-stream") {
		t.Fatalf("a rejected resume must not start a stream (EVT-134)")
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Problem body must be JSON; got %v", err)
	}
	if body.Code != events.ResumeFromInvalidCode {
		t.Fatalf("Problem code must be %s (EVT-134); got %q", events.ResumeFromInvalidCode, body.Code)
	}
}

// TestSSE_AgedOutResumeFromEmitsGapFirst: a resume_from older than the retention
// horizon is a retention_expired gap — an event: gap frame is streamed first
// (id = to_id = the oldest retained id), then delivery resumes at that id
// inclusive, never a silent loss (EVT-140/141/143).
func TestSSE_AgedOutResumeFromEmitsGapFirst(t *testing.T) {
	log := events.NewEventLog(2) // bounded: idA ages out when idC lands
	log.Append(autoEnv(idA))
	log.Append(autoEnv(idB))
	log.Append(autoEnv(idC))
	// retained now: idB, idC; oldest = idB.
	notify := make(chan struct{}, 1)

	srv := httptest.NewServer(New(log, notify))
	defer srv.Close()

	br, closeConn := dialSSE(t, srv, "resume_from="+idA, nil)
	defer closeConn()

	// frame 1: the gap loss marker, id = to_id = idB, reason retention_expired.
	g := readFrameWithin(t, br, 2*time.Second)
	if g.event != "gap" {
		t.Fatalf("aged-out resume must emit an event: gap first (EVT-104/140); got event=%q", g.event)
	}
	if g.id != idB {
		t.Fatalf("gap id must be to_id = the oldest retained id %s (EVT-104/141); got %q", idB, g.id)
	}
	var marker struct {
		FromID string `json:"from_id"`
		ToID   string `json:"to_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(g.data), &marker); err != nil {
		t.Fatalf("gap data must be the {from_id,to_id,reason} marker; got %v data=%s", err, g.data)
	}
	if marker.FromID != idA || marker.ToID != idB || marker.Reason != "retention_expired" {
		t.Fatalf("gap marker must be {from_id:%s,to_id:%s,reason:retention_expired} (EVT-140/141); got %+v", idA, idB, marker)
	}

	// then delivery resumes AT to_id inclusive: idB, then idC — no silent loss.
	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idB {
		t.Fatalf("gap resume must deliver AT to_id inclusive %s (EVT-143); got event=%q id=%q", idB, f.event, f.id)
	}
	if f := readFrameWithin(t, br, 2*time.Second); f.event != "event" || f.id != idC {
		t.Fatalf("gap resume must then deliver %s in order; got event=%q id=%q", idC, f.event, f.id)
	}
}
