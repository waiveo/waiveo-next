//go:build unix

package identity

// Power-pull torture harness (§10 appliance-physics). The relay is an
// appliance that has died of power and disk before
// (deploy-box-disk-fills-with-dangling-images): §10 makes crash-atomicity of
// the operational store a gate, not a nicety. This harness is the dev-side
// proxy for the on-box power-pull bench rig: it re-invokes this test binary as
// a subprocess "writer" that opens the operational store and, in a tight loop,
// commits coherent apply-units (a durable telemetry entry + an advanced
// last-applied {generation, hash} + an advanced clock floor + an advanced seq
// high-water), then SIGKILLs it mid-write at a delay derived from the
// iteration index (not wall-clock randomness, so a failure reproduces). The
// parent reopens the store and asserts full recovery: the DB opens clean,
// identity is intact, and no torn last-applied surfaces — every persisted
// apply-unit is present in full or absent in full, never half-applied
// (REL-055/056 atomic swap, REL-090/093 durable telemetry, REL-130 clock
// floor).
//
// What a SIGKILL genuinely exercises. A killed process loses nothing already
// handed to the OS: the page cache outlives the process, so SIGKILL cannot
// simulate the RAM-loss of a real power cut. WAL + synchronous=FULL's fsync
// ordering (Task 1) is what protects that last step, and it is verified on the
// real hardware bench rig (Deferred). What SIGKILL DOES expose, and what this
// proxy gates, is atomic-commit recovery: a coherent multi-object apply-unit
// committed as one transaction never surfaces torn across a kill, while the
// same unit written WITHOUT the store's write discipline (raw journal_mode=OFF,
// non-atomic statements) surfaces a torn last-applied that the harness's own
// verifier catches — the "teeth" the durability test's clean-recovery claim
// would be worthless without (TestPowerPullTortureHarnessHasTeeth).

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Writer-subprocess protocol. TestMain routes an invocation carrying
// envWriterMode into the writer instead of the test suite; the parent hands it
// a scratch DB path via envWriterDB and reads a single "READY\n" line off its
// stdout before killing, so every kill lands during active writing rather than
// on a store that has not begun to write.
const (
	envWriterMode = "RELAY_POWERPULL_WRITER"
	envWriterDB   = "RELAY_POWERPULL_DB"

	// writerModeAtomic commits each apply-unit through the store's real write
	// discipline (identity.Open → WAL + synchronous=FULL) as a single
	// transaction. writerModeTorn deliberately drops that discipline (raw
	// journal_mode=OFF, each apply-unit written as separate non-atomic
	// statements) so a mid-write kill can strand a torn last-applied.
	writerModeAtomic = "atomic"
	writerModeTorn   = "torn"

	// powerPullRelayID is the fixture relay identity the writer persists once
	// before its write loop; the parent asserts it survives every kill intact.
	powerPullRelayID = "01J8Z4K4N5P6Q7R8S9T0V1W3A1"

	// powerPullClockBase anchors the writer's derived clock-floor / recorded-at
	// values so the parent can recompute and check them against the recovered
	// generation (REL-130 monotonic floor).
	powerPullClockBase int64 = 1_752_800_000_000

	// atomicWarmupTicks apply-units are committed before the atomic writer
	// announces READY, so a recovered store always holds a non-trivial history
	// and the parent can tell that the writer genuinely progressed under kill
	// (recovered generation > warmup).
	atomicWarmupTicks = 10
)

// powerPullHash is the deterministic {generation → hash} relation both the
// writer (when persisting last-applied) and the parent (when verifying the
// recovered pair) compute, so a torn generation/hash pair — a hash that does
// not match its own generation — is detectable on reopen (REL-055).
func powerPullHash(generation int64) string {
	return fmt.Sprintf("sha256:%016x", generation)
}

// TestMain routes a subprocess carrying envWriterMode into the power-pull
// writer (which never returns — it is SIGKILLed by its parent, or exits
// nonzero on a setup error); an ordinary invocation runs the test suite.
func TestMain(m *testing.M) {
	if mode := os.Getenv(envWriterMode); mode != "" {
		powerPullWriter(mode, os.Getenv(envWriterDB))
		os.Exit(0) // unreachable in practice: the parent SIGKILLs the writer.
	}
	os.Exit(m.Run())
}

// powerPullWriter is the subprocess entry point. It persists a fixed identity
// once, then loops committing apply-units until its parent kills it. On any
// setup error it exits with a distinct nonzero code so a harness failure is
// never mistaken for a clean recovery.
func powerPullWriter(mode, dbPath string) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		os.Exit(10)
	}

	switch mode {
	case writerModeAtomic:
		// Real write discipline: WAL + synchronous=FULL, apply-unit = one tx.
		st, err := Open(dbPath)
		if err != nil {
			os.Exit(11)
		}
		if err := st.SetIdentity(powerPullRelayID, []byte("cert-fixture"), priv); err != nil {
			os.Exit(12)
		}
		writeAtomicTicks(st)

	case writerModeTorn:
		// Deliberately WITHOUT the store's write discipline: a raw connection
		// with journal_mode=OFF and no transaction around the apply-unit, so a
		// kill between the entry-append and the generation-advance strands a
		// torn last-applied. This is the negative control that gives the
		// harness teeth.
		db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)")
		if err != nil {
			os.Exit(21)
		}
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(schema); err != nil {
			os.Exit(22)
		}
		if _, err := db.Exec(telemetrySchema); err != nil {
			os.Exit(23)
		}
		keyPEM, err := marshalPrivateKey(priv)
		if err != nil {
			os.Exit(24)
		}
		if _, err := db.Exec(
			`INSERT INTO relay_identity (id, relay_id, cert_pem, private_key_pem) VALUES (1, ?, ?, ?)`,
			powerPullRelayID, []byte("cert-fixture"), keyPEM,
		); err != nil {
			os.Exit(25)
		}
		writeTornTicks(db)

	default:
		os.Exit(9)
	}
}

// writeAtomicTicks commits apply-units as single transactions through the
// disciplined store, forever. After atomicWarmupTicks it announces READY; the
// parent then kills it mid-transaction. Because each unit is one atomic commit,
// the last committed unit is present in full and any in-flight one is absent in
// full on reopen (no torn last-applied).
func writeAtomicTicks(st *Store) {
	tick := int64(0)
	commit := func() {
		tick++
		tx, err := st.db.Begin()
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := tx.Exec(
			`INSERT INTO telemetry_queue (seq, schema, payload, subject, recorded_at) VALUES (?, ?, ?, ?, ?)`,
			tick, "automation.run", []byte(fmt.Sprintf(`{"tick":%d}`, tick)), powerPullRelayID, powerPullClockBase+tick,
		); err != nil {
			return
		}
		if _, err := tx.Exec(
			`INSERT INTO last_applied_generation (id, generation, hash) VALUES (1, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET generation = excluded.generation, hash = excluded.hash`,
			tick, powerPullHash(tick),
		); err != nil {
			return
		}
		if _, err := tx.Exec(
			`INSERT INTO clock_floor (id, floor_ms) VALUES (1, ?)
			 ON CONFLICT (id) DO UPDATE SET floor_ms = excluded.floor_ms WHERE excluded.floor_ms > clock_floor.floor_ms`,
			powerPullClockBase+tick,
		); err != nil {
			return
		}
		if _, err := tx.Exec(
			`INSERT INTO telemetry_seq_high_water (id, seq) VALUES (1, ?)
			 ON CONFLICT (id) DO UPDATE SET seq = excluded.seq WHERE excluded.seq > telemetry_seq_high_water.seq`,
			tick,
		); err != nil {
			return
		}
		_ = tx.Commit()
	}

	for i := 0; i < atomicWarmupTicks; i++ {
		commit()
	}
	announceReady()
	for {
		commit()
	}
}

// writeTornTicks writes apply-units WITHOUT the store's atomic discipline: the
// telemetry entry and the last-applied generation are separate, un-transacted
// statements. It warms up with a few complete units, then appends one more
// entry, announces READY while the matching generation-advance is still
// outstanding, and hangs — so the parent's immediate kill deterministically
// strands a torn last-applied (a durable entry present whose generation was
// never advanced to match it).
func writeTornTicks(db *sql.DB) {
	tick := int64(0)
	appendEntry := func() {
		tick++
		_, _ = db.Exec(
			`INSERT INTO telemetry_queue (seq, schema, payload, subject, recorded_at) VALUES (?, ?, ?, ?, ?)`,
			tick, "automation.run", []byte(fmt.Sprintf(`{"tick":%d}`, tick)), powerPullRelayID, powerPullClockBase+tick,
		)
	}
	advanceGeneration := func() {
		_, _ = db.Exec(
			`INSERT INTO last_applied_generation (id, generation, hash) VALUES (1, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET generation = excluded.generation, hash = excluded.hash`,
			tick, powerPullHash(tick),
		)
	}

	for i := 0; i < 5; i++ {
		appendEntry()
		advanceGeneration()
	}
	// Open the torn window: entry committed, generation not yet advanced.
	appendEntry()
	announceReady()
	time.Sleep(30 * time.Second) // parent kills us here, inside the window.
	advanceGeneration()
	for {
		appendEntry()
		advanceGeneration()
	}
}

// announceReady tells the parent the writer has begun writing and it is safe to
// kill. The newline-terminated token is what the parent blocks on.
func announceReady() {
	fmt.Println("READY")
	_ = os.Stdout.Sync()
}

// recovery is the parent's verdict on a reopened store.
type recovery struct {
	clean      bool
	detail     string
	generation int64
}

// spawnWriterAndKill launches the writer subprocess in the given mode against
// dbPath, waits for its READY line, waits killDelay (derived from the caller's
// iteration index — never wall-clock randomness), SIGKILLs it, and reaps it.
func spawnWriterAndKill(t *testing.T, mode, dbPath string, killDelay time.Duration) {
	t.Helper()

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), envWriterMode+"="+mode, envWriterDB+"="+dbPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start writer: %v", err)
	}

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "READY" {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatalf("writer never became READY (line=%q, err=%v) — subprocess setup failed", line, err)
	}

	time.Sleep(killDelay)
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL writer: %v", err)
	}
	_ = cmd.Wait() // "signal: killed" is expected.
}

// verifyRecovery reopens the operational store through the real Open (which
// re-applies WAL and would surface a corrupt DB) and returns whether it
// recovered to a fully self-consistent committed state: DB integrity ok,
// identity intact, and — the crux — the last-applied generation exactly matches
// the durable telemetry history (generation == count == max(seq) ==
// high-water), the {generation, hash} pair is internally consistent, the clock
// floor tracks the generation, and every telemetry payload is well-formed with
// a contiguous, unique seq. A torn apply-unit breaks one of these equalities.
func verifyRecovery(dbPath string) recovery {
	db, err := sql.Open("sqlite", "file:"+dbPath+
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return recovery{detail: fmt.Sprintf("reopen: %v", err)}
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return recovery{detail: fmt.Sprintf("integrity_check errored: %v", err)}
	}
	if integrity != "ok" {
		return recovery{detail: fmt.Sprintf("integrity_check = %q", integrity)}
	}

	// Identity intact (REL-142): persisted once before the write loop.
	var relayID string
	var keyPEM []byte
	switch err := db.QueryRow(`SELECT relay_id, private_key_pem FROM relay_identity WHERE id = 1`).Scan(&relayID, &keyPEM); {
	case err == sql.ErrNoRows:
		return recovery{detail: "identity missing after recovery"}
	case err != nil:
		return recovery{detail: fmt.Sprintf("read identity: %v", err)}
	}
	if relayID != powerPullRelayID {
		return recovery{detail: fmt.Sprintf("relay_id = %q, want %q", relayID, powerPullRelayID)}
	}
	if _, err := unmarshalPrivateKey(keyPEM); err != nil {
		return recovery{detail: fmt.Sprintf("identity private key did not recover: %v", err)}
	}

	// Last-applied {generation, hash} — the pair the swap is atomic over.
	var generation int64
	var hash string
	switch err := db.QueryRow(`SELECT generation, hash FROM last_applied_generation WHERE id = 1`).Scan(&generation, &hash); {
	case err == sql.ErrNoRows:
		return recovery{detail: "last-applied generation missing after recovery"}
	case err != nil:
		return recovery{detail: fmt.Sprintf("read last-applied: %v", err)}
	}
	if hash != powerPullHash(generation) {
		return recovery{detail: fmt.Sprintf("TORN last-applied: generation %d carries hash %q, want %q", generation, hash, powerPullHash(generation))}
	}

	// Durable telemetry history: contiguous, unique, well-formed.
	rows, err := db.Query(`SELECT seq, payload FROM telemetry_queue ORDER BY seq`)
	if err != nil {
		return recovery{detail: fmt.Sprintf("query telemetry: %v", err)}
	}
	defer func() { _ = rows.Close() }()

	var count, maxSeq int64
	for rows.Next() {
		var seq int64
		var payload []byte
		if err := rows.Scan(&seq, &payload); err != nil {
			return recovery{detail: fmt.Sprintf("scan telemetry: %v", err)}
		}
		count++
		if seq != count {
			return recovery{detail: fmt.Sprintf("telemetry seq gap: row %d has seq %d", count, seq)}
		}
		var body struct {
			Tick int64 `json:"tick"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return recovery{detail: fmt.Sprintf("telemetry seq %d payload malformed: %v", seq, err)}
		}
		if body.Tick != seq {
			return recovery{detail: fmt.Sprintf("telemetry seq %d payload tick %d mismatch", seq, body.Tick)}
		}
		maxSeq = seq
	}
	if err := rows.Err(); err != nil {
		return recovery{detail: fmt.Sprintf("iterate telemetry: %v", err)}
	}

	// The atomic-apply cross-object invariant: last-applied generation is in
	// lockstep with the durable history it was committed alongside.
	if generation != maxSeq || count != maxSeq {
		return recovery{detail: fmt.Sprintf("TORN apply-unit: generation=%d max(seq)=%d count=%d (not in lockstep)", generation, maxSeq, count)}
	}

	// Clock floor tracks the generation (REL-130 monotonic floor).
	var floor int64
	switch err := db.QueryRow(`SELECT floor_ms FROM clock_floor WHERE id = 1`).Scan(&floor); {
	case err == sql.ErrNoRows:
		return recovery{detail: "clock floor missing after recovery"}
	case err != nil:
		return recovery{detail: fmt.Sprintf("read clock floor: %v", err)}
	}
	if floor != powerPullClockBase+generation {
		return recovery{detail: fmt.Sprintf("clock floor = %d, want %d", floor, powerPullClockBase+generation)}
	}

	// Seq high-water resumes strictly above every issued seq (REL-091).
	var highWater int64
	switch err := db.QueryRow(`SELECT seq FROM telemetry_seq_high_water WHERE id = 1`).Scan(&highWater); {
	case err == sql.ErrNoRows:
		return recovery{detail: "seq high-water missing after recovery"}
	case err != nil:
		return recovery{detail: fmt.Sprintf("read high-water: %v", err)}
	}
	if highWater != generation {
		return recovery{detail: fmt.Sprintf("seq high-water = %d, want %d", highWater, generation)}
	}

	return recovery{clean: true, generation: generation}
}

// TestPowerPullTortureRecovery is the §10 appliance-physics gate: across many
// iterations, a disciplined writer is SIGKILLed mid-write at an
// iteration-index-derived delay, and the reopened operational store MUST
// recover to a fully self-consistent committed state every single time — no
// torn last-applied {generation, hash}, no half-applied apply-unit, identity
// and durable telemetry intact (REL-055/056 atomic swap, REL-090/093 durable
// telemetry, REL-130 clock floor). Committed durable state always survives an
// abrupt kill.
func TestPowerPullTortureRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("power-pull torture harness spawns subprocesses; skipped under -short")
	}

	const iterations = 25
	const killSpreadMs = 15

	var minGen, maxGen int64 = 1<<62 - 1, 0
	for i := 0; i < iterations; i++ {
		dbPath := filepath.Join(t.TempDir(), fmt.Sprintf("relay-%d.db", i))
		// Kill delay derived from the iteration index — deterministic and
		// reproducible, sweeping 1..killSpreadMs ms across the writer's commit
		// cadence so kills land at varied points mid-write, never wall-clock
		// randomness that would make a failure un-reproducible.
		killDelay := time.Duration(1+i%killSpreadMs) * time.Millisecond
		spawnWriterAndKill(t, writerModeAtomic, dbPath, killDelay)

		rec := verifyRecovery(dbPath)
		if !rec.clean {
			t.Fatalf("iteration %d (kill @ %v): store did NOT recover cleanly: %s", i, killDelay, rec.detail)
		}
		if rec.generation < minGen {
			minGen = rec.generation
		}
		if rec.generation > maxGen {
			maxGen = rec.generation
		}
	}

	// Every recovery held at least the warmup history (the writer committed
	// before READY), and at least one kill landed after real progress under the
	// write loop — evidence the writer was genuinely killed mid-write, not on an
	// idle or unstarted store.
	if minGen < atomicWarmupTicks {
		t.Fatalf("recovered generation %d < warmup %d: writer did not commit its warmup before a kill", minGen, atomicWarmupTicks)
	}
	if maxGen <= atomicWarmupTicks {
		t.Fatalf("max recovered generation %d never exceeded warmup %d: kills never landed mid-write-loop", maxGen, atomicWarmupTicks)
	}
	t.Logf("recovered cleanly across %d SIGKILLs; committed generation ranged %d..%d", iterations, minGen, maxGen)
}

// TestPowerPullTortureHarnessHasTeeth is the negative control that proves
// TestPowerPullTortureRecovery's clean-recovery claim is meaningful: the SAME
// verifier, run against a writer that drops the store's write discipline (raw
// journal_mode=OFF, non-atomic apply-units), MUST detect a torn last-applied on
// reopen. If it did not, the durability test could pass vacuously. The torn
// writer announces READY with a telemetry entry committed but its matching
// generation-advance still outstanding, so the parent's kill deterministically
// strands the tear.
func TestPowerPullTortureHarnessHasTeeth(t *testing.T) {
	if testing.Short() {
		t.Skip("power-pull torture harness spawns subprocesses; skipped under -short")
	}

	const iterations = 4
	for i := 0; i < iterations; i++ {
		dbPath := filepath.Join(t.TempDir(), fmt.Sprintf("torn-%d.db", i))
		// READY is announced inside the torn window; kill immediately (no delay).
		spawnWriterAndKill(t, writerModeTorn, dbPath, 0)

		rec := verifyRecovery(dbPath)
		if rec.clean {
			t.Fatalf("iteration %d: harness has NO teeth — a torn, undisciplined write recovered as clean; the durability assertion is worthless", i)
		}
		if !strings.Contains(rec.detail, "TORN") {
			t.Fatalf("iteration %d: expected a torn-last-applied detection, got: %s", i, rec.detail)
		}
		t.Logf("iteration %d: harness correctly rejected torn write: %s", i, rec.detail)
	}
}
