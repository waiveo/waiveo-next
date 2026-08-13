package packrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/packhost"
)

// packrun is the join between "a pack is installed" and "a pack is running".
// Everything either side of it was already built and tested; nothing connected
// them, so an extension could be installed, served, invoked and updated and
// never RUN.
//
// The assertions that matter here are about what reaches the DISK and what
// reaches exec — a start that writes the wrong bytes, or execs the wrong path,
// looks identical to a working one until an operator wonders why their extension
// is stale.

type fakeStore struct {
	packs []store.Pack
	files map[string][]byte // packID + "\x00" + name
}

func (f *fakeStore) ListPacks(context.Context) ([]store.Pack, error) { return f.packs, nil }

func (f *fakeStore) GetPackFile(_ context.Context, packID, kind, name string) ([]byte, bool, error) {
	if kind != store.PackFileCode {
		return nil, false, nil
	}
	b, ok := f.files[packID+"\x00"+name]
	return b, ok, nil
}

type fakeStarter struct{ specs []packhost.Spec }

func (f *fakeStarter) Start(_ context.Context, spec packhost.Spec) (packhost.Running, error) {
	f.specs = append(f.specs, spec)
	return packhost.Running{ID: spec.ID, Version: spec.Version, PID: 4242}, nil
}

func pack(id, version string, enabled bool, runtime any) store.Pack {
	doc := map[string]any{"id": id, "version": version}
	if runtime != nil {
		doc["runtime"] = runtime
	}
	body, _ := json.Marshal(doc)
	return store.Pack{ID: id, Version: version, Enabled: enabled, Manifest: body}
}

func host(t *testing.T, st *fakeStore, sp *fakeStarter) *Host {
	t.Helper()
	return New(st, sp, t.TempDir(), "01J8Z0B0000000000000000000")
}

// --- the headline ------------------------------------------------------------

func TestAnEnabledCodeCarryingPackIsMaterialisedAndStarted(t *testing.T) {
	binary := []byte{0x7f, 'E', 'L', 'F', 0x00, 0xff}
	st := &fakeStore{
		packs: []store.Pack{pack("acme/menu", "1.2.0", true, map[string]any{"entry": "bin/pack"})},
		files: map[string][]byte{"acme/menu\x00bin/pack": binary},
	}
	sp := &fakeStarter{}
	h := host(t, st, sp)

	results, err := h.StartAll(context.Background())
	if err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v, want one success", results)
	}
	if len(sp.specs) != 1 {
		t.Fatalf("the supervisor was asked to start %d pack(s), want 1", len(sp.specs))
	}

	// It execs the file it just wrote, not a bare relative path that would
	// resolve against the feeder's working directory.
	argv := sp.specs[0].Argv
	if len(argv) != 1 || !filepath.IsAbs(argv[0]) {
		t.Fatalf("argv = %v, want one absolute path", argv)
	}
	if !strings.HasSuffix(argv[0], filepath.Join("acme__menu", "1.2.0", "bin", "pack")) {
		t.Errorf("argv[0] = %q, want it under the pack's own version directory", argv[0])
	}

	// The BYTES are the ones from the store, unchanged — this is the whole
	// point of the code-carrying tier, and a truncated or re-encoded binary
	// would exec just as happily up to the moment it did not.
	got, err := os.ReadFile(argv[0])
	if err != nil {
		t.Fatalf("read materialised entry: %v", err)
	}
	if string(got) != string(binary) {
		t.Fatalf("materialised bytes = %#v, want %#v", got, binary)
	}

	// Executable, and only by its owner: this host is about to run it, and a
	// group- or world-writable executable is a way to make it run something else.
	info, err := os.Stat(argv[0])
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o500 {
		t.Errorf("mode = %#o, want 0500 (r-x, owner only)", perm)
	}
	if sp.specs[0].Version != "1.2.0" || sp.specs[0].ID != "acme/menu" {
		t.Errorf("spec identity = %s@%s", sp.specs[0].ID, sp.specs[0].Version)
	}
}

// Per-VERSION directories, because a hot swap runs the replacement BEFORE
// retiring the incumbent — one directory per pack would have the new bytes
// overwrite the file the live process was started from.
func TestTwoVersionsOfOnePackDoNotShareADirectory(t *testing.T) {
	st := &fakeStore{files: map[string][]byte{
		"acme/menu\x00bin/pack": []byte("v1"),
	}}
	sp := &fakeStarter{}
	h := host(t, st, sp)

	one, err := h.Materialize(context.Background(), pack("acme/menu", "1.0.0", true, nil), runtimeBlock{Entry: "bin/pack"})
	if err != nil {
		t.Fatalf("materialize v1: %v", err)
	}
	st.files["acme/menu\x00bin/pack"] = []byte("v2")
	two, err := h.Materialize(context.Background(), pack("acme/menu", "2.0.0", true, nil), runtimeBlock{Entry: "bin/pack"})
	if err != nil {
		t.Fatalf("materialize v2: %v", err)
	}
	if one == two {
		t.Fatalf("both versions materialised to %s — a swap would overwrite the running binary", one)
	}
	first, _ := os.ReadFile(one)
	if string(first) != "v1" {
		t.Errorf("the older version's file now reads %q — it was overwritten", first)
	}
}

// A rewrite has to work: the file from the previous boot is 0500, and
// os.WriteFile does not change an existing file's mode, so writing over it
// without removing it first fails EACCES. Boot twice in one test.
func TestABootOverAPreviousBootRewritesTheEntry(t *testing.T) {
	st := &fakeStore{
		packs: []store.Pack{pack("acme/menu", "1.0.0", true, map[string]any{"entry": "bin/pack"})},
		files: map[string][]byte{"acme/menu\x00bin/pack": []byte("first")},
	}
	sp := &fakeStarter{}
	h := host(t, st, sp)

	if _, err := h.StartAll(context.Background()); err != nil {
		t.Fatalf("first boot: %v", err)
	}
	st.files["acme/menu\x00bin/pack"] = []byte("second")
	results, err := h.StartAll(context.Background())
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("second boot failed: %v", results[0].Err)
	}
	got, _ := os.ReadFile(sp.specs[1].Argv[0])
	if string(got) != "second" {
		t.Fatalf("after a rebooot the entry reads %q, want the store's current bytes", got)
	}
}

// --- what must NOT run -------------------------------------------------------

// The entry path is UNTRUSTED manifest data. The install pipeline checks that it
// names a file in the artifact, not that it is a sane relative path — so this is
// the only thing standing between a publisher's string and an executable written
// wherever it points, as the feeder's user, on every boot.
func TestAnUnsafeEntryPathIsRefusedAndNothingIsWritten(t *testing.T) {
	for _, entry := range []string{
		"../../../tmp/evil",
		"/etc/cron.d/evil",
		"bin/../../../../evil",
		"",
		`bin\pack`,
	} {
		t.Run(entry, func(t *testing.T) {
			st := &fakeStore{
				packs: []store.Pack{pack("acme/menu", "1.0.0", true, map[string]any{"entry": entry})},
				files: map[string][]byte{"acme/menu\x00" + entry: []byte("payload")},
			}
			sp := &fakeStarter{}
			h := host(t, st, sp)

			results, err := h.StartAll(context.Background())
			if err != nil {
				t.Fatalf("StartAll: %v", err)
			}
			if len(results) != 1 || results[0].Err == nil {
				t.Fatalf("entry %q was not refused: %+v", entry, results)
			}
			if len(sp.specs) != 0 {
				t.Fatalf("a refused pack was still started: %+v", sp.specs)
			}
		})
	}
}

// Same guard on `exec[0]`, which is a second untrusted path in the same document
// and reaches exec directly.
func TestAnUnsafeExecPathIsRefused(t *testing.T) {
	st := &fakeStore{
		packs: []store.Pack{pack("acme/menu", "1.0.0", true, map[string]any{
			"entry": "bin/pack", "exec": []string{"../../../bin/sh", "-c", "true"},
		})},
		files: map[string][]byte{"acme/menu\x00bin/pack": []byte("payload")},
	}
	sp := &fakeStarter{}
	h := host(t, st, sp)

	results, _ := h.StartAll(context.Background())
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("an escaping exec[0] was not refused: %+v", results)
	}
	if len(sp.specs) != 0 {
		t.Fatalf("it was started anyway: %+v", sp.specs)
	}
}

// A declared `exec` is honoured — the default is only a default.
func TestADeclaredExecIsResolvedAgainstThePacksOwnDirectory(t *testing.T) {
	st := &fakeStore{
		packs: []store.Pack{pack("acme/menu", "1.0.0", true, map[string]any{
			"entry": "bin/pack", "exec": []string{"bin/pack", "--serve"},
		})},
		files: map[string][]byte{"acme/menu\x00bin/pack": []byte("payload")},
	}
	sp := &fakeStarter{}
	h := host(t, st, sp)

	if _, err := h.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	argv := sp.specs[0].Argv
	if len(argv) != 2 || argv[1] != "--serve" {
		t.Fatalf("argv = %v, want the declared flags preserved", argv)
	}
	if !filepath.IsAbs(argv[0]) || !strings.Contains(argv[0], "acme__menu") {
		t.Errorf("argv[0] = %q, want it resolved into the pack's directory", argv[0])
	}
}

// --- what must be SKIPPED, and why each is different -------------------------

func TestDisabledAndDeclarativePacksAreNotStarted(t *testing.T) {
	st := &fakeStore{
		packs: []store.Pack{
			// Disabled means "do not run this" (MKT-097). Starting it because it
			// happens to carry code would make the toggle a lie.
			pack("acme/off", "1.0.0", false, map[string]any{"entry": "bin/pack"}),
			// A purely declarative pack — pages, data, locales — is a SUPPORTED
			// shape, not a misconfiguration, so it is skipped silently.
			pack("acme/declarative", "1.0.0", true, nil),
		},
		files: map[string][]byte{"acme/off\x00bin/pack": []byte("payload")},
	}
	sp := &fakeStarter{}
	h := host(t, st, sp)

	results, err := h.StartAll(context.Background())
	if err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %+v, want none — neither pack should be started or reported", results)
	}
	if len(sp.specs) != 0 {
		t.Fatalf("something was started: %+v", sp.specs)
	}
}

// One broken pack must not stop the box booting, and must not stop the packs
// that DO work — they are the ones an operator is relying on.
func TestOnePacksFailureDoesNotStopTheOthers(t *testing.T) {
	st := &fakeStore{
		packs: []store.Pack{
			pack("acme/broken", "1.0.0", true, map[string]any{"entry": "bin/missing"}),
			pack("acme/works", "1.0.0", true, map[string]any{"entry": "bin/pack"}),
		},
		files: map[string][]byte{"acme/works\x00bin/pack": []byte("payload")},
	}
	sp := &fakeStarter{}
	h := host(t, st, sp)

	results, err := h.StartAll(context.Background())
	if err != nil {
		t.Fatalf("StartAll must not fail the boot: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want both packs reported, got %+v", results)
	}
	var broken, works *Result
	for i := range results {
		switch results[i].PackID {
		case "acme/broken":
			broken = &results[i]
		case "acme/works":
			works = &results[i]
		}
	}
	if broken == nil || broken.Err == nil {
		t.Errorf("the broken pack was not reported as failed: %+v", broken)
	}
	if works == nil || works.Err != nil {
		t.Errorf("the working pack did not start: %+v", works)
	}
	if len(sp.specs) != 1 || sp.specs[0].ID != "acme/works" {
		t.Errorf("started = %+v, want only acme/works", sp.specs)
	}
}
