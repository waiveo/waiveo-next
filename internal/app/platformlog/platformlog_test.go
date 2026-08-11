package platformlog

import (
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
)

// clockAt returns a settable clock so a test can place records in time.
func clockAt(start int64) (func() int64, func(int64)) {
	now := start
	return func() int64 { return now }, func(v int64) { now = v }
}

// TestTheStandardLoggerIsCapturedWithNothingAtTheCallSite is the property the
// whole "tee, don't instrument" design rests on: a line written through the
// ordinary `log` package — with no knowledge of this buffer — is captured.
//
// If this ever stops holding, the diagnostics page silently becomes a page about
// whichever code somebody remembered to instrument.
func TestTheStandardLoggerIsCapturedWithNothingAtTheCallSite(t *testing.T) {
	now, _ := clockAt(1_752_537_600_000)
	b := New(16, now)

	var stderr strings.Builder
	lg := log.New(io.MultiWriter(&stderr, b), "", log.LstdFlags)
	lg.Printf("waiveo-feeder: listening on :7420")

	snap := b.Read(Filter{})
	if len(snap.Records) != 1 {
		t.Fatalf("captured %d record(s), want 1: %+v", len(snap.Records), snap.Records)
	}
	got := snap.Records[0]
	if got.Source != "waiveo-feeder" {
		t.Errorf("source = %q, want waiveo-feeder", got.Source)
	}
	if got.Message != "listening on :7420" {
		t.Errorf("message = %q, want the line with its prefix removed", got.Message)
	}
	if got.TSMs != 1_752_537_600_000 {
		t.Errorf("ts = %d, want the capture clock — never the stdlib line's own local-time prefix", got.TSMs)
	}
	// The tee must not have swallowed the line on its way to stderr: journald is
	// the real log and this buffer is the convenience.
	if !strings.Contains(stderr.String(), "listening on :7420") {
		t.Errorf("stderr got %q — the capture must be a TEE, never a redirect", stderr.String())
	}
}

// TestWriteNeverErrorsSoItCannotSilenceStderr: this buffer sits inside an
// io.MultiWriter, which aborts on the first writer that errors. An error here
// would take stderr — and therefore journald — down with it.
func TestWriteNeverErrorsSoItCannotSilenceStderr(t *testing.T) {
	now, _ := clockAt(1)
	b := New(2, now)
	for i := 0; i < 100; i++ {
		p := []byte(strings.Repeat("x", 9000) + "\n")
		n, err := b.Write(p)
		if err != nil {
			t.Fatalf("Write returned %v; it must never error", err)
		}
		if n != len(p) {
			t.Fatalf("Write reported %d of %d bytes; a short write aborts an io.MultiWriter just as an error does", n, len(p))
		}
	}
}

// TestAPartialWriteIsNotClassifiedAsAWholeLine: io.Writer makes no promise that
// one Write is one line. A record built from half a sentence would be
// classified, filtered and displayed as a line nobody wrote.
func TestAPartialWriteIsNotClassifiedAsAWholeLine(t *testing.T) {
	now, _ := clockAt(1)
	b := New(8, now)

	_, _ = b.Write([]byte("waiveo-relay: the ECP probe "))
	if got := b.Read(Filter{}).Retained; got != 0 {
		t.Fatalf("a write with no newline produced %d record(s); it must be held until the line completes", got)
	}
	_, _ = b.Write([]byte("failed after 12s\n"))

	snap := b.Read(Filter{})
	if len(snap.Records) != 1 {
		t.Fatalf("got %d record(s), want the two writes joined into one: %+v", len(snap.Records), snap.Records)
	}
	if snap.Records[0].Message != "the ECP probe failed after 12s" {
		t.Errorf("message = %q, want the whole joined line", snap.Records[0].Message)
	}
	if snap.Records[0].Level != LevelError {
		t.Errorf("level = %q, want error — the marker was in the SECOND fragment", snap.Records[0].Level)
	}
}

// TestTheRingDropsOldestAndSaysSo. A bounded buffer that quietly loses lines
// tells an operator "nothing happened" when what happened is "everything, and it
// scrolled away" — which sends them to the wrong hypothesis.
func TestTheRingDropsOldestAndSaysSo(t *testing.T) {
	now, _ := clockAt(1_000)
	b := New(4, now)
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(b, "svc: line %d\n", i)
	}
	snap := b.Read(Filter{})
	if snap.Retained != 4 {
		t.Fatalf("retained %d, want 4 (the ring's capacity)", snap.Retained)
	}
	if snap.Dropped != 6 {
		t.Errorf("dropped = %d, want 6 — an operator must be able to tell a quiet box from a wrapped buffer", snap.Dropped)
	}
	if snap.Capacity != 4 {
		t.Errorf("capacity = %d, want 4", snap.Capacity)
	}
	// Newest first, and the survivors are the newest four.
	want := []string{"line 10", "line 9", "line 8", "line 7"}
	for i, w := range want {
		if snap.Records[i].Message != w {
			t.Errorf("record %d = %q, want %q (newest first)", i, snap.Records[i].Message, w)
		}
	}
}

// TestALimitTakesTheNEWESTMatchesNotTheOldest. Limiting before reversing would
// hand a diagnostics page the oldest 200 lines of a busy incident, which is the
// half that has already been read.
func TestALimitTakesTheNEWESTMatchesNotTheOldest(t *testing.T) {
	now, _ := clockAt(1)
	b := New(50, now)
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(b, "svc: line %d\n", i)
	}
	snap := b.Read(Filter{Limit: 3})
	if len(snap.Records) != 3 {
		t.Fatalf("got %d record(s), want 3", len(snap.Records))
	}
	if snap.Records[0].Message != "line 20" || snap.Records[2].Message != "line 18" {
		t.Errorf("limited page = %q..%q, want the three NEWEST", snap.Records[0].Message, snap.Records[2].Message)
	}
	if snap.Matched != 20 {
		t.Errorf("matched = %d, want 20 — the page must be able to say how many it is showing OF how many", snap.Matched)
	}
}

// TestFiltersNarrowOnLevelSourceAndText, each independently and together.
func TestFiltersNarrowOnLevelSourceAndText(t *testing.T) {
	now, _ := clockAt(1)
	b := New(50, now)
	fmt.Fprint(b, "waiveo-feeder: listening on :7420\n")
	fmt.Fprint(b, "waiveo-relay: ECP command failed after 12s\n")
	fmt.Fprint(b, "waiveo-relay: retrying the app peer connection\n")
	fmt.Fprint(b, "waiveo-feeder: pruned 4 expired events\n")

	if got := b.Read(Filter{Level: LevelError}).Matched; got != 1 {
		t.Errorf("level=error matched %d, want 1", got)
	}
	if got := b.Read(Filter{Level: LevelWarn}).Matched; got != 1 {
		t.Errorf("level=warn matched %d, want 1", got)
	}
	if got := b.Read(Filter{Source: "waiveo-relay"}).Matched; got != 2 {
		t.Errorf("source=waiveo-relay matched %d, want 2", got)
	}
	if got := b.Read(Filter{Contains: "ECP"}).Matched; got != 1 {
		t.Errorf("contains=ECP matched %d, want 1", got)
	}
	if got := b.Read(Filter{Contains: "ecp"}).Matched; got != 1 {
		t.Errorf("contains is case-insensitive: matched %d, want 1", got)
	}
	if got := b.Read(Filter{Source: "waiveo-relay", Level: LevelError}).Matched; got != 1 {
		t.Errorf("combined filter matched %d, want 1", got)
	}
}

// TestSourcesAndCountsDescribeTheWHOLEBufferNotTheFilteredPage. A source filter
// built from the filtered results is a control that can only ever offer the
// option already chosen — the classic dead-end where the UI narrows itself into
// a corner an operator cannot get out of.
func TestSourcesAndCountsDescribeTheWHOLEBufferNotTheFilteredPage(t *testing.T) {
	now, _ := clockAt(1)
	b := New(50, now)
	fmt.Fprint(b, "waiveo-feeder: listening\n")
	fmt.Fprint(b, "waiveo-relay: ECP command failed\n")
	fmt.Fprint(b, "http: TLS handshake error from 10.0.0.4\n")

	snap := b.Read(Filter{Source: "waiveo-feeder"})
	if len(snap.Records) != 1 {
		t.Fatalf("filtered page has %d record(s), want 1", len(snap.Records))
	}
	want := []string{"http", "waiveo-feeder", "waiveo-relay"}
	if strings.Join(snap.Sources, ",") != strings.Join(want, ",") {
		t.Errorf("sources = %v, want every retained source sorted (%v) even under a filter", snap.Sources, want)
	}
	if snap.LevelCounts[LevelError] != 2 {
		t.Errorf("error count = %d, want 2 — the counts describe the buffer, not the page", snap.LevelCounts[LevelError])
	}
	if snap.Retained != 3 {
		t.Errorf("retained = %d, want 3", snap.Retained)
	}
}

// TestRetainedFromMsIsTheOldestRecordsInstant — the honest answer to "how far
// back does this page see", which for an in-process ring is never "since boot".
func TestRetainedFromMsIsTheOldestRecordsInstant(t *testing.T) {
	now, set := clockAt(1_000)
	b := New(2, now)
	fmt.Fprint(b, "svc: one\n")
	set(2_000)
	fmt.Fprint(b, "svc: two\n")
	set(3_000)
	fmt.Fprint(b, "svc: three\n") // evicts "one"

	snap := b.Read(Filter{})
	if snap.RetainedFromMs != 2_000 {
		t.Errorf("retained_from = %d, want 2000 — the oldest SURVIVING record, not the first ever written", snap.RetainedFromMs)
	}
}

// TestAnEmptyBufferReadsAsEmptyRatherThanPanicking, and reports a usable
// Snapshot: a diagnostics page renders at boot, before anything has been logged.
func TestAnEmptyBufferReadsAsEmptyRatherThanPanicking(t *testing.T) {
	now, _ := clockAt(1)
	snap := New(4, now).Read(Filter{Level: LevelError, Limit: 10})
	if snap.Retained != 0 || len(snap.Records) != 0 || snap.RetainedFromMs != 0 {
		t.Fatalf("empty buffer read as %+v", snap)
	}
	if snap.LevelCounts == nil {
		t.Error("LevelCounts is nil on an empty buffer; a page rendering counts would have to nil-check every read")
	}
}

// TestConcurrentWritesAndReadsDoNotRace is run under -race in CI. The standard
// logger serializes its own writes, but Read is called from HTTP handlers.
func TestConcurrentWritesAndReadsDoNotRace(t *testing.T) {
	now, _ := clockAt(1)
	b := New(64, now)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				fmt.Fprintf(b, "writer-%d: line %d\n", w, i)
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = b.Read(Filter{Limit: 10})
			}
		}()
	}
	wg.Wait()
}

// TestNewRequiresAClock, for internal/app/screens' reason: every value this type
// produces is a timestamp, and a nil clock could only answer zero.
func TestNewRequiresAClock(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New(16, nil) returned; it must refuse")
		}
	}()
	_ = New(16, nil)
}

// TestValidLevelIsTheClosedSet — a filter value outside it must be refusable at
// the api boundary, because a silently-non-matching filter reads as a quiet box.
func TestValidLevelIsTheClosedSet(t *testing.T) {
	for _, l := range Levels {
		if !ValidLevel(string(l)) {
			t.Errorf("ValidLevel(%q) = false for a declared level", l)
		}
	}
	for _, bad := range []string{"", "ERROR", "debug", "trace", "critical"} {
		if ValidLevel(bad) {
			t.Errorf("ValidLevel(%q) = true", bad)
		}
	}
}
