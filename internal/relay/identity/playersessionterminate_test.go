package identity

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// playersessionterminate_test.go covers the durable session drop a revocation
// performs: a channel token minted before a screen was revoked must not come
// back to life on the next boot, which is exactly what an in-memory-only drop
// would allow.

// TestTerminatePlayerSessionsForScreensTombstonesRatherThanDeletes pins the
// shape as much as the effect. The row SURVIVES its own termination on purpose:
// player/1 PLY-072 requires a token naming a currently-revoked screen be
// refused CHANNEL_TOKEN_REVOKED specifically, and a relay that had deleted the
// row could only say CHANNEL_TOKEN_INVALID — it would no longer know which
// screen the token named.
func TestTerminatePlayerSessionsForScreensTombstonesRatherThanDeletes(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const screen = "01J8Z3K4N5P6Q7R8S9T0V1W2X6"
	hash := HashToken("ct-live")
	if err := store.SetPlayerSession(hash, screen, 1752545000000); err != nil {
		t.Fatalf("SetPlayerSession: %v", err)
	}

	rec, ok, err := store.PlayerSession(hash)
	if err != nil || !ok {
		t.Fatalf("PlayerSession before termination: ok=%v err=%v", ok, err)
	}
	if rec.Terminated() {
		t.Fatal("a freshly minted session reads as terminated")
	}

	n, err := store.TerminatePlayerSessionsForScreens([]string{screen}, 1752500000000)
	if err != nil {
		t.Fatalf("TerminatePlayerSessionsForScreens: %v", err)
	}
	if n != 1 {
		t.Errorf("terminated %d session(s), want 1", n)
	}

	rec, ok, err = store.PlayerSession(hash)
	if err != nil {
		t.Fatalf("PlayerSession after termination: %v", err)
	}
	if !ok {
		t.Fatal("the terminated session's row is gone — it must remain RESOLVABLE so the relay can still answer CHANNEL_TOKEN_REVOKED for it (PLY-072)")
	}
	if !rec.Terminated() {
		t.Error("the session reads as live after being terminated")
	}
	if rec.ScreenID != screen {
		t.Errorf("terminated session's screen_id = %q, want %q — the binding is what the revoked answer is built from", rec.ScreenID, screen)
	}
}

// TestTerminatePlayerSessionsForScreensOnlyTouchesTheNamedScreens is the blast-radius
// property: a revocation names screens, and dropping a sibling screen's live
// credential would take a working screen dark for a decision that was never
// about it.
func TestTerminatePlayerSessionsForScreensOnlyTouchesTheNamedScreens(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.SetPlayerSession(HashToken("ct-a"), "screen-a", 1); err != nil {
		t.Fatalf("SetPlayerSession(a): %v", err)
	}
	if err := store.SetPlayerSession(HashToken("ct-b"), "screen-b", 1); err != nil {
		t.Fatalf("SetPlayerSession(b): %v", err)
	}

	if _, err := store.TerminatePlayerSessionsForScreens([]string{"screen-a"}, 1752500000000); err != nil {
		t.Fatalf("TerminatePlayerSessionsForScreens: %v", err)
	}

	b, ok, err := store.PlayerSession(HashToken("ct-b"))
	if err != nil || !ok {
		t.Fatalf("PlayerSession(b): ok=%v err=%v", ok, err)
	}
	if b.Terminated() {
		t.Error("revoking screen-a terminated screen-b's session")
	}
}

// TestTerminatePlayerSessionsForScreensDropsEveryScreenItNames is the other half
// of the blast radius, and the one the batched statement can get wrong in a way
// no single-screen call ever could: every id in the set has to reach the `IN`
// list. A statement built from only the first — or only the last — id would
// leave the rest of a multi-screen revocation durably LIVE while the caller
// reads a nil error and moves on, so the failure is silent by construction.
//
// The reported count is asserted too. It is what the caller would log, and a
// count that claimed rows the statement never touched would make the silent
// failure look like success in the one record an operator has of it.
func TestTerminatePlayerSessionsForScreensDropsEveryScreenItNames(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	named := []string{"screen-a", "screen-b", "screen-c"}
	for _, id := range append(slices.Clone(named), "screen-untouched") {
		if err := store.SetPlayerSession(HashToken("ct-"+id), id, 1); err != nil {
			t.Fatalf("SetPlayerSession(%s): %v", id, err)
		}
	}

	n, err := store.TerminatePlayerSessionsForScreens(named, 1752500000000)
	if err != nil {
		t.Fatalf("TerminatePlayerSessionsForScreens: %v", err)
	}
	if n != int64(len(named)) {
		t.Errorf("reported %d session(s) dropped, want %d — one per named screen", n, len(named))
	}

	for _, id := range named {
		rec, ok, err := store.PlayerSession(HashToken("ct-" + id))
		if err != nil || !ok {
			t.Fatalf("PlayerSession(%s): ok=%v err=%v", id, ok, err)
		}
		if !rec.Terminated() {
			t.Errorf("%s was named in the set but its session is still live — every named screen has to reach the statement, not just one of them", id)
		}
	}

	rec, ok, err := store.PlayerSession(HashToken("ct-screen-untouched"))
	if err != nil || !ok {
		t.Fatalf("PlayerSession(untouched): ok=%v err=%v", ok, err)
	}
	if rec.Terminated() {
		t.Error("a screen the set never named had its session dropped")
	}
}

// TestTerminatePlayerSessionsForScreensSpansItsChunkBound pins that a set larger
// than one `IN` list is still applied in FULL. The chunking exists so an
// oversized set cannot fail the whole drop on SQLite's host-parameter limit, but
// a chunk loop that stops early — or that reuses the first chunk — would drop
// exactly the screens a small test never notices.
func TestTerminatePlayerSessionsForScreensSpansItsChunkBound(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// One past a whole chunk, so the last screen is alone in the second one.
	ids := make([]string, terminateChunk+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("screen-%04d", i)
		if err := store.SetPlayerSession(HashToken("ct-"+ids[i]), ids[i], 1); err != nil {
			t.Fatalf("SetPlayerSession(%s): %v", ids[i], err)
		}
	}

	n, err := store.TerminatePlayerSessionsForScreens(ids, 1752500000000)
	if err != nil {
		t.Fatalf("TerminatePlayerSessionsForScreens over %d screens: %v", len(ids), err)
	}
	if n != int64(len(ids)) {
		t.Errorf("reported %d session(s) dropped, want %d", n, len(ids))
	}

	last := ids[len(ids)-1]
	rec, ok, err := store.PlayerSession(HashToken("ct-" + last))
	if err != nil || !ok {
		t.Fatalf("PlayerSession(%s): ok=%v err=%v", last, ok, err)
	}
	if !rec.Terminated() {
		t.Errorf("%s rode the second chunk and was not dropped", last)
	}
}

// TestTerminatePlayerSessionsForScreensKeepsTheFirstStamp asserts a repeated
// termination does not re-stamp an already-dropped session. The stamp records
// when the relay ACTED; a revocation set restated on every later snapshot
// (REL-066) would otherwise walk it forward forever, and the count it reports
// would claim work it did not do.
func TestTerminatePlayerSessionsForScreensKeepsTheFirstStamp(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hash := HashToken("ct-repeat")
	if err := store.SetPlayerSession(hash, "screen-a", 1); err != nil {
		t.Fatalf("SetPlayerSession: %v", err)
	}
	if _, err := store.TerminatePlayerSessionsForScreens([]string{"screen-a"}, 1000); err != nil {
		t.Fatalf("TerminatePlayerSessionsForScreens (first): %v", err)
	}

	n, err := store.TerminatePlayerSessionsForScreens([]string{"screen-a"}, 2000)
	if err != nil {
		t.Fatalf("TerminatePlayerSessionsForScreens (second): %v", err)
	}
	if n != 0 {
		t.Errorf("second termination reported %d row(s) dropped, want 0 — the session was already dropped", n)
	}

	rec, ok, err := store.PlayerSession(hash)
	if err != nil || !ok {
		t.Fatalf("PlayerSession: ok=%v err=%v", ok, err)
	}
	if rec.TerminatedAt != 1000 {
		t.Errorf("terminated_at = %d, want 1000 (the FIRST termination)", rec.TerminatedAt)
	}
}

// TestTerminatePlayerSessionsRefusesTheLiveSentinel guards the one value that
// cannot mean "terminated": 0 is what a live row carries, so writing it would
// produce tombstones that read back as live sessions.
func TestTerminatePlayerSessionsRefusesTheLiveSentinel(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.TerminatePlayerSessionsForScreens([]string{"screen-a"}, 0); err == nil {
		t.Error("TerminatePlayerSessionsForScreens(_, 0) returned no error — 0 is the not-terminated sentinel, so it must be refused rather than written")
	}
}

// TestOpenMigratesPreTerminatedPlayerSessionTable asserts a
// player_channel_tokens created by an EARLIER build keeps working when it is
// upgraded — the same `CREATE TABLE IF NOT EXISTS` no-op the last-applied and
// telemetry migrations exist for — and that a session minted before the column
// existed reads back LIVE rather than as an accidental tombstone.
func TestOpenMigratesPreTerminatedPlayerSessionTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE player_channel_tokens (
		token_hash  TEXT PRIMARY KEY,
		screen_id   TEXT NOT NULL,
		expires_at  INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create pre-terminated player_channel_tokens: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO player_channel_tokens (token_hash, screen_id, expires_at) VALUES (?, 'screen-a', 1752545000000)`,
		HashToken("ct-old"),
	); err != nil {
		t.Fatalf("seed pre-terminated session: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw handle: %v", err)
	}

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open over a pre-terminated store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rec, ok, err := store.PlayerSession(HashToken("ct-old"))
	if err != nil || !ok {
		t.Fatalf("PlayerSession over a migrated store: ok=%v err=%v", ok, err)
	}
	if rec.Terminated() {
		t.Error("a session minted before terminated_at existed reads as terminated after the migration — the upgrade would take every already-paired screen dark")
	}

	// And the migrated table can be terminated against.
	if _, err := store.TerminatePlayerSessionsForScreens([]string{"screen-a"}, 1752500000000); err != nil {
		t.Fatalf("TerminatePlayerSessionsForScreens over a migrated store: %v", err)
	}
	rec, _, err = store.PlayerSession(HashToken("ct-old"))
	if err != nil {
		t.Fatalf("PlayerSession after post-migration termination: %v", err)
	}
	if !rec.Terminated() {
		t.Error("post-migration termination did not take")
	}
}
