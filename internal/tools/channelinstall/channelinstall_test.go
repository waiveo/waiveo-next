package channelinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// channelIndexArtifact/channelIndexSigned/channelIndexEnvelope mirror
// channel-index/1's Wire shapes (contracts/channel-index.md `ChannelIndex`)
// field-for-field -- artifact_id/kind/version/arch/status/digest/size/
// download_url, wrapped in the signed/signatures envelope -- so a fixture
// built here is exactly the document
// scripts/install-from-channel-index.sh is contracted to parse.
type channelIndexArtifact struct {
	ArtifactID  string `json:"artifact_id"`
	Kind        string `json:"kind"`
	Version     string `json:"version"`
	Arch        string `json:"arch"`
	Status      string `json:"status"`
	Digest      string `json:"digest"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"download_url"`
}

type channelIndexSigned struct {
	Role          string                 `json:"role"`
	FormatVersion string                 `json:"format_version"`
	Channel       string                 `json:"channel"`
	Version       int                    `json:"version"`
	Artifacts     []channelIndexArtifact `json:"artifacts"`
}

type channelIndexEnvelope struct {
	Signed     channelIndexSigned  `json:"signed"`
	Signatures []map[string]string `json:"signatures"`
}

// sha256Hex returns b's sha256 digest as lowercase hex, matching the digest
// scripts/install-from-channel-index.sh computes over a downloaded artifact
// (before the "sha256:" prefix, CHI-021).
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// scriptPath returns the absolute path to
// scripts/install-from-channel-index.sh, resolved from this test file's own
// location (runtime.Caller) rather than from go test's working directory.
func scriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to resolve this test file's path")
	}
	// thisFile: <repoRoot>/internal/tools/channelinstall/channelinstall_test.go
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	p := filepath.Join(repoRoot, "scripts", "install-from-channel-index.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("install script not found at %s: %v", p, err)
	}
	return p
}

// newFixtureServer starts an HTTP server serving artifactBytes's entries at
// /artifacts/<name>, and buildArtifacts(baseURL)'s resulting ChannelIndex
// envelope (channel-index/1 Wire shapes) at /index.json. baseURL is the
// server's own address, known from its pre-bound Listener before any
// handler is registered or the server starts accepting connections -- so
// every field this fixture serves is fixed before Start(), and there is
// nothing mutated afterward for the race detector to catch.
func newFixtureServer(t *testing.T, channel string, artifactBytes map[string][]byte, buildArtifacts func(baseURL string) []channelIndexArtifact) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	srv := httptest.NewUnstartedServer(mux)
	baseURL := "http://" + srv.Listener.Addr().String()

	for name, data := range artifactBytes {
		mux.HandleFunc("/artifacts/"+name, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(data)
		})
	}

	env := channelIndexEnvelope{
		Signed: channelIndexSigned{
			Role:          "targets",
			FormatVersion: "1.0",
			Channel:       channel,
			Version:       1,
			Artifacts:     buildArtifacts(baseURL),
		},
		Signatures: []map[string]string{{"key_id": "ed25519:test-fixture", "sig": "test-sig"}},
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal fixture ChannelIndex: %v", err)
	}
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})

	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// runScript runs scripts/install-from-channel-index.sh INDEX_URL CHANNEL
// with INSTALL_ROOT bound to installRoot, returning its combined stdout+
// stderr and the *exec.ExitError (nil on a zero exit).
func runScript(t *testing.T, indexURL, channel, installRoot string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", scriptPath(t), indexURL, channel)
	cmd.Env = append(os.Environ(), "INSTALL_ROOT="+installRoot)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// assertNotInstalled fails t if path exists -- used by every rejection-path
// test to prove a hard-failed run left INSTALL_ROOT untouched, never a
// partial install.
func assertNotInstalled(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s not to exist, stat err = %v", path, err)
	}
}

// TestInstallResolvesVerifiesAndInstalls proves the happy path end to end:
// among a decoy platform-release entry, a wrong-arch relay-release entry,
// and a non-active relay-release entry sharing the same artifact_id, the
// script resolves exactly the one active relay-release/linux/amd64 entry
// (CHI-020/041/043), verifies its digest/size, and installs it executable
// under INSTALL_ROOT with no .prev backup (nothing pre-existed to replace).
func TestInstallResolvesVerifiesAndInstalls(t *testing.T) {
	artifact := []byte("waiveo-relay-fixture-binary-v1\n")
	digest := "sha256:" + sha256Hex(artifact)

	srv := newFixtureServer(t, "relay-v1", map[string][]byte{"waiveo-relay": artifact}, func(baseURL string) []channelIndexArtifact {
		return []channelIndexArtifact{
			{ArtifactID: "waiveo-relay", Kind: "relay-release", Version: "1.0.0", Arch: "linux/amd64", Status: "active", Digest: digest, Size: int64(len(artifact)), DownloadURL: baseURL + "/artifacts/waiveo-relay"},
			{ArtifactID: "waiveo-relay", Kind: "relay-release", Version: "0.9.0", Arch: "linux/amd64", Status: "deprecated", Digest: "sha256:" + strings.Repeat("0", 64), Size: 3, DownloadURL: baseURL + "/artifacts/missing"},
			{ArtifactID: "waiveo-app", Kind: "platform-release", Version: "1.0.0", Arch: "linux/amd64", Status: "active", Digest: "sha256:" + strings.Repeat("0", 64), Size: 3, DownloadURL: baseURL + "/artifacts/missing"},
			{ArtifactID: "waiveo-relay", Kind: "relay-release", Version: "1.0.0", Arch: "linux/arm64", Status: "active", Digest: "sha256:" + strings.Repeat("0", 64), Size: 3, DownloadURL: baseURL + "/artifacts/missing"},
		}
	})

	installRoot := t.TempDir()
	out, err := runScript(t, srv.URL+"/index.json", "relay-v1", installRoot)
	if err != nil {
		t.Fatalf("install script failed: %v\noutput:\n%s", err, out)
	}

	installed := filepath.Join(installRoot, "waiveo-relay")
	got, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("read installed binary: %v\noutput:\n%s", err, out)
	}
	if string(got) != string(artifact) {
		t.Errorf("installed content = %q, want %q", got, artifact)
	}

	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("stat installed binary: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("installed binary mode = %v, want executable", info.Mode())
	}

	assertNotInstalled(t, installed+".prev")
}

// TestInstallReinstallBacksUpPreviousBinary proves a second run against the
// same INSTALL_ROOT (a new, higher version resolved) replaces the installed
// binary and leaves the PRIOR one recoverable at "<name>.prev" -- the
// documented rollback path (`cp waiveo-relay.prev waiveo-relay`).
func TestInstallReinstallBacksUpPreviousBinary(t *testing.T) {
	installRoot := t.TempDir()

	v1 := []byte("waiveo-relay-fixture-binary-v1\n")
	srv1 := newFixtureServer(t, "relay-v1", map[string][]byte{"waiveo-relay": v1}, func(baseURL string) []channelIndexArtifact {
		return []channelIndexArtifact{
			{ArtifactID: "waiveo-relay", Kind: "relay-release", Version: "1.0.0", Arch: "linux/amd64", Status: "active", Digest: "sha256:" + sha256Hex(v1), Size: int64(len(v1)), DownloadURL: baseURL + "/artifacts/waiveo-relay"},
		}
	})
	if out, err := runScript(t, srv1.URL+"/index.json", "relay-v1", installRoot); err != nil {
		t.Fatalf("first install failed: %v\noutput:\n%s", err, out)
	}

	v2 := []byte("waiveo-relay-fixture-binary-v2-longer\n")
	srv2 := newFixtureServer(t, "relay-v1", map[string][]byte{"waiveo-relay": v2}, func(baseURL string) []channelIndexArtifact {
		return []channelIndexArtifact{
			{ArtifactID: "waiveo-relay", Kind: "relay-release", Version: "1.1.0", Arch: "linux/amd64", Status: "active", Digest: "sha256:" + sha256Hex(v2), Size: int64(len(v2)), DownloadURL: baseURL + "/artifacts/waiveo-relay"},
		}
	})
	out, err := runScript(t, srv2.URL+"/index.json", "relay-v1", installRoot)
	if err != nil {
		t.Fatalf("second install failed: %v\noutput:\n%s", err, out)
	}

	current, err := os.ReadFile(filepath.Join(installRoot, "waiveo-relay"))
	if err != nil {
		t.Fatalf("read current binary: %v\noutput:\n%s", err, out)
	}
	if string(current) != string(v2) {
		t.Errorf("current binary = %q, want v2 %q", current, v2)
	}

	prev, err := os.ReadFile(filepath.Join(installRoot, "waiveo-relay.prev"))
	if err != nil {
		t.Fatalf("read .prev backup: %v", err)
	}
	if string(prev) != string(v1) {
		t.Errorf(".prev backup = %q, want v1 %q", prev, v1)
	}
}

// TestInstallRejectsDigestMismatch proves a served artifact whose bytes do
// not hash to the index's own signed-in-band digest (CHI-021) is a hard
// failure -- nonzero exit, DIGEST_MISMATCH reported, and nothing installed.
func TestInstallRejectsDigestMismatch(t *testing.T) {
	artifact := []byte("waiveo-relay-fixture-binary-corrupt\n")
	wrongDigest := "sha256:" + strings.Repeat("0", 64) // never matches artifact's real hash

	srv := newFixtureServer(t, "relay-v1", map[string][]byte{"waiveo-relay": artifact}, func(baseURL string) []channelIndexArtifact {
		return []channelIndexArtifact{
			{ArtifactID: "waiveo-relay", Kind: "relay-release", Version: "9.9.9", Arch: "linux/amd64", Status: "active", Digest: wrongDigest, Size: int64(len(artifact)), DownloadURL: baseURL + "/artifacts/waiveo-relay"},
		}
	})

	installRoot := t.TempDir()
	out, err := runScript(t, srv.URL+"/index.json", "relay-v1", installRoot)
	if err == nil {
		t.Fatalf("expected install to fail on digest mismatch, it succeeded\noutput:\n%s", out)
	}
	if !strings.Contains(out, "DIGEST_MISMATCH") {
		t.Errorf("output does not report DIGEST_MISMATCH:\n%s", out)
	}
	assertNotInstalled(t, filepath.Join(installRoot, "waiveo-relay"))
}

// TestInstallRejectsChannelMismatch proves the script checks the fetched
// index's own .signed.channel against the CHANNEL argument (a sanity check
// against a misconfigured or migrated INDEX_URL, CHI-080) rather than
// trusting whichever URL happened to answer.
func TestInstallRejectsChannelMismatch(t *testing.T) {
	artifact := []byte("waiveo-relay-fixture-binary\n")
	srv := newFixtureServer(t, "relay-v1", map[string][]byte{"waiveo-relay": artifact}, func(baseURL string) []channelIndexArtifact {
		return []channelIndexArtifact{
			{ArtifactID: "waiveo-relay", Kind: "relay-release", Version: "1.0.0", Arch: "linux/amd64", Status: "active", Digest: "sha256:" + sha256Hex(artifact), Size: int64(len(artifact)), DownloadURL: baseURL + "/artifacts/waiveo-relay"},
		}
	})

	installRoot := t.TempDir()
	out, err := runScript(t, srv.URL+"/index.json", "relay-v2", installRoot) // fixture names relay-v1
	if err == nil {
		t.Fatalf("expected install to fail on channel mismatch, it succeeded\noutput:\n%s", out)
	}
	assertNotInstalled(t, filepath.Join(installRoot, "waiveo-relay"))
}

// TestInstallRejectsNoMatchingArtifact proves an index carrying no active
// relay-release/linux/amd64 entry (only a platform-release entry here) is a
// hard failure with a clear message, never a silent no-op success.
func TestInstallRejectsNoMatchingArtifact(t *testing.T) {
	srv := newFixtureServer(t, "relay-v1", nil, func(baseURL string) []channelIndexArtifact {
		return []channelIndexArtifact{
			{ArtifactID: "waiveo-app", Kind: "platform-release", Version: "1.0.0", Arch: "linux/amd64", Status: "active", Digest: "sha256:" + strings.Repeat("0", 64), Size: 3, DownloadURL: baseURL + "/artifacts/missing"},
		}
	})

	installRoot := t.TempDir()
	out, err := runScript(t, srv.URL+"/index.json", "relay-v1", installRoot)
	if err == nil {
		t.Fatalf("expected install to fail with no matching artifact, it succeeded\noutput:\n%s", out)
	}
	if !strings.Contains(out, "no active relay-release artifact") {
		t.Errorf("output does not explain the no-match failure:\n%s", out)
	}
	assertNotInstalled(t, filepath.Join(installRoot, "waiveo-relay"))
}
