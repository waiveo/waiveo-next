package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// consoleroutes_test.go pins the one property that the console's route table and
// the feeder's route table — two lists that live in this repo, are edited by
// different people on different days, and had never been compared — must jointly
// satisfy: EVERY console route reaches the SPA.
//
// # The defect this exists for
//
// The console declared a real page at `/content`: routed in web/src/App.tsx,
// listed in the primary nav, with a component behind it. The feeder mounts the
// content ORIGIN at `/content/` (contenturl.PathPrefix) on the SAME mux that
// serves the SPA. Go's ServeMux answers a subtree pattern by redirecting the
// slashless form into it, so `GET /content` was a 307 to `/content/` and then the
// origin's own 403 Problem document — raw JSON, no chrome, no nav, no way back
// but the browser's Back button. The page existed, was routed, was in the rail,
// and was unreachable.
//
// Nothing caught it, and each half was individually correct. The web tests render
// the SPA with no server mux, so the route resolves there. `npm run dev:ui` serves
// the SPA for `/content` and proxies only `/api`, so it works in dev too. It broke
// ONLY in the real embedded-console deployment — the one arrangement nothing
// exercised.
//
// # Why it is derived rather than listed
//
// A hand-written list of "the paths the server owns" is the same class of artifact
// as the bug: it would be right on the day it was written and silently wrong the
// first time someone mounted a new prefix. Both sides are read from source:
//
//   - the SERVER patterns, from the `http.ServeMux` that `main` builds — every
//     `Handle`/`HandleFunc` on it, plus every function that is handed the mux and
//     registers on it in turn (enroll.Server.Register mounts four bootstrap
//     routes nobody would think to list). Pattern arguments are resolved through
//     package constants, so `contenturl.PathPrefix` counts as `/content/`. A call
//     that hands the mux to something this scan cannot resolve FAILS rather than
//     being skipped.
//   - the CONSOLE routes, from `<Route path=…>` in web/src/App.tsx, and the nav
//     targets from the shell's NAV_TREE (web/src/shell/nav-tree.ts).
//
// # And why the matching is delegated
//
// The collision rule is not "same string": it is Go's own subtree/redirect
// semantics, which is exactly the subtlety that hid the defect. So the derived
// patterns are registered on a REAL http.ServeMux and each console route is asked
// of it — a route the SPA gets answers with the pattern `/`, and anything else is
// a route an operator cannot reach.

// ── the property ────────────────────────────────────────────────────────────

func TestNoConsoleRouteIsShadowedByAServerRoute(t *testing.T) {
	root := moduleRoot(t)
	server := serverPatterns(t, root)
	routes := consoleRoutes(t, root)

	// Completeness first. Both sides are derived, so a derivation that quietly
	// stopped finding anything would make every assertion below vacuously true —
	// the failure mode this repo has shipped before. These name members
	// literally, and say why, so DELETING a route from either list is as visible
	// as adding one.
	for _, want := range []string{"/", "/api/v1/", "/content/", "/events/v1", "/healthz", "/relay/v1", "/telemetry/v1/push", "/enroll"} {
		if !slices.Contains(server, want) {
			t.Fatalf("derived server patterns %v are missing %q — the scan over main()'s mux found the wrong thing (or a route was removed and this list not updated); every assertion below would be vacuous", server, want)
		}
	}
	// Named members chosen to survive the 2026-08-19 console strip: the front
	// door, the ONE ship target still installed (Discovery), the surface that
	// installs it, and the two identity/box pages that belong to no pack. The
	// list used to name /screens and /media, which have since been deleted along
	// with the rest of the slidecast surface.
	for _, want := range []string{"/", "/devices", "/extensions", "/login", "/system"} {
		if !slices.Contains(routes, want) {
			t.Fatalf("derived console routes %v are missing %q — the scan over web/src/App.tsx found the wrong thing", routes, want)
		}
	}
	// An anti-vacuity floor, not a product rule. Fourteen routes survive the
	// strip; a scan that found fewer than ten has broken rather than the console
	// having shrunk again, and it is the SCAN this guard is about.
	if len(routes) < 10 {
		t.Fatalf("derived only %d console routes (%v); the console has more than that, so the scan is broken", len(routes), routes)
	}

	// The console's own mux, standing in for the feeder's: the derived patterns
	// with placeholder handlers, and the SPA's catch-all at "/". http.ServeMux
	// does the matching, including the trailing-slash redirect that caused this.
	mux := http.NewServeMux()
	for _, pat := range server {
		mux.Handle(pat, http.NotFoundHandler())
	}

	for _, route := range routes {
		path := concreteURLPath(route)
		req := httptest.NewRequest(http.MethodGet, path, nil)
		_, pattern := mux.Handler(req)
		if pattern != "/" {
			rec := httptest.NewRecorder()
			h, _ := mux.Handler(req)
			h.ServeHTTP(rec, req)
			t.Errorf("console route %q (as %q) is answered by the server pattern %q, not by the SPA catch-all: status %d, Location %q.\n"+
				"An operator clicking it lands on the server's response instead of the page. Move the CONSOLE route unless the server prefix is genuinely free to move — %q is on the wire in signed content urls and in relay/1 REL-061's grammar, which is why /content was the half that stayed.",
				route, path, pattern, rec.Code, rec.Header().Get("Location"), pattern)
		}
	}
}

func TestEveryNavItemPointsAtADeclaredConsoleRoute(t *testing.T) {
	root := moduleRoot(t)
	routes := consoleRoutes(t, root)
	nav := navTargets(t, root)

	// Seven rail targets survive the 2026-08-19 strip (Overview; Discovery's All
	// devices; Extensions' Installed; Platform's Settings, API keys, Activity,
	// System). The threshold was 10 against the pre-strip rail — it is lowered
	// rather than deleted because what it guards is a DERIVATION that found
	// nothing, which would make every assertion below vacuously true.
	if len(nav) < 5 {
		t.Fatalf("derived only %d nav targets (%v); the primary rail has more, so the scan is broken", len(nav), nav)
	}
	for _, want := range []string{"/", "/devices", "/system"} {
		if !slices.Contains(nav, want) {
			t.Fatalf("derived nav targets %v are missing %q — the scan over the shell's NAV_TREE found the wrong thing", nav, want)
		}
	}
	for _, to := range nav {
		if !slices.Contains(routes, to) {
			t.Errorf("the primary nav offers %q, which web/src/App.tsx declares no <Route> for — clicking it renders nothing inside the shell. Declared routes: %v", to, routes)
		}
	}
}

// TestEveryConsoleRouteIsReachable is the converse property, and the one the
// nav RESTRUCTURE made worth pinning: the test above catches a nav entry that
// leads nowhere, and this one catches a PAGE that nothing leads to.
//
// It exists because grouping thirteen flat siblings into a tree is exactly the
// operation that loses one. A dropped node still compiles, still typechecks, and
// still renders a perfectly good console — the page is simply gone, and the only
// way to notice is to remember it existed. An orphaned page is a worse outcome
// than the flat rail this replaced, so it is a build failure.
//
// Both sides are derived, and the exemptions are too. A route that is
// deliberately off the rail — the sign-in page, the account page in the header,
// a developer reference surface reached by address, a pack's own page — is declared in
// OFF_RAIL_ROUTES *in the nav tree itself*, next to the door it is reached
// through. That is what keeps this from degenerating into the thing it is
// guarding against: there is no list of exceptions in this file to quietly grow
// whenever the check is inconvenient. Adding a <Route> and nothing else fails,
// and the two ways to fix it are to put it in the nav or to write down how an
// operator gets there.
func TestEveryConsoleRouteIsReachable(t *testing.T) {
	root := moduleRoot(t)
	routes := consoleRoutes(t, root)
	nav := navTargets(t, root)
	offRail := offRailRoutes(t, root)

	// Completeness on the third derived list, for the same reason the others
	// have one: an OFF_RAIL_ROUTES scan that found nothing would make every
	// route below look unreachable, and one that matched the whole file would
	// make every route look excused. The second failure is the dangerous one —
	// it passes.
	if len(offRail) < 3 {
		t.Fatalf("derived only %d off-rail routes (%v); the console has more, so the scan over OFF_RAIL_ROUTES is broken", len(offRail), offRail)
	}
	// /studio was the canonical off-rail example until it was deleted with the
	// slidecast surface; /security is the durable one — the header's account
	// link, which acts on the principal rather than on a console resource. The
	// pack-page route joined them on 2026-08-19, when the owner removed the
	// pack-contributed rail section: every `/p/...` page is now reached from
	// Extensions > Installed, and naming it here means DELETING that declaration
	// is a loud failure rather than a page nothing leads to.
	for _, want := range []string{"/login", "/security", "/p/:publisher/:name/*"} {
		if !slices.Contains(offRail, want) {
			t.Fatalf("derived off-rail routes %v are missing %q — the scan over OFF_RAIL_ROUTES found the wrong thing", offRail, want)
		}
	}
	// ...and nothing may be BOTH, which would mean a page was quietly excused
	// while it was on the rail all along — the excuse would then outlive its
	// removal from the nav and hide the orphan this test exists to catch.
	for _, to := range offRail {
		if slices.Contains(nav, to) {
			t.Errorf("%q is declared in OFF_RAIL_ROUTES but IS on the primary rail; drop the declaration, or it will excuse the page for good the day it leaves the nav", to)
		}
	}

	// Every declaration has to SAY the door. Read the limit of this honestly:
	// what follows checks that a justification was written, not that it is true —
	// only a test that drives the named door can do that, which is why the pack
	// pages' door is clicked in web/src/routes/extensions/extensions-route.test.tsx
	// rather than asserted here. What this does buy is that OFF_RAIL_ROUTES
	// cannot decay into a bare list of paths: an entry costs an argument, and an
	// argument is the thing a reviewer can disagree with. Without it the whole
	// guard degrades toward "every route is either reachable or declared
	// unreachable", which is a tautology dressed as a property.
	doors := offRailDoors(t, root)
	for _, to := range offRail {
		via, ok := doors[to]
		if !ok {
			t.Errorf("OFF_RAIL_ROUTES declares %q with no reachedVia this scan can read; the declaration is the DOOR, and a `to:` on its own excuses the page while recording nothing", to)
			continue
		}
		if len(strings.TrimSpace(via)) < 60 || strings.Contains(via, "TODO") || strings.Contains(via, "TBD") {
			t.Errorf("OFF_RAIL_ROUTES declares %q with a reachedVia that says nothing (%q). Name the surface an operator actually reaches it from, and why it is not a rail destination.", to, via)
		}
	}

	for _, route := range routes {
		if slices.Contains(nav, route) || slices.Contains(offRail, route) {
			continue
		}
		t.Errorf("web/src/App.tsx declares a page at %q that NOTHING navigates to: it is in no group in web/src/shell/nav-tree.ts and is not declared in that file's OFF_RAIL_ROUTES.\n"+
			"An operator can only reach it by typing the URL. Put it in the tree, or — if it genuinely is not a rail destination — add it to OFF_RAIL_ROUTES with the door it IS reached through.\n"+
			"Rail targets: %v\nOff-rail: %v", route, nav, offRail)
	}
}

// concreteURLPath turns a react-router path pattern into a URL an operator could
// actually be at: `:param` segments and a trailing `*` splat become sample
// segments, since it is the STATIC prefix that decides which server pattern claims
// the request.
func concreteURLPath(route string) string {
	segs := strings.Split(route, "/")
	for i, s := range segs {
		switch {
		case strings.HasPrefix(s, ":"):
			segs[i] = "sample"
		case s == "*":
			segs[i] = "sample"
		}
	}
	out := strings.Join(segs, "/")
	if out == "" {
		return "/"
	}
	return out
}

// ── deriving the server's patterns ──────────────────────────────────────────

// serverPatterns returns every path pattern registered on the http.ServeMux that
// `main` builds and serves, sorted and de-duplicated.
func serverPatterns(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	dir := filepath.Join(root, "cmd", "waiveo-feeder")
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	pkg, ok := pkgs["main"]
	if !ok {
		t.Fatalf("no package main in %s", dir)
	}

	var mainFn *ast.FuncDecl
	for _, f := range pkg.Files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if ok && fn.Recv == nil && fn.Name.Name == "main" && fn.Body != nil {
				mainFn = fn
			}
		}
	}
	if mainFn == nil {
		t.Fatal("cmd/waiveo-feeder has no func main() — the scan cannot find the mux the binary serves")
	}

	muxVar := serveMuxVarIn(mainFn)
	if muxVar == "" {
		t.Fatal("func main() has no `x := http.NewServeMux()` — the console-serving mux moved, and this scan must move with it")
	}

	consts := stringConstIndex(t, root)
	funcs := funcIndex(t, root)

	patterns := map[string]bool{}
	var handedOff []*ast.CallExpr
	collectRegistrations(mainFn.Body, muxVar, patterns, &handedOff, consts, t)

	// Second level: anything main hands the mux to may register on it in turn.
	for _, call := range handedOff {
		name := muxCalleeName(call)
		if name == "" {
			t.Fatalf("main() passes the mux to a call this scan cannot name (%T) — teach serverPatterns about it rather than letting a registration go uncounted", call.Fun)
		}
		decls, found := funcs[name]
		if !found {
			t.Fatalf("main() passes the mux to %q, which this scan cannot find a declaration for in this module — teach serverPatterns about it rather than letting a registration go uncounted", name)
		}
		for _, fn := range decls {
			param := serveMuxParamName(fn)
			if param == "" {
				// Takes the mux as a plain http.Handler (the trace-id wrapper
				// does): it cannot register anything.
				continue
			}
			collectRegistrations(fn.Body, param, patterns, nil, consts, t)
		}
	}

	out := make([]string, 0, len(patterns))
	for p := range patterns {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// collectRegistrations walks body for `muxVar.Handle(...)` / `.HandleFunc(...)`
// calls, resolving each pattern argument to its string, and (when handedOff is
// non-nil) records every other call that is given muxVar as an argument.
func collectRegistrations(body *ast.BlockStmt, muxVar string, into map[string]bool, handedOff *[]*ast.CallExpr, consts map[string]string, t *testing.T) {
	if body == nil {
		return
	}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok && isIdent(sel.X, muxVar) && (sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc") {
			if len(call.Args) == 0 {
				return true
			}
			pat, err := staticString(call.Args[0], consts)
			if err != nil {
				t.Fatalf("%s.%s's pattern argument is not a string this scan can resolve (%v) — a route registered from a computed value is invisible to the collision check", muxVar, sel.Sel.Name, err)
			}
			into[pat] = true
			return true
		}
		if handedOff == nil {
			return true
		}
		for _, arg := range call.Args {
			if isIdent(arg, muxVar) {
				*handedOff = append(*handedOff, call)
				break
			}
		}
		return true
	})
}

// serveMuxVarIn returns the name assigned by the first `x := http.NewServeMux()`
// in fn, or "".
func serveMuxVarIn(fn *ast.FuncDecl) string {
	found := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewServeMux" || !isIdent(sel.X, "http") {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			found = id.Name
		}
		return false
	})
	return found
}

// serveMuxParamName returns the name of fn's `*http.ServeMux` parameter, or "".
func serveMuxParamName(fn *ast.FuncDecl) string {
	if fn.Type.Params == nil {
		return ""
	}
	for _, f := range fn.Type.Params.List {
		star, ok := f.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ServeMux" || !isIdent(sel.X, "http") {
			continue
		}
		if len(f.Names) > 0 {
			return f.Names[0].Name
		}
	}
	return ""
}

func muxCalleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// staticString resolves a pattern argument: a string literal, or an identifier /
// package selector naming a package-level string constant.
func staticString(e ast.Expr, consts map[string]string) (string, error) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", errNotString
		}
		return strconv.Unquote(v.Value)
	case *ast.Ident:
		if s, ok := consts[v.Name]; ok {
			return s, nil
		}
		return "", errNotString
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			if s, ok := consts[x.Name+"."+v.Sel.Name]; ok {
				return s, nil
			}
		}
		return "", errNotString
	}
	return "", errNotString
}

type constErr string

func (e constErr) Error() string { return string(e) }

const errNotString = constErr("not a resolvable string constant")

// stringConstIndex maps `Name` and `pkg.Name` to the value of every package-level
// string constant in the module, so `contenturl.PathPrefix` resolves to
// `/content/` without this file spelling that path itself.
func stringConstIndex(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	eachGoFile(t, root, func(file *ast.File, _ string) {
		pkg := file.Name.Name
		for _, d := range file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
					continue
				}
				lit, ok := vs.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				out[pkg+"."+vs.Names[0].Name] = val
				out[vs.Names[0].Name] = val
			}
		}
	})
	return out
}

// funcIndex maps a function or method name to every declaration of it in the
// module — enough to find the body of whatever main hands the mux to.
func funcIndex(t *testing.T, root string) map[string][]*ast.FuncDecl {
	t.Helper()
	out := map[string][]*ast.FuncDecl{}
	eachGoFile(t, root, func(file *ast.File, _ string) {
		for _, d := range file.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
				out[fn.Name.Name] = append(out[fn.Name.Name], fn)
			}
		}
	})
	return out
}

func eachGoFile(t *testing.T, root string, visit func(*ast.File, string)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web", "player-v3":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		visit(file, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// ── deriving the console's routes ───────────────────────────────────────────

var routePathRe = regexp.MustCompile(`<Route\s+path="([^"]*)"`)

// consoleRoutes returns every path web/src/App.tsx declares a <Route> for.
func consoleRoutes(t *testing.T, root string) []string {
	t.Helper()
	src := readRepoFile(t, root, filepath.Join("web", "src", "App.tsx"))
	seen := map[string]bool{}
	for _, m := range routePathRe.FindAllStringSubmatch(src, -1) {
		p := m[1]
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		seen[p] = true
	}
	// The layout <Route> that wraps the shell carries no path; the root page does
	// (`path="/"`), so "/" is present on its own account.
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

var navToRe = regexp.MustCompile(`\bto:\s*"([^"]+)"`)

// navTreeFile is where the console's information architecture lives, as data.
// It used to be a flat NAV_ITEMS array inside app-shell.tsx; it became a nested
// tree in its own module when the rail was grouped by product area, and this
// scan followed it — the derivation moves with the nav, which is the whole point
// of deriving rather than listing.
var navTreeFile = filepath.Join("web", "src", "shell", "nav-tree.ts")

// navTargets returns every `to:` in the shell's NAV_TREE — the rail's
// destinations, at whatever depth they sit. Group nodes carry no `to` (a group
// is a product area, not a page), so they contribute nothing and need no
// special handling here.
func navTargets(t *testing.T, root string) []string {
	t.Helper()
	return tableTargets(t, root, "export const NAV_TREE")
}

// offRailRoutes returns every `to:` in OFF_RAIL_ROUTES — the pages that are
// deliberately not rail destinations, each declared alongside the door it IS
// reached through.
func offRailRoutes(t *testing.T, root string) []string {
	t.Helper()
	return tableTargets(t, root, "export const OFF_RAIL_ROUTES")
}

// offRailEntryRe reads one OFF_RAIL_ROUTES entry — its route and the prose that
// names the door. Deliberately anchored on the PAIR: an entry whose reachedVia
// this cannot read is reported as missing rather than silently skipped, since a
// scan that quietly finds nothing is how a justification requirement becomes a
// formality.
var offRailEntryRe = regexp.MustCompile(`(?s)to:\s*"([^"]+)",\s*reachedVia:\s*"((?:[^"\\]|\\.)*)"`)

// offRailDoors maps each off-rail route to the prose justifying it.
func offRailDoors(t *testing.T, root string) map[string]string {
	t.Helper()
	src := readRepoFile(t, root, navTreeFile)
	start := strings.Index(src, "export const OFF_RAIL_ROUTES")
	if start < 0 {
		t.Fatalf("%s has no OFF_RAIL_ROUTES — the console navigation moved and this scan must move with it", navTreeFile)
	}
	end := strings.Index(src[start:], "\n];")
	if end < 0 {
		t.Fatalf("could not find the end of OFF_RAIL_ROUTES in %s", navTreeFile)
	}
	out := map[string]string{}
	for _, m := range offRailEntryRe.FindAllStringSubmatch(src[start:start+end], -1) {
		out[m[1]] = m[2]
	}
	return out
}

// tableTargets pulls the `to:` values out of one top-level array literal in the
// nav tree module. The terminator is `\n];` at column zero, which only the
// OUTER array can match — a group's nested `children` array closes at an indent,
// so a group's leaves are collected as part of the tree they belong to rather
// than ending the scan early.
func tableTargets(t *testing.T, root, decl string) []string {
	t.Helper()
	src := readRepoFile(t, root, navTreeFile)
	start := strings.Index(src, decl)
	if start < 0 {
		t.Fatalf("%s has no %q — the console navigation moved and this scan must move with it", navTreeFile, decl)
	}
	end := strings.Index(src[start:], "\n];")
	if end < 0 {
		t.Fatalf("could not find the end of %q in %s", decl, navTreeFile)
	}
	block := src[start : start+end]
	seen := map[string]bool{}
	for _, m := range navToRe.FindAllStringSubmatch(block, -1) {
		seen[m[1]] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func readRepoFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// moduleRoot walks up from this package to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
