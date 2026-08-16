package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	examplepacks "github.com/maaxton/waiveo-next/examples/packs"
	"github.com/maaxton/waiveo-next/internal/app/packrun"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/packhost"
)

// packruntime_e2e_test.go is the FIRST EXTENSION actually running: the pilot
// pack's compiled binary, installed through the real signature-gated route,
// materialised and started by packrun under the real supervisor, proving
// readiness by redeeming its tier grant over real HTTP, leasing real work from
// the queue, doing it, and answering — the whole loop the platform was built to
// carry, exercised end to end with no fakes on either side of the process
// boundary.
//
// Every prior test proved one wall. A pack that starts and cannot lease, or
// leases and cannot answer, or answers into a queue that recorded nothing,
// passes all of those and still isn't an extension platform. This file is where
// that claim is earned.

// builtBackupsPack compiles the pilot's entry once for the package.
var (
	backupsPackOnce sync.Once
	backupsPackBin  []byte
	backupsPackErr  error
)

func builtBackupsPackBinary(t *testing.T) []byte {
	t.Helper()
	backupsPackOnce.Do(func() {
		dir := t.TempDir()
		bin := filepath.Join(dir, "backups")
		out, err := exec.Command("go", "build", "-o", bin,
			"github.com/maaxton/waiveo-next/examples/packs/backups/cmd/backups").CombinedOutput()
		if err != nil {
			backupsPackErr = err
			t.Logf("build pilot pack: %v\n%s", err, out)
			return
		}
		backupsPackBin, backupsPackErr = os.ReadFile(bin)
	})
	if backupsPackErr != nil {
		t.Fatalf("build pilot pack binary: %v", backupsPackErr)
	}
	return backupsPackBin
}

// installBackupsPilot installs the REAL pilot artifact: the shipped tree from
// examples/packs/backups with the compiled entry injected exactly as
// `make example-pack` injects it, signed by the fixture publisher. Installing a
// hand-rolled lookalike here would exempt the shipped manifest — the one that
// declares `runtime` — from ever meeting the installer.
func installBackupsPilot(t *testing.T, e *testEnv, bin []byte) {
	t.Helper()
	raw, err := examplepacks.PackZipWithFiles("backups", map[string][]byte{"bin/backups": bin})
	if err != nil {
		t.Fatalf("assemble pilot artifact: %v", err)
	}
	resp, body := e.do(t, http.MethodPost, "/api/v1/packs", signPack(t, raw, "waiveo/backups", "1.0.0"), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("install pilot: %d %s", resp.StatusCode, body)
	}
}

func TestThePilotExtensionRunsTheWholeLoopForReal(t *testing.T) {
	bin := builtBackupsPackBinary(t)
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	installBackupsPilot(t, e, bin)

	// The real supervisor over the real auth store: readiness below is the pack
	// redeeming its grant over HTTP, not a fake flipping a bit.
	sup := packhost.New(e.auth.Store, packhost.Options{
		ReadyTimeout: 15 * time.Second,
		StopGrace:    5 * time.Second,
	})
	host := packrun.New(e.store, sup, t.TempDir(), rowScopeNodeA, e.ts.URL, nil)

	results, err := host.StartAll(context.Background())
	if err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("the pilot must start (readiness = its grant redeemed over HTTP): %+v", results)
	}
	defer sup.StopAll()

	// Queue real work through the management route, exactly as an operator does.
	resp, body := invokeAction(t, e, "waiveo/backups", "run-backup", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("invoke run-backup: %d %+v", resp.StatusCode, body)
	}
	invocationID, _ := body["invocation_id"].(string)
	if invocationID == "" {
		t.Fatalf("invoke returned no invocation_id: %+v", body)
	}

	// The far side of every boundary at once: the verdict must arrive in the
	// STORE, written by the pack through the result route. Polled with a real
	// deadline — the pack is a real process on its own clock.
	inv := awaitInvocationDone(t, e, invocationID, 20*time.Second)
	if inv.State != store.InvocationSucceeded {
		t.Fatalf("invocation ended %q (error %s %s), want success",
			inv.State, inv.ErrorCode, inv.ErrorMessage)
	}
	var result struct {
		Archive string `json:"archive"`
		Bytes   int    `json:"bytes"`
		SHA256  string `json:"sha256"`
	}
	if err := json.Unmarshal(inv.Result, &result); err != nil {
		t.Fatalf("decode result %s: %v", inv.Result, err)
	}
	if want := "backup-" + invocationID + ".tar.gz"; result.Archive != want {
		t.Errorf("archive = %q, want %q — named after the invocation, not the clock", result.Archive, want)
	}
	if result.Bytes <= 0 {
		t.Errorf("bytes = %d, want a real archive", result.Bytes)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(result.SHA256) {
		t.Errorf("sha256 = %q, want a hex digest", result.SHA256)
	}

	// Shutdown is the host's half of the lifecycle: closing stdin must end the
	// process without it being recorded as a crash.
	sup.StopAll()
	if exits := sup.Exits(); len(exits) != 0 {
		t.Fatalf("a deliberate stop was recorded as a crash: %+v", exits)
	}
}

// A second invocation through the SAME running pack: the loop must return to
// leasing after answering, or every extension handles exactly one action per
// boot and the platform quietly becomes restart-per-call.
func TestThePilotServesASecondInvocationWithoutRestarting(t *testing.T) {
	bin := builtBackupsPackBinary(t)
	e := newEnv(t)
	e.seedPlacementNodes(t, rowScopeNodeA, rowScopeNodeB)
	installBackupsPilot(t, e, bin)

	sup := packhost.New(e.auth.Store, packhost.Options{
		ReadyTimeout: 15 * time.Second,
		StopGrace:    5 * time.Second,
	})
	host := packrun.New(e.store, sup, t.TempDir(), rowScopeNodeA, e.ts.URL, nil)
	results, err := host.StartAll(context.Background())
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("StartAll: %v %+v", err, results)
	}
	defer sup.StopAll()

	first := mustInvoke(t, e)
	if inv := awaitInvocationDone(t, e, first, 20*time.Second); inv.State != store.InvocationSucceeded {
		t.Fatalf("first invocation: %q (%s %s)", inv.State, inv.ErrorCode, inv.ErrorMessage)
	}
	second := mustInvoke(t, e)
	if inv := awaitInvocationDone(t, e, second, 20*time.Second); inv.State != store.InvocationSucceeded {
		t.Fatalf("second invocation: %q (%s %s) — the pack answered once and stopped serving",
			inv.State, inv.ErrorCode, inv.ErrorMessage)
	}
}

func mustInvoke(t *testing.T, e *testEnv) string {
	t.Helper()
	resp, body := invokeAction(t, e, "waiveo/backups", "run-backup", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("invoke run-backup: %d %+v", resp.StatusCode, body)
	}
	id, _ := body["invocation_id"].(string)
	if id == "" {
		t.Fatalf("invoke returned no invocation_id: %+v", body)
	}
	return id
}

// awaitInvocationDone polls the store until the invocation leaves the
// pending/leased states or the deadline passes. Polling the STORE and not the
// wire: the store is where the verdict lives, and it cannot be patched around.
func awaitInvocationDone(t *testing.T, e *testEnv, id string, deadline time.Duration) store.PackInvocation {
	t.Helper()
	stop := time.Now().Add(deadline)
	for {
		inv, err := e.store.GetPackInvocation(context.Background(), id)
		if err != nil {
			t.Fatalf("GetPackInvocation(%s): %v", id, err)
		}
		if inv.State != store.InvocationPending && inv.State != store.InvocationLeased {
			return inv
		}
		if time.Now().After(stop) {
			t.Fatalf("invocation %s still %q after %s", id, inv.State, deadline)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
