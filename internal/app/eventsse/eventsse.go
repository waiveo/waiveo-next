// Package eventsse is the app-side live /events/v1 SSE server (events/1
// EVT-100–105, 130–144): the GET handler a subscriber connects to to WATCH the
// platform in real time. It reads the shared events.EventLog the telemetry
// ingest (internal/app/eventingest) writes into, resolves the connection's
// resume point via events.Resolve (fresh | resumed | gap | RESUME_FROM_INVALID),
// streams the resolved backlog using the built SSE framing
// (events.SSEEventLine / events.SSEGapLine), and then pushes every NEW event
// live as the ingest appends it — woken by a notify channel and flushing the
// ResponseWriter after each frame.
//
// It re-implements none of that machinery: the ordering/retention log
// (events.EventLog), the four-outcome resume resolution (events.Resolve), the
// EVT-103/104 line framing (events.SSEEventLine / events.SSEGapLine), and the
// api/1 Problem shape (apihttp.WriteProblem) are all called, not rebuilt.
//
// Deferred, documented seams: the WebSocket binding (Go stdlib has no websocket;
// the WS frame logic in internal/events/delivery.go is already conformance-driven
// but a live WS server is a dependency decision), and binding authentication
// (EVT-110–114) + the roles-based visible set (EVT-120), which land with the
// security model — the POC endpoint is unauthenticated.
package eventsse

import (
	stdlog "log"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// server is the GET /events/v1 SSE handler. It holds the shared, read-side view
// of the event log and the notify channel the writer (the ingest) signals after
// each Append, so a connected subscriber is woken to drain the newly-appended
// backlog rather than polling. The log itself is the single shared instance the
// ingest writes; this handler only reads it (Resolve / After), on the connection
// goroutine, woken by notify.
type server struct {
	log    *events.EventLog
	notify <-chan struct{}
	// logf records a non-fatal streaming hiccup (an envelope that fails to
	// serialize, EVT-103) — it defaults to the stdlib logger and is a field so a
	// corrupt frame is logged and skipped, never emitted.
	logf func(format string, args ...any)
}

// New returns the GET /events/v1 SSE handler streaming log to subscribers.
// notify is the wake signal the writer (internal/app/eventingest) sends after
// each Append: every connected subscriber drains the log's newly-appended tail
// when it fires, so new events push live (EVT-100). log is shared, read-only
// here, with that ingest writer.
func New(log *events.EventLog, notify <-chan struct{}) http.Handler {
	return &server{log: log, notify: notify, logf: stdlog.Printf}
}

// ServeHTTP handles an SSE subscribe (EVT-100–105, 130–144). It selects the SSE
// binding from the request shape (EVT-001/100 — a WS upgrade is refused, the live
// WS server is deferred), resolves the resume point (Last-Event-ID header over
// resume_from query, EVT-102) via events.Resolve, streams the resolved backlog
// (a leading event: gap for a retention_expired resume, EVT-104/140), then
// blocks streaming each newly-appended event live until the client disconnects
// (request context) — a malformed or unrecorded resume_from is a
// RESUME_FROM_INVALID Problem written before any event (EVT-134).
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	traceID := apihttp.TraceID(r)

	if r.Method != http.MethodGet {
		apihttp.WriteProblem(w, r, traceID, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method Not Allowed")
		return
	}

	// Binding selection at the shared path (EVT-001/100): a WS upgrade selects the
	// WS binding, which is deferred (Go stdlib has no websocket); only an
	// Accept: text/event-stream request is served here.
	switch events.SelectBinding(r.Header.Get("Upgrade") != "", r.Header.Get("Accept")) {
	case events.BindingSSE:
		// proceed
	case events.BindingWS:
		apihttp.WriteProblem(w, r, traceID, http.StatusNotImplemented, "WS_BINDING_DEFERRED", "The events/1 WebSocket binding is not yet served; connect over SSE (Accept: text/event-stream)")
		return
	default:
		apihttp.WriteProblem(w, r, traceID, http.StatusNotAcceptable, "SSE_REQUIRED", "events/1 requires Accept: text/event-stream")
		return
	}

	// The SSE stream needs an incrementally flushable writer; without one there is
	// no live push, so refuse rather than buffer the whole (unbounded) stream.
	flusher, ok := w.(http.Flusher)
	if !ok {
		apihttp.WriteProblem(w, r, traceID, http.StatusInternalServerError, "STREAMING_UNSUPPORTED", "Streaming is not supported")
		return
	}

	// Resolve the resume point: a Last-Event-ID header (a browser-native reconnect)
	// takes precedence over a resume_from query parameter (EVT-102).
	resumeFrom := r.Header.Get("Last-Event-ID")
	if resumeFrom == "" {
		resumeFrom = r.URL.Query().Get("resume_from")
	}
	outcome, rerr := events.Resolve(s.log, resumeFrom)
	if rerr != nil {
		// A malformed or never-recorded resume_from is refused before any event is
		// delivered — never silently treated as a fresh subscribe (EVT-134).
		apihttp.WriteProblem(w, r, traceID, http.StatusBadRequest, rerr.Code, "The resume_from was malformed or names an event the platform never recorded")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// lastID tracks the highest id already delivered to this subscriber, so the
	// live loop's After(lastID) yields exactly the not-yet-seen tail (gap-free,
	// duplicate-free, EVT-133/143).
	var lastID string
	switch outcome.Result {
	case events.ResumeResultFresh:
		// Fresh: deliver only events from connection time forward (EVT-132), so
		// watermark at the current head — the live loop streams strictly after it.
		lastID = s.headID()
	case events.ResumeResultResumed:
		// Resume strictly after the requested id; if its backlog is empty (the id
		// is the head), the watermark is that id itself.
		lastID = resumeFrom
	case events.ResumeResultGap:
		// A retention_expired discontinuity: mark it before any event (EVT-104/140),
		// then delivery resumes AT to_id inclusive via outcome.Events below.
		s.writeString(w, events.SSEGapLine(*outcome.Gap))
		lastID = outcome.ResumeAtID
	}

	// Stream the resolved backlog in id order (After(resume_from) for a clean
	// resume; the from-oldest inclusive slice for a gap; empty for fresh).
	for _, env := range outcome.Events {
		s.writeEvent(w, env)
		lastID = env.ID
	}
	flusher.Flush()

	// Live: drain the newly-appended tail on every wake until the client goes away.
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-s.notify:
			if !open {
				return // the writer closed notify (shutdown)
			}
			for _, env := range s.log.After(lastID) {
				s.writeEvent(w, env)
				lastID = env.ID
			}
			flusher.Flush()
		}
	}
}

// writeEvent frames env as an SSE event line (EVT-103) and writes it. An
// envelope that fails to serialize is logged and skipped — never emitted as a
// corrupt line (SSEEventLine's own contract) — so one bad record can't poison the
// stream.
func (s *server) writeEvent(w http.ResponseWriter, env events.Envelope) {
	line, err := events.SSEEventLine(env)
	if err != nil {
		s.logf("eventsse: dropping unserializable event id %q: %v (EVT-103)", env.ID, err)
		return
	}
	s.writeString(w, line)
}

// writeString writes s to w, ignoring the error: a mid-stream write failure is a
// dropped subscriber connection, which the request-context select already
// observes and closes on — there is nothing to recover here.
func (s *server) writeString(w http.ResponseWriter, str string) {
	_, _ = w.Write([]byte(str))
}

// headID is the id of the newest event currently retained — the fresh-subscribe
// watermark (EVT-132), so the live loop's After(headID) starts strictly after
// what already existed at connection time. It is "" for an empty log (After("")
// then yields the whole first live batch).
func (s *server) headID() string {
	all := s.log.After("")
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1].ID
}
