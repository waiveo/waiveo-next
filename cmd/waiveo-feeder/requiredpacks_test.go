package main

// requiredpacks_test.go is the reachability half of the required-pack floor
// (marketplace/1 MKT-093a/MKT-093b).
//
// The floor itself has thorough tests in internal/app/store and
// internal/app/packs, and every one of them passed while this binary wired
// api.WithPackTrust and never api.WithRequiredPacks — so on a real box the store
// held a nil roster, the in-transaction check returned nil on every call, and the
// floor refused nothing at all. A test of the mechanism cannot catch that. Only
// a test of the WIRING can, which is what this file is.
//
// It makes the claim in three pieces, weakest evidence to strongest:
//
//   - loadConfig: the roster path is a real deployment knob with a default that
//     leaves dev/CI unchanged, and it is pinned absolute at config load.
//   - main's SOURCE: the value packs.LoadRoster returns is the value handed to
//     api.WithRequiredPacks, inside the api.New call. Same technique, and same
//     limits, as TestMainStartsTheConsoleBinding — see its doc.
//   - the BUILT BINARY: a subprocess run of the actual shipped executable, which
//     is the only evidence here that does not depend on reading main at all.

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- (1) the deployment knob ----------------------------------------------

// TestLoadConfigRequiredPacksDefaultAndOverride pins the default at the
// make-dev-local path and the env override at whatever a box provisions.
//
// The default matters more than it looks: MKT-093a's default is that an ABSENT
// roster makes no pack required, and this path does not exist in a dev checkout
// or on CI, so wiring the roster changes nothing for anyone who has not authored
// one. A default pointing at a file the tooling creates would silently make
// every dev run a roster-bearing deployment.
func TestLoadConfigRequiredPacksDefaultAndOverride(t *testing.T) {
	def := loadConfig(func(string) string { return "" })
	wantDefault, err := filepath.Abs(".dev/feeder-required-packs.json")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if def.requiredPacksPath != wantDefault {
		t.Errorf("default requiredPacksPath = %q, want %q", def.requiredPacksPath, wantDefault)
	}
	if _, err := os.Stat(wantDefault); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the default roster path exists (%v) — a dev checkout must be an ABSENT-roster deployment (MKT-093a)", err)
	}

	env := map[string]string{"WAIVEO_FEEDER_REQUIRED_PACKS": "/etc/waiveo/required-packs.json"}
	got := loadConfig(func(k string) string { return env[k] })
	if got.requiredPacksPath != "/etc/waiveo/required-packs.json" {
		t.Errorf("requiredPacksPath = %q, want the explicit override", got.requiredPacksPath)
	}
}

// TestLoadConfigPinsTheRequiredPacksPathAbsolute: a relative roster path would
// mean the file this host consults follows the process's working directory, so
// the same deployment launched from elsewhere would resolve a different roster —
// and "absent means nothing is required" makes that failure SILENT, since the
// wrong-cwd host simply finds no file and lifts every floor.
func TestLoadConfigPinsTheRequiredPacksPathAbsolute(t *testing.T) {
	env := map[string]string{"WAIVEO_FEEDER_REQUIRED_PACKS": "relative/required-packs.json"}
	got := loadConfig(func(k string) string { return env[k] })
	if !filepath.IsAbs(got.requiredPacksPath) {
		t.Fatalf("requiredPacksPath = %q, want an absolute path pinned at config load", got.requiredPacksPath)
	}
	if !strings.HasSuffix(got.requiredPacksPath, "relative/required-packs.json") {
		t.Fatalf("requiredPacksPath = %q, want the configured path resolved against the cwd", got.requiredPacksPath)
	}
}

// ---- (2) main's own source -------------------------------------------------

// parseFeederMain returns main.go's `func main` declaration.
//
// consolebinding_test.go parses main the same way for its own reachability
// claim. The few lines are duplicated rather than shared because the two files
// are asserting independent things about the same function, and a shared helper
// would make deleting one claim look like it deleted the other's scaffolding.
func parseFeederMain(t *testing.T) *ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "main" {
			return fn
		}
	}
	t.Fatal("main.go declares no func main")
	return nil
}

// isPkgCall reports whether e is a call of pkg.name(...).
func isPkgCall(e ast.Expr, pkg, name string) (*ast.CallExpr, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return nil, false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return nil, false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != pkg {
		return nil, false
	}
	return call, true
}

// TestMainWiresTheLoadedRosterIntoTheAPI is the wiring claim, and it asserts the
// CHAIN rather than the presence of two independent statements.
//
// The defect it exists against is not "nobody called LoadRoster". It is the one
// this binary actually shipped: the floor implemented, the option available, and
// the option never passed — so the store's roster stayed nil and the check
// returned nil on every call. A test that merely found a LoadRoster call would
// pass against a main that loaded the roster, logged it, and threw it away.
//
// So it binds the two ends together: it reads the identifier packs.LoadRoster's
// result is assigned to, then requires that THAT identifier is the argument to
// api.WithRequiredPacks, and that the option sits in api.New's own argument list
// rather than anywhere in the function.
//
// Same limits as TestMainStartsTheConsoleBinding, restated because they apply
// here too: this catches the wiring being DELETED or REROUTED, not the whole
// api.New call being made unreachable behind a condition that is never true. The
// subprocess test below is what does not depend on reading main at all.
func TestMainWiresTheLoadedRosterIntoTheAPI(t *testing.T) {
	mainFn := parseFeederMain(t)

	// (a) which identifier holds the resolved roster.
	var rosterIdent string
	var loadArg string
	ast.Inspect(mainFn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
			return true
		}
		call, ok := isPkgCall(as.Rhs[0], "packs", "LoadRoster")
		if !ok {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			rosterIdent = id.Name
		}
		if len(call.Args) == 1 {
			if sel, ok := call.Args[0].(*ast.SelectorExpr); ok {
				loadArg = sel.Sel.Name
			}
		}
		return true
	})
	if rosterIdent == "" {
		t.Fatal("func main never assigns the result of packs.LoadRoster — no deployment could declare a required-pack roster at all (MKT-093a)")
	}
	if loadArg != "requiredPacksPath" {
		t.Fatalf("packs.LoadRoster is called with cfg.%s, want cfg.requiredPacksPath — the roster path must be the one loadConfig pinned absolute", loadArg)
	}

	// (b) that identifier reaches the store, through api.New's option list.
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
			opt, ok := isPkgCall(arg, "api", "WithRequiredPacks")
			if !ok || len(opt.Args) != 1 {
				continue
			}
			if id, ok := opt.Args[0].(*ast.Ident); ok && id.Name == rosterIdent {
				wired = true
			}
		}
		return true
	})
	if !wired {
		t.Fatalf("api.New in func main is not passed api.WithRequiredPacks(%s) — the store's roster stays nil, the in-transaction floor check returns nil on every call, and the floor enforces NOTHING on a real box (MKT-093b)", rosterIdent)
	}
}

// ---- (3) the built binary --------------------------------------------------

// feederBinary builds the actual shipped executable once for the tests below.
// This is the only evidence in this file that does not rest on reading main.go.
func feederBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "waiveo-feeder")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/waiveo-feeder")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the feeder: %v\n%s", err, out)
	}
	return bin
}

// runFeeder starts the built feeder with the given env overrides in its own
// scratch directory and returns its combined output and the run error. Every
// case here exits during boot, so there is no listener, no socket and nothing to
// drain; the context bounds the run in case a regression makes one of them
// proceed further than it should.
func runFeeder(t *testing.T, bin string, env map[string]string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	// A scratch cwd, so the relative .dev/ defaults main still carries for the
	// signing identity and the enrollment registry land in a temp dir rather than
	// in the repo.
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "WAIVEO_FEEDER_LISTEN=127.0.0.1:0")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestFeederBinaryRefusesToStartOnAnUnresolvableRoster is the done-bar's
// fail-closed half, proven against the SHIPPED EXECUTABLE.
//
// MKT-093a: an unresolvable roster "MUST NOT be treated as an empty one" — the
// deployment declared restrictions the host failed to learn, so the host must
// refuse to start rather than proceed with every floor silently lifted. The
// binary is handed a truncated roster document and must exit non-zero, before
// serving anything, naming the file.
//
// The second run is what keeps the first honest: the SAME binary, the SAME env,
// and a VALID roster in the same place boots past the roster and reports the
// entries it read. Without it, a feeder that failed to start for any reason
// whatsoever — or one that refused every roster including good ones — would pass.
func TestFeederBinaryRefusesToStartOnAnUnresolvableRoster(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the feeder binary")
	}
	bin := feederBinary(t)

	dir := t.TempDir()
	roster := filepath.Join(dir, "required-packs.json")

	// The scratch state the boot would otherwise create in the repo. contentPath
	// is deliberately a FILE, not a directory: it makes the boot fail
	// deterministically at the content origin, which is the first step AFTER the
	// roster load, so the valid-roster run below exits at a known point instead of
	// binding a listener this test would then have to tear down.
	contentFile := filepath.Join(dir, "content-is-a-file")
	if err := os.WriteFile(contentFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write content stand-in: %v", err)
	}
	env := map[string]string{
		"WAIVEO_FEEDER_REQUIRED_PACKS": roster,
		"WAIVEO_FEEDER_CONTENT_DIR":    contentFile,
		"WAIVEO_FEEDER_STORE":          filepath.Join(dir, "store.db"),
		"WAIVEO_FEEDER_AUTH_DIR":       filepath.Join(dir, "auth"),
		"WAIVEO_FEEDER_ARCHIVE_DIR":    filepath.Join(dir, "archive"),
		"WAIVEO_FEEDER_KEY_DIR":        filepath.Join(dir, "keys"),
	}

	// --- unresolvable: a truncated document.
	if err := os.WriteFile(roster, []byte(`{"format":"required-packs/1","required":[`), 0o600); err != nil {
		t.Fatalf("write roster: %v", err)
	}
	out, err := runFeeder(t, bin, env)
	if err == nil {
		t.Fatalf("the feeder STARTED with an unresolvable roster — it would serve with every required-pack floor lifted (MKT-093a)\n%s", out)
	}
	if !strings.Contains(out, roster) {
		t.Fatalf("the refusal does not name the roster file, so an operator cannot find it:\n%s", out)
	}
	if !strings.Contains(out, "UNRESOLVABLE") {
		t.Fatalf("the refusal does not say the roster is unresolvable rather than empty:\n%s", out)
	}
	// It refused BEFORE reaching the content origin — the first thing after the
	// roster load, and the thing the valid run below dies on. This is what makes
	// "refuses to start" mean "before it did anything", not "eventually".
	if strings.Contains(out, "open content store") {
		t.Fatalf("the feeder got past the roster and on to the content origin before refusing:\n%s", out)
	}

	// --- valid: the same binary, the same env, a roster it can resolve.
	const valid = `{"format":"required-packs/1","required":[{"pack_id":"waiveo/system","floor_version":"1.4.0"}]}`
	if err := os.WriteFile(roster, []byte(valid), 0o600); err != nil {
		t.Fatalf("rewrite roster: %v", err)
	}
	out, err = runFeeder(t, bin, env)
	if err == nil {
		t.Fatalf("the feeder did not exit at the deliberately broken content dir; this test's staging assumption is wrong:\n%s", out)
	}
	if !strings.Contains(out, "waiveo/system@1.4.0") {
		t.Fatalf("the shipped binary did not report the roster entry it read from %s — the operator's declaration did not reach the process:\n%s", roster, out)
	}
	if !strings.Contains(out, "open content store") {
		t.Fatalf("the feeder did not reach the content origin, so it did not get past the roster load on a VALID roster:\n%s", out)
	}
}
