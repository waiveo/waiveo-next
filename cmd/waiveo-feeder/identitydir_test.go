package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The identity every relay pins at enrollment used to load from a hardcoded
// RELATIVE path with no override, so which identity a deployment presented was a
// function of its working directory. Because signing.LoadOrCreate CREATES when it
// finds nothing, a wrong cwd did not fail — it minted a new one, after which every
// enrolled relay refuses the certificate at TLS (REL-137) with no path back but
// re-enrolling the fleet. Two worktrees in two directories did exactly this to the
// lab (HV-23).

func TestIdentityAndEnrollDirsAreOverridableAndAbsolute(t *testing.T) {
	env := map[string]string{
		"WAIVEO_FEEDER_IDENTITY_DIR": "relative/identity",
		"WAIVEO_FEEDER_ENROLL_DIR":   "relative/enroll",
	}
	cfg := loadConfig(func(k string) string { return env[k] })

	for name, got := range map[string]string{
		"identityDir": cfg.identityDir,
		"enrollDir":   cfg.enrollDir,
	} {
		if !filepath.IsAbs(got) {
			t.Errorf("%s = %q, which is RELATIVE — the deployment's identity would then follow the process's working directory, "+
				"and a wrong one mints a fresh identity that locks out every enrolled relay", name, got)
		}
	}
	if !strings.HasSuffix(cfg.identityDir, filepath.Join("relative", "identity")) {
		t.Errorf("identityDir = %q, which does not honour WAIVEO_FEEDER_IDENTITY_DIR", cfg.identityDir)
	}
	if !strings.HasSuffix(cfg.enrollDir, filepath.Join("relative", "enroll")) {
		t.Errorf("enrollDir = %q, which does not honour WAIVEO_FEEDER_ENROLL_DIR", cfg.enrollDir)
	}
}

func TestIdentityDirDefaultsAreStillAbsolute(t *testing.T) {
	// The defaults are the make-dev-local relative constants. Pinning them to
	// absolute AT CONFIG LOAD is what makes the identity independent of where the
	// process is launched from, which is the whole fix — a default that stays
	// relative reintroduces it for every deployment that sets no env var, i.e.
	// the appliance.
	cfg := loadConfig(func(string) string { return "" })
	if !filepath.IsAbs(cfg.identityDir) || !filepath.IsAbs(cfg.enrollDir) {
		t.Fatalf("defaults are not absolute: identityDir=%q enrollDir=%q", cfg.identityDir, cfg.enrollDir)
	}
}
