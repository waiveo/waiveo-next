package playercontentcache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// source_test.go is the shared reader these guards are built on: it loads a real
// shipped BrightScript file, strips comments, and slices out one routine's body
// so an assertion is scoped to the function it is about rather than to the whole
// file.
//
// Scoping matters more than it looks. "Program.brs calls wvTrimContentCache
// somewhere" is satisfied by a call in dead code, or in the wrong function, or
// in a comment; "wvDoProgram calls it, after the item loop, before it publishes"
// is the property. Every assertion below therefore names a routine.

// The real shipped sources. Parsing the files the Roku actually runs — never a
// fixture copy — is the whole point: a copy drifts, and a guard reading a
// drifted copy passes while the shipped player regresses.
var (
	programPath     = filepath.Join("..", "..", "..", "player-v3", "source", "Program.brs")
	photonScenePath = filepath.Join("..", "..", "..", "player-v3", "components", "PhotonScene.brs")
	photonSceneXML  = filepath.Join("..", "..", "..", "player-v3", "components", "PhotonScene.xml")
)

// line is one source line stripped of comments and indentation, keeping its
// 1-based number so a failure can point at what a reader must open.
type line struct {
	n    int
	text string
}

// readBrs loads a BrightScript file as comment-free, blank-free lines.
//
// Comment stripping is quote-aware. These files are full of console strings, and
// a naive strip would truncate any line whose string literal contains an
// apostrophe — silently changing what this guard believes the code is, in the
// direction of believing a call is absent when it is present.
func readBrs(t *testing.T, path string) []line {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []line
	for i, text := range strings.Split(string(raw), "\n") {
		stripped := strings.TrimSpace(stripComment(text))
		if stripped == "" {
			continue
		}
		out = append(out, line{n: i + 1, text: stripped})
	}
	if len(out) == 0 {
		t.Fatalf("%s parsed to no code lines", path)
	}
	return out
}

// stripComment removes a trailing BrightScript `'` comment, ignoring
// apostrophes inside double-quoted strings.
func stripComment(text string) string {
	inString := false
	for i, r := range text {
		switch r {
		case '"':
			inString = !inString
		case '\'':
			if !inString {
				return text[:i]
			}
		}
	}
	return text
}

// routineBody returns the lines strictly inside `function <name>(` / `sub
// <name>(`, up to its matching `end function` / `end sub`.
//
// A routine this file cannot find is a FATAL failure rather than a skipped
// assertion: a guard that quietly passes because the thing it guards was renamed
// is worse than no guard, since the rename is exactly when the behaviour is most
// likely to have been dropped.
func routineBody(t *testing.T, lines []line, name string) []line {
	t.Helper()
	openers := []string{"function " + name + "(", "sub " + name + "("}
	start, closer := -1, ""
	for i, l := range lines {
		for _, o := range openers {
			if strings.HasPrefix(l.text, o) {
				start = i + 1
				closer = "end function"
				if strings.HasPrefix(o, "sub ") {
					closer = "end sub"
				}
			}
		}
		if start >= 0 {
			break
		}
	}
	if start < 0 {
		t.Fatalf("no `function %s(` or `sub %s(` found — this guard cannot read a player it does not recognise", name, name)
	}
	for i := start; i < len(lines); i++ {
		if lines[i].text == closer {
			return lines[start:i]
		}
	}
	t.Fatalf("routine %s has no %s", name, closer)
	return nil
}

// indexOfCall returns the position within body of the first line calling
// `callee`, or -1. It matches the callee followed by `(`, so `wvEnsureContent`
// does not match a hypothetical `wvEnsureContentX`.
func indexOfCall(body []line, callee string) int {
	for i, l := range body {
		if strings.Contains(l.text, callee+"(") {
			return i
		}
	}
	return -1
}

// mustCall asserts body calls callee and returns where.
func mustCall(t *testing.T, body []line, routine, callee string) int {
	t.Helper()
	i := indexOfCall(body, callee)
	if i < 0 {
		t.Fatalf("%s does not call %s", routine, callee)
	}
	return i
}

// countCalls counts the lines in body that call callee.
func countCalls(body []line, callee string) int {
	n := 0
	for _, l := range body {
		if strings.Contains(l.text, callee+"(") {
			n++
		}
	}
	return n
}

// contains reports whether any line of body contains needle.
func contains(body []line, needle string) bool {
	for _, l := range body {
		if strings.Contains(l.text, needle) {
			return true
		}
	}
	return false
}
