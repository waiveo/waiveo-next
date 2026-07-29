package eventingest

// error_registry_test.go pins the property that makes this door's refusals
// MACHINE-READABLE: every code it can put in front of a caller is a value some
// contract's closed registry publishes. api/1 API-011 states it directly — "A
// server MUST NOT emit a `code` value outside the registry" — and this route
// answers in api/1's Problem shape by its own declaration (API-010).
//
// It is the same oracle internal/app/eventsse/error_registry_test.go applies to
// the watch door, pointed at the intake door, and for the same reason: this
// package used to answer a wrong method with METHOD_NOT_ALLOWED, a code no
// registry anywhere in this repo publishes. Checking only that one literal would
// close the instance; reading every code-shaped literal in the package's source
// closes the class, including a code invented later on a path no test drives.
//
// The registries are parsed from the CONTRACT FILES, never listed here. A list
// written down in a test agrees with whatever the code does; reading the
// contract is what makes this an oracle.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
)

// contractRegistry reads one contract's `## Error taxonomy` table: the first
// backticked cell of each row is a published code.
func contractRegistry(t *testing.T, contract string) map[string]bool {
	t.Helper()
	_, self, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(self), "..", "..", "..", "contracts", contract)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", contract, err)
	}
	codes := map[string]bool{}
	inSection := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "## ") {
			inSection = line == "## Error taxonomy"
			continue
		}
		if !inSection || !strings.HasPrefix(line, "| `") {
			continue
		}
		cell := strings.TrimPrefix(line, "| `")
		if i := strings.Index(cell, "`"); i > 0 {
			codes[cell[:i]] = true
		}
	}
	return codes
}

// publishedCodes is the set this route may draw from: api/1's registry (the
// Problem shape it answers in), events/1's (the log it writes into — AUTH_REQUIRED
// is EVT-113's own code for exactly this refusal), and relay/1's (the message it
// is a transport for). All three are PUBLISHED tables a client can switch over;
// the union is reuse-by-name, not permission to mint.
func publishedCodes(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	// api/1 and events/1 ONLY — deliberately not relay/1. relay/1's registry is
	// the vocabulary of its own wire frames (REL-007), a channel with no HTTP
	// methods; unioning it here would admit every relay frame code as a legal
	// answer on this HTTP door, which is the opposite of what this oracle is
	// for. (Confirmed: with relay/1 in the union, answering a 405 with
	// CHANNEL_BINDING_INVALID passed.) The sibling oracle in internal/app/eventsse
	// unions exactly these two.
	for _, c := range []string{"api-1.md", "events-1.md"} {
		for code := range contractRegistry(t, c) {
			out[code] = c
		}
	}
	// Both directions, so a parser reading the wrong thing cannot pass.
	// Both directions, so a parser reading the wrong thing cannot pass.
	if out["AUTH_REQUIRED"] != "events-1.md" || out["METHOD_NOT_ALLOWED"] != "api-1.md" {
		t.Fatalf("the Error taxonomy parser did not read the registries correctly: AUTH_REQUIRED=%q METHOD_NOT_ALLOWED=%q",
			out["AUTH_REQUIRED"], out["METHOD_NOT_ALLOWED"])
	}
	// A relay/1-only code must NOT resolve here: this door answers in api/1's
	// vocabulary, and admitting relay frame codes would make the oracle green for
	// answers no HTTP client can interpret.
	if out["MALFORMED_MESSAGE"] != "" {
		t.Fatalf("a relay/1 wire-frame code resolved as publishable on this HTTP door (from %q) — the union is too wide", out["MALFORMED_MESSAGE"])
	}
	// A genuinely invented code must not resolve, or the parser is matching too
	// much and the whole oracle is decorative.
	if out["NOT_A_REAL_PUBLISHED_CODE"] != "" {
		t.Fatal("the Error taxonomy parser reported an invented code as published; it is matching too much")
	}
	return out
}

// codeShapedLiteral matches the shape every published error code has: uppercase
// ASCII words joined by underscores. It is deliberately an OVER-approximation —
// it matches candidates rather than known codes, which is what lets it catch an
// invented code on a path nobody drove.
var codeShapedLiteral = regexp.MustCompile(`^[A-Z][A-Z0-9]{3,}(_[A-Z0-9]+)*$`)

// TestNoUnpublishedErrorCodeLiteralInThisPackage enumerates every code-shaped
// string literal in the package's own source and requires each to be published.
func TestNoUnpublishedErrorCodeLiteralInThisPackage(t *testing.T) {
	published := publishedCodes(t)

	found := map[string][]string{} // code -> the files it appears in
	_, self, _, _ := runtime.Caller(0)
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, filepath.Dir(self), func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse the package source: %v", err)
	}
	pkg, ok := pkgs["eventingest"]
	if !ok {
		t.Fatalf("package eventingest was not found beside %s", self)
	}
	for file, f := range pkg.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, isLit := n.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil || !codeShapedLiteral.MatchString(value) {
				return true
			}
			found[value] = append(found[value], filepath.Base(file))
			return true
		})
	}

	if len(found) == 0 {
		t.Fatal("no code-shaped literal was found anywhere in the package; this check is not reading the source")
	}
	for code, files := range found {
		if _, ok := published[code]; !ok {
			t.Errorf("%s mints the error code %q, which no contract's error registry publishes (api/1 API-011)",
				strings.Join(files, ", "), code)
		}
	}
}

// TestEveryRefusalCodeIsPublished drives the refusals this door can actually
// answer with and checks the code it EMITTED — read off the response's own
// Problem-Code header — against the published registries. It is the half of the
// oracle that catches a code arriving through a variable, which no source scan
// sees.
func TestEveryRefusalCodeIsPublished(t *testing.T) {
	published := publishedCodes(t)

	// An ingest that authorizes nobody: every authenticated caller is refused,
	// which is one of the two refusal paths. The other two need no wiring.
	handler := New(nil, siteScope, seqIDs(), testWallMs, func(string, string) bool { return false })

	cases := []struct {
		name    string
		request func(t *testing.T) *http.Request
	}{
		{"a method this door does not serve", func(*testing.T) *http.Request {
			return httptest.NewRequest(http.MethodGet, "/telemetry/v1/push", nil)
		}},
		{"no verified client certificate", func(*testing.T) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/telemetry/v1/push", strings.NewReader(`{}`))
		}},
		{"a relay identity this feeder never enrolled", func(t *testing.T) *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/telemetry/v1/push", strings.NewReader(`{}`))
			testRelay().Present(r)
			return r
		}},
	}

	seen := map[string]bool{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, c.request(t))

			if rec.Code < 400 {
				t.Fatalf("expected a refusal; got %d body=%s", rec.Code, rec.Body.String())
			}
			code := rec.Header().Get(apihttp.ProblemCodeHeader)
			if code == "" {
				t.Fatalf("a refusal must carry a machine-readable code (API-010); got status %d body=%s", rec.Code, rec.Body.String())
			}
			if registry, ok := published[code]; !ok {
				t.Fatalf("this door answered with code %q, which NO contract's error registry publishes — "+
					"a client's error handling is a switch over a published table, so an unminted code is an untyped refusal (API-011)", code)
			} else {
				t.Logf("%s → %d %s (%s)", c.name, rec.Code, code, registry)
			}
			seen[code] = true
		})
	}

	// A guard against the table degrading to one code for every condition: these
	// are genuinely distinct refusals and must stay distinguishable.
	if len(seen) < 3 {
		t.Fatalf("the driven refusals produced only %d distinct codes (%v); they are not exercising distinct conditions", len(seen), seen)
	}
}
