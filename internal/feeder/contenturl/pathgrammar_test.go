package contenturl

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
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
// # What it does and does not prove
//
// It proves no file outside this package writes the literal. It does not prove
// nobody assembles the same path some other way — `"/cont" + "ent/"`,
// path.Join, a fmt verb — and a determined author can defeat it. That is
// acceptable: the failure this guards is a FORGOTTEN obligation, not a
// circumvented one, and someone assembling the grammar by hand under a different
// spelling has to work past a compiler error and this test's own name to do it.
//
// Test files are exempt. A test legitimately writes the unsigned form to assert
// against it (internal/app/api/content_test.go builds the expected url that way),
// and a test is not a producer: nothing it constructs is served to a screen.

// grammarOwner is the one package permitted to spell the path segment: the
// package that mints and verifies it.
const grammarOwner = "internal/feeder/contenturl"

func TestEveryContentURLIsMintedByThisPackage(t *testing.T) {
	root := moduleRoot(t)

	offenders := 0
	owned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip anything that is not this module's own Go source. web/ carries
			// node_modules; testdata and corpora carry fixtures nobody compiles.
			switch d.Name() {
			case ".git", "node_modules", "testdata", "web", "player-v3":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Errorf("parse %s: %v", rel, parseErr)
			return nil
		}
		inOwner := strings.HasPrefix(filepath.ToSlash(rel), grammarOwner+"/")
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || !strings.Contains(v, PathPrefix) {
				return true
			}
			if inOwner {
				owned++
				return true
			}
			offenders++
			t.Errorf("%s spells the content path %q at %s.\n"+
				"Every content URL a screen or a console fetches must be minted by %s — contenturl.Signer.Mint for a "+
				"producer, contenturl.PathPrefix for the route it is served at. A url assembled here is a url nobody "+
				"signed, and against an origin that enforces signatures (which every real deployment is, because "+
				"cmd/waiveo-feeder loads-or-creates the key unconditionally) it answers 403 to the screen while every "+
				"gate stays green. That is exactly how no image or video layer could display on any screen.",
				rel, PathPrefix, fset.Position(lit.Pos()), grammarOwner)
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
	if offenders == 0 && owned == 0 {
		t.Fatal("unreachable: guarded above")
	}
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
