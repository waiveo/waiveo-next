package castbundle

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// streaminghalf_test.go holds the fence around the two-phase producer API, and
// it is a SYNTAX-TREE test on purpose.
//
// The defect it exists for is not "these four refusals were in the wrong place".
// It is that a caller which had already committed an HTTP 200 hand-copied a
// SUBSET of this package's refusals into its own pre-flight — one of five — and
// nothing could tell anyone the subset was incomplete, then or ever. Splitting
// the producer into NewPlan (every refusal) and Stream (no refusals) makes the
// subset uncopyable, but only for as long as Stream stays unable to refuse.
//
// A behavioural test cannot check that: it can only prove the refusals that
// exist today do not fire late. The property that has to hold is about refusals
// that do not exist yet, so the test reads the code.

// refusalSentinels are the error values this package refuses with. None of them
// may be reachable from the streaming half.
var refusalSentinels = []string{"ErrTooLarge", "ErrNotABundle", "ErrDamaged", "ErrAssetMismatch", "ErrIncomplete"}

// errorMinters are the ways new error values get made. The streaming half may
// PROPAGATE an error out of the writer it was handed; it may not invent one,
// because an invented error at that point is a refusal nobody can act on.
var errorMinters = []string{"Errorf", "New", "refuse"}

// TestNothingCanBeRefusedOnceTheBundleIsStreaming reads (*Plan).Stream's own
// syntax tree and fails if it grows the ability to say no.
//
// Three checks, each failing for a different edit:
//
//   - ONE value-returning statement. A second one is how a refusal gets added:
//     the natural way to write `if len(x) > cap { return fmt.Errorf(...) }` is
//     an early return, and there is no early return here to imitate. (The bare
//     `return` inside the write closure carries no value and cannot refuse
//     anything, so it is not counted.)
//   - No refusal sentinel. A refusal expressed by wrapping ErrTooLarge is the
//     exact shape all five of NewPlan's refusals have.
//   - No error minted. fmt.Errorf, errors.New and this package's own refuse()
//     are the only ways to make one; the single legal error here came out of
//     the caller's io.Writer.
//
// If a future change genuinely needs a refusal at stream time, this test is the
// conversation: there is no such thing, because the response header is already
// gone. The refusal belongs in NewPlan, which every calling side already runs.
func TestNothingCanBeRefusedOnceTheBundleIsStreaming(t *testing.T) {
	fn := findMethod(t, "internal/castbundle/castbundle.go", "Plan", "Stream")

	var returns []*ast.ReturnStmt
	var sentinels []string
	var minted []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ReturnStmt:
			// Only value-returning statements can carry a refusal out.
			if len(v.Results) > 0 {
				returns = append(returns, v)
			}
		case *ast.Ident:
			for _, s := range refusalSentinels {
				if v.Name == s {
					sentinels = append(sentinels, s)
				}
			}
		case *ast.CallExpr:
			if name := calleeName(v.Fun); name != "" {
				for _, m := range errorMinters {
					if name == m {
						minted = append(minted, name)
					}
				}
			}
		}
		return true
	})

	if len(returns) != 1 {
		t.Errorf("(*Plan).Stream has %d value-returning statements, want exactly 1.\n"+
			"A second return is how a refusal is added to the streaming half — and a refusal there reaches an operator "+
			"as a truncated zip, which their box calls 'this bundle is damaged' and blames on the wrong machine. "+
			"Put it in NewPlan: every calling side already runs that, and the export route turns its error into a Problem document.",
			len(returns))
	}
	if len(sentinels) > 0 {
		t.Errorf("(*Plan).Stream references the refusal sentinel(s) %v. Refusals belong in NewPlan, before any header is committed.", sentinels)
	}
	if len(minted) > 0 {
		t.Errorf("(*Plan).Stream mints an error with %v. The only error this half may return is the one its io.Writer gave it; "+
			"anything else is a refusal arriving after the response started.", minted)
	}
	if len(returns) == 1 {
		if _, ok := returns[0].Results[0].(*ast.Ident); !ok {
			t.Errorf("(*Plan).Stream's single return is not a plain identifier; it must hand back the accumulated writer error and nothing else")
		}
	}
}

// TestNewPlanStillHoldsTheRefusals is the other half of the same fence, and it
// is here because the first one is satisfiable by deleting every refusal in the
// package. It asserts NewPlan is where they live.
func TestNewPlanStillHoldsTheRefusals(t *testing.T) {
	fn := findMethod(t, "internal/castbundle/castbundle.go", "", "NewPlan")
	refusals := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if ok && calleeName(call.Fun) == "refuse" {
			refusals++
		}
		return true
	})
	// Six today: asset count, missing bytes, per-asset size, asset total,
	// manifest size, overhead reserve. Checked as a FLOOR rather than an
	// equality, so ADDING a refusal is not a test edit and removing one is.
	if refusals < 6 {
		t.Fatalf("NewPlan makes %d refusals; it made 6 when this fence was written. A refusal that left NewPlan either went to "+
			"the streaming half (where it cannot be reported) or vanished (where it cannot be enforced).", refusals)
	}
}

// TestEveryPlanNewPlanAcceptsCanBeWritten is the behavioural companion: for
// inputs at each refusal's boundary — the largest thing NewPlan still says yes
// to — Stream into an infallible writer must not fail. A plan that cannot be
// written is a refusal that arrived one phase too late.
func TestEveryPlanNewPlanAcceptsCanBeWritten(t *testing.T) {
	maximalCast := manyImageCast(MaxAssets)
	big := make([]byte, MaxAssetBytes)
	big[0], big[len(big)-1] = 0xE1, 0xE1
	bigCast := CastPayload{Name: "One full-size image", Slides: []datamodel.CastSlide{{ID: "s1",
		Layers: []wire.Layer{{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: AssetRefOf(big)}}}}}

	cases := []struct {
		name   string
		cast   CastPayload
		assets map[string][]byte
	}{
		{"the ordinary fixture", twoImageCast(), fixtureAssets()},
		{"exactly MaxAssets images", maximalCast, tinyAssetsFor(MaxAssets)},
		{"one image at exactly MaxAssetBytes", bigCast, map[string][]byte{AssetRefOf(big): big}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := NewPlan(Manifest{Cast: tc.cast}, tc.assets)
			if err != nil {
				t.Fatalf("NewPlan refused a case that is inside every limit: %v", err)
			}
			var buf bytes.Buffer
			if err := plan.Stream(&buf); err != nil {
				t.Fatalf("Stream failed on an accepted plan: %v — a refusal that fires here fires after the 200 is on the wire", err)
			}
			if buf.Len() == 0 {
				t.Fatal("Stream produced no bytes and no error, which is the zero-byte .cast an operator saves and cannot use")
			}
		})
	}
}

// manyImageCast builds a cast referencing n distinct tiny images, one layer
// each — the shape that proved the finding through the real HTTP surface.
func manyImageCast(n int) CastPayload {
	slide := datamodel.CastSlide{ID: "s1"}
	for i := 0; i < n; i++ {
		slide.Layers = append(slide.Layers, wire.Layer{
			Kind: wire.LayerKindImage, X: 0, Y: 0, W: 16, H: 16, AssetRef: AssetRefOf(tinyAsset(i)),
		})
	}
	return CastPayload{Name: "Many images", Slides: []datamodel.CastSlide{slide}}
}

func tinyAssetsFor(n int) map[string][]byte {
	out := make(map[string][]byte, n)
	for i := 0; i < n; i++ {
		b := tinyAsset(i)
		out[AssetRefOf(b)] = b
	}
	return out
}

// tinyAsset is the i-th distinct three-byte "image".
func tinyAsset(i int) []byte {
	return []byte{byte(i), byte(i >> 8), byte(i >> 16)}
}

// findMethod parses a file in this repository and returns the named function or
// method declaration, failing with an instruction rather than a nil pointer.
func findMethod(t *testing.T, relPath, receiverType, name string) *ast.FuncDecl {
	t.Helper()
	// The test binary runs in the package directory; the path is stated relative
	// to the module root so the instruction below names something a human can
	// open.
	path := relPath[strings.LastIndex(relPath, "/")+1:]
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v — if this file moved, MOVE THIS FENCE, do not delete it", relPath, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name {
			continue
		}
		if receiverType == "" {
			if fn.Recv == nil {
				return fn
			}
			continue
		}
		if fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		if recvTypeName(fn.Recv.List[0].Type) == receiverType {
			return fn
		}
	}
	t.Fatalf("no %s in %s; the fence around the streaming half can no longer be applied", name, relPath)
	return nil
}

func recvTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// calleeName is the bare function name of a call, for both `f()` and `pkg.f()`.
func calleeName(fun ast.Expr) string {
	switch v := fun.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}
