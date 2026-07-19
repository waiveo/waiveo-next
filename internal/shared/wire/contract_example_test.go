package wire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// scheduleSectionJSONKeys returns the JSON tag names of every ScheduleSection
// field, in declaration (canonical marshal) order. Deriving the expected key
// set from the shipped struct itself is what keeps the contract-example guard
// below from ever drifting away from the wire shape: adding or renaming a field
// on ScheduleSection changes what the worked example is required to show.
func scheduleSectionJSONKeys() []string {
	t := reflect.TypeOf(ScheduleSection{})
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		keys = append(keys, name)
	}
	return keys
}

// moduleRoot walks up from this test file's own directory to the module root
// (the directory containing go.mod), so the contract path resolves regardless
// of the process working directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found walking up from %s", file)
		}
		dir = parent
	}
}

// extractStateSnapshotExample returns the JSON body of the one fenced ```json
// block in the contract carrying a `"type": "state.snapshot"` message, with
// whole-line `//` comments stripped so it parses as strict JSON.
func extractStateSnapshotExample(t *testing.T, md string) string {
	t.Helper()
	const fence = "```json"
	rest := md
	for {
		i := strings.Index(rest, fence)
		if i < 0 {
			break
		}
		rest = rest[i+len(fence):]
		end := strings.Index(rest, "```")
		if end < 0 {
			break
		}
		block := rest[:end]
		rest = rest[end+3:]
		if !strings.Contains(block, `"type": "state.snapshot"`) {
			continue
		}
		var b strings.Builder
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
		return b.String()
	}
	t.Fatal("no worked state.snapshot ```json example found in contracts/relay-1.md")
	return ""
}

// TestContractWorkedExampleScheduleMatchesShape guards the single full worked
// `state.snapshot` example in contracts/relay-1.md — the only complete worked
// snapshot message in the repo, and the shape reference an engineer (or the
// relay-side schedule resolver, internal/relay/schedulehost) reads to learn a
// section's wire shape — against the shipped wire.ScheduleSection (REL-065).
// Every other section in that example matches its Go struct's field names
// exactly; the `schedule` section MUST too, carrying exactly the seven
// scheduling-core row arrays (scope_nodes, playlists, schedules,
// validity_windows, dayparts, fallbacks, preset_batches) — never a stale or
// renamed key such as `presets`. Deriving the expected key set from the struct
// via reflection means this test would have caught the example lagging the type
// (e.g. `{playlists, dayparts, presets}`).
func TestContractWorkedExampleScheduleMatchesShape(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "contracts", "relay-1.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}

	block := extractStateSnapshotExample(t, string(raw))

	var msg struct {
		Body struct {
			Sections struct {
				Schedule map[string]json.RawMessage `json:"schedule"`
			} `json:"sections"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(block), &msg); err != nil {
		t.Fatalf("parse worked state.snapshot example: %v\n%s", err, block)
	}
	sched := msg.Body.Sections.Schedule
	if sched == nil {
		t.Fatal("worked state.snapshot example carries no `schedule` section")
	}

	got := make([]string, 0, len(sched))
	for k := range sched {
		got = append(got, k)
	}
	sort.Strings(got)

	want := scheduleSectionJSONKeys()
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)

	if !reflect.DeepEqual(got, sortedWant) {
		t.Errorf("contracts/relay-1.md worked example `schedule` keys = %v,\n want exactly wire.ScheduleSection's keys %v", got, want)
	}

	// Each present row array must be the empty array `[]` (never null / an
	// object), matching the REL-060 empty-array discipline the shipped section
	// marshals — the worked example shows the empty first-photon shape.
	for _, k := range want {
		v, ok := sched[k]
		if !ok {
			continue // key-set mismatch already reported above
		}
		if strings.TrimSpace(string(v)) != "[]" {
			t.Errorf("worked example schedule.%s = %s, want [] (empty array — REL-060/065)", k, v)
		}
	}
}
