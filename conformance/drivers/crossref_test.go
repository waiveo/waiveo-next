// Package drivers holds checks that apply across every conformance driver
// rather than to one of them.
//
// This file exists because of a specific failure. The relay/1 driver's REL-070
// Pass rationale said the requirement's two side-effect fields were "vacuously
// satisfied", because the apply path had no side effects when the note was
// written. The apply path later grew real side effects — cancelling in-flight
// schedule-resolve loops, re-installing served programs, replacing pairing
// grants — and the note stayed. It had become a false statement about shipped
// behaviour, and nothing failed, because nothing checked.
//
// A Pass rationale is not decoration. It is the record of what a green driver
// did and did not observe, and it is what a reader consults instead of
// re-deriving the coverage themselves. A rationale that names a test as the
// place a property IS pinned is stronger than one that hand-waves — and more
// dangerous when it rots, because a reader who sees a named test stops looking.
//
// This checks the half that CAN be checked mechanically: that every test
// function and every source file a driver names actually exists. It cannot
// check whether a rationale's reasoning is still true — that is what review is
// for — but a claim whose evidence has been renamed away is the loud, common
// case, and it is now a build failure rather than a thing someone might notice.
package drivers

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoRoot is two levels up from conformance/drivers.
const repoRoot = "../.."

var (
	// testNameRe matches a Go test function name as it appears in prose.
	testNameRe = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*\b`)
	// goPathRe matches a repo-relative path to a Go file. It requires a slash, so
	// a bare "driver.go" self-reference is not treated as a repo path.
	goPathRe = regexp.MustCompile(`\b[a-z][\w.-]*(?:/[\w.-]+)+\.go\b`)
	// funcDeclRe finds a test function's declaration.
	funcDeclRe = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\s*\(`)
)

// skipDir keeps the scan inside the repo proper. The workspace has carried
// stale full copies of this tree beside it, and a name that resolves only in
// one of those is exactly the false positive this check must not produce.
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".claude", "web":
		return true
	}
	return strings.Contains(name, "-mut")
}

// declaredTestNames is every Test function declared anywhere in the Go tree.
func declaredTestNames(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range funcDeclRe.FindAllStringSubmatch(string(src), -1) {
			names[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the Go tree for test declarations: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("found zero test functions in the tree — the scan is broken, and a broken scan passes every assertion below")
	}
	return names
}

// driverSources returns each conformance driver's source, keyed by its path.
func driverSources(t *testing.T) map[string]string {
	t.Helper()
	matches, err := filepath.Glob("*/driver.go")
	if err != nil {
		t.Fatalf("glob driver sources: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("found zero conformance drivers — the glob is wrong, and this file would then check nothing while passing")
	}
	out := make(map[string]string, len(matches))
	for _, m := range matches {
		src, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		out[m] = string(src)
	}
	return out
}

// TestDriverProseNamesOnlyTestsThatExist: a driver that cites a test as the
// place a property is pinned must cite one that exists.
//
// This is the check that would have caught the REL-070 rationale's successor
// failure mode. It does not catch the original — a claim can be false while
// every name in it resolves — but it removes the class where the evidence was
// renamed or deleted out from under the claim, which is silent today.
func TestDriverProseNamesOnlyTestsThatExist(t *testing.T) {
	declared := declaredTestNames(t)
	for path, src := range driverSources(t) {
		seen := map[string]bool{}
		for _, name := range testNameRe.FindAllString(src, -1) {
			if seen[name] {
				continue
			}
			seen[name] = true
			if !declared[name] {
				t.Errorf("%s names %s, which is not declared anywhere in the tree.\n"+
					"A driver citing a test is citing EVIDENCE — a reader who sees a named test stops looking for the "+
					"coverage themselves. Either restore the reference, or rewrite the claim to say what is actually "+
					"observed and where.", path, name)
			}
		}
	}
}

// TestDriverProseNamesOnlyFilesThatExist is the same rule for source paths. A
// rationale pointing at cmd/waiveo-relay/livepull.go as the seam where an
// effect lives is worth exactly as much as that path being real.
func TestDriverProseNamesOnlyFilesThatExist(t *testing.T) {
	for path, src := range driverSources(t) {
		seen := map[string]bool{}
		for _, ref := range goPathRe.FindAllString(src, -1) {
			if seen[ref] {
				continue
			}
			seen[ref] = true
			// Import paths are not file paths; they are checked by the compiler.
			if strings.HasPrefix(ref, "github.com/") {
				continue
			}
			if _, err := os.Stat(filepath.Join(repoRoot, ref)); err != nil {
				t.Errorf("%s names the file %s, which does not exist.\n"+
					"A rationale pointing at a seam is worth exactly as much as the path being real.", path, ref)
			}
		}
	}
}

// TestCrossReferenceScanActuallyFindsReferences guards the guard.
//
// Both checks above pass trivially against a driver set that yields no
// references at all — a regex that stopped matching, a glob that found nothing,
// a walk that skipped the tree. Each of those failures looks identical to
// success, so the population is asserted rather than assumed: this is the same
// mistake as a corpus case that reports covered because it exercises nothing.
func TestCrossReferenceScanActuallyFindsReferences(t *testing.T) {
	var names, paths int
	var withRefs []string
	for path, src := range driverSources(t) {
		n := len(testNameRe.FindAllString(src, -1))
		p := len(goPathRe.FindAllString(src, -1))
		names += n
		paths += p
		if n+p > 0 {
			withRefs = append(withRefs, path)
		}
	}
	sort.Strings(withRefs)
	if names == 0 {
		t.Error("the scan found zero test-name references across every driver — the checks above are passing on an empty set")
	}
	if paths == 0 {
		t.Error("the scan found zero file-path references across every driver — the checks above are passing on an empty set")
	}
	t.Logf("cross-reference scan: %d test name(s), %d file path(s) across %d driver(s) carrying references: %s",
		names, paths, len(withRefs), strings.Join(withRefs, ", "))
}
