package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// walkHarness builds a runOptions wired to a fake installer, a fake
// environment, and a captured output stream — everything the fleet walk needs
// except a Roku.
type walkHarness struct {
	opts    runOptions
	out     *bytes.Buffer
	visited []string
}

func newWalkHarness(t *testing.T, roster string, install installFunc) *walkHarness {
	t.Helper()
	h := &walkHarness{out: &bytes.Buffer{}}

	envPath := filepath.Join(t.TempDir(), "dev-lab.env")
	if err := os.WriteFile(envPath, []byte("export ROKU_DEV_PASSWORD=abcd\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	h.opts = runOptions{
		Devices: roster,
		Src:     filepath.Join("..", "..", "player-v3"),
		Timeout: 2 * time.Second,
		EnvFile: envPath,
		Getenv:  func(string) string { return "" },
		Client:  &http.Client{},
		Out:     h.out,
		Installer: func(ctx context.Context, client *http.Client, dev device, creds credentials, zip []byte) (installOutcome, error) {
			h.visited = append(h.visited, dev.Name)
			return install(ctx, client, dev, creds, zip)
		},
	}
	return h
}

// One unreachable screen must not strand the rest of the wall. This is the
// property that separates a fleet tool from a loop around curl.
func TestRunContinuesPastAFailedDevice(t *testing.T) {
	h := newWalkHarness(t, "a=10.0.0.1,b=10.0.0.2,c=10.0.0.3",
		func(_ context.Context, _ *http.Client, dev device, _ credentials, _ []byte) (installOutcome, error) {
			switch dev.Name {
			case "b":
				return installOutcome{}, errors.New("connection refused")
			case "c":
				return installOutcome{OK: false, Detail: "Install Failure: Compilation Failed"}, nil
			default:
				return installOutcome{OK: true, Detail: "Install Success"}, nil
			}
		})

	failures, err := run(context.Background(), h.opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if failures != 2 {
		t.Errorf("failures = %d, want 2 (one transport error, one firmware failure)", failures)
	}
	if got := strings.Join(h.visited, ","); got != "a,b,c" {
		t.Errorf("visited %q, want every device walked in roster order despite the failures", got)
	}

	out := h.out.String()
	for _, want := range []string{"a", "OK", "connection refused", "Compilation Failed", "1/3 installed", "2 FAILED"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}

// A firmware "Install Failure" is a 200 with HTML in it. Counting it as
// success is the exact failure mode that lets an operator walk away from a
// wall that never got the build.
func TestRunCountsFirmwareFailureAsFailure(t *testing.T) {
	h := newWalkHarness(t, "a=10.0.0.1",
		func(context.Context, *http.Client, device, credentials, []byte) (installOutcome, error) {
			return installOutcome{OK: false, Detail: "Install Failure: Compilation Failed"}, nil
		})
	failures, err := run(context.Background(), h.opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if failures != 1 {
		t.Fatalf("failures = %d, want 1", failures)
	}
}

func TestRunDryRunTouchesNothing(t *testing.T) {
	h := newWalkHarness(t, "a=10.0.0.1,b=10.0.0.2",
		func(context.Context, *http.Client, device, credentials, []byte) (installOutcome, error) {
			t.Error("dry run reached the installer")
			return installOutcome{}, nil
		})
	h.opts.DryRun = true

	failures, err := run(context.Background(), h.opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if failures != 0 || len(h.visited) != 0 {
		t.Fatalf("dry run installed something: failures=%d visited=%v", failures, h.visited)
	}
	out := h.out.String()
	if !strings.Contains(out, "DRY-RUN") || !strings.Contains(out, "nothing was installed") {
		t.Errorf("dry-run report does not say it was a dry run:\n%s", out)
	}
	// It still resolves and packages, which is what makes it a useful check.
	if !strings.Contains(out, "packaged") {
		t.Errorf("dry run skipped packaging, so it proves nothing about the build:\n%s", out)
	}
}

// The per-device timeout is applied by run, not by the caller. A wedged screen
// costs one timeout, not the whole walk.
func TestRunAppliesThePerDeviceTimeout(t *testing.T) {
	var deadlines []time.Duration
	h := newWalkHarness(t, "a=10.0.0.1,b=10.0.0.2",
		func(ctx context.Context, _ *http.Client, _ device, _ credentials, _ []byte) (installOutcome, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Error("the installer was handed a context with no deadline")
				return installOutcome{OK: true}, nil
			}
			deadlines = append(deadlines, time.Until(deadline))
			return installOutcome{OK: true, Detail: "Install Success"}, nil
		})
	h.opts.Timeout = 500 * time.Millisecond

	if _, err := run(context.Background(), h.opts); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(deadlines) != 2 {
		t.Fatalf("saw %d deadline(s), want one per device", len(deadlines))
	}
	for i, d := range deadlines {
		if d <= 0 || d > 500*time.Millisecond {
			t.Errorf("device %d got %s of budget, want a fresh ~500ms deadline of its own", i, d)
		}
	}
}

func TestRunRefusesWithoutARoster(t *testing.T) {
	h := newWalkHarness(t, "",
		func(context.Context, *http.Client, device, credentials, []byte) (installOutcome, error) {
			t.Error("installer reached with no roster")
			return installOutcome{}, nil
		})
	if _, err := run(context.Background(), h.opts); err == nil {
		t.Fatal("run proceeded with no devices")
	}
}

// Nothing is touched until the credential resolves: updating three screens and
// then discovering the password is missing is the worst possible ordering.
func TestRunRefusesBeforeTouchingAnythingWhenTheCredentialIsMissing(t *testing.T) {
	h := newWalkHarness(t, "a=10.0.0.1",
		func(context.Context, *http.Client, device, credentials, []byte) (installOutcome, error) {
			t.Error("installer reached with no credential")
			return installOutcome{}, nil
		})
	h.opts.EnvFile = filepath.Join(t.TempDir(), "absent.env")

	_, err := run(context.Background(), h.opts)
	if err == nil {
		t.Fatal("run proceeded with no dev password")
	}
	if !strings.Contains(err.Error(), "ROKU_DEV_PASSWORD") {
		t.Errorf("error %q does not say what to set", err)
	}
	if len(h.visited) != 0 {
		t.Errorf("devices %v were touched before the credential check", h.visited)
	}
}

// With no -devices, the roster is the relay's own ECP target list — and its
// ECP ports must not be dialled for a sideload.
func TestRunFallsBackToTheRelayECPRoster(t *testing.T) {
	h := newWalkHarness(t, "",
		func(_ context.Context, _ *http.Client, dev device, _ credentials, _ []byte) (installOutcome, error) {
			if dev.Addr() != "10.0.0.9:80" {
				t.Errorf("dialled %s, want the dev installer port", dev.Addr())
			}
			return installOutcome{OK: true, Detail: "Install Success"}, nil
		})
	h.opts.Getenv = func(k string) string {
		if k == "WAIVEO_RELAY_ECP_TARGETS" {
			return "screen.hanger=10.0.0.9:8060"
		}
		return ""
	}

	failures, err := run(context.Background(), h.opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if failures != 0 {
		t.Fatalf("failures = %d, want 0", failures)
	}
	if got := strings.Join(h.visited, ","); got != "screen.hanger" {
		t.Errorf("visited %q, want the ECP roster's entity id as the label", got)
	}
	if !strings.Contains(h.out.String(), "WAIVEO_RELAY_ECP_TARGETS") {
		t.Errorf("the report does not say which roster answered:\n%s", h.out.String())
	}
}

func TestRunUsesAPrebuiltZipWhenGiven(t *testing.T) {
	data, _, err := buildChannelZip(fakeChannel(t))
	if err != nil {
		t.Fatalf("buildChannelZip: %v", err)
	}
	zipPath := filepath.Join(t.TempDir(), "channel.zip")
	if err := os.WriteFile(zipPath, data, 0o644); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	var delivered []byte
	h := newWalkHarness(t, "a=10.0.0.1",
		func(_ context.Context, _ *http.Client, _ device, _ credentials, zip []byte) (installOutcome, error) {
			delivered = zip
			return installOutcome{OK: true, Detail: "Install Success"}, nil
		})
	h.opts.Zip = zipPath

	if _, err := run(context.Background(), h.opts); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !bytes.Equal(delivered, data) {
		t.Error("the prebuilt zip's bytes were not what reached the device")
	}
}
