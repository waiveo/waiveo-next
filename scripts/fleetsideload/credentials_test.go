package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The real dev-lab env file is a list of shell `export` lines. Parsing it
// wrong does not fail loudly — it produces "no Roku dev password" against a
// file that plainly contains one, and sends the operator to the vault for a
// credential that was never missing.
func TestLoadEnvFileParsesExportLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev-lab.env")
	body := strings.Join([]string{
		"# Waiveo Dev Lab Environment",
		"",
		"export ROKU_DEV_IP=192.168.50.21",
		`export ROKU_DEV_USER="rokudev"`,
		"export ROKU_DEV_PASSWORD='s3cret'",
		"BARE_KEY=bare",
		"not an assignment",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	values, err := loadEnvFile(path)
	if err != nil {
		t.Fatalf("loadEnvFile: %v", err)
	}
	for k, want := range map[string]string{
		"ROKU_DEV_IP":       "192.168.50.21",
		"ROKU_DEV_USER":     "rokudev",
		"ROKU_DEV_PASSWORD": "s3cret",
		"BARE_KEY":          "bare",
	} {
		if values[k] != want {
			t.Errorf("values[%q] = %q, want %q", k, values[k], want)
		}
	}
	if _, ok := values["not an assignment"]; ok {
		t.Error("a non-assignment line became a key")
	}
}

func TestLoadEnvFileMissingIsNotAnError(t *testing.T) {
	// The process environment alone may carry everything needed (a CI runner,
	// or a caller who already eval'd the shell helper), so an absent file is a
	// legitimate state, not a failure.
	values, err := loadEnvFile(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatalf("a missing env file must not be fatal: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("missing file yielded %v", values)
	}
}

func TestResolveCredentialsEnvironmentBeatsFile(t *testing.T) {
	env := map[string]string{"ROKU_DEV_PASSWORD": "from-env"}
	file := map[string]string{"ROKU_DEV_PASSWORD": "from-file", "ROKU_DEV_USER": "fileuser"}

	creds, err := resolveCredentials(func(k string) string { return env[k] }, file, "", "/tmp/x.env")
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if creds.Password != "from-env" {
		t.Errorf("password came from the file; an explicitly exported value must win")
	}
	if creds.User != "fileuser" {
		t.Errorf("user = %q, want the file's value when the environment sets none", creds.User)
	}
}

func TestResolveCredentialsUserFallbackAndOverride(t *testing.T) {
	none := func(string) string { return "" }
	creds, err := resolveCredentials(none, map[string]string{"ROKU_DEV_PASSWORD": "pw"}, "", "")
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if creds.User != defaultRokuUser {
		t.Errorf("user = %q, want the platform default %q", creds.User, defaultRokuUser)
	}

	overridden, err := resolveCredentials(none, map[string]string{"ROKU_DEV_PASSWORD": "pw", "ROKU_DEV_USER": "fileuser"}, "flaguser", "")
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if overridden.User != "flaguser" {
		t.Errorf("user = %q, want the -user override to beat every other source", overridden.User)
	}
}

func TestResolveCredentialsMissingPasswordPointsAtTheFile(t *testing.T) {
	_, err := resolveCredentials(func(string) string { return "" }, map[string]string{}, "", "/home/op/.config/waiveo/dev-lab.env")
	if err == nil {
		t.Fatal("a missing password resolved to a credential")
	}
	if !strings.Contains(err.Error(), "/home/op/.config/waiveo/dev-lab.env") {
		t.Errorf("error %q does not name the file the operator has to fix", err)
	}
}

func TestDefaultEnvFilePathHonoursOverride(t *testing.T) {
	got := defaultEnvFilePath(func(k string) string {
		if k == envFileVar {
			return "/custom/place.env"
		}
		return ""
	})
	if got != "/custom/place.env" {
		t.Errorf("defaultEnvFilePath = %q, want the %s override honoured (same variable the legacy shell helper reads)", got, envFileVar)
	}
}
