package castbundle

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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
//
// # The correction of this round — read this before adding anything to a list
//
// The first version of this fence had three holes, and all three were the same
// hole: it ENUMERATED BY HAND the very sets whose hand-enumeration is the defect
// the round exists to close.
//
//   - It listed the five refusal sentinels in a `var` in this file. A SIXTH
//     sentinel added to castbundle.go's own `var` block was invisible to it —
//     which is precisely "a hand-copied subset of this package's refusals,
//     silently incomplete", the original defect, relocated into its own fence.
//   - It counted only `return`s with results, so a NAMED result plus a bare
//     `return` carried a value out invisibly. Combined with the point above,
//     a refusal could be written into Stream in the same shape as all the
//     others and the fence stayed green.
//   - It walked only Stream's OWN body, so moving a refusal one call deep into
//     a same-package helper — the most ordinary refactor there is — reopened
//     the zero-byte `.cast` with both packages still testing `ok`.
//
// And a FOURTH, found in the re-review of the fix for the third: the widened
// walk went one level and justified stopping there with "a helper that calls a
// helper has to pass through it". It does — but this fence asks what a body
// CONTAINS, not what it executes, and an intermediate that forwards contains
// nothing. `Stream -> checkEntries -> entryCountRule{refuse(…)}` was green. Two
// extract-method refactors in sequence is not an exotic shape, so the walk is
// now transitive; the `seen` map that bounds it was already there.
//
// So nothing below is enumerated that can be derived. The refusal sentinels are
// read out of this package's syntax tree, the error TYPES a refusal can be
// constructed as are found by their `Error() string` method, and the walk
// follows same-package calls out of Stream to any depth. The only names still
// written down are `errors.New` and `fmt.Errorf`, which are facts about the
// language rather than facts about this package: this package's own minter
// (`refuse`) is reached by the call walk, and would still be reached if it were
// renamed.

// stdlibErrorMinters are the two ways the standard library makes an error value.
// They are the ONLY hand-written names left in this fence, and they are safe to
// write down because they cannot change when this package does.
//
// This package's own minter is deliberately NOT listed. `refuse` builds a
// *Refusal, so the call walk below reaches it through Stream and flags the
// composite literal inside it — by shape, not by name — which keeps working when
// somebody renames it.
var stdlibErrorMinters = []string{"Errorf", "New"}

// TestNothingCanBeRefusedOnceTheBundleIsStreaming reads (*Plan).Stream's own
// syntax tree — and the tree of every same-package function it calls — and fails
// if the streaming half grows the ability to say no.
//
// Four checks, each failing for a different edit:
//
//   - ONE value-returning statement in Stream. A second one is how a refusal
//     gets added: the natural way to write `if len(x) > cap { return
//     fmt.Errorf(...) }` is an early return, and there is no early return here
//     to imitate. (The bare `return` inside the write closure carries no value
//     and cannot refuse anything, so it is not counted.)
//   - NO NAMED RESULT on Stream. A named result turns `return` into a
//     value-returning statement that the check above cannot see, which is half
//     of how a refusal was smuggled past the previous version of this fence.
//   - No refusal sentinel, anywhere Stream can reach, at any depth. A refusal
//     expressed by wrapping ErrTooLarge is the exact shape all six of NewPlan's
//     refusals have — and the sentinel set is DERIVED from castbundle.go, so a
//     seventh one added tomorrow is covered without editing this file.
//   - No error minted, anywhere Stream can reach, at any depth. fmt.Errorf,
//     errors.New and constructing one of this package's own error types are the
//     ways to make one; the single legal error here came out of the caller's
//     io.Writer.
//
// If a future change genuinely needs a refusal at stream time, this test is the
// conversation: there is no such thing, because the response header is already
// gone. The refusal belongs in NewPlan, which every calling side already runs.
func TestNothingCanBeRefusedOnceTheBundleIsStreaming(t *testing.T) {
	pkg := parseCastbundlePackage(t)
	sentinels := pkg.refusalSentinels(t)
	errorTypes := pkg.errorTypes()
	t.Logf("derived from the package: %d refusal sentinel(s) %v; %d error type(s) %v",
		len(sentinels), sentinels, len(errorTypes), errorTypes)

	fn := pkg.find(t, "Plan", "Stream")

	// A named result makes `return` carry a value. Rejected outright rather than
	// counted, because the shape has no use here — Stream accumulates one
	// `failure` and hands it back — and allowing it would mean this fence had to
	// reason about which bare returns are refusals.
	if named := namedResultsOf(fn); len(named) > 0 {
		t.Errorf("(*Plan).Stream declares the NAMED result(s) %v.\n"+
			"A named result turns every bare `return` into a value-returning statement, which is how a refusal gets out of here "+
			"without adding a return this fence can count. Declare the result unnamed and hand back the accumulated writer error.", named)
	}

	var returns []*ast.ReturnStmt
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if v, ok := n.(*ast.ReturnStmt); ok && len(v.Results) > 0 {
			// Only value-returning statements can carry a refusal out.
			returns = append(returns, v)
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
	if len(returns) == 1 {
		if _, ok := returns[0].Results[0].(*ast.Ident); !ok {
			t.Errorf("(*Plan).Stream's single return is not a plain identifier; it must hand back the accumulated writer error and nothing else")
		}
	}

	// The reachable set: Stream, plus every same-package function reachable from
	// it TRANSITIVELY. Depth 1 was not enough — `Stream -> checkEntries ->
	// entryCountRule{refuse(…)}` cleared it, because the intermediate forwards
	// and therefore carries no sentinel and mints nothing. See reachableFrom.
	reach := pkg.reachableFrom(t, fn, "(*Plan).Stream")
	for _, scope := range reach {
		found, minted := scope.refusalsIn(sentinels, errorTypes)
		if len(found) > 0 {
			t.Errorf("%s references the refusal sentinel(s) %v. Refusals belong in NewPlan, before any header is committed.%s",
				scope.what, found, scope.viaNote())
		}
		if len(minted) > 0 {
			t.Errorf("%s mints an error with %v. The only error the streaming half may return is the one its io.Writer gave it; "+
				"anything else is a refusal arriving after the response started.%s", scope.what, minted, scope.viaNote())
		}
	}
	if len(reach) == 1 {
		t.Logf("(*Plan).Stream calls no same-package function; the transitive half of this fence is vacuous today, which is the intended shape")
	}
}

// TestNewPlanStillHoldsTheRefusals is the other half of the same fence, and it
// is here because the first one is satisfiable by deleting every refusal in the
// package. It asserts NewPlan is where they live.
func TestNewPlanStillHoldsTheRefusals(t *testing.T) {
	fn := parseCastbundlePackage(t).find(t, "", "NewPlan")
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

// ---- the package's own syntax tree ------------------------------------------

// castbundlePackage is every non-test source file of this package, parsed once,
// with its top-level functions indexed.
//
// The WHOLE package rather than one file, because both holes this fence had to
// close are about things a single file's declarations cannot answer: which
// sentinels exist (any file may declare one) and where a call goes (any file may
// define the callee).
type castbundlePackage struct {
	files []*ast.File
	// funcs is keyed by funcKey — "" receiver for a plain function, "Type.Name"
	// for a method — so a call to `refuse()` and a call to `p.something()`
	// resolve through the same map.
	funcs map[string]*ast.FuncDecl
}

func funcKey(receiverType, name string) string {
	if receiverType == "" {
		return name
	}
	return receiverType + "." + name
}

// parseCastbundlePackage parses this package's non-test files. The test binary
// runs in the package directory, so "." is that directory.
func parseCastbundlePackage(t *testing.T) *castbundlePackage {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v — if this package moved, MOVE THIS FENCE, do not delete it", err)
	}
	pkg := &castbundlePackage{funcs: map[string]*ast.FuncDecl{}}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v — if this file moved, MOVE THIS FENCE, do not delete it", name, err)
		}
		pkg.files = append(pkg.files, file)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			recv := ""
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				recv = recvTypeName(fn.Recv.List[0].Type)
			}
			pkg.funcs[funcKey(recv, fn.Name.Name)] = fn
		}
	}
	if len(pkg.files) == 0 {
		t.Fatal("no non-test source files found in the package directory; the fence around the streaming half can no longer be applied")
	}
	return pkg
}

// find returns the named function or method, failing with an instruction rather
// than a nil pointer.
func (p *castbundlePackage) find(t *testing.T, receiverType, name string) *ast.FuncDecl {
	t.Helper()
	fn, ok := p.funcs[funcKey(receiverType, name)]
	if !ok {
		t.Fatalf("no %s in this package; the fence around the streaming half can no longer be applied", funcKey(receiverType, name))
	}
	return fn
}

// refusalSentinels is the set of package-level error values this package refuses
// with, READ OUT OF THE PACKAGE rather than listed here.
//
// A package-level `var` whose initialiser mints an error is a refusal sentinel:
// that is the shape all six of castbundle.go's have, and it is the shape a
// seventh will have. Deriving it is the point — the previous version of this
// fence carried the five names of the day in a slice, so a sentinel added to
// castbundle.go's `var` block was one this test had never heard of, which is the
// hand-maintained-subset defect the whole round exists to close.
//
// It FAILS on an empty result. A derivation that silently finds nothing is a
// fence that passes for the wrong reason, and this package is known to have at
// least six (TestNewPlanStillHoldsTheRefusals will not let it have fewer).
func (p *castbundlePackage) refusalSentinels(t *testing.T) []string {
	t.Helper()
	var out []string
	types := p.errorTypes()
	for _, file := range p.files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i < len(vs.Values) && mintsAnError(vs.Values[i], types) {
						out = append(out, name.Name)
					}
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no refusal sentinels could be derived from this package's syntax tree.\n" +
			"This package refuses with at least six of them, so finding none means the derivation has stopped matching how they " +
			"are declared — and a fence whose input set is empty passes everything. Fix the derivation; do not go back to listing " +
			"the names, which is the defect this round closed.")
	}
	return out
}

// errorTypes is every type this package declares that has an `Error() string`
// method — i.e. every type a refusal can be CONSTRUCTED as.
//
// Derived rather than listed for the same reason the sentinels are. It is what
// lets this fence see `refuse` as a minter without knowing its name: `refuse`
// returns a &Refusal{…}, Refusal has an Error method, so building one is minting
// an error wherever it happens.
func (p *castbundlePackage) errorTypes() []string {
	var out []string
	for _, file := range p.files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "Error" || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			if fn.Type.Params != nil && len(fn.Type.Params.List) != 0 {
				continue
			}
			if name := recvTypeName(fn.Recv.List[0].Type); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// scope is one function body the fence has to be satisfied about, plus the whole
// call chain it was reached by, so a failure names the refactor that introduced
// it rather than just the body it landed in.
type scope struct {
	what string
	via  string // "" for Stream itself; otherwise the full chain from it
	body *ast.BlockStmt
}

func (s scope) viaNote() string {
	if s.via == "" {
		return ""
	}
	return "\nIt is reached as " + s.via + ", which is the same thing at any depth: a refusal N calls deep still fires after the " +
		"response header is committed. Extracting a check into a helper — or into a helper's helper — does not move it to a phase " +
		"where it can be reported."
}

// refusalsIn reports the refusal sentinels this scope references and the ways it
// mints a new error.
func (s scope) refusalsIn(sentinels, errorTypes []string) (found, minted []string) {
	ast.Inspect(s.body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Ident:
			for _, name := range sentinels {
				if v.Name == name {
					found = append(found, name)
				}
			}
		case *ast.CallExpr, *ast.CompositeLit:
			if m := mintDescription(v.(ast.Expr), errorTypes); m != "" {
				minted = append(minted, m)
			}
		}
		return true
	})
	return found, minted
}

// reachableFrom is fn's own body plus the body of every same-package function
// reachable from it, TRANSITIVELY.
//
// It walked one level, and the reason given for stopping there was wrong: "a
// helper that calls a helper has to pass through it" is true of control flow and
// says nothing about this fence, which asks what each body CONTAINS. An
// intermediate that merely forwards — `Stream` → `checkEntries` →
// `entryCountRule{refuse(…)}` — carries no sentinel and mints nothing, so at
// depth 1 the fence was green on a package that refuses mid-stream. Two ordinary
// extract-method refactors, applied one after the other, defeated it.
//
// So the walk is a worklist over FuncDecls rather than one pass over one body.
// Each callee is resolved against ITS OWN locals and receiver name — a helper's
// parameter called `refuse` must shadow the package function for that body and
// not for its caller's — which is the whole reason this cannot be a recursive
// Inspect over a shared closure.
//
// Two call shapes resolve, and they are the two a refactor produces: a bare
// `helper()` naming a package function, and `p.helper()` naming a method on the
// receiver's own type. Names bound INSIDE a body are excluded, so the `write :=
// func (…)` closure in Stream is not mistaken for a package function that
// happens to share its name.
//
// Termination is `seen`, keyed by funcKey, so a cycle (or a diamond) is visited
// once. `via` records the whole chain rather than the immediate caller, because
// at depth 2+ "reached from checkEntries" does not tell anyone which edge to
// look at.
func (p *castbundlePackage) reachableFrom(t *testing.T, fn *ast.FuncDecl, what string) []scope {
	t.Helper()

	type work struct {
		fn    *ast.FuncDecl
		chain string // how it was reached, "" for the root
	}
	out := []scope{{what: what, body: fn.Body}}
	seen := map[string]bool{}
	queue := []work{{fn: fn, chain: ""}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		here := what
		if cur.chain != "" {
			here = cur.chain
		}
		local := localNamesOf(cur.fn)
		receiver := ""
		if cur.fn.Recv != nil && len(cur.fn.Recv.List) == 1 && len(cur.fn.Recv.List[0].Names) == 1 {
			receiver = cur.fn.Recv.List[0].Names[0].Name
		}
		receiverType := ""
		if cur.fn.Recv != nil && len(cur.fn.Recv.List) == 1 {
			receiverType = recvTypeName(cur.fn.Recv.List[0].Type)
		}

		ast.Inspect(cur.fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var key string
			switch f := call.Fun.(type) {
			case *ast.Ident:
				if local[f.Name] {
					return true
				}
				key = funcKey("", f.Name)
			case *ast.SelectorExpr:
				x, ok := f.X.(*ast.Ident)
				if !ok || receiver == "" || x.Name != receiver {
					return true
				}
				key = funcKey(receiverType, f.Sel.Name)
			default:
				return true
			}
			callee, ok := p.funcs[key]
			if !ok || callee.Body == nil || seen[key] {
				return true
			}
			seen[key] = true
			chain := here + " -> " + key
			out = append(out, scope{what: key, via: chain, body: callee.Body})
			queue = append(queue, work{fn: callee, chain: chain})
			return true
		})
	}
	return out
}

// localNamesOf is every identifier fn binds itself — parameters, results,
// receiver, `:=` targets, `var`/`const` declarations and range variables — so a
// call through one of them is never resolved against the package's functions.
func localNamesOf(fn *ast.FuncDecl) map[string]bool {
	local := map[string]bool{}
	addFields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				local[n.Name] = true
			}
		}
	}
	addFields(fn.Recv)
	addFields(fn.Type.Params)
	addFields(fn.Type.Results)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			if v.Tok == token.DEFINE {
				for _, lhs := range v.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						local[id.Name] = true
					}
				}
			}
		case *ast.RangeStmt:
			if v.Tok == token.DEFINE {
				for _, e := range []ast.Expr{v.Key, v.Value} {
					if id, ok := e.(*ast.Ident); ok {
						local[id.Name] = true
					}
				}
			}
		case *ast.GenDecl:
			for _, spec := range v.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, n := range vs.Names {
						local[n.Name] = true
					}
				}
			}
		case *ast.FuncLit:
			addFields(v.Type.Params)
			addFields(v.Type.Results)
		}
		return true
	})
	return local
}

// mintsAnError reports whether expr makes a NEW error value.
func mintsAnError(expr ast.Expr, errorTypes []string) bool {
	return mintDescription(expr, errorTypes) != ""
}

// mintDescription names how expr mints an error, or "" if it does not.
//
// Two shapes: a call to one of the standard library's error constructors, and a
// composite literal of a type this package gave an Error method to. The second
// is what makes `refuse` visible without naming it.
func mintDescription(expr ast.Expr, errorTypes []string) string {
	switch v := expr.(type) {
	case *ast.CallExpr:
		name := calleeName(v.Fun)
		for _, m := range stdlibErrorMinters {
			if name == m {
				return name
			}
		}
	case *ast.CompositeLit:
		lit := v.Type
		if star, ok := lit.(*ast.StarExpr); ok {
			lit = star.X
		}
		if id, ok := lit.(*ast.Ident); ok {
			for _, tn := range errorTypes {
				if id.Name == tn {
					return "a " + tn + " literal"
				}
			}
		}
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			return mintDescription(v.X, errorTypes)
		}
	}
	return ""
}

// namedResultsOf is the names Stream (or any function) gives its results.
func namedResultsOf(fn *ast.FuncDecl) []string {
	var out []string
	if fn.Type.Results == nil {
		return nil
	}
	for _, f := range fn.Type.Results.List {
		for _, n := range f.Names {
			out = append(out, n.Name)
		}
	}
	return out
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
