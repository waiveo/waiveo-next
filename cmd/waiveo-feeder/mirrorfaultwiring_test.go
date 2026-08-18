package main

import (
	"bytes"
	"database/sql"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/shared/wire"

	_ "modernc.org/sqlite"
)

// mirrorfaultwiring_test.go drives the reporting through ApplyCandidates against
// a REAL store that really cannot be written to, and reads the REAL log.
//
// mirrorfaults_test.go beside it exercises the tracker's arithmetic directly,
// which is the right way to say "1017 failures produce 33 lines". What it cannot
// say is that the sink is wired to it at all: every one of those tests passes
// with candidatemirror.go reverted to the single unconditional log.Printf it
// replaced. The wiring — first-failure wording, the collapse, the recovery line,
// and the counts the lines carry — is what cost seven days on box .12 when it
// was absent, so it is verified here from the outside, on the same fault.
//
// The fault forged is box .12's exactly: `discovered_devices` with no
// `open_ports` column, which is not a transient disk problem and can never
// re-converge, and which the store reports on every single write.

// breakTheMirror drops the column the mirror write names, reproducing the state
// box .12 was found in. Done through a second connection so the store keeps its
// own handle open, exactly as a running feeder does.
func breakTheMirror(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dsn+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`ALTER TABLE discovered_devices DROP COLUMN open_ports`); err != nil {
		t.Fatalf("forge the missing column: %v", err)
	}
}

// healTheMirror puts the column back, as the next boot's migration would.
func healTheMirror(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dsn+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`ALTER TABLE discovered_devices ADD COLUMN open_ports TEXT NOT NULL DEFAULT '[]'`); err != nil {
		t.Fatalf("heal the column: %v", err)
	}
}

func TestMirrorFailureIsReportedThenCollapsedThenRecovered(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "app.db")
	st := cmOpenStore(t, dsn)
	t.Cleanup(func() { _ = st.Close() })

	// The app's clock, injected: a minute per relay report, which is the real
	// reporting cadence.
	nowMs := int64(1_752_800_000_000)
	sink := newCandidateMirror(devices.New(cmSite, func() int64 { return 10_000 }), st, func() int64 { return nowMs })

	var out bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&out)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	report := func() {
		t.Helper()
		if err := sink.ApplyCandidates(cmRelayA, []wire.DeviceCandidate{
			cmCandidate(cmNativeA, "Lobby TV", "192.168.50.31:8060"),
		}); err != nil {
			t.Fatalf("ApplyCandidates must not fail the relay's report: %v", err)
		}
		nowMs += 60_000
	}

	// A healthy mirror says nothing at all.
	report()
	if out.Len() != 0 {
		t.Fatalf("a working mirror must be silent; got:\n%s", out.String())
	}

	breakTheMirror(t, dsn)

	// The FIRST failure is reported immediately, and says what is actually at
	// stake — not "retried on the next report", which was true and useless.
	out.Reset()
	report()
	first := out.String()
	if strings.Count(first, "\n") != 1 {
		t.Fatalf("the first failure must produce exactly one line; got:\n%s", first)
	}
	for _, want := range []string{"FAILED", "durable mirror", "restart will lose the device list", "open_ports"} {
		if !strings.Contains(first, want) {
			t.Fatalf("the first-failure line must carry %q; got:\n%s", want, first)
		}
	}

	// A week of the same failure, one report a minute: it must neither repeat
	// per report nor go quiet, and the lines must carry the count and the age.
	out.Reset()
	const week = 7 * 24 * 60
	for i := 0; i < week; i++ {
		report()
	}
	lines := strings.Count(out.String(), "\n")
	if lines == 0 {
		t.Fatalf("a continuing fault must keep saying so; got no lines over %d reports", week)
	}
	if lines > 40 {
		t.Fatalf("a week-long fault must not log per report; got %d lines over %d reports:\n%s",
			lines, week, out.String())
	}
	if !strings.Contains(out.String(), "in a row over") {
		t.Fatalf("a repeated failure must say how many and for how long; got:\n%s", out.String())
	}
	last := strings.TrimSpace(out.String())
	last = last[strings.LastIndex(last, "\n")+1:]
	if !strings.Contains(last, "needs looking at") {
		t.Fatalf("the last line of a continuing fault must still escalate; got %q", last)
	}

	// And recovery is a fact of its own: "no issues" and "the reporting stopped"
	// must not read the same.
	healTheMirror(t, dsn)
	out.Reset()
	report()
	recovered := out.String()
	if !strings.Contains(recovered, "working again after") {
		t.Fatalf("a mirror that starts working again must say so; got:\n%s", recovered)
	}
	if !strings.Contains(recovered, "consecutive failure(s)") {
		t.Fatalf("the recovery line must report the size of what just ended; got:\n%s", recovered)
	}

	// Once. A healthy mirror is silent again immediately.
	out.Reset()
	report()
	if out.Len() != 0 {
		t.Fatalf("recovery must be reported once, not on every later report; got:\n%s", out.String())
	}
}
