package packs_test

// rosterfile_test.go drives the required-pack roster's resolution from disk
// (marketplace/1 MKT-093a) and, above all, the distinction the requirement's
// amendment turns on: an ABSENT or EMPTY roster makes no pack required, while an
// UNRESOLVABLE one — a roster the host cannot read, parse, or validate — must
// NOT be treated as an empty one.
//
// Every unresolvable case below asserts TWO things, and the second is the one
// that matters. The first is that LoadRoster returned an error. The second is
// that the Roster it returned alongside that error REFUSES: a loader that
// returned an error and an empty roster would satisfy the first assertion while
// shipping exactly the defect the amendment closed, because a caller that logs
// the error and carries on — or a future caller that drops it — would be running
// with every floor lifted.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/store"
)

// writeRoster writes body to a fresh 0700 temp directory as a 0600 roster and
// returns its path — the shape a correctly provisioned deployment has, so a test
// that wants to exercise something OTHER than the integrity checks is not
// tripping them by accident.
func writeRoster(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "required-packs.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write roster: %v", err)
	}
	return path
}

// assertRefusesEverything is the load-bearing assertion for every unresolvable
// case: the roster reports EVERY pack as required at a floor nothing satisfies,
// so the store refuses every install and every uninstall while it is wired.
//
// It deliberately probes a pack id no roster in this file names. An unresolved
// roster must not need to have heard of a pack to refuse it.
func assertRefusesEverything(t *testing.T, r packs.Roster, what string) {
	t.Helper()
	if r.Resolved() {
		t.Fatalf("%s: roster reports itself RESOLVED — an unresolvable roster resolved to a usable one", what)
	}
	const probe = "some/pack-nobody-declared"
	floor, required := r.RequiredFloor(probe)
	if !required {
		t.Fatalf("%s: RequiredFloor(%q) says NOT required — the unresolvable roster degraded to an empty one, which MKT-093a forbids", what, probe)
	}
	// No version satisfies it, including one far above any plausible floor.
	for _, v := range []string{"0.0.0", "1.0.0", "99999.99999.99999"} {
		if r.MeetsFloor(v, floor) {
			t.Fatalf("%s: version %s satisfies the unresolvable roster's floor %q — the floor is not fail-closed", what, v, floor)
		}
	}
	// And it declares nothing, so an operator report cannot mistake it for a
	// roster that merely happens to be empty.
	if got := r.Declared(); len(got) != 0 {
		t.Fatalf("%s: unresolved roster declared %v, want nothing", what, got)
	}
}

// TestZeroRosterRefusesEveryPack pins the structural property the whole design
// rests on: the Roster ZERO VALUE is the unresolved, fail-closed one.
//
// This is what makes "a parse error cannot degrade to nothing-required"
// structural rather than merely handled. Every error return in LoadRoster
// returns this value, so there is no error path that can hand a caller a
// permissive roster — not by oversight, not by a later edit that forgets, and
// not by a caller that ignores the error entirely.
func TestZeroRosterRefusesEveryPack(t *testing.T) {
	var r packs.Roster
	assertRefusesEverything(t, r, "the zero Roster")
	assertRefusesEverything(t, packs.Roster{}, "an explicit Roster{}")
}

// TestNewRosterErrorReturnsTheUnresolvedRoster: the same property at the other
// constructor. A malformed declared roster must not hand back a usable empty one.
func TestNewRosterErrorReturnsTheUnresolvedRoster(t *testing.T) {
	r, err := packs.NewRoster(map[string]string{"not-a-pack-id": "1.0.0"})
	if err == nil {
		t.Fatal("NewRoster accepted an unqualified pack id")
	}
	assertRefusesEverything(t, r, "NewRoster's error return")
}

// TestNewRosterEmptyIsResolved is the other side of the distinction: declaring
// nothing, successfully, is a RESOLVED roster that requires nothing (MKT-093a's
// default). Without this the fail-closed zero value would be indistinguishable
// from "the deployment declared nothing", and every unprovisioned host would
// refuse to remove packs nobody called essential.
func TestNewRosterEmptyIsResolved(t *testing.T) {
	r, err := packs.NewRoster(nil)
	if err != nil {
		t.Fatalf("NewRoster(nil): %v", err)
	}
	if !r.Resolved() {
		t.Fatal("NewRoster(nil) is not resolved — an explicitly empty roster is a successful resolution, not a failure")
	}
	if _, required := r.RequiredFloor("acme/menu-board"); required {
		t.Fatal("an empty roster makes a pack required — MKT-093a's default is that it makes NONE required")
	}
}

// ---- absent and empty ------------------------------------------------------

// TestLoadRosterAbsentFileMakesNoPackRequired is MKT-093a's default, and the
// state every dev checkout, CI run and unprovisioned box is in. It MUST NOT fail
// closed: an absent roster withholds a restriction, and refusing there would
// decline to uninstall packs no deployment ever declared essential.
func TestLoadRosterAbsentFileMakesNoPackRequired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-roster.json")
	r, err := packs.LoadRoster(path)
	if err != nil {
		t.Fatalf("LoadRoster on an absent file: %v — absent is the contract's default, not an error", err)
	}
	if !r.Resolved() {
		t.Fatal("an absent roster resolved to the UNRESOLVED value — absent and unresolvable are different cases (MKT-093a)")
	}
	if _, required := r.RequiredFloor("waiveo/system"); required {
		t.Fatal("an absent roster made a pack required")
	}
}

// TestLoadRosterExplicitlyEmptyMakesNoPackRequired: a document that says, in so
// many words, that nothing is required. Same outcome as absent, reached by a
// declaration rather than by the file not being there.
func TestLoadRosterExplicitlyEmptyMakesNoPackRequired(t *testing.T) {
	path := writeRoster(t, `{"format":"required-packs/1","required":[]}`)
	r, err := packs.LoadRoster(path)
	if err != nil {
		t.Fatalf("LoadRoster on an explicitly empty roster: %v", err)
	}
	if !r.Resolved() {
		t.Fatal("an explicitly empty roster is unresolved")
	}
	if _, required := r.RequiredFloor("waiveo/system"); required {
		t.Fatal("an explicitly empty roster made a pack required")
	}
}

// ---- the happy path --------------------------------------------------------

// TestLoadRosterReadsADeclaredRoster is the done-bar's first half: a deployment
// can declare a roster and this host resolves it. Without it, nothing an
// operator can author reaches the floor at all.
func TestLoadRosterReadsADeclaredRoster(t *testing.T) {
	path := writeRoster(t, `{
	  "format": "required-packs/1",
	  "required": [
	    { "pack_id": "waiveo/system", "floor_version": "1.4.0" },
	    { "pack_id": "acme/menu-board", "floor_version": "2.0.0" }
	  ]
	}`)
	r, err := packs.LoadRoster(path)
	if err != nil {
		t.Fatalf("LoadRoster: %v", err)
	}
	if !r.Resolved() {
		t.Fatal("a valid roster resolved to the unresolved value")
	}
	floor, required := r.RequiredFloor("waiveo/system")
	if !required || floor != "1.4.0" {
		t.Fatalf("waiveo/system = (%q, %v), want (\"1.4.0\", true)", floor, required)
	}
	// The floor is enforced under MKT-050a's component-wise numeric order, not a
	// string one: 1.10.0 is ABOVE 1.4.0 and 1.3.9 is below it.
	if !r.MeetsFloor("1.10.0", floor) {
		t.Error("1.10.0 does not meet floor 1.4.0 — the comparison is lexicographic, which inverts MKT-050a's order")
	}
	if r.MeetsFloor("1.3.9", floor) {
		t.Error("1.3.9 meets floor 1.4.0")
	}
	if _, required := r.RequiredFloor("someone/else"); required {
		t.Error("a pack the roster does not name is required — required status is roster-only (MKT-093a)")
	}
	want := []string{"acme/menu-board@2.0.0", "waiveo/system@1.4.0"}
	if got := r.Declared(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Declared() = %v, want %v (sorted, for the boot report)", got, want)
	}
}

// ---- UNRESOLVABLE: the crux ------------------------------------------------

// TestLoadRosterUnresolvableNeverDegradesToEmpty is the requirement's amendment,
// case by case. Each input is something a host cannot read, parse, or validate;
// each must error AND return a roster that refuses everything.
//
// The two cases worth naming individually, because they are the ones a plain
// "is it valid JSON" loader gets wrong:
//
//   - "a different well-formed document": the trust-anchors file, verbatim. It
//     is valid JSON and unmarshals into the roster struct without complaint,
//     yielding a document with zero entries — a loader that only asked "is this
//     valid JSON" would resolve the WRONG FILE, successfully, to a roster that
//     requires nothing. Two independent guards catch it (the format discriminant
//     and the unknown `anchors` field), which is the point of having both.
//   - "wrong format discriminant" and "no format discriminant" are what the
//     discriminant alone catches: a document with the right SHAPE and the wrong
//     identity, where nothing else is out of place.
//   - "the entries field misspelled": the same failure reached by a typo rather
//     than by the wrong file. `requird` is not `required`, so the document has
//     the right format and no entries, and only the unknown-field check sees it.
func TestLoadRosterUnresolvableNeverDegradesToEmpty(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"truncated JSON", `{"format":"required-packs/1","required":[`},
		{"not JSON at all", "required-packs\nwaiveo/system 1.0.0\n"},
		{"empty file", ""},
		{"a JSON array, not a document", `[{"pack_id":"waiveo/system","floor_version":"1.0.0"}]`},
		{"no format discriminant", `{"required":[{"pack_id":"waiveo/system","floor_version":"1.0.0"}]}`},
		{"wrong format discriminant", `{"format":"pack-trust-anchors/1","required":[]}`},
		{"a different well-formed document", `{"format":"pack-trust-anchors/1","anchors":[{"namespace":"waiveo","key_id":"ed25519:00","public_key":"AA=="}]}`},
		{"the entries field misspelled", `{"format":"required-packs/1","requird":[{"pack_id":"waiveo/system","floor_version":"1.0.0"}]}`},
		{"an unknown entry field", `{"format":"required-packs/1","required":[{"pack_id":"waiveo/system","floor":"1.0.0"}]}`},
		{"a duplicated pack id", `{"format":"required-packs/1","required":[{"pack_id":"waiveo/system","floor_version":"1.0.0"},{"pack_id":"waiveo/system","floor_version":"3.0.0"}]}`},
		{"an unqualified pack id", `{"format":"required-packs/1","required":[{"pack_id":"system","floor_version":"1.0.0"}]}`},
		{"a pack id outside MAN-001's grammar", `{"format":"required-packs/1","required":[{"pack_id":"Waiveo/System","floor_version":"1.0.0"}]}`},
		{"a floor outside MAN-002's grammar", `{"format":"required-packs/1","required":[{"pack_id":"waiveo/system","floor_version":"1.0"}]}`},
		{"a missing floor", `{"format":"required-packs/1","required":[{"pack_id":"waiveo/system"}]}`},
		{"a second document after the first", `{"format":"required-packs/1","required":[{"pack_id":"waiveo/system","floor_version":"1.0.0"}]}{"format":"required-packs/1","required":[]}`},
		{"trailing garbage", `{"format":"required-packs/1","required":[]} and then some`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRoster(t, tc.body)
			r, err := packs.LoadRoster(path)
			if err == nil {
				t.Fatalf("LoadRoster accepted %s — %q", tc.name, tc.body)
			}
			assertRefusesEverything(t, r, tc.name)
		})
	}
}

// TestLoadRosterRefusesANonRegularFile: a directory, a device, or — the reason
// this is a check rather than a consequence of the read failing — a FIFO, which
// would block the boot forever on a read that never returns.
func TestLoadRosterRefusesANonRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roster-as-a-directory")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	r, err := packs.LoadRoster(path)
	if err == nil {
		t.Fatal("LoadRoster accepted a directory as the roster")
	}
	// The refusal must be the CATEGORICAL one — "not a regular file" — rather than
	// whatever the read happened to fail with. A directory fails the read anyway;
	// a FIFO does not fail it, it blocks in it (see the test below), so the guard
	// has to be the file type and not the read's luck.
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v, want the categorical non-regular-file refusal", err)
	}
	assertRefusesEverything(t, r, "a directory at the roster path")
}

// TestLoadRosterRefusesAFifoWithoutBlocking is why the file-type check is a
// check and not a consequence.
//
// A FIFO at the roster path is readable — os.Stat succeeds, the mode bits can be
// 0600, the containing directory can be pristine — and reading it blocks until
// somebody writes. Without the file-type guard the boot would hang there
// forever: not a refusal, not a start, and no log line past the one before it.
// A host that never finishes booting is worse than one that refuses, because
// nothing reports why.
func TestLoadRosterRefusesAFifoWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "required-packs.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo is unavailable here (%v); the guard is still exercised by the directory case", err)
	}

	type result struct {
		r   packs.Roster
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := packs.LoadRoster(path)
		done <- result{r, err}
	}()
	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("LoadRoster accepted a FIFO as the roster")
		}
		assertRefusesEverything(t, got.r, "a FIFO at the roster path")
	case <-time.After(10 * time.Second):
		// The goroutine is parked in the open/read and will not return; it dies
		// with the test binary.
		t.Fatal("LoadRoster BLOCKED on a FIFO at the roster path — the boot would hang there forever rather than refuse")
	}
}

// TestLoadRosterRefusesAnOversizeDocument: the roster is read whole at boot, so
// the read is bounded. A roster is a list of ids and versions; nothing real
// approaches the cap.
func TestLoadRosterRefusesAnOversizeDocument(t *testing.T) {
	path := writeRoster(t, `{"format":"required-packs/1","required":[]}`+strings.Repeat(" ", 1<<20))
	r, err := packs.LoadRoster(path)
	if err == nil {
		t.Fatal("LoadRoster accepted an oversize roster")
	}
	assertRefusesEverything(t, r, "an oversize roster")
}

// ---- integrity -------------------------------------------------------------

// TestLoadRosterRefusesAWritableRoster is the anchors' own posture applied here,
// and the argument for it is in rosterfile.go's doc: writing a roster is not the
// null grant it looks like. Adding an entry makes a pack un-uninstallable (an
// attacker pins their own pack in place); removing one lifts the floor that
// forbids a downgrade to a known-vulnerable version. Neither direction is
// harmless, so a roster whose integrity the host is not enforcing is refused
// rather than read.
//
// It is refused as UNRESOLVABLE, not as empty — which is exactly why the
// distinction has to be structural: the fail-closed reading of "I cannot trust
// this file" must not be the permissive one.
func TestLoadRosterRefusesAWritableRoster(t *testing.T) {
	for _, mode := range []os.FileMode{0o666, 0o646, 0o620, 0o602} {
		dir := t.TempDir()
		path := filepath.Join(dir, "required-packs.json")
		if err := os.WriteFile(path, []byte(`{"format":"required-packs/1","required":[]}`), 0o600); err != nil {
			t.Fatalf("write roster: %v", err)
		}
		// Chmod after the write: os.WriteFile's mode is masked by the umask, so
		// the group/world bits would not survive being passed to it.
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("chmod %04o: %v", mode, err)
		}
		r, err := packs.LoadRoster(path)
		if err == nil {
			t.Fatalf("LoadRoster read a roster at mode %04o", mode)
		}
		assertRefusesEverything(t, r, "a group/world-writable roster")
	}
}

// TestLoadRosterRefusesAWritableRosterDirectory closes the bypass the file-mode
// check alone leaves open: an attacker who can write the CONTAINING DIRECTORY
// renames their own 0600 document over the roster path, and the file's mode
// check passes on a file they authored. Checking the directory is what makes the
// file check mean anything.
func TestLoadRosterRefusesAWritableRosterDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "required-packs.json")
	if err := os.WriteFile(path, []byte(`{"format":"required-packs/1","required":[{"pack_id":"waiveo/system","floor_version":"1.0.0"}]}`), 0o600); err != nil {
		t.Fatalf("write roster: %v", err)
	}
	// Sanity: at a sane directory mode this exact roster loads. Without this the
	// test would pass against a loader that refuses everything.
	if _, err := packs.LoadRoster(path); err != nil {
		t.Fatalf("the roster does not load at a 0700 directory: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	r, err := packs.LoadRoster(path)
	if err == nil {
		t.Fatal("LoadRoster read a roster out of a world-writable directory — anyone who can write that directory can replace the roster with one that lifts every floor")
	}
	assertRefusesEverything(t, r, "a roster in a world-writable directory")
}

// TestLoadRosterAcceptsAStickyRosterDirectory: the sticky bit forbids non-owners
// from renaming or unlinking files they do not own, which is the attack the
// directory check exists to stop — so refusing there would be a false refusal.
func TestLoadRosterAcceptsAStickyRosterDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sticky")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "required-packs.json")
	if err := os.WriteFile(path, []byte(`{"format":"required-packs/1","required":[{"pack_id":"waiveo/system","floor_version":"1.0.0"}]}`), 0o600); err != nil {
		t.Fatalf("write roster: %v", err)
	}
	// os.ModeSticky, not the raw 0o1000 bit: Go's FileMode carries sticky in its
	// own high bits, so 0o1777 would set an ordinary permission bit and not the
	// sticky one at all.
	if err := os.Chmod(dir, 0o777|os.ModeSticky); err != nil {
		t.Fatalf("chmod sticky: %v", err)
	}
	r, err := packs.LoadRoster(path)
	if err != nil {
		t.Fatalf("LoadRoster refused a roster in a STICKY 1777 directory: %v", err)
	}
	if floor, required := r.RequiredFloor("waiveo/system"); !required || floor != "1.0.0" {
		t.Fatalf("waiveo/system = (%q, %v), want (\"1.0.0\", true)", floor, required)
	}
}

// ---- what an unresolved roster does to a REAL store ------------------------

// TestUnresolvedRosterRefusesEveryPackMutationInTheStore takes the fail-closed
// value all the way through the transactions MKT-093b puts the refusal inside,
// rather than stopping at the accessor.
//
// It is the behavioural meaning of "refuse every pack removal, if it is already
// running": a deployment that wires an unresolved roster instead of refusing to
// start gets a host that will not remove a pack and will not install one, rather
// than one running with every floor lifted. Both directions are asserted,
// because refusing only removals would still let a below-floor version land.
func TestUnresolvedRosterRefusesEveryPackMutationInTheStore(t *testing.T) {
	st, in, signer, reg := updateFixture(t)
	ctx := context.Background()

	publishVersion(t, reg, signer, "1.0.0")
	if _, err := in.InstallRef(ctx, packs.Ref{PackID: "acme/menu-board", TrustChannel: "community"}); err != nil {
		t.Fatalf("InstallRef 1.0.0: %v", err)
	}
	p, found, err := st.GetPack(ctx, "acme/menu-board")
	if err != nil || !found {
		t.Fatalf("GetPack: found=%v err=%v", found, err)
	}

	// The roster is now UNRESOLVABLE. Nothing about the pack changed; what
	// changed is that this host no longer knows what the deployment requires.
	st.SetRequiredPacks(packs.Roster{})

	// (i) removal, refused — the pack is not on any roster the host can read, and
	// that is exactly why it must not be removed.
	err = st.UninstallPack(ctx, "acme/menu-board", p.Revision)
	var ferr *store.RequiredPackFloorError
	if !errors.As(err, &ferr) {
		t.Fatalf("UninstallPack under an unresolved roster: err = %v, want *RequiredPackFloorError", err)
	}
	if _, stillThere, _ := st.GetPack(ctx, "acme/menu-board"); !stillThere {
		t.Fatal("the refused uninstall removed the pack anyway")
	}

	// (ii) install, refused too. A host that cannot name the floors cannot
	// establish that an install leaves a required pack at or above one.
	publishVersion(t, reg, signer, "2.0.0")
	_, err = in.Update(ctx, "acme/menu-board")
	if code := refusalCode(t, err); code != "REQUIRED_PACK_FLOOR" {
		t.Fatalf("Update under an unresolved roster: code = %s, want REQUIRED_PACK_FLOOR", code)
	}

	// And the sanity half: with a RESOLVED empty roster — the absent-file case —
	// the very same uninstall succeeds. Without this the test above would pass
	// against a store that refused every uninstall unconditionally.
	empty, err := packs.NewRoster(nil)
	if err != nil {
		t.Fatalf("NewRoster(nil): %v", err)
	}
	st.SetRequiredPacks(empty)
	p, _, err = st.GetPack(ctx, "acme/menu-board")
	if err != nil {
		t.Fatalf("GetPack: %v", err)
	}
	if err := st.UninstallPack(ctx, "acme/menu-board", p.Revision); err != nil {
		t.Fatalf("UninstallPack under a RESOLVED empty roster: %v — an empty roster requires nothing (MKT-093a)", err)
	}
}

// TestUnresolvedRosterRefusesAParseableFloor pins the SECOND of the two reasons
// an unresolved roster refuses, independently of the first.
//
// The type documents two: RequiredFloor hands back a sentinel that is not a
// MAN-002 version, AND MeetsFloor refuses outright when the roster is
// unresolved. Every existing assertion took its floor FROM RequiredFloor, so it
// passed if either reason held and could not tell them apart — both guards were
// individually removable with the whole suite green. That matters because the
// code's own comment names the hazard: somebody later "fixes" the sentinel into
// a valid-looking version, at which point the free meetsFloor used by the revert
// narrowing (which has no resolved guard) would admit every candidate.
func TestUnresolvedRosterRefusesAParseableFloor(t *testing.T) {
	var zero packs.Roster
	if zero.MeetsFloor("9.9.9", "1.0.0") {
		t.Error("an unresolved roster admitted a version against a parseable floor — MeetsFloor's own resolved guard is not doing anything")
	}
	if packs.ValidVersion(packs.UnresolvedFloor) {
		t.Errorf("UnresolvedFloor %q parses as a MAN-002 version — the sentinel's second refusal reason is gone", packs.UnresolvedFloor)
	}
}
