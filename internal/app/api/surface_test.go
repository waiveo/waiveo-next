package api

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// surface_test.go is the drift check between the api/1 surface this package
// SERVES and the one api/openapi.yaml DECLARES.
//
// Nothing compared the two before, and they had drifted in both directions at
// once: operations declared with no handler behind them (a generated client got
// a method for each, and calling it 404d) and routes served with no declaration
// (unreachable from any generated client, unconstrained by any schema, invisible
// to anyone reading the document to learn what this API is). Neither kind of gap
// announces itself — a document is not executed, and a mux does not read.
//
// The check is cheap because both sides are enumerable exactly:
//
//   - the served side comes from the router (api.go), which records every
//     pattern in the same call that registers it, driven here by the SAME
//     mountAll the running server is built from — so a route added to the server
//     is a route added to this list, with nothing to remember;
//   - the declared side is the document's own `paths` block, read with a real
//     YAML parser rather than pattern-matched out of the text.
//
// It cannot pass vacuously. The comparison is two-way and both sides are
// asserted non-empty, so a broken enumeration is the loudest possible failure
// rather than a quiet agreement: an empty served set makes every declared
// operation unserved, and an empty declared set makes every served route
// undeclared.

// openAPIPath is the document this package's surface is checked against,
// relative to this package's directory.
const openAPIPath = "../../../api/openapi.yaml"

// httpMethods are the keys of a `paths` entry that name an OPERATION. Anything
// else at that level (`parameters`, `summary`, `description`, `servers`, a
// `$ref`, an `x-` extension) describes the path item rather than an operation
// on it, and is deliberately not part of the surface.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// TestDeclaredSurfaceMatchesServedSurface fails when an operation is declared
// with no handler, or a route is mounted with no declaration.
//
// A failure is fixed by changing whichever side is wrong — declaring the route,
// removing the declaration, or removing the route. There is deliberately no
// allowlist to add an exception to: an exception here is exactly the state this
// check exists to make impossible, and "declared but not yet implemented" is the
// shape the four operations that motivated it were in.
func TestDeclaredSurfaceMatchesServedSurface(t *testing.T) {
	served := servedSurface()
	declared := declaredSurface(t)

	// Non-emptiness is asserted before the diff, not because the diff would
	// otherwise pass — it would fail, loudly, in one direction — but so that a
	// broken enumeration reports itself as broken rather than as ~50 unrelated
	// drift lines the reader has to diagnose.
	if len(served) == 0 {
		t.Fatal("the served surface is empty: mountAll registered no routes, so this check proves nothing")
	}
	if len(declared) == 0 {
		t.Fatalf("the declared surface is empty: no operations were read out of %s, so this check proves nothing", openAPIPath)
	}

	for _, op := range sortedKeys(served) {
		if !declared[op] {
			t.Errorf("served with no declaration: %s is mounted by internal/app/api but no operation in %s declares it — "+
				"either declare it (it is reachable today, and every generated client is blind to it) or stop mounting it",
				op, openAPIPath)
		}
	}
	for _, op := range sortedKeys(declared) {
		if !served[op] {
			t.Errorf("declared with no handler: %s is declared in %s but internal/app/api mounts no route for it — "+
				"every generated client offers this operation and the server answers 404; either implement it or remove the declaration",
				op, openAPIPath)
		}
	}
}

// servedSurface enumerates the operations this package mounts, as normalized
// "METHOD /path" keys.
//
// It drives the real mountAll against a zero-valued server: registration reads
// none of the server's collaborators (a handler does, but no handler runs here),
// so no store, authenticator or device plane has to be stood up to learn what
// the surface IS. authHandlers is nil for the same reason — the four auth routes
// are registered with method values, which are only dereferenced when called.
func servedSurface() map[string]bool {
	srv := &server{families: map[string]resourceConfig{}}
	rt, rootRT := newRouter(), newRouter()
	srv.mountAll(rt, rootRT, nil)

	out := map[string]bool{}
	for _, pattern := range append(append([]string{}, rt.patterns...), rootRT.patterns...) {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			continue
		}
		// The document's `servers` entry is the /api/v1 mount point, so its
		// paths are written relative to it while a mux pattern is absolute.
		out[normalizeOperation(method, strings.TrimPrefix(path, apiPrefix))] = true
	}
	return out
}

// declaredSurface enumerates the operations api/openapi.yaml declares, as the
// same normalized keys.
func declaredSurface(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(openAPIPath))
	if err != nil {
		t.Fatalf("read %s: %v", openAPIPath, err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", openAPIPath, err)
	}
	out := map[string]bool{}
	for path, item := range doc.Paths {
		for key := range item {
			if httpMethods[strings.ToLower(key)] {
				out[normalizeOperation(key, path)] = true
			}
		}
	}
	return out
}

// normalizeOperation renders one operation as the key both sides are compared
// on: an uppercase method, a space, and the path with every template parameter
// collapsed to a bare `{}`.
//
// Collapsing the parameter NAMES is what makes the comparison about the surface
// rather than about spelling. The two sides name the same parameter differently
// on purpose — the document reads `/scope-nodes/{scope_node_id}` because a
// generated client's argument should say what it is, while the mux reads
// `/scope-nodes/{id}` because one generic handler serves five resource families
// — and neither is wrong. What must agree is the shape: the same method, the
// same literal segments, in the same places, with a parameter wherever the other
// has one. Go's multi-segment wildcard (`{path...}`) collapses to the same `{}`
// as the document's `{path}`, which is the closest OpenAPI can express it.
func normalizeOperation(method, path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			segments[i] = "{}"
		}
	}
	return strings.ToUpper(method) + " " + strings.Join(segments, "/")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
