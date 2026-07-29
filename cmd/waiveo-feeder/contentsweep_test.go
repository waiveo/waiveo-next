package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/contentgc"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// ackFrame is a relay's state.ack as the connection server records it (REL-054).
func ackFrame(t *testing.T, relayID string, appliedGeneration int64) wire.Frame {
	t.Helper()
	f, err := wire.NewFrame(wire.FrameTypeStateAck, "01J8ZM00000000000000000ACK", relayID,
		wire.StateAckBody{AppliedGeneration: appliedGeneration})
	if err != nil {
		t.Fatalf("build a state.ack frame: %v", err)
	}
	return f
}

func connected(relayIDs ...string) func() []relayconn.ConnectedRelay {
	return func() []relayconn.ConnectedRelay {
		out := make([]relayconn.ConnectedRelay, 0, len(relayIDs))
		for _, id := range relayIDs {
			out = append(out, relayconn.ConnectedRelay{RelayID: id})
		}
		return out
	}
}

func acks(frames map[string]wire.Frame) func(string) (wire.Frame, bool) {
	return func(relayID string) (wire.Frame, bool) {
		f, ok := frames[relayID]
		return f, ok
	}
}

// TestContentSweepFleetConvergence pins every answer the fleet oracle gives,
// because each one licenses or forbids a deletion that cannot be undone.
//
// It is asked "is the fleet at generation N", never "what is the lowest
// generation" — a minimum cannot express a relay that has run AHEAD, and such a
// relay is serving a program built from rows the caller never read.
func TestContentSweepFleetConvergence(t *testing.T) {
	const relayA, relayB = "01J8ZM000000000000000000A1", "01J8ZM000000000000000000B2"

	t.Run("no relay is enrolled: converged for any target", func(t *testing.T) {
		// Content reaches a screen only through a relay. With none entitled to
		// connect, no program is in play and there is no older generation to
		// protect. This MUST hold for a real generation, not just for zero: a
		// relay-less box whose store has ever been written to would otherwise
		// never reclaim, and would report the fleet as KNOWN while doing so, so
		// nothing would ever say why.
		for _, target := range []int64{0, 1, 42} {
			converged, known := contentSweepFleetFloor(
				func() []string { return nil }, connected(), acks(nil))(target)
			if !converged || !known {
				t.Fatalf("target %d: (converged=%v, known=%v), want (true, true)", target, converged, known)
			}
		}
	})

	t.Run("one relay at the target", func(t *testing.T) {
		converged, known := contentSweepFleetFloor(
			func() []string { return []string{relayA} },
			connected(relayA),
			acks(map[string]wire.Frame{relayA: ackFrame(t, relayA, 42)}))(42)
		if !converged || !known {
			t.Fatalf("(converged=%v, known=%v), want (true, true)", converged, known)
		}
	})

	t.Run("a relay BEHIND the target: not converged", func(t *testing.T) {
		converged, known := contentSweepFleetFloor(
			func() []string { return []string{relayA, relayB} },
			connected(relayA, relayB),
			acks(map[string]wire.Frame{
				relayA: ackFrame(t, relayA, 42),
				relayB: ackFrame(t, relayB, 39),
			}))(42)
		if converged || !known {
			t.Fatalf("(converged=%v, known=%v), want (false, true) — relayB still serves an older program", converged, known)
		}
	})

	t.Run("a relay AHEAD of the target: not converged", func(t *testing.T) {
		// The case a floor cannot see. Under a minimum, relayA at the target
		// reports the target and the sweep proceeds — while relayB serves a
		// program built from rows this sweep never read.
		converged, known := contentSweepFleetFloor(
			func() []string { return []string{relayA, relayB} },
			connected(relayA, relayB),
			acks(map[string]wire.Frame{
				relayA: ackFrame(t, relayA, 42),
				relayB: ackFrame(t, relayB, 99),
			}))(42)
		if converged {
			t.Fatalf("(converged=true, known=%v) with a relay ahead of the reference set — a minimum would have hidden it", known)
		}
	})

	t.Run("an enrolled relay that is not connected: unknown", func(t *testing.T) {
		// The case the whole oracle exists for. relayB is offline; its screens
		// keep fetching content from this origin using whatever program it applied
		// before it went quiet, and nothing here can say what that was.
		converged, known := contentSweepFleetFloor(
			func() []string { return []string{relayA, relayB} },
			connected(relayA),
			acks(map[string]wire.Frame{relayA: ackFrame(t, relayA, 42)}))(42)
		if known || converged {
			t.Fatalf("(converged=%v, known=%v) with an enrolled relay offline; its screens would go blank", converged, known)
		}
	})

	t.Run("connected but never acknowledged: unknown", func(t *testing.T) {
		converged, known := contentSweepFleetFloor(
			func() []string { return []string{relayA} }, connected(relayA), acks(nil))(42)
		if known || converged {
			t.Fatalf("(converged=%v, known=%v) from a relay that has not said what it applied", converged, known)
		}
	})

	t.Run("an ack whose body will not decode: unknown", func(t *testing.T) {
		bad := ackFrame(t, relayA, 42)
		bad.Body = json.RawMessage(`{"applied_generation": "not a number"}`)
		converged, known := contentSweepFleetFloor(
			func() []string { return []string{relayA} }, connected(relayA),
			acks(map[string]wire.Frame{relayA: bad}))(42)
		if known || converged {
			t.Fatalf("(converged=%v, known=%v) from bytes that did not parse", converged, known)
		}
	})
}

// TestContentSweepFailureReclaimsNothing pins the loop's failure posture. The
// cadence in main calls this with whatever the pass returns; the requirement is
// that a pass which could not be decided leaves every asset in place, so the
// worst a broken sweep does to a running box is let disk keep growing.
func TestContentSweepFailureReclaimsNothing(t *testing.T) {
	dir := t.TempDir()
	content, err := origin.Open(dir)
	if err != nil {
		t.Fatalf("origin.Open: %v", err)
	}
	ref, err := content.Add([]byte("an asset a failing sweep must not touch"))
	if err != nil {
		t.Fatalf("origin.Add: %v", err)
	}

	sweeper, err := contentgc.New(contentgc.Config{
		Origin:     content,
		References: unreadableRefs{},
		Fleet:      func(int64) (bool, bool) { return true, true },
		NowMs:      func() int64 { return 0 },
	})
	if err != nil {
		t.Fatalf("contentgc.New: %v", err)
	}

	runContentSweep(context.Background(), sweeper)

	if _, err := os.Stat(filepath.Join(dir, strings.TrimPrefix(ref, "sha256:"))); err != nil {
		t.Fatalf("an asset was reclaimed by a sweep that could not read the reference set: %v", err)
	}
}

type unreadableRefs struct{}

func (unreadableRefs) Generation(context.Context) (int64, error) { return 1, nil }

func (unreadableRefs) WithContentReferences(context.Context, func(store.ContentReferences) error) error {
	return errUnreadable
}

type unreadableError struct{}

func (unreadableError) Error() string { return "the playlist table could not be read" }

var errUnreadable = unreadableError{}

// TestMainRunsTheContentSweep is the reachability guard: the sweep must be built
// AND run by the shipping binary.
//
// A sweeper that main constructs and never ticks is indistinguishable, from every
// other test in this tree, from one that runs — and "the component exists but
// nothing a deployment runs reaches it" is the exact state the required-pack
// floor was in before its option was wired. Both halves are checked because they
// fail independently: dropping the ticker call leaves construction in place, and
// dropping construction fails the build.
//
// KNOW WHAT THIS DOES NOT CATCH, the same limit TestMainStartsTheConsoleBinding
// records: it catches the call being DELETED, not the call being made
// unreachable by a condition that is never true.
func TestMainRunsTheContentSweep(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var mainFn *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "main" {
			mainFn = fn
			break
		}
	}
	if mainFn == nil {
		t.Fatal("main.go declares no func main")
	}
	called := map[string]bool{}
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok {
			called[ident.Name] = true
		}
		return true
	})
	if !called["newContentSweeper"] {
		t.Error("func main does not call newContentSweeper — the content origin would grow without bound on a running feeder")
	}
	if !called["runContentSweep"] {
		t.Error("func main never calls runContentSweep — the sweeper would be built and never run, which no other test in this tree can tell apart from working")
	}
}
