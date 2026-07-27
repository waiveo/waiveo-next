package eventsse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/events"
)

// binding_parity_test.go is the oracle for the property that makes events/1 ONE
// contract with two bindings rather than two feeds that happen to share a path:
// a WS subscriber and an SSE subscriber, connected to the same Hub with the same
// subscribe parameters, receive the same envelopes, in the same order, with the
// same gaps marked in the same places.
//
// Each case drives BOTH bindings over ONE underlying event sequence and compares
// what they delivered — so a divergence fails here even when each binding's own
// tests still pass. Every case also asserts the delivered sequence against an
// independently stated expectation, so the two bindings agreeing on the WRONG
// stream is a failure too, not a pass.
//
// What is deliberately NOT compared: the WS `hello-ack`. EVT-105 says so
// outright — "connection setup that WS performs via hello/hello-ack (EVT-091-092)
// is, on SSE, entirely carried by the initial request's query parameters and the
// stream's first event" — so it has no SSE counterpart by design, and the ack's
// own resume_result is driven by ws_test.go instead.

// parityFrame is one delivered frame reduced to what the two bindings MUST
// agree on: which kind of frame it is, which id it names, and the body it
// carries — the WS frame's own `type` discriminator and the SSE `event:` field
// name the same three kinds, so `kind` is directly comparable.
type parityFrame struct {
	kind string
	id   string
	body string
}

// gapBody is the loss marker both bindings carry, in one canonical form: the WS
// gap frame adds a `type` discriminator the SSE `event: gap` field already
// supplies, and that difference is framing, not content (EVT-094 vs EVT-104).
type gapBody struct {
	FromID *string `json:"from_id"`
	ToID   string  `json:"to_id"`
	Reason string  `json:"reason"`
}

func canonicalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("canonicalizing a frame body: %v", err)
	}
	return string(b)
}

// sseParityFrames parses a whole SSE response body into parity frames. It is
// used where the Hub has been CLOSED before the connection is made, which is
// what makes the delivered set observable with no timer at all: the handler
// writes and flushes its whole resolved backlog before reaching the live select,
// and that select returns at once on the closed done channel, so the body is
// exactly "everything this subscriber was owed", then EOF.
func sseParityFrames(t *testing.T, body string) []parityFrame {
	t.Helper()
	var out []parityFrame
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var f sseFrame
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
		out = append(out, normalizeSSEFrame(t, f))
	}
	return out
}

// normalizeSSEFrame turns one SSE frame into a parity frame, decoding its data
// through the SAME types the WS side is decoded through so the comparison is
// about content rather than about two spellings of the same JSON.
func normalizeSSEFrame(t *testing.T, f sseFrame) parityFrame {
	t.Helper()
	switch f.event {
	case events.FrameTypeEvent:
		var env events.Envelope
		if err := json.Unmarshal([]byte(f.data), &env); err != nil {
			t.Fatalf("an SSE event frame's data must be the envelope (EVT-103): %v data=%s", err, f.data)
		}
		return parityFrame{kind: events.FrameTypeEvent, id: f.id, body: canonicalJSON(t, env)}
	case events.FrameTypeGap:
		var g gapBody
		if err := json.Unmarshal([]byte(f.data), &g); err != nil {
			t.Fatalf("an SSE gap frame's data must be the loss marker (EVT-104): %v data=%s", err, f.data)
		}
		return parityFrame{kind: events.FrameTypeGap, id: f.id, body: canonicalJSON(t, g)}
	default:
		t.Fatalf("unexpected SSE frame kind %q", f.event)
		return parityFrame{}
	}
}

// normalizeWSFrame is normalizeSSEFrame's counterpart for a WS frame.
func normalizeWSFrame(t *testing.T, f wsFrame) parityFrame {
	t.Helper()
	switch f.Type {
	case events.FrameTypeEvent:
		return parityFrame{kind: events.FrameTypeEvent, id: f.Event.ID, body: canonicalJSON(t, f.Event)}
	case events.FrameTypeGap:
		return parityFrame{kind: events.FrameTypeGap, id: f.ToID,
			body: canonicalJSON(t, gapBody{FromID: f.FromID, ToID: f.ToID, Reason: f.Reason})}
	default:
		t.Fatalf("unexpected WS frame type %q on a delivery stream", f.Type)
		return parityFrame{}
	}
}

// sseQueryFor renders a WS hello's three subscribe fields in SSE's own
// query-parameter form (EVT-091 vs EVT-101/102). Deriving one from the other
// rather than writing both out per case is deliberate: a case cannot
// accidentally ask the two bindings for different subscriptions and then be
// surprised that they delivered different streams.
func sseQueryFor(h events.HelloFrame) string {
	v := url.Values{}
	if h.ResumeFrom != "" {
		v.Set("resume_from", h.ResumeFrom)
	}
	if h.Selector != "" {
		v.Set("selector", h.Selector)
	}
	for _, s := range h.Schemas {
		v.Add("schemas", s)
	}
	return v.Encode()
}

// schemaEnv is autoEnv under a different registered schema — the EVT-124 cases
// need at least two schemas in one stream to have anything to restrict.
func schemaEnv(id, schema string) events.Envelope {
	env := autoEnv(id)
	env.Schema = schema
	return env
}

func ptr(s string) *string { return &s }

// TestBindingParity_SameEnvelopeStream drives both bindings over one event
// sequence per case and requires them to deliver the same frames.
func TestBindingParity_SameEnvelopeStream(t *testing.T) {
	cases := []struct {
		name string
		// retention bounds the log (0 = unbounded), which is how the
		// retention_expired case is set up.
		retention int
		// seed is recorded BEFORE either binding connects; live is appended
		// after both have.
		seed  []events.Envelope
		live  []events.Envelope
		hello events.HelloFrame
		// want is the exact frame sequence BOTH bindings must deliver, stated
		// independently of what either one actually does.
		want []parityFrame
	}{
		{
			name: "a fresh subscribe delivers the live tail and no backlog",
			seed: []events.Envelope{autoEnv(idA)},
			live: []events.Envelope{autoEnv(idB), autoEnv(idC)},
			want: []parityFrame{
				{kind: "event", id: idB, body: canonicalJSON(t, autoEnv(idB))},
				{kind: "event", id: idC, body: canonicalJSON(t, autoEnv(idC))},
			},
		},
		{
			name:  "a resume replays the backlog after the cursor, then continues live",
			seed:  []events.Envelope{autoEnv(idA), autoEnv(idB), autoEnv(idC)},
			live:  []events.Envelope{autoEnv(idD)},
			hello: events.HelloFrame{ResumeFrom: idA},
			want: []parityFrame{
				{kind: "event", id: idB, body: canonicalJSON(t, autoEnv(idB))},
				{kind: "event", id: idC, body: canonicalJSON(t, autoEnv(idC))},
				{kind: "event", id: idD, body: canonicalJSON(t, autoEnv(idD))},
			},
		},
		{
			// The empty-backlog resume: the cursor IS the head, so there is
			// nothing to replay and the watermark must be the cursor itself. A
			// binding that mishandled it would re-deliver the whole retained log.
			name:  "a resume from the head replays nothing",
			seed:  []events.Envelope{autoEnv(idA), autoEnv(idB)},
			live:  []events.Envelope{autoEnv(idC)},
			hello: events.HelloFrame{ResumeFrom: idB},
			want: []parityFrame{
				{kind: "event", id: idC, body: canonicalJSON(t, autoEnv(idC))},
			},
		},
		{
			// retention 2: idA has aged out by the time idC lands, so a resume
			// from it is a marked gap, and delivery resumes AT to_id inclusive
			// (EVT-140/141/143).
			name:      "a resume past the retention horizon is a marked gap on both bindings",
			retention: 2,
			seed:      []events.Envelope{autoEnv(idA), autoEnv(idB), autoEnv(idC)},
			hello:     events.HelloFrame{ResumeFrom: idA},
			want: []parityFrame{
				{kind: "gap", id: idB, body: canonicalJSON(t, gapBody{FromID: ptr(idA), ToID: idB, Reason: events.ReasonRetentionExpired})},
				{kind: "event", id: idB, body: canonicalJSON(t, autoEnv(idB))},
				{kind: "event", id: idC, body: canonicalJSON(t, autoEnv(idC))},
			},
		},
		{
			name: "a schemas restriction removes the same events from both bindings",
			live: []events.Envelope{
				autoEnv(idA),
				schemaEnv(idB, events.SchemaBoxVitals),
				autoEnv(idC),
			},
			hello: events.HelloFrame{Schemas: []string{events.SchemaAutomationRun}},
			want: []parityFrame{
				{kind: "event", id: idA, body: canonicalJSON(t, autoEnv(idA))},
				{kind: "event", id: idC, body: canonicalJSON(t, autoEnv(idC))},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			log := events.NewEventLog(c.retention)
			for _, env := range c.seed {
				log.Append(env)
			}
			hub := NewHub(log)
			srv := httptest.NewServer(newTestServer(hub))
			defer srv.Close()

			// BOTH connect before any live append, so both hold the same fresh
			// watermark and are owed the same events.
			br, closeSSE := dialSSE(t, srv, sseQueryFor(c.hello), nil)
			defer closeSSE()
			conn, _ := openWS(t, srv, testAuth().Credential(), c.hello)

			for _, env := range c.live {
				hub.Append(env)
			}

			sse := make([]parityFrame, 0, len(c.want))
			ws := make([]parityFrame, 0, len(c.want))
			for range c.want {
				sse = append(sse, normalizeSSEFrame(t, readFrameWithin(t, br, 3*time.Second)))
				ws = append(ws, normalizeWSFrame(t, conn.next(t, 3*time.Second)))
			}

			if !reflect.DeepEqual(sse, c.want) {
				t.Fatalf("the SSE binding delivered the wrong stream\n got %+v\nwant %+v", sse, c.want)
			}
			if !reflect.DeepEqual(ws, c.want) {
				t.Fatalf("the WS binding delivered a DIFFERENT stream than SSE over the same events\n  ws %+v\n sse %+v", ws, sse)
			}
		})
	}
}

// TestBindingParity_ScopeFilteringAndSelectorAreIdentical is the security half of
// the same property: EVT-120/123's per-event boundary and EVT-121's narrowing
// selector must reach the same verdict on both bindings. A WS binding that
// filtered even slightly differently would be a way to read events the SSE
// binding refuses — the reason the filter is built once, by shared code, rather
// than per transport.
//
// Both sides read a CLOSED Hub's resolved backlog, which is what makes "these
// events were never delivered" a deterministic observation rather than a timing
// guess: SSE ends at EOF, WS ends at the shutdown close, and each stream is
// complete when it ends.
func TestBindingParity_ScopeFilteringAndSelectorAreIdentical(t *testing.T) {
	subtreeOfScreenA := "scope_node subtree " + screenANode

	for _, c := range []struct {
		name     string
		selector string
		alice    []string
		bob      []string
	}{
		{
			name: "each principal sees exactly its own subtree",
			// idA and idC are in site A (alice's binding), idB and idD in site B
			// (bob's); the sentinel is in site A. Neither principal may see the
			// other's, on either binding.
			alice: []string{idA, idC, sentinelID},
			bob:   []string{idB, idD},
		},
		{
			// A narrowing selector INSIDE the principal's reach: screen A's own
			// events only, so idC (at site A, above the named node) is removed
			// for alice. For bob the same term names a node outside his reach,
			// which must read as an ordinary empty result rather than an error
			// (EVT-122) — identically on both bindings.
			name:     "a narrowing selector intersects the visible set the same way",
			selector: subtreeOfScreenA,
			alice:    []string{idA, sentinelID},
			bob:      nil,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newScopeEnv(t)
			e.appendWorld()
			// Closing the Hub makes both streams terminate on their own.
			e.hub.Close()

			for _, who := range []struct {
				name string
				cred authtest.Credential
				want []string
			}{
				{"the site-A principal", e.alice, c.alice},
				{"the site-B principal", e.bob, c.bob},
			} {
				hello := events.HelloFrame{ResumeFrom: seedID, Selector: c.selector}
				sse := sseBacklogFrames(t, e, who.cred, sseQueryFor(hello))
				ws := wsBacklogFrames(t, e.srv, who.cred, hello)

				if got := frameIDs(sse); !reflect.DeepEqual(got, who.want) {
					t.Fatalf("%s: the SSE backlog is wrong\n got %v\nwant %v", who.name, got, who.want)
				}
				if !reflect.DeepEqual(ws, sse) {
					t.Fatalf("%s: the WS binding filtered DIFFERENTLY than SSE over the same events\n  ws %+v\n sse %+v",
						who.name, ws, sse)
				}
			}
		})
	}
}

func frameIDs(frames []parityFrame) []string {
	var out []string
	for _, f := range frames {
		out = append(out, f.id)
	}
	return out
}

// sseBacklogFrames reads one SSE connection's whole resolved backlog to EOF.
func sseBacklogFrames(t *testing.T, e *scopeEnv, cred authtest.Credential, query string) []parityFrame {
	t.Helper()
	resp, err := http.DefaultClient.Do(e.request(t, cred, query))
	if err != nil {
		t.Fatalf("dialing SSE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("the SSE stream must open 200; got %d %s", resp.StatusCode, body)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the SSE backlog to EOF: %v", err)
	}
	return sseParityFrames(t, string(body))
}

// wsBacklogFrames reads one WS connection's whole resolved backlog, ending on the
// shutdown close the already-Closed Hub produces (EVT-096's UNAVAILABLE) rather
// than on a timer.
func wsBacklogFrames(t *testing.T, srv *httptest.Server, cred authtest.Credential, hello events.HelloFrame) []parityFrame {
	t.Helper()
	conn, _ := openWS(t, srv, cred, hello)
	var out []parityFrame
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		_, data, err := conn.conn.Read(ctx)
		cancel()
		if err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) {
				t.Fatalf("the WS backlog stream must end in a close handshake; got %v", err)
			}
			if ce.Reason != events.CloseUnavailable {
				t.Fatalf("a shutdown close must name %s (EVT-096); got %q", events.CloseUnavailable, ce.Reason)
			}
			return out
		}
		var f wsFrame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("a server frame must be UTF-8 JSON (EVT-002): %v data=%s", err, data)
		}
		out = append(out, normalizeWSFrame(t, f))
	}
	t.Fatal("the WS backlog stream never ended")
	return nil
}
