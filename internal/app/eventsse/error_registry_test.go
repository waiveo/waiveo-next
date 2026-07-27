package eventsse

import (
	"context"
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

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
)

// error_registry_test.go pins the property that makes an events/1 error
// MACHINE-READABLE: every code this surface can put in front of a client is a
// value some contract's closed registry publishes. api/1 API-011 states it
// directly — "A server MUST NOT emit a code value outside the registry" — and
// EVT-096 states the WS-close half of it.
//
// A code minted here and published nowhere is not a smaller version of the same
// thing: a client's error handling is a switch over a published table, so an
// unpublished value is indistinguishable from a bug, and it silently converts a
// typed refusal into an untyped one. This surface used to answer a WS upgrade
// with exactly that.
//
// The property is checked THREE ways, because each alone has a blind spot:
//
//  1. Driven (TestEveryRefusalCodeIsPublished): every refusal path is exercised
//     through the real handler and the code it actually emitted is read off the
//     response. This catches codes that arrive through a variable — a selector
//     parse error's own code, a resume rejection's — which no source scan sees.
//  2. Enumerated (TestNoUnpublishedErrorCodeLiteralInThisPackage): every
//     code-shaped string literal anywhere in the package's source is checked,
//     whether or not a test happens to reach it. This is the check that catches
//     a NEW invented code on a path nobody drove yet.
//  3. WS closes (TestEveryWSCloseNamesAPublishedCode): the taxonomy codes this
//     package can name in a close frame, read out of the source rather than
//     listed by hand.
//
// The registries themselves are parsed from the CONTRACT FILES. Writing the
// published codes down in this test would make it agree with whatever the code
// does; reading them from the contract is what makes it an oracle.

// contractRegistry reads one contract's Error taxonomy table: the first
// backticked cell of each row in the `## Error taxonomy` section is a published
// code.
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

// publishedCodes is the set an events/1 response may draw from: events/1's own
// registry, plus api/1's for the conditions events/1 names no code for.
//
// The second half is not a loophole — it is the reuse-by-name discipline
// player/1's PLY-007 defines and api/1's own API-013 applies, and this binding
// already depends on it for FORBIDDEN (auth.EventsCodes has no events/1
// authorization code to use). What it is NOT is permission to mint: both halves
// are PUBLISHED registries a client can switch over.
func publishedCodes(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for code := range contractRegistry(t, "api-1.md") {
		out[code] = "api/1"
	}
	for code := range contractRegistry(t, "events-1.md") {
		out[code] = "events/1" // events/1's own registry wins the attribution
	}
	// Both directions, so a parser reading the wrong thing cannot pass.
	if out["AUTH_REQUIRED"] != "events/1" || out["VALIDATION_FAILED"] != "api/1" {
		t.Fatalf("the Error taxonomy parser did not read the two registries correctly: AUTH_REQUIRED=%q VALIDATION_FAILED=%q",
			out["AUTH_REQUIRED"], out["VALIDATION_FAILED"])
	}
	if out["WS_BINDING_DEFERRED"] != "" {
		t.Fatal("the Error taxonomy parser reported an unpublished code as published; it is matching too much")
	}
	return out
}

// TestEveryRefusalCodeIsPublished drives every refusal this surface can answer
// with and checks the code it ACTUALLY emitted — read off the response's own
// Problem-Code header, which apihttp writes from the same value the body
// carries — against the published registries.
func TestEveryRefusalCodeIsPublished(t *testing.T) {
	published := publishedCodes(t)

	// A principal holding no role binding anywhere: authenticated, authorized
	// for nothing (SEC-005). It is the only way to reach the FORBIDDEN branch,
	// which the shared authenticator answers for both bindings.
	unbound, err := authtest.New(authtest.Config{})
	if err != nil {
		t.Fatalf("authtest.New: %v", err)
	}
	defer unbound.Close()
	p, err := unbound.Store.CreatePrincipal(t.Context(), auth.KindUser, "unbound")
	if err != nil {
		t.Fatalf("create an unbound principal: %v", err)
	}
	unboundSession, err := unbound.Store.MintSession(t.Context(), p.PrincipalID, auth.TokenKindSession, "",
		auth.AALStandard, map[string]string{"user_agent": "test", "ip_class": auth.IPClassLoopback})
	if err != nil {
		t.Fatalf("mint a session for the unbound principal: %v", err)
	}

	// A handler whose scope-tree read FAILS, for the one server-side refusal
	// that cannot be provoked from the request alone.
	brokenTree := New(NewHub(events.NewEventLog(0)), testAuth().Auth,
		func(context.Context) ([]datamodel.ScopeNode, error) { return nil, os.ErrClosed })

	ok := newTestServer(NewHub(events.NewEventLog(0)))

	cases := []struct {
		name    string
		handler http.Handler
		request func() *http.Request
	}{
		{"no credential at all", ok, func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/events/v1", nil)
			r.Header.Set("Accept", "text/event-stream")
			return r
		}},
		{"a credential in the query string", ok, func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/events/v1?token="+testAuth().Token, nil)
			r.Header.Set("Accept", "text/event-stream")
			return r
		}},
		{"a principal with no role binding", New(NewHub(events.NewEventLog(0)), unbound.Auth, emptyScopeTree), func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/events/v1", nil)
			r.Header.Set("Accept", "text/event-stream")
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: unboundSession.Token})
			return r
		}},
		{"a method other than GET", ok, func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/events/v1", nil)
			r.Header.Set("Accept", "text/event-stream")
			testAuth().Authorize(r)
			return r
		}},
		{"a request selecting neither binding", ok, func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/events/v1", nil)
			r.Header.Set("Accept", "application/json")
			testAuth().Authorize(r)
			return r
		}},
		{"a WS upgrade offering no events/1 subprotocol", ok, func() *http.Request {
			r := upgradeRequest("/events/v1")
			testAuth().Authorize(r)
			return r
		}},
		{"a selector that does not parse", ok, func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/events/v1?selector=kind+%3D+%3D+screen", nil)
			r.Header.Set("Accept", "text/event-stream")
			testAuth().Authorize(r)
			return r
		}},
		{"a resume_from that is malformed", ok, func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/events/v1?resume_from=not_a_ulid", nil)
			r.Header.Set("Accept", "text/event-stream")
			testAuth().Authorize(r)
			return r
		}},
		{"an unresolvable visible set", brokenTree, func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/events/v1", nil)
			r.Header.Set("Accept", "text/event-stream")
			testAuth().Authorize(r)
			return r
		}},
	}

	seen := map[string]bool{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			// The refusals under test never open a stream, so a plain recorder
			// is enough; a case that DID open one would hang here rather than
			// pass, which is its own useful signal.
			c.handler.ServeHTTP(rec, c.request())

			if rec.Code < 400 {
				t.Fatalf("expected a refusal; got %d body=%s", rec.Code, rec.Body.String())
			}
			code := rec.Header().Get(apihttp.ProblemCodeHeader)
			if code == "" {
				t.Fatalf("a refusal must carry a machine-readable code (API-010); got status %d body=%s", rec.Code, rec.Body.String())
			}
			registry, published := publishedRegistryOf(published, code)
			if !published {
				t.Fatalf("this surface answered with code %q, which NO contract's error registry publishes — "+
					"a client's error handling is a switch over a published table, so an unminted code is an untyped refusal (API-011)", code)
			}
			t.Logf("%s → %d %s (%s)", c.name, rec.Code, code, registry)
			seen[code] = true
		})
	}

	// A guard against the whole table silently degrading to "everything answers
	// AUTH_REQUIRED": the refusals above are genuinely distinct conditions and
	// must not collapse onto one code.
	if len(seen) < 5 {
		t.Fatalf("the driven refusals produced only %d distinct codes (%v); they are not exercising distinct conditions", len(seen), seen)
	}
}

func publishedRegistryOf(published map[string]string, code string) (string, bool) {
	registry, ok := published[code]
	return registry, ok
}

// codeShapedLiteral matches the shape every published error code has: uppercase
// ASCII words joined by underscores. It is deliberately an OVER-approximation —
// it matches candidates rather than known codes, which is what lets this catch
// an invented code on a path no test drives.
var codeShapedLiteral = regexp.MustCompile(`^[A-Z][A-Z0-9]{3,}(_[A-Z0-9]+)*$`)

// TestNoUnpublishedErrorCodeLiteralInThisPackage enumerates every code-shaped
// string literal in the package's own source and requires each to be published.
//
// It reads the SOURCE rather than the set of codes someone remembered to write
// down, which is the difference between checking the one defect that was found
// and checking the class it belongs to.
func TestNoUnpublishedErrorCodeLiteralInThisPackage(t *testing.T) {
	published := publishedCodes(t)

	found := map[string][]string{} // code -> the files it appears in
	for file, f := range parsePackageSource(t) {
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
			t.Errorf("%s mints the error code %q, which no contract's error registry publishes (API-011/EVT-096)", strings.Join(files, ", "), code)
		}
	}
}

// wsCloseConstants maps the name of each events.Close* constant to its value.
// TestEveryWSCloseNamesAPublishedCode reads the NAMES this package actually
// uses out of the source and resolves them here, so adding a close that names a
// constant missing from this map fails rather than passing unchecked.
var wsCloseConstants = map[string]string{
	"CloseIdleTimeout":       events.CloseIdleTimeout,
	"CloseAuthRequired":      events.CloseAuthRequired,
	"CloseSlowConsumer":      events.CloseSlowConsumer,
	"CloseResumeFromInvalid": events.CloseResumeFromInvalid,
	"CloseSelectorInvalid":   events.CloseSelectorInvalid,
	"CloseUnavailable":       events.CloseUnavailable,
	"CloseInternal":          events.CloseInternal,
}

// TestEveryWSCloseNamesAPublishedCode is EVT-096 on this side of the boundary:
// internal/events proves that whatever CloseReason RETURNS is a published code;
// this proves the WS binding only ever asks it for one of those, so the
// unclassified fallback is a safety net rather than the thing doing the work.
func TestEveryWSCloseNamesAPublishedCode(t *testing.T) {
	published := publishedCodes(t)

	var constants []string
	openErrorCloses := 0
	for file, f := range parsePackageSource(t) {
		if filepath.Base(file) != "ws.go" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			sel, isSel := call.Fun.(*ast.SelectorExpr)
			if !isSel || sel.Sel.Name != "closeWith" || len(call.Args) != 1 {
				return true
			}
			arg, isSel := call.Args[0].(*ast.SelectorExpr)
			if !isSel {
				t.Errorf("a WS close names something this check cannot classify: %#v", call.Args[0])
				return true
			}
			switch pkg := arg.X.(type) {
			case *ast.Ident:
				if pkg.Name == "events" {
					constants = append(constants, arg.Sel.Name)
					return true
				}
				// The one close whose code arrives through a value: the
				// subscribe refusal. Its three possible codes are checked
				// below, at their own sources.
				if pkg.Name == "oerr" && arg.Sel.Name == "code" {
					openErrorCloses++
					return true
				}
			}
			t.Errorf("a WS close names %v.%s, which this check cannot classify", arg.X, arg.Sel.Name)
			return true
		})
	}

	if len(constants) == 0 || openErrorCloses == 0 {
		t.Fatalf("ws.go's close sites were not found (constants=%d, openError=%d); this check is not reading the source",
			len(constants), openErrorCloses)
	}
	for _, name := range constants {
		code, known := wsCloseConstants[name]
		if !known {
			t.Errorf("ws.go closes with events.%s, which this test cannot resolve — add it to wsCloseConstants so its value is checked", name)
			continue
		}
		if _, ok := published[code]; !ok {
			t.Errorf("ws.go closes naming %q (events.%s), which no contract's error registry publishes (EVT-096)", code, name)
		}
	}

	// The subscribe refusal's own three codes, taken from the producers that
	// actually mint them rather than from a list written down here.
	_, perr := apiselector.Parse("kind = = screen")
	if perr == nil {
		t.Fatal("the selector fixture must fail to parse for this check to reach a code")
	}
	for _, code := range []string{perr.Code, events.ResumeFromInvalidCode, "INTERNAL"} {
		if _, ok := published[code]; !ok {
			t.Errorf("a WS subscribe refusal can close naming %q, which no contract's error registry publishes (EVT-096)", code)
		}
	}
}

// parsePackageSource parses this package's non-test source files.
func parsePackageSource(t *testing.T) map[string]*ast.File {
	t.Helper()
	_, self, _, _ := runtime.Caller(0)
	dir := filepath.Dir(self)
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse the package source: %v", err)
	}
	pkg, ok := pkgs["eventsse"]
	if !ok {
		t.Fatalf("package eventsse was not found under %s", dir)
	}
	return pkg.Files
}
