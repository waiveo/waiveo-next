package contenturl

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// pathgrammar_test.go pins the ONE structural rule that makes an unsigned
// content URL impossible to write by accident: the `/content/` path segment is
// spelled in exactly one package — this one — and every other file in the
// module reaches the grammar through PathPrefix, URL, UnsignedURL or
// Signer.Mint.
//
// # Why the rule is a source scan rather than a behavioural check
//
// Because the property being protected is that a certain KIND OF CODE does not
// exist, and a behavioural test can only observe the code that does. The
// end-to-end sweep next door
// (snapshot.TestEveryContentURLTheSnapshotCarriesIsFetchableAtTheOrigin) fetches
// every url a built generation carries from the real origin, which catches a new
// producer the moment its output reaches a snapshot — but it cannot see a
// producer on a surface that fixture does not drive. That is not hypothetical:
// the defect this pair exists for shipped on FOUR producers at once, and the one
// an operator actually hit was the api's upload response, which no snapshot
// carries at all.
//
// # What it checks, exactly
//
// Over EVERY non-test .go file in the module — every directory, including
// `internal/app/web/` and anything else whose name happens to collide with a
// skipped one — it rejects a string literal that:
//
//   - contains the whole `/content/` grammar (contentPathSpelled), or
//   - contains `content` as a slash-adjacent PATH SEGMENT — `"/content"`,
//     `"content/"` — which is how the grammar gets assembled from halves
//     (`"/content" + "/"`, a const segment joined at a call site), or
//   - is the bare segment `"content"` passed to a path JOINER — path.Join,
//     filepath.Join, url.URL.JoinPath — which is the other ordinary way to
//     build the same path without ever writing it.
//
// # What it does NOT catch, stated so nobody reads a guarantee into it
//
// It is a source scan, so it sees literals and call shapes and nothing else. All
// of these get past it:
//
//   - a segment that never appears as a literal: built from a variable, a
//     config value, a rune slice, a template rendered at runtime — including
//     `fmt.Sprintf("%s/%s/%s", base, seg, hex)` where `seg` is a variable;
//   - a formatter whose FORMAT is itself computed, so the slash never appears
//     in a literal either;
//   - a joiner this file does not name (a helper in some other package that
//     itself joins path segments);
//   - anything in the two allowlisted places below, and anything outside Go
//     source entirely — the web console, player-v3, a shell script.
//
// That is acceptable and is the honest bar: the failure this guards is a
// FORGOTTEN obligation, not a circumvented one. Its complement is the
// behavioural sweep next door
// (snapshot.TestEveryContentURLTheSnapshotCarriesIsFetchableAtTheOrigin), which
// cannot see a producer no fixture drives but does not care HOW a url was
// spelled — between them, a new producer has to be both unspelled and undriven
// to hide. Neither is a proof.
//
// Test files are exempt. A test legitimately writes the unsigned form to assert
// against it (internal/app/api/content_test.go builds the expected url that way),
// and a test is not a producer: nothing it constructs is served to a screen.

// grammarOwner is the one package permitted to spell the path segment: the
// package that mints and verifies it.
const grammarOwner = "internal/feeder/contenturl"

// scanFloor is a lower bound on the non-test .go files this walk must reach. It
// exists because the failure mode of a source scan is not a wrong answer, it is a
// SILENT one: the previous version skipped directories by bare name, so any tree
// named `web` anywhere — `internal/app/web/`, say — was invisible, and nothing
// said so. A count that collapses is the observable symptom of that, whatever
// caused it.
//
// It is deliberately CLOSE to actual (363 at the time of writing) rather than
// conservative. At its first value of 200 it did not do the job it was added
// for: a reviewer narrowed skipDir to also skip `internal/app` — a hundred files,
// and the tree holding the api producers HV-1 was actually found on — and this
// test still passed, because 263 clears 200 comfortably. A floor that survives
// losing the most important quarter of the module is a floor that only detects
// catastrophe.
//
// Deleting files is legitimate; the tripwire is meant to be re-armed by hand when
// that happens, which is a one-line edit a reviewer can see, not a silent slide.
const scanFloor = 340

// requiredTrees is the coverage assertion the count alone cannot make: which
// SUBTREES the walk must reach, and roughly how much of each.
//
// A global count is a single number that many different narrowings can satisfy;
// this is what makes "the walk stopped entering the api package" a distinct,
// named failure rather than an arithmetic one. The minimums are deliberately well
// under actual (the parenthesised figures) — they are here to catch a tree going
// to ZERO or near it, not to track its size.
//
// A tree is keyed by its first path segment, or its first two under `internal/`,
// which is the granularity at which this module is actually organized.
//
// EVERY tree the walk reaches is listed, not just the interesting ones, and that
// is the point rather than thoroughness for its own sake. Ten trees were left
// out of the first version, and each of them was a hole in BOTH checks at once:
// `internal/manifest` is 14 files against a floor with 24 files of slack, so the
// walk could have stopped entering it and the count would still have cleared —
// silently, which is the exact failure mode this guard exists to make loud. Two
// of the omissions were the ones that mattered most: `internal/datamodel`, where
// the `Layer`/`Slide` types a derived url hangs off are declared, and
// `internal/slidelive`, which resolves content. A tree with no row here is a
// tree this guard cannot notice losing.
var requiredTrees = map[string]int{
	"cmd":                    8,  // 14: every binary, including the feeder that mounts the origin
	"internal/app":           60, // 100: the api producers HV-1 was found on
	"internal/feeder":        8,  // 13: the origin, the snapshot builder, this package
	"internal/packhost":      1,  // 2: the pack process supervisor and its tests
	"internal/relay":         30, // 57: the relay-side producers REL-066d will add to
	"internal/shared":        15, // 27: wire, where a url-bearing type would be declared
	"internal/rules":         20, // 36
	"internal/events":        8,  // 16
	"conformance":            20, // 36: the contract suites
	"scripts":                8,  // 15: the make-dev loops, one of which is allowlisted
	"internal/archive":       4,  // 8: the export/restore path
	"internal/manifest":      8,  // 14: pack manifests, which declare playable content
	"internal/datamodel":     4,  // 8: where Layer and Slide are declared
	"internal/derive":        5,  // 11: the off-appliance rasterizer, incl. derive/qr
	"internal/deviceclass":   3,  // 6
	"internal/virtualplayer": 2,  // 4: the synthetic player, which consumes served urls
	"internal/tools":         1,  // 3
	"internal/castbundle":    1,  // 1: the portable .cast bundle writer/reader, which streams content bytes
	"extensions":             1,  // 2: the first-party extensions this platform ships (waiveo/discovery) and their runtime binaries
	"internal/slidelive":     1,  // 2: content resolution
	"internal/packsig":       1,  // 2
	"api":                    1,  // 2
	"examples":               1,  // 1: the in-repo example pack
}

// treeOf is the key requiredTrees is counted by: the first path segment, or the
// first two under `internal/`.
func treeOf(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) >= 2 && parts[0] == "internal" {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// allowedSpellings are the paths permitted to spell a `content` path segment
// outside the owner, each with the reason it is not the content-origin grammar.
// Matched as a prefix of the module-relative, slash-separated path, so a
// directory entry covers everything beneath it.
//
// It is deliberately a written list rather than a pattern: two entries, each
// with a reason a reader can check, is a baseline; a pattern is a hole.
//
// # The granularity, stated because it is coarser than it looks
//
// An exemption is per FILE (or per directory), not per literal. Nothing here
// says "this one string in this one function"; it says "the heuristics do not
// apply to anything in this file". So a second, genuinely wrong `content` path
// spelled anywhere else in internal/app/api/api.go would go unflagged — the file
// is 1200 lines of route mounting, and the exemption covers all of it. The
// heuristics are the only thing exempted (the whole `/content/` grammar is still
// reported from an allowlisted path, see the check in the walk), which is what
// keeps the blast radius to half-spellings and joiner shapes.
//
// # The one shape that has no escape here
//
// If api/openapi.yaml ever grew a route like `GET /content/{asset_ref}`, the
// generated code under api/gen/ would spell the whole `/content/` grammar as a
// literal and this test would fail — and the allowlist could not silence it,
// because an allowlisted path is exempt from the heuristics only, never from the
// full grammar. That is the intended answer, not an oversight: a second route
// serving content bytes is exactly the thing this guard exists to make someone
// argue for. Whoever adds it changes the rule deliberately (or names the new
// route something that is not this grammar), rather than dropping a line into a
// map.
var allowedSpellings = map[string]string{
	// The app API's OWN /api/v1/content route — the upload and the listing. A
	// different path space entirely: it is mounted under apiPrefix, it answers
	// JSON, and it is where a caller GOES to be handed a minted url. It is not a
	// producer of the origin's fetch grammar; the handler behind it mints
	// through contenturl.Signer (internal/app/api/content.go).
	//
	// File-wide, per the note above: every literal in api.go is exempt from the
	// heuristics, not just the route string this entry is about.
	"internal/app/api/api.go": "the app api's own /api/v1/content route (upload + listing), not the content origin's fetch path",
	// Generated from api/openapi.yaml, which describes that same app api. Nothing
	// here can be a content-origin producer, because the document it is generated
	// from does not describe the content origin — and if that ever stopped being
	// true (a `/content/{asset_ref}` route), this entry would not save it.
	"api/gen/": "generated client/server for the app api surface, from api/openapi.yaml",
	// The `make dev` content smoke, which POSTs to that same /api/v1/content
	// route and then fetches the url the SERVER returned — it constructs no
	// content-origin url of its own, which is the entire point of the loop.
	"scripts/contentloop/": "the make-dev smoke posts to /api/v1/content and fetches the url the server returned, never one it built",
	// The off-appliance rasterizer's API client, which talks to that same
	// /api/v1/content route: it POSTs the PNG it rendered, and to READ bytes back
	// it GETs the listing and fetches the url the SERVER published for the ref
	// (Client.FetchAsset). It composes no content-origin url of its own — the
	// same shape, and the same reason, as the contentloop entry above.
	"internal/derive/client.go": "waiveo-derive posts to /api/v1/content and fetches the url the listing returned, never one it built",
}

// pathSegmentSpelled matches a string literal that carries `content` with a
// slash on at least one side: `"/content"`, `"content/"`, and the whole
// `"/content/"`. That is the grammar, or either half of it as a concatenation
// would leave it.
//
// The slash is what makes this usable rather than a swamp. The bare word
// `"content"` is all over this module and almost never a path — it is the
// `content` DISPLAY MODE (datamodel.Resolve, playerserver, schedulehost), a
// `content_type` value, an audit resource name, a "content-url.key" filename —
// so a rule that flagged it would flag a dozen files that have nothing to do
// with the origin, and a guard nobody can keep green is a guard that gets
// deleted. The one genuinely path-shaped use of the bare word — passing it to a
// joiner — is caught by pathJoiners below instead, where there is no ambiguity
// about what it means.
var pathSegmentSpelled = regexp.MustCompile(`(/content(/|$))|(^content/)`)

// spellsContentSegment reports whether lit is a path (or path fragment) carrying
// `content` as a slash-delimited segment.
//
// Two exclusions keep it usable, and both are about what a PATH looks like:
//
//   - a literal containing a SPACE is prose, not a path. Without this, an
//     ordinary explanatory string — "…carrying priority/display/content
//     unmodified onto an issued Lease…" — is an offence, and the guard becomes
//     something authors route around by rewording comments.
//   - the segment must be slash-DELIMITED, not merely slash-preceded. Every
//     import of this package is `…/feeder/contenturl`, and a rule that flagged
//     those would flag the fifteen files that correctly use it.
func spellsContentSegment(lit string) bool {
	if strings.ContainsRune(lit, ' ') {
		return false
	}
	return pathSegmentSpelled.MatchString(lit)
}

// pathJoiners are the calls that build a path from segments, where the segment
// `"content"` needs no slash of its own. Matched on the SELECTOR (`Join`,
// `JoinPath`, `Sprintf`) rather than the full expression, so `path.Join`,
// `filepath.Join`, `url.JoinPath`, a `*url.URL`'s method and a formatted path
// all land.
//
// `Sprintf` is qualified further (isPathJoin): only when its FORMAT carries a
// slash, so `fmt.Sprintf("%s/%s/%s", base, "content", hex)` is a path being
// built while `fmt.Sprintf("kind %s", "content")` is a message.
var pathJoiners = map[string]bool{"Join": true, "JoinPath": true, "Sprintf": true}

func TestEveryContentURLIsMintedByThisPackage(t *testing.T) {
	root := moduleRoot(t)

	scanned := 0
	owned := 0
	byTree := map[string]int{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := relPath(root, path)
		if d.IsDir() {
			if skipDir(rel, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Errorf("parse %s: %v", rel, parseErr)
			return nil
		}
		scanned++
		byTree[treeOf(rel)]++
		inOwner := strings.HasPrefix(rel, grammarOwner+"/")
		// An allowlisted path is exempt from the HEURISTICS only — the half
		// spellings and the joiner shapes, which are the rules that can mistake
		// another `content` for this one. It is NOT exempt from the whole
		// `/content/` grammar, which is unambiguous wherever it appears: an
		// exemption is a statement that a file talks about a different content
		// path, not a licence to build this one.
		_, exempt := allowedSpelling(rel)
		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.ImportSpec:
				// An import path is not a url. Skipped explicitly rather than
				// left to the segment rule, so the reason is stated once instead
				// of inferred from a regex.
				return false
			case *ast.BasicLit:
				lit, uerr := stringLit(v)
				if uerr != nil {
					return true
				}
				if strings.Contains(lit, PathPrefix) {
					if inOwner {
						owned++
						return true
					}
					reportSpelling(t, rel, fset.Position(v.Pos()), lit, "spells the whole content path")
					return true
				}
				if inOwner || exempt {
					return true
				}
				if spellsContentSegment(lit) {
					reportSpelling(t, rel, fset.Position(v.Pos()), lit,
						"spells `content` as a path segment — half of the grammar is still the grammar, and "+
							"`\"/content\" + \"/\"` assembles it")
				}
			case *ast.CallExpr:
				if inOwner || exempt || !isPathJoin(v) {
					return true
				}
				for _, arg := range v.Args {
					blit, ok := arg.(*ast.BasicLit)
					if !ok {
						continue
					}
					lit, uerr := stringLit(blit)
					if uerr != nil || lit != "content" {
						continue
					}
					reportSpelling(t, rel, fset.Position(blit.Pos()), lit,
						"joins a bare `content` segment onto a path — the same grammar, assembled without a slash")
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// Completeness. If PathPrefix were ever inlined away, or the scan pointed at
	// the wrong tree, every check above would match nothing and this test would
	// pass while asserting nothing at all.
	if owned == 0 {
		t.Errorf("the scan found the content path spelled NOWHERE, not even in %s — it is matching nothing and would "+
			"pass whatever the module does", grammarOwner)
	}
	// …and the walk really reached the module rather than one directory of it.
	// The previous version skipped by BARE DIRECTORY NAME, so `internal/app/web/`
	// would have been skipped as if it were the console; the count is what makes
	// a future narrowing of the walk visible instead of silent.
	if scanned < scanFloor {
		t.Errorf("the scan parsed only %d non-test .go files, fewer than the %d this module has had for a long time — "+
			"the walk is skipping part of the tree, and a guard that stops reaching a directory fails by going quiet. "+
			"If files were deliberately deleted, lower scanFloor in the same commit so the tripwire stays armed at the "+
			"new size rather than being left with slack a future narrowing can hide in",
			scanned, scanFloor)
	}
	// …and it reached each TREE, which is the failure a single total cannot
	// express. Skipping `internal/app` alone — the api producers HV-1 was found on
	// — leaves 263 files, which cleared the old floor of 200 with room to spare.
	for tree, min := range requiredTrees {
		if byTree[tree] < min {
			t.Errorf("the scan parsed %d non-test .go files under %s/, fewer than the %d expected there — the walk is "+
				"not reaching that tree, so every content-url producer inside it is invisible to this guard while the "+
				"guard goes on passing", byTree[tree], tree, min)
		}
	}
	// …and the REVERSE, which is what makes requiredTrees' own claim ("every tree
	// the walk reaches is listed") true rather than aspirational. The loop above
	// can only check trees somebody remembered to list; a tree added to the module
	// tomorrow is guarded by neither check, and the list goes stale in exactly the
	// silent way it was written to prevent. A new tree is a one-row edit here.
	for tree, n := range byTree {
		if _, listed := requiredTrees[tree]; !listed {
			t.Errorf("the scan parsed %d non-test .go files under %s/, a tree requiredTrees does not list — add a row "+
				"for it (a floor well under its actual size, like its neighbours) so this guard can notice that tree "+
				"going quiet later", n, tree)
		}
	}
	// The allowlist is a liability, not a feature: an entry naming a file that no
	// longer exists is an exemption nobody is watching.
	for prefix := range allowedSpellings {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(prefix))); err != nil {
			t.Errorf("allowedSpellings names %q, which is not in the tree (%v) — delete the entry rather than "+
				"leaving a standing exemption for a path nobody can check", prefix, err)
		}
	}
}

// reportSpelling files one offence, with the reason a hand-assembled content
// path is a defect rather than a style question.
func reportSpelling(t *testing.T, rel string, pos token.Position, lit, what string) {
	t.Helper()
	t.Errorf("%s %s (%q) at %s.\n"+
		"Every content URL a screen or a console fetches must be minted by %s — contenturl.Signer.Mint for a "+
		"producer, contenturl.PathPrefix for the route it is served at. A url assembled here is a url nobody "+
		"signed, and against an origin that enforces signatures (which every real deployment is, because "+
		"cmd/waiveo-feeder loads-or-creates the key unconditionally) it answers 403 to the screen while every "+
		"gate stays green. That is exactly how no image or video layer could display on any screen.\n"+
		"If this really is a different `content` — the app api's own route, say — add it to allowedSpellings with "+
		"the reason, where a reviewer can see it.",
		rel, what, lit, pos, grammarOwner)
}

// relPath is path relative to the module root, slash-separated. The root itself
// is "".
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	if rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// skipDir reports whether the walk should skip this directory.
//
// `web` and `player-v3` are skipped BY PATH, anchored at the module root, not by
// name: they are two specific non-Go trees at the top of this repo, and skipping
// every directory that happens to share their name would make a producer in, say,
// `internal/app/web/` invisible to the whole guard. `node_modules` and `testdata`
// are skipped by name anywhere, which is safe for a different reason — neither can
// contain Go source the toolchain compiles.
//
// Dot- and underscore-prefixed directories are skipped for the STRONGEST reason
// available: the go tool ignores them outright, so nothing inside one is ever
// compiled as part of this module. That is derived from the toolchain's own rule
// rather than listed, which matters because the list would otherwise need a new
// row every time some tool claims a dotted directory at the repo root. `.git` used
// to be that list's only entry; `.claude/worktrees/` is why the list was wrong.
//
// That worktrees case is worth naming, because the failure it produced looked
// alarming and was not: an agent working in an isolated git worktree checks out a
// NESTED COPY of this whole repo under `.claude/`, and the walk found every
// legitimate content-path producer a second time, reported at a path inside the
// copy. A guard that fires on files the compiler cannot see is a guard that cries
// wolf on every parallel track, and a guard people learn to ignore is worse than
// no guard — this one is load-bearing (it is the anti-regression fence for the
// defect where no image or video could display on any screen).
func skipDir(rel, name string) bool {
	if name == "" {
		return false
	}
	if c := name[0]; c == '.' || c == '_' {
		return true
	}
	switch name {
	case "node_modules", "testdata":
		return true
	}
	return rel == "web" || rel == "player-v3"
}

// allowedSpelling reports whether rel is exempt, and why.
func allowedSpelling(rel string) (string, bool) {
	for prefix, why := range allowedSpellings {
		if rel == prefix || strings.HasPrefix(rel, prefix) {
			return why, true
		}
	}
	return "", false
}

// stringLit unquotes a STRING BasicLit, or reports that the node is not one.
func stringLit(lit *ast.BasicLit) (string, error) {
	if lit.Kind != token.STRING {
		return "", errNotAString
	}
	return strconv.Unquote(lit.Value)
}

var errNotAString = errors.New("not a string literal")

// isPathJoin reports whether call is a path-joining call: `path.Join`,
// `filepath.Join`, `url.JoinPath`, a `*url.URL`'s `JoinPath` method, or a
// `Sprintf` whose format string spells a path (contains a slash).
func isPathJoin(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !pathJoiners[sel.Sel.Name] {
		return false
	}
	if sel.Sel.Name != "Sprintf" {
		return true
	}
	if len(call.Args) == 0 {
		return false
	}
	format, ok := call.Args[0].(*ast.BasicLit)
	if !ok {
		return false
	}
	lit, err := stringLit(format)
	return err == nil && strings.ContainsRune(lit, '/')
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
