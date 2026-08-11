package main

// marketplace_test.go is the reachability half of the marketplace
// (marketplace/1 MKT-060/060a/061).
//
// internal/app/packs holds ~8,700 lines of resolution — sealed-envelope
// verification, anti-rollback, hold eligibility, channel auto-tracking — with
// thorough tests, and every one of them passed while this binary called
// api.WithPackTrust and never api.WithMarketplace. So on a real box srv.market
// was nil, the installer was built with no registry, and the console's "Resolve
// and install" answered MARKETPLACE_REF_UNRESOLVED — "this deployment has no
// registry sources configured" — for a resolver that was complete and simply
// never mounted. A test of the mechanism cannot catch that. Only a test of the
// WIRING can, which is what this file is. It is the same shape, and for the same
// reason, as requiredpacks_test.go beside it, whose helpers it reuses.
//
// Three pieces, weakest evidence to strongest:
//
//   - loadConfig: the sources path is a real deployment knob with a default that
//     leaves dev/CI unchanged, and it is pinned absolute at config load.
//   - main's SOURCE: the chain LoadSources → NewMarket → api.WithMarketplace,
//     asserted link by link rather than as three independent statements.
//   - the BUILT BINARY: subprocess runs of the actual shipped executable, which
//     do not depend on reading main at all.

import (
	"errors"
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- (1) the deployment knob ----------------------------------------------

// TestLoadConfigPackSourcesDefaultAndOverride pins the default at the
// make-dev-local path and the env override at whatever a box provisions.
//
// The default matters for the same reason the roster's does: an absent document
// means no registry source, and this path does not exist in a dev checkout or on
// CI, so mounting the marketplace changes nothing for anyone who has not
// authored one. A default pointing at a file some tool creates would silently
// make every dev run a registry-bearing deployment.
func TestLoadConfigPackSourcesDefaultAndOverride(t *testing.T) {
	def := loadConfig(func(string) string { return "" })
	wantDefault, err := filepath.Abs(".dev/feeder-pack-sources.json")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if def.packSourcesPath != wantDefault {
		t.Errorf("default packSourcesPath = %q, want %q", def.packSourcesPath, wantDefault)
	}
	if _, err := os.Stat(wantDefault); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the default sources path exists (%v) — a dev checkout must be a no-registry deployment", err)
	}

	env := map[string]string{"WAIVEO_FEEDER_PACK_SOURCES": "/etc/waiveo/pack-sources.json"}
	got := loadConfig(func(k string) string { return env[k] })
	if got.packSourcesPath != "/etc/waiveo/pack-sources.json" {
		t.Errorf("packSourcesPath = %q, want the explicit override", got.packSourcesPath)
	}
}

// TestLoadConfigPinsThePackSourcesPathAbsolute: a relative path would mean the
// document this host consults follows the process's working directory, so the
// same deployment launched from elsewhere would resolve against a different
// registry list — and because "absent means no source" is silent, a wrong-cwd
// box would simply refuse every marketplace install with no indication that it
// was reading the wrong file. Worse, a writable cwd would let anything that can
// create a file there choose the registries this box installs code from.
func TestLoadConfigPinsThePackSourcesPathAbsolute(t *testing.T) {
	env := map[string]string{"WAIVEO_FEEDER_PACK_SOURCES": "relative/pack-sources.json"}
	got := loadConfig(func(k string) string { return env[k] })
	if !filepath.IsAbs(got.packSourcesPath) {
		t.Fatalf("packSourcesPath = %q, want an absolute path pinned at config load", got.packSourcesPath)
	}
	if !strings.HasSuffix(got.packSourcesPath, "relative/pack-sources.json") {
		t.Fatalf("packSourcesPath = %q, want the configured path resolved against the cwd", got.packSourcesPath)
	}
}

// ---- (2) main's own source -------------------------------------------------

// TestMainWiresTheLoadedSourcesIntoTheAPI asserts the CHAIN, not the presence of
// three statements.
//
// The defect it exists against is precisely the one this binary shipped: the
// resolver implemented, the option available, and the option never passed. A
// test that merely found a LoadSources call would pass against a main that read
// the document, logged the sources it found, and threw them away — which reads,
// in a log, exactly like a working marketplace.
//
// So it binds the ends together: the identifier packs.LoadSources' result is
// assigned to must be the one packs.NewMarket is built over, and the identifier
// NewMarket's result is assigned to must be the argument to api.WithMarketplace
// inside api.New's own argument list.
//
// Same limits as the roster's equivalent: this catches the wiring being DELETED
// or REROUTED, not the whole api.New call being made unreachable behind a
// condition that is never true. The subprocess tests below are what do not
// depend on reading main.
func TestMainWiresTheLoadedSourcesIntoTheAPI(t *testing.T) {
	mainFn := parseFeederMain(t)

	// (a) which identifier holds the loaded source list, and from which path.
	var sourcesIdent, loadArg string
	ast.Inspect(mainFn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
			return true
		}
		call, ok := isPkgCall(as.Rhs[0], "packs", "LoadSources")
		if !ok {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			sourcesIdent = id.Name
		}
		if len(call.Args) == 1 {
			if sel, ok := call.Args[0].(*ast.SelectorExpr); ok {
				loadArg = sel.Sel.Name
			}
		}
		return true
	})
	if sourcesIdent == "" {
		t.Fatal("func main never assigns the result of packs.LoadSources — no deployment could declare a registry source at all (MKT-060)")
	}
	if loadArg != "packSourcesPath" {
		t.Fatalf("packs.LoadSources is called with cfg.%s, want cfg.packSourcesPath — the sources path must be the one loadConfig pinned absolute", loadArg)
	}

	// (b) that list becomes a Market, on the clock-floor-aware reading rather
	// than the host clock: hold_hours eligibility is judged at resolution time
	// (MKT-042), and an appliance whose RTC has been rolled back must not have a
	// staged-rollout hold wished away by it (SEC-066-068).
	var marketIdent string
	ast.Inspect(mainFn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
			return true
		}
		call, ok := isPkgCall(as.Rhs[0], "packs", "NewMarket")
		if !ok {
			return true
		}
		if len(call.Args) != 2 {
			return true
		}
		clock, clockOK := call.Args[0].(*ast.Ident)
		list, listOK := call.Args[1].(*ast.Ident)
		if !clockOK || !listOK || list.Name != sourcesIdent {
			return true
		}
		if clock.Name != "nowMs" {
			t.Errorf("packs.NewMarket takes its clock from %s, want nowMs — the persisted-floor-aware reading, not the host clock (SEC-066-068)", clock.Name)
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			marketIdent = id.Name
		}
		return true
	})
	if marketIdent == "" {
		t.Fatalf("func main never builds a packs.NewMarket over %s — the declared sources are read and discarded, which in a log looks exactly like a working marketplace", sourcesIdent)
	}

	// (c) …and that Market reaches the api handler, through api.New's own
	// option list.
	wired := false
	ast.Inspect(mainFn, func(n ast.Node) bool {
		expr, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		newCall, ok := isPkgCall(expr, "api", "New")
		if !ok {
			return true
		}
		for _, arg := range newCall.Args {
			opt, ok := isPkgCall(arg, "api", "WithMarketplace")
			if !ok || len(opt.Args) != 1 {
				continue
			}
			if id, ok := opt.Args[0].(*ast.Ident); ok && id.Name == marketIdent {
				wired = true
			}
		}
		return true
	})
	if !wired {
		t.Fatalf("api.New in func main is not passed api.WithMarketplace(%s) — srv.market stays nil, the installer is built with no registry, and every marketplace reference refuses on a real box (MKT-060a)", marketIdent)
	}
}

// ---- (3) the built binary --------------------------------------------------

// packSourcesEnv is the scratch state a subprocess boot needs, plus the
// deliberately broken content dir that stops the boot at a known point.
//
// contentPath is a FILE, not a directory, exactly as the roster's own subprocess
// test does it: the content origin is the first thing after the sources load, so
// every run here exits there — after the sources report and before anything binds
// a listener this test would have to tear down.
func packSourcesEnv(t *testing.T, dir, sourcesPath string) map[string]string {
	t.Helper()
	contentFile := filepath.Join(dir, "content-is-a-file")
	if err := os.WriteFile(contentFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write content stand-in: %v", err)
	}
	return map[string]string{
		"WAIVEO_FEEDER_PACK_SOURCES": sourcesPath,
		"WAIVEO_FEEDER_CONTENT_DIR":  contentFile,
		"WAIVEO_FEEDER_STORE":        filepath.Join(dir, "store.db"),
		"WAIVEO_FEEDER_AUTH_DIR":     filepath.Join(dir, "auth"),
		"WAIVEO_FEEDER_ARCHIVE_DIR":  filepath.Join(dir, "archive"),
		"WAIVEO_FEEDER_KEY_DIR":      filepath.Join(dir, "keys"),
	}
}

// TestFeederBinaryReportsTheRegistrySourcesItLoaded proves, against the SHIPPED
// EXECUTABLE, that an operator's document reaches the process — and in the order
// they wrote it (MKT-061).
//
// Order is asserted by INDEX, not by both ids appearing: a boot that read the
// list and reversed it, or iterated a map, would print both lines and mean a
// different resolution preference.
func TestFeederBinaryReportsTheRegistrySourcesItLoaded(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the feeder binary")
	}
	bin := feederBinary(t)

	dir := t.TempDir()
	registry := filepath.Join(dir, "registry")
	if err := os.MkdirAll(registry, 0o700); err != nil {
		t.Fatalf("mkdir registry: %v", err)
	}
	sources := filepath.Join(dir, "pack-sources.json")
	doc := `{"format":"registry-sources/1","sources":[
		{"id":"local","channel":"marketplace/stable","index_url":"file://` + filepath.ToSlash(registry) + `/index.json","reserved_namespaces":["waiveo"]},
		{"id":"upstream","channel":"marketplace/stable","index_url":"https://registry.example/index.json"}
	]}`
	if err := os.WriteFile(sources, []byte(doc), 0o600); err != nil {
		t.Fatalf("write sources: %v", err)
	}

	out, err := runFeeder(t, bin, packSourcesEnv(t, dir, sources))
	if err == nil {
		t.Fatalf("the feeder did not exit at the deliberately broken content dir; this test's staging assumption is wrong:\n%s", out)
	}
	if !strings.Contains(out, "registry source 1/2: id=local") {
		t.Fatalf("the shipped binary did not report the FIRST declared source, in first position — the operator's declared resolution preference (MKT-061) did not reach the process:\n%s", out)
	}
	if !strings.Contains(out, "registry source 2/2: id=upstream") {
		t.Fatalf("the shipped binary did not report the second declared source in second position:\n%s", out)
	}
	// MKT-062's host authorization, and MKT-063's stale mark, both visible in the
	// boot report: an operator has no other way to confirm what a source is
	// permitted to serve, because nothing publishes it over the api.
	if !strings.Contains(out, "reserved-namespaces=[waiveo]") {
		t.Errorf("the boot report does not say which reserved namespace the source is authorized for (MKT-062):\n%s", out)
	}
	if !strings.Contains(out, "id=local") || !strings.Contains(out, "stale_source=true") {
		t.Errorf("the boot report does not mark the file:// source stale_source (MKT-063):\n%s", out)
	}
	if !strings.Contains(out, "open content store") {
		t.Fatalf("the feeder did not reach the content origin, so it did not get past the sources load:\n%s", out)
	}
}

// TestFeederBinaryStaysUpWithNoMarketplaceOnAnUnusableDocument is the fail-closed
// half, and it asserts BOTH directions of "fail closed and say so".
//
// A broken sources document must not take the box down: unlike the required-pack
// roster it withholds a capability rather than a restriction, so a host that
// carries on can install nothing it could not install before and stays reachable
// for the operator to fix the file. Refusing to boot there would take a screen
// fleet offline over a JSON comma.
//
// And it must not go quiet: the run has to name the file and say that NO source
// is configured. Without that half, a feeder that silently ignored the document
// would pass this test — which is the exact degradation the whole thing is
// written against.
func TestFeederBinaryStaysUpWithNoMarketplaceOnAnUnusableDocument(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the feeder binary")
	}
	bin := feederBinary(t)

	dir := t.TempDir()
	sources := filepath.Join(dir, "pack-sources.json")
	// Truncated: valid up to the point it stops.
	if err := os.WriteFile(sources, []byte(`{"format":"registry-sources/1","sources":[`), 0o600); err != nil {
		t.Fatalf("write sources: %v", err)
	}

	out, err := runFeeder(t, bin, packSourcesEnv(t, dir, sources))
	if err == nil {
		t.Fatalf("the feeder did not exit at the deliberately broken content dir; this test's staging assumption is wrong:\n%s", out)
	}
	// It got PAST the sources load — an unusable source list is not fatal.
	if !strings.Contains(out, "open content store") {
		t.Fatalf("the feeder stopped at the sources document instead of carrying on with no marketplace — a JSON comma took the box down:\n%s", out)
	}
	if !strings.Contains(out, sources) {
		t.Fatalf("the refusal does not name the sources file, so an operator cannot find it:\n%s", out)
	}
	if !strings.Contains(out, "NO registry source is configured") {
		t.Fatalf("the boot did not say that no registry source is configured — a silently ignored document is indistinguishable from a working one until an install refuses:\n%s", out)
	}
}

// TestFeederBinaryWithNoSourcesDocumentSaysSo covers the DEFAULT deployment: no
// document at all.
//
// It is here because absent and unusable must be distinguishable in the log, and
// because this is the state every existing box is in — the change must be a
// no-op for them, loudly enough that an operator who MEANT to provision a
// document can see it is not being read.
func TestFeederBinaryWithNoSourcesDocumentSaysSo(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the feeder binary")
	}
	bin := feederBinary(t)

	dir := t.TempDir()
	sources := filepath.Join(dir, "not-authored.json")

	out, err := runFeeder(t, bin, packSourcesEnv(t, dir, sources))
	if err == nil {
		t.Fatalf("the feeder did not exit at the deliberately broken content dir; this test's staging assumption is wrong:\n%s", out)
	}
	if !strings.Contains(out, "NO registry sources document at "+sources) {
		t.Fatalf("an unprovisioned box does not say where it looked for a sources document:\n%s", out)
	}
	// …and it is not reported as a BROKEN document, which would send an operator
	// looking for a file they never wrote.
	if strings.Contains(out, "NO registry source is configured") {
		t.Fatalf("an absent document is reported as an unusable one:\n%s", out)
	}
	if !strings.Contains(out, "open content store") {
		t.Fatalf("the feeder did not reach the content origin:\n%s", out)
	}
}
