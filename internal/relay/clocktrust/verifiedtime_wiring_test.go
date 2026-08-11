package clocktrust_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestApplyVerifiedTimeStillHasNoProductionCaller is a TRIPWIRE, not an approval.
//
// ApplyVerifiedTime is documented as the only path to a trusted clock, and while
// it has no production caller a relay boots untrusted (REL-131), never persists
// the state, and Engine.dispatchSchedule therefore refuses to read the wall clock
// forever (RUL-370). The consequence is that no `time`, `time_pattern` or `sun`
// trigger can EVER fire — confirmed on hardware, twice, with generation applies
// verified: zero dispatches (HV-20).
//
// Because that cannot be fixed cheaply, the console instead SAYS SO where an
// operator picks such a trigger (`msg:auto.t.wallClockDead`). That warning is
// only honest while the limitation holds, and a warning that outlives its cause
// is the stale comment this programme has now been bitten by twice — once badly
// enough to cost a whole feature (the Variables create form believed a comment
// claiming the grammar could not carry a chosen placement).
//
// So this test fails the moment a verified-time source is wired. It is not
// asserting that the gap is correct; it is making the console's claim about the
// gap impossible to leave behind.
func TestApplyVerifiedTimeStillHasNoProductionCaller(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	var callers []string

	for _, tree := range []string{"cmd", "internal"} {
		err := filepath.Walk(filepath.Join(root, tree), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				// Skip nested checkouts: an isolated agent worktree under .claude/
				// is a second copy of this repo, and the go tool never compiles a
				// dot-prefixed directory either.
				if n := info.Name(); strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// The definition itself lives in this package; a call is elsewhere.
			if strings.Contains(filepath.ToSlash(path), "internal/relay/clocktrust/") {
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for _, line := range strings.Split(string(b), "\n") {
				code := line
				if i := strings.Index(code, "//"); i >= 0 {
					code = code[:i] // a doc comment naming it is not a caller
				}
				if strings.Contains(code, "ApplyVerifiedTime(") {
					callers = append(callers, path)
					break
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}

	if len(callers) > 0 {
		t.Fatalf("ApplyVerifiedTime now has a production caller (%s).\n"+
			"That is GOOD — it means a verified-time source exists and wall-clock triggers can fire.\n"+
			"Now remove the console's warning that they cannot: `msg:auto.t.wallClockDead` in\n"+
			"web/src/routes/automations/automations-route.tsx, its node in page.uis.json, and the\n"+
			"tests naming it. Then delete this test. See HV-20.", strings.Join(callers, ", "))
	}
}
