package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/clocktrust"
)

// renderNode prints an AST node back to source, so a wiring assertion can
// read a callback's body rather than walking it.
func renderNode(t *testing.T, n ast.Node) string {
	t.Helper()
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, token.NewFileSet(), n); err != nil {
		t.Fatalf("render node: %v", err)
	}
	return buf.String()
}

// recorder collects what connReporter would have logged.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) logf(format string, v ...any) {
	r.mu.Lock()
	r.lines = append(r.lines, fmt.Sprintf(format, v...))
	r.mu.Unlock()
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func (r *recorder) joined() string { return strings.Join(r.all(), "\n") }

// fakeClock advances only when a test says so, so the elapsed-time claims in
// these lines are asserted rather than approximated.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestReporter() (*connReporter, *recorder, *fakeClock) {
	rec := &recorder{}
	clk := &fakeClock{t: time.Date(2026, 8, 11, 20, 44, 0, 0, time.UTC)}
	return newConnReporter(rec.logf, clk.now), rec, clk
}

// TestReportAttemptThins is the volume rule, stated as a table. 1074
// identical lines is what HV-22 produced and it is not reporting — it is
// noise that buries the report. But thinning too hard loses a brief blip
// entirely, so the first few attempts are always reported.
func TestReportAttemptThins(t *testing.T) {
	cases := []struct {
		n    int
		want bool
		why  string
	}{
		{1, true, "the first failure is the one an operator is most likely to be watching for"},
		{2, true, "a second failure distinguishes a blip from an outage"},
		{3, true, ""},
		{4, true, "first power of two past the always-report window"},
		{5, false, "thinning has to start somewhere or a long outage is 300 lines"},
		{6, false, ""},
		{7, false, ""},
		{8, true, ""},
		{9, false, ""},
		{15, false, ""},
		{16, true, ""},
		{31, false, ""},
		{32, true, ""},
		{255, false, ""},
		{256, true, "an hours-long outage still marks its passage"},
	}
	for _, tc := range cases {
		if got := reportAttempt(tc.n); got != tc.want {
			t.Errorf("reportAttempt(%d) = %v, want %v%s", tc.n, got, tc.want, func() string {
				if tc.why == "" {
					return ""
				}
				return " — " + tc.why
			}())
		}
	}
}

// TestReportedLinesForALongOutageStayReadable is the whole point of the
// thinning, measured against the real incident: 2h33m at a 30s backoff cap
// is ~307 attempts, which produced 1074 log lines in the field.
func TestReportedLinesForALongOutageStayReadable(t *testing.T) {
	reported := 0
	for n := 1; n <= 307; n++ {
		if reportAttempt(n) {
			reported++
		}
	}
	if reported > 15 {
		t.Fatalf("a 2h33m outage would log %d failure lines; the whole point is that it does not bury its own report", reported)
	}
	if reported < 6 {
		t.Fatalf("a 2h33m outage would log only %d failure lines; too sparse to show the outage progressing", reported)
	}
}

// TestDisconnectedNamesTheCauseAndTheUptime: the loss line has to carry the
// client's own cause (which half of the transport noticed) and how long the
// connection had lasted — a connection that died after 12s and one that died
// after 6 hours point at different problems.
func TestDisconnectedNamesTheCauseAndTheUptime(t *testing.T) {
	r, rec, _ := newTestReporter()
	r.disconnected(fmt.Errorf("relayconn: connection died on the read side: EOF"), 12*time.Second)

	line := rec.joined()
	for _, want := range []string{"LOST", "12s", "on the read side", "EOF", "re-dialling"} {
		if !strings.Contains(line, want) {
			t.Errorf("loss line %q does not mention %q", line, want)
		}
	}
	// It must also tell an operator what is still working, or the line reads
	// as "the fleet is down" when the screens are in fact still playing.
	if !strings.Contains(line, "REL-055/061") {
		t.Errorf("loss line %q does not say the screens keep serving the last applied generation offline", line)
	}
}

// TestConnectFailedReportsElapsedOutage: each reported failure says how long
// the relay has been offline. Without it, a thinned line is undated and an
// operator reading attempt 128 cannot tell whether that is two minutes or
// two hours.
func TestConnectFailedReportsElapsedOutage(t *testing.T) {
	r, rec, clk := newTestReporter()
	r.disconnected(fmt.Errorf("read side: EOF"), time.Minute)

	clk.advance(90 * time.Second)
	r.connectFailed(fmt.Errorf("dial tcp: connection refused"), 1, 500*time.Millisecond)
	clk.advance(2 * time.Hour)
	r.connectFailed(fmt.Errorf("dial tcp: connection refused"), 128, 30*time.Second)

	lines := rec.all()
	if len(lines) != 3 {
		t.Fatalf("logged %d line(s), want 3 (one loss + two reported attempts): %v", len(lines), lines)
	}
	if !strings.Contains(lines[1], "1m30s offline") {
		t.Errorf("first failure line %q does not carry the elapsed outage", lines[1])
	}
	if !strings.Contains(lines[2], "2h1m30s offline") {
		t.Errorf("thinned failure line %q does not carry the elapsed outage", lines[2])
	}
	if !strings.Contains(lines[2], "attempt 128") {
		t.Errorf("thinned failure line %q does not say which attempt it is", lines[2])
	}
}

// TestConnectFailedSuppressesThinnedAttempts: the attempts reportAttempt
// filters out must produce NO line at all.
func TestConnectFailedSuppressesThinnedAttempts(t *testing.T) {
	r, rec, _ := newTestReporter()
	for n := 1; n <= 20; n++ {
		r.connectFailed(fmt.Errorf("dial tcp: connection refused"), n, time.Second)
	}
	// 1,2,3,4,8,16 = 6
	if got := len(rec.all()); got != 6 {
		t.Fatalf("20 failed attempts logged %d line(s), want 6: %v", got, rec.all())
	}
}

// TestPinMismatchIsReportedOnceAndUnconditionally is the second rule: a
// condition retrying cannot fix must say so, ahead of the thinning (the
// attempt count is beside the point when no attempt will ever succeed), and
// exactly once per outage (it is a multi-line instruction block; repeating
// it recreates the volume problem it lives inside).
func TestPinMismatchIsReportedOnceAndUnconditionally(t *testing.T) {
	r, rec, _ := newTestReporter()
	pinErr := fmt.Errorf("relayconn: Dial: TLS dial 127.0.0.1:7420: %w", clocktrust.ErrAppPeerKeyMismatch)

	// Attempts 5..7 are thinned away, so if the remedy rode the thinned line
	// it would not appear at all.
	for n := 5; n <= 7; n++ {
		r.connectFailed(pinErr, n, time.Second)
	}

	joined := rec.joined()
	if n := strings.Count(joined, "REL-137"); n == 0 {
		t.Fatal("a trust-pin mismatch produced no REL-137 remedy line even though every attempt carried it")
	}
	if n := strings.Count(joined, "Re-dialling will not fix this"); n != 1 {
		t.Fatalf("the REL-137 remedy block appeared %d time(s) across three attempts, want exactly 1", n)
	}
	for _, want := range []string{"re-enroll this relay", "WAIVEO_FEEDER_URL", "DIFFERENT app peer"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the REL-137 remedy does not mention %q — both real causes have to be named, and a second process holding the same address is the one that actually happened", want)
		}
	}
}

// TestPinRemedyReturnsAfterRecovery: the once-per-outage suppression must
// RESET, or a box that hits the same misconfiguration next week is told
// nothing.
func TestPinRemedyReturnsAfterRecovery(t *testing.T) {
	r, rec, _ := newTestReporter()
	pinErr := fmt.Errorf("dial: %w", clocktrust.ErrAppPeerKeyMismatch)

	r.connectFailed(pinErr, 1, time.Second)
	r.connected()
	r.disconnected(fmt.Errorf("read side: EOF"), time.Minute)
	r.connectFailed(pinErr, 1, time.Second)

	if n := strings.Count(rec.joined(), "Re-dialling will not fix this"); n != 2 {
		t.Fatalf("the REL-137 remedy appeared %d time(s) across two separate outages, want 2 — the suppression is per-outage, not per-process", n)
	}
}

// TestOrdinaryFailureCarriesNoPinRemedy is the mirror: a connection-refused
// really is a wait-and-see condition, and telling an operator to re-enroll
// over one would send them to rebuild trust that is perfectly intact.
func TestOrdinaryFailureCarriesNoPinRemedy(t *testing.T) {
	r, rec, _ := newTestReporter()
	r.connectFailed(fmt.Errorf("relayconn: Dial: TLS dial 127.0.0.1:7420: dial tcp: connect: connection refused"), 1, time.Second)
	if strings.Contains(rec.joined(), "REL-137") {
		t.Fatalf("an ordinary transport failure was reported as a trust-pin mismatch: %q", rec.joined())
	}
}

// TestConnectedReportsRecoveryOnlyAfterAnOutage: the boot connection must
// print nothing. A "RE-ESTABLISHED" line on every healthy start is a line an
// operator learns to skip, and then skips on the day it matters.
func TestConnectedReportsRecoveryOnlyAfterAnOutage(t *testing.T) {
	t.Run("clean boot is silent", func(t *testing.T) {
		r, rec, _ := newTestReporter()
		r.connected()
		if got := rec.all(); len(got) != 0 {
			t.Fatalf("a first, clean connection logged %v, want nothing", got)
		}
	})

	t.Run("recovery is reported", func(t *testing.T) {
		r, rec, clk := newTestReporter()
		r.disconnected(fmt.Errorf("read side: EOF"), time.Minute)
		r.connectFailed(fmt.Errorf("connection refused"), 1, time.Second)
		r.connectFailed(fmt.Errorf("connection refused"), 2, time.Second)
		clk.advance(3 * time.Second)
		r.connected()

		line := rec.all()[len(rec.all())-1]
		for _, want := range []string{"RE-ESTABLISHED", "2 failed attempt(s)", "3s offline"} {
			if !strings.Contains(line, want) {
				t.Errorf("recovery line %q does not mention %q", line, want)
			}
		}
	})
}

// TestIsTrustPinMismatchSurvivesTheRealWrapping: the sentinel is matched
// with errors.Is, and the error it has to match travels out of crypto/tls's
// VerifyPeerCertificate hook and through relayconn's own %w wrapping. Both
// links are asserted here, and the whole chain was confirmed against a
// running feeder whose identity had been swapped.
func TestIsTrustPinMismatchSurvivesTheRealWrapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bare sentinel", clocktrust.ErrAppPeerKeyMismatch, true},
		{"as relayconn wraps it", fmt.Errorf("relayconn: Dial: TLS dial 127.0.0.1:7420: %w", clocktrust.ErrAppPeerKeyMismatch), true},
		{"double wrapped", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", clocktrust.ErrAppPeerKeyMismatch)), true},
		{"a different clocktrust refusal", clocktrust.ErrAppPeerCertOutsideWindow, false},
		{"connection refused", fmt.Errorf("dial tcp: connect: connection refused"), false},
		// A string match on the message would pass this and errors.Is does
		// not: the text is not the contract, the sentinel is.
		{"same words, no sentinel", fmt.Errorf("clocktrust: app peer certificate SubjectPublicKeyInfo does not match the enrollment-anchored pin (REL-137)"), false},
	}
	for _, tc := range cases {
		if got := isTrustPinMismatch(tc.err); got != tc.want {
			t.Errorf("%s: isTrustPinMismatch = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestConnHolderClearDropsTheClient: the corpse-clearing half of HV-22.
func TestConnHolderClearDropsTheClient(t *testing.T) {
	h := &connHolder{}
	if h.get() != nil {
		t.Fatal("a fresh holder is not nil")
	}
	// A non-nil pointer is all this needs; the holder never dereferences it.
	h.set(nil)
	h.clear()
	if h.get() != nil {
		t.Fatal("clear left a client in the holder")
	}
}

// TestMainClearsTheLiveConnectionOnDisconnect is a check on main's SOURCE,
// the same trade TestMainStartsTheAutomationDriveLoops records and for the
// same reason: the unit tests above prove connHolder.clear and connReporter
// work, and cannot prove main wires either one.
//
// That gap is precisely the defect. Before this change the supervisor
// cleared its OWN reference and this binary's holder was never told, so a
// dead client stayed the process's live connection for 2h33m and every
// screen-status report wrote to it. A holder with a clear method nobody
// calls is the same bug with an extra function in it.
func TestMainClearsTheLiveConnectionOnDisconnect(t *testing.T) {
	mainFn := parseRelayMainFunc(t)

	var supervisorCfg *ast.CompositeLit
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "StartSupervisor" {
			return true
		}
		if len(call.Args) == 1 {
			if lit, ok := call.Args[0].(*ast.CompositeLit); ok {
				supervisorCfg = lit
			}
		}
		return true
	})
	if supervisorCfg == nil {
		t.Fatal("func main never calls relayconn.StartSupervisor with a config literal")
	}

	fields := map[string]ast.Expr{}
	for _, el := range supervisorCfg.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok {
			fields[key.Name] = kv.Value
		}
	}

	onDisc, ok := fields["OnDisconnected"]
	if !ok {
		t.Fatal("the supervisor config sets no OnDisconnected. Nothing then drops the dead client from liveConn, and the screen-status reporter goes on writing to a socket the supervisor abandoned — HV-22 verbatim.")
	}
	body := renderNode(t, onDisc)
	if !strings.Contains(body, "liveConn.clear()") {
		t.Errorf("OnDisconnected does not call liveConn.clear(); it reads:\n%s", body)
	}
	if !strings.Contains(body, "connReport.disconnected") {
		t.Errorf("OnDisconnected does not report the loss to the operator; it reads:\n%s", body)
	}

	if _, ok := fields["OnConnectFailed"]; !ok {
		t.Error("the supervisor config sets no OnConnectFailed. Every redial then fails in silence, which is why HV-22's log contained not one line about ~300 refused connection attempts.")
	}
	onConn, ok := fields["OnConnected"]
	if !ok {
		t.Fatal("the supervisor config sets no OnConnected")
	}
	if !strings.Contains(renderNode(t, onConn), "connReport.connected()") {
		t.Error("OnConnected does not report recovery; an operator who saw the loss is never told it came back")
	}
}
