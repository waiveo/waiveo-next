package events

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// corpusEVT091 loads the EVT-091 delivery case — the Task 1 oracle: a WS client
// opens with subprotocol events.v1+json and a hello with no resume_from; the
// server acks resume_result: fresh (EVT-090/091/092/132).
type helloCase struct {
	Input struct {
		Subprotocol string     `json:"subprotocol"`
		Frame       HelloFrame `json:"frame"`
	} `json:"input"`
	Expected struct {
		Frame HelloAckFrame `json:"frame"`
	} `json:"expected"`
}

func loadHelloCase(t *testing.T) helloCase {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "..",
		"conformance", "corpora", "events-1", "EVT-091-valid-hello-fresh-subscribe.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read EVT-091 corpus: %v", err)
	}
	var c helloCase
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal EVT-091 corpus: %v", err)
	}
	return c
}

// TestHello_FreshSubscribe_MatchesCorpus (EVT-090/091/092/132): the EVT-091
// hello carries subprotocol events.v1+json and no resume_from; the server acks
// resume_result: fresh. Asserted against the corpus's own expected frame.
func TestHello_FreshSubscribe_MatchesCorpus(t *testing.T) {
	c := loadHelloCase(t)

	if got, ok := NegotiateSubprotocol([]string{c.Input.Subprotocol}); !ok || got != Subprotocol {
		t.Fatalf("subprotocol %q must negotiate to %q (EVT-090); got (%q,%v)", c.Input.Subprotocol, Subprotocol, got, ok)
	}
	if err := ValidateHelloFirst(c.Input.Frame.Type); err != nil {
		t.Fatalf("a hello first frame must be accepted (EVT-091); got %v", err)
	}
	ack, handled := AckHello(c.Input.Frame)
	if !handled {
		t.Fatal("a hello with no resume_from must be handled as a fresh subscribe (EVT-132)")
	}
	if !reflect.DeepEqual(ack, c.Expected.Frame) {
		t.Fatalf("hello-ack must match the corpus expected frame %+v; got %+v", c.Expected.Frame, ack)
	}
	if ack.Type != "hello-ack" || ack.ResumeResult != "fresh" {
		t.Fatalf("hello-ack must be {type:hello-ack, resume_result:fresh} (EVT-092); got %+v", ack)
	}
}

// TestAckHello_ResumeFromDeferredToResolve (EVT-134): a hello carrying a
// resume_from is NOT acked fresh here — its outcome (resumed|gap|invalid) is
// resolved against the event log by Resolve (resume.go). AckHello reports
// handled=false so a caller never silently treats a resume_from as fresh.
func TestAckHello_ResumeFromDeferredToResolve(t *testing.T) {
	h := HelloFrame{Type: "hello", ResumeFrom: "01J8Z3K4N5P6Q7R8S9T0V1W2Y6"}
	if _, handled := AckHello(h); handled {
		t.Fatal("a hello with a resume_from must not be acked fresh here (EVT-134); it routes through Resolve")
	}
}

// TestValidateHelloFirst_NonHelloRejected (EVT-091): the first WS frame MUST be
// hello; any other first frame closes the connection.
func TestValidateHelloFirst_NonHelloRejected(t *testing.T) {
	if err := ValidateHelloFirst("hello"); err != nil {
		t.Fatalf("hello must be accepted as the first frame; got %v", err)
	}
	for _, ft := range []string{"event", "gap", "hello-ack", "ping", "pong", ""} {
		if err := ValidateHelloFirst(ft); !errors.Is(err, ErrHelloRequired) {
			t.Fatalf("a first frame %q must be rejected with ErrHelloRequired (EVT-091); got %v", ft, err)
		}
	}
}

// TestNegotiateSubprotocol (EVT-090): only events.v1+json is accepted; no
// offer or a different one is refused.
func TestNegotiateSubprotocol(t *testing.T) {
	if got, ok := NegotiateSubprotocol([]string{"events.v1+json"}); !ok || got != "events.v1+json" {
		t.Fatalf("events.v1+json must be accepted; got (%q,%v)", got, ok)
	}
	if got, ok := NegotiateSubprotocol([]string{"json", "events.v2+json"}); ok || got != "" {
		t.Fatalf("a non-matching subprotocol must be refused; got (%q,%v)", got, ok)
	}
	if got, ok := NegotiateSubprotocol(nil); ok || got != "" {
		t.Fatalf("no offered subprotocol must be refused (EVT-090); got (%q,%v)", got, ok)
	}
}

// TestSelectBinding (EVT-001/100): a WS Upgrade selects ws; an
// Accept: text/event-stream with no Upgrade selects sse; neither selects "".
func TestSelectBinding(t *testing.T) {
	cases := []struct {
		hasUpgrade bool
		accept     string
		want       string
	}{
		{true, "", "ws"},
		{true, "text/event-stream", "ws"}, // an Upgrade wins over Accept
		{false, "text/event-stream", "sse"},
		{false, "text/event-stream, */*", "sse"},
		{false, "application/json", ""},
		{false, "", ""},
	}
	for _, tc := range cases {
		if got := SelectBinding(tc.hasUpgrade, tc.accept); got != tc.want {
			t.Fatalf("SelectBinding(%v,%q) = %q; want %q", tc.hasUpgrade, tc.accept, got, tc.want)
		}
	}
}

// TestSSEEventLine (EVT-103): an event is framed event: event / id: <env.id> /
// data: <the envelope as one JSON line>, terminated by a blank line.
func TestSSEEventLine(t *testing.T) {
	env := Envelope{
		ID:             "01J8Z3K4N5P6Q7R8S9T0V1W2Y7",
		Schema:         "box.vitals",
		TS:             1752537600000,
		ScopeNode:      "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
		TraceID:        "01J8Z3K4N5P6Q7R8S9T0V1W2Y7",
		CostClass:      "telemetry",
		RetentionClass: "telemetry-standard",
		Origin:         "relay",
		Payload:        json.RawMessage(`{"relay_id":"01J8Z3K4N5P6Q7R8S9T0V1W2ZF"}`),
	}
	line, err := SSEEventLine(env)
	if err != nil {
		t.Fatalf("SSEEventLine: %v", err)
	}
	data, _ := json.Marshal(env)
	want := "event: event\nid: 01J8Z3K4N5P6Q7R8S9T0V1W2Y7\ndata: " + string(data) + "\n\n"
	if line != want {
		t.Fatalf("SSE event line mismatch (EVT-103):\n got %q\nwant %q", line, want)
	}
	// The data field must round-trip back to the same envelope.
	body := strings.TrimSuffix(strings.TrimPrefix(strings.Split(line, "\n")[2], "data: "), "")
	var back Envelope
	if err := json.Unmarshal([]byte(body), &back); err != nil {
		t.Fatalf("SSE data line must be valid envelope JSON: %v", err)
	}
	if back.ID != env.ID {
		t.Fatalf("round-tripped envelope id = %q; want %q", back.ID, env.ID)
	}
}

// TestSSEEventLine_InvalidPayloadErrors: an envelope whose payload is not valid
// JSON cannot be framed — SSEEventLine surfaces the marshal error rather than
// emitting a corrupt data line.
func TestSSEEventLine_InvalidPayloadErrors(t *testing.T) {
	env := Envelope{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", Payload: json.RawMessage(`{not json`)}
	if _, err := SSEEventLine(env); err == nil {
		t.Fatal("an envelope with an invalid payload must produce an error, not a corrupt SSE line")
	}
}

// TestSSEGapLine (EVT-104): a gap is framed event: gap / id: <to_id> /
// data: <the {from_id,to_id,reason} loss marker>, so a native reconnect's
// Last-Event-ID lands exactly at the resumed point.
func TestSSEGapLine(t *testing.T) {
	from := "01J8Z3K4N5P6Q7R8S9T0V1W2Y6"
	g := Gap(&from, "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "retention_expired")
	line := SSEGapLine(g)
	want := "event: gap\nid: 01J8Z3K4N5P6Q7R8S9T0V1W2Y7\n" +
		`data: {"from_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Y6","to_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Y7","reason":"retention_expired"}` +
		"\n\n"
	if line != want {
		t.Fatalf("SSE gap line mismatch (EVT-104):\n got %q\nwant %q", line, want)
	}
}

// TestSSEGapLine_NullFromID (EVT-140): from_id is null — present, not omitted —
// when the subscriber has no prior known point.
func TestSSEGapLine_NullFromID(t *testing.T) {
	g := Gap(nil, "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "buffer_exceeded")
	line := SSEGapLine(g)
	if !strings.Contains(line, `"from_id":null`) {
		t.Fatalf("a gap with no prior point must render from_id:null (EVT-140); got %q", line)
	}
}

// TestGapFrame_JSON (EVT-094/140): the WS gap frame carries type:gap and the
// {from_id,to_id,reason} loss marker; from_id is null when nil, a value when set.
func TestGapFrame_JSON(t *testing.T) {
	from := "01J8Z3K4N5P6Q7R8S9T0V1W2Y6"
	b, err := json.Marshal(Gap(&from, "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "retention_expired"))
	if err != nil {
		t.Fatalf("marshal gap frame: %v", err)
	}
	want := `{"type":"gap","from_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Y6","to_id":"01J8Z3K4N5P6Q7R8S9T0V1W2Y7","reason":"retention_expired"}`
	if string(b) != want {
		t.Fatalf("WS gap frame mismatch (EVT-094):\n got %s\nwant %s", b, want)
	}
	bn, _ := json.Marshal(Gap(nil, "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "buffer_exceeded"))
	if !strings.Contains(string(bn), `"from_id":null`) {
		t.Fatalf("a nil from_id must marshal to null (EVT-140); got %s", bn)
	}
}

// TestEventFrame_JSON (EVT-093): a delivered event is a type:event frame
// wrapping the durable-event envelope under the event key.
func TestEventFrame_JSON(t *testing.T) {
	env := Envelope{ID: "01J8Z3K4N5P6Q7R8S9T0V1W2Y7", Schema: "box.vitals", Payload: json.RawMessage(`{}`)}
	b, err := json.Marshal(EventFrame{Type: "event", Event: env})
	if err != nil {
		t.Fatalf("marshal event frame: %v", err)
	}
	var back struct {
		Type  string   `json:"type"`
		Event Envelope `json:"event"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal event frame: %v", err)
	}
	if back.Type != "event" || back.Event.ID != env.ID {
		t.Fatalf("event frame must wrap the envelope under event (EVT-093); got %s", b)
	}
	// Event() is what the live WS binding builds, so it must produce that same
	// frame rather than a second spelling of the discriminator.
	if got := Event(env); got.Type != FrameTypeEvent || got.Event.ID != env.ID {
		t.Fatalf("Event() must build the {type:event, event:<envelope>} frame (EVT-093); got %+v", got)
	}
}

// TestPingPong_JSON (EVT-095): the ping/pong keepalive frames.
func TestPingPong_JSON(t *testing.T) {
	pb, _ := json.Marshal(Ping())
	if string(pb) != `{"type":"ping"}` {
		t.Fatalf(`ping frame must be {"type":"ping"} (EVT-095); got %s`, pb)
	}
	qb, _ := json.Marshal(Pong())
	if string(qb) != `{"type":"pong"}` {
		t.Fatalf(`pong frame must be {"type":"pong"} (EVT-095); got %s`, qb)
	}
}

// TestCloseReason (EVT-096): a server-initiated close names one of this
// contract's error-taxonomy codes; an unrecognized code falls back to INTERNAL.
func TestCloseReason(t *testing.T) {
	for _, code := range []string{
		CloseIdleTimeout, CloseAuthRequired, CloseSlowConsumer,
		CloseResumeFromInvalid, CloseSelectorInvalid, CloseUnavailable, CloseInternal,
	} {
		if got := CloseReason(code); got != code {
			t.Fatalf("CloseReason(%q) must name the code (EVT-096); got %q", code, got)
		}
	}
	// A code from NO registry — the class of value EVT-096 exists to keep off
	// the wire — is folded to the taxonomy's unclassified entry rather than
	// echoed.
	for _, code := range []string{"PROTOCOL_VIOLATION", "WS_BINDING_DEFERRED", "nonsense", ""} {
		if got := CloseReason(code); got != CloseInternal {
			t.Fatalf("CloseReason(%q) must fall back to %s (EVT-096); got %q", code, CloseInternal, got)
		}
	}
}

// TestServerCloseCodesAreContractTaxonomyCodes is EVT-096's real oracle: "A
// server-initiated close MUST carry a WS close reason naming one of this
// contract's error-taxonomy codes". The set of names this package can put on the
// wire is therefore checked against the CONTRACT'S OWN table, parsed out of
// contracts/events-1.md, rather than against a second copy of the table written
// down in this test — a copy would agree with the code by construction and prove
// nothing about the contract.
//
// It is the enforcement point for the whole property, because CloseReason is the
// only way a reason reaches the wire: whatever a caller asks for, the value
// emitted is a member of serverCloseCodes or the unclassified fallback, and this
// test pins every member of that set to a published code.
func TestServerCloseCodesAreContractTaxonomyCodes(t *testing.T) {
	taxonomy := contractErrorTaxonomy(t)
	// Both directions, so the parser cannot pass this check by returning
	// everything (or nothing): a code the table really publishes is present, and
	// the code this binding's refusal used to invent is not.
	if !taxonomy["SLOW_CONSUMER_DISCONNECTED"] {
		t.Fatal("the Error taxonomy parser did not find SLOW_CONSUMER_DISCONNECTED in contracts/events-1.md; it is reading the wrong thing")
	}
	if taxonomy["WS_BINDING_DEFERRED"] {
		t.Fatal("the Error taxonomy parser reported an unpublished code as published; it is matching too much")
	}
	for code := range serverCloseCodes {
		if !taxonomy[code] {
			t.Errorf("a server-initiated WS close may name %q, which contracts/events-1.md's Error taxonomy does not publish — "+
				"EVT-096 closes the vocabulary to that table", code)
		}
	}
	// And the fallback itself, which is emitted for every unrecognized code.
	if !taxonomy[CloseInternal] {
		t.Errorf("CloseReason's fallback %q is not a published error-taxonomy code", CloseInternal)
	}
}

// contractErrorTaxonomy reads the `code` column of contracts/events-1.md's
// "Error taxonomy" table. The table's rows are `| ` + "`CODE`" + ` | meaning |
// retryable |`, so the first backticked cell of each row inside that section is
// the published code.
func contractErrorTaxonomy(t *testing.T) map[string]bool {
	t.Helper()
	_, self, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(self), "..", "..", "contracts", "events-1.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	codes := map[string]bool{}
	inSection := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "## ") {
			inSection = line == "## Error taxonomy"
			continue
		}
		if !inSection || !strings.HasPrefix(line, "| `") {
			continue
		}
		cell := strings.TrimPrefix(line, "| `")
		if i := strings.Index(cell, "`"); i > 0 {
			codes[cell[:i]] = true
		}
	}
	return codes
}
