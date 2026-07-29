package devcred

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sampleKey is a well-formed api-key token shape (auth.MintToken's
// `wv_<aal>_<64 hex>`), with an obviously-fake secret segment. It is test data,
// never a credential.
const sampleKey = "wv_std_" + "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// withKeyFile points PathEnv at a fresh file holding token at mode, and returns
// its path.
func withKeyFile(t *testing.T, token string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), KeyFile)
	if err := os.WriteFile(path, []byte(token+"\n"), mode); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	// os.WriteFile's mode is masked by the process umask, so set it explicitly —
	// otherwise the permission cases below would test the umask, not Load.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod key file: %v", err)
	}
	t.Setenv(PathEnv, path)
	return path
}

func TestLoadReadsAWellFormedKey(t *testing.T) {
	withKeyFile(t, sampleKey, 0o600)
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != sampleKey {
		t.Fatalf("Load returned a different token than the file held")
	}
}

// TestLoadRefusesAnAccessibleKeyFile is the discipline packsig.FileAnchors
// applies to a trust root, tightened for a secret: a trust anchors document is
// refused when group- or world-WRITABLE, and this file is refused when group- or
// world-readable too, because readability is how a bearer credential leaks.
func TestLoadRefusesAnAccessibleKeyFile(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o604, 0o640, 0o660, 0o666} {
		withKeyFile(t, sampleKey, mode)
		_, err := Load()
		if err == nil {
			t.Fatalf("mode %04o was accepted; want a refusal", mode)
		}
		if !strings.Contains(err.Error(), ProvisionCommand) {
			t.Fatalf("mode %04o refusal does not name %q: %v", mode, ProvisionCommand, err)
		}
	}
}

// TestLoadRefusalsNameTheProvisioningStepAndNeverTheKey covers the two rules the
// package doc commits to: a refusal tells the developer exactly what to run, and
// no refusal ever renders the file's contents.
func TestLoadRefusalsNameTheProvisioningStepAndNeverTheKey(t *testing.T) {
	cases := []struct {
		name    string
		content string
		mode    os.FileMode
		absent  bool
	}{
		{name: "absent", absent: true},
		{name: "empty", content: "", mode: 0o600},
		{name: "not a token", content: "hunter2-not-a-token", mode: 0o600},
		{name: "session token, not an api key", content: "wvs_std_" + strings.Repeat("a", 64), mode: 0o600},
		{name: "world readable", content: sampleKey, mode: 0o644},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.absent {
				t.Setenv(PathEnv, filepath.Join(t.TempDir(), "nope"))
			} else {
				withKeyFile(t, tc.content, tc.mode)
			}
			_, err := Load()
			if err == nil {
				t.Fatalf("want a refusal")
			}
			if !strings.Contains(err.Error(), ProvisionCommand) {
				t.Fatalf("refusal does not name %q: %v", ProvisionCommand, err)
			}
			if tc.content != "" && strings.Contains(err.Error(), tc.content) {
				t.Fatalf("refusal rendered the file's contents: %v", err)
			}
		})
	}
}

// TestWriteThenLoadRoundTripsAt0600 proves the writer produces exactly what the
// reader insists on, so a provisioning run can never leave a file its own probes
// will refuse.
func TestWriteThenLoadRoundTripsAt0600(t *testing.T) {
	t.Setenv(PathEnv, filepath.Join(t.TempDir(), "keys", KeyFile))
	path, err := Write(sampleKey)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("written mode %04o, want 0600", mode)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := dir.Mode().Perm(); mode != 0o700 {
		t.Fatalf("parent dir mode %04o, want 0700", mode)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load after Write: %v", err)
	}
	if got != sampleKey {
		t.Fatalf("round trip changed the token")
	}
}

// TestClientAttachesBearerOnLoopback is the whole point of the helper: the
// probes get a client that authenticates without any of them constructing a
// header.
func TestClientAttachesBearerOnLoopback(t *testing.T) {
	withKeyFile(t, sampleKey, 0o600)

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	c, err := Client(3 * time.Second)
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	resp, err := c.Get(srv.URL) // httptest binds 127.0.0.1
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if want := "Bearer " + sampleKey; got != want {
		t.Fatalf("Authorization header = %q, want the bearer credential", got)
	}
}

// TestClientWithholdsTheKeyOffLoopback is the leak guard. A header attached by a
// RoundTripper is re-added on every hop, so a probe that followed a redirect off
// the box would carry the credential to whatever answered; the transport
// attaches it to loopback destinations only.
func TestClientWithholdsTheKeyOffLoopback(t *testing.T) {
	withKeyFile(t, sampleKey, 0o600)

	offBox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("credential followed a redirect off the loopback host: %q", h)
		}
	}))
	defer offBox.Close()

	// A URL whose host is a name rather than a loopback literal — the transport
	// must not resolve it and must not trust it.
	target := strings.Replace(offBox.URL, "127.0.0.1", "localhost", 1)

	c, err := Client(3 * time.Second)
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	resp, err := c.Get(target)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
}

// TestInsecureClientCarriesNoCredential pins that the uncredentialed client is
// genuinely uncredentialed — the one scripts/contentloop uses to stand in for a
// screen fetching from the content origin.
func TestInsecureClientCarriesNoCredential(t *testing.T) {
	withKeyFile(t, sampleKey, 0o600)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("InsecureClient sent %q", h)
		}
	}))
	defer srv.Close()

	resp, err := InsecureClient(3 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
}

func TestPathPrefersTheEnvOverride(t *testing.T) {
	t.Setenv(PathEnv, "")
	if want := filepath.Join(DefaultDir, KeyFile); Path() != want {
		t.Fatalf("default Path() = %q, want %q", Path(), want)
	}
	t.Setenv(PathEnv, "/tmp/elsewhere/key")
	if Path() != "/tmp/elsewhere/key" {
		t.Fatalf("override ignored: %q", Path())
	}
}
