package emergencykit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testWorkspaceID = "01J8Z3K4N5P6Q7R8S9T0V1W2ZC"
	testNow         = int64(1752537600000)
)

func testIDs() func() string {
	n := 0
	return func() string {
		n++
		return "01J8Z3K4N5P6Q7R8S9T0V1W2Z" + string(rune('A'+n-1))
	}
}

func issue(t *testing.T, dir string) Kit {
	t.Helper()
	k, err := Issue(dir, testWorkspaceID, testNow, testIDs())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return k
}

func TestAnIssuedPassphraseVerifies(t *testing.T) {
	dir := t.TempDir()
	kit := issue(t, dir)

	ok, err := Verify(dir, kit.RecoveryPassphrase)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Error("the passphrase this kit was issued with does not verify")
	}
}

// TestRegeneratingInvalidatesThePreviousPassphrase is ARC-114, and it is the
// requirement the whole design serves: a lost or exposed kit is neutralized by
// generating another, without a factory reset.
func TestRegeneratingInvalidatesThePreviousPassphrase(t *testing.T) {
	dir := t.TempDir()
	first := issue(t, dir)
	second := issue(t, dir)

	if first.RecoveryPassphrase == second.RecoveryPassphrase {
		t.Fatal("regenerating produced the same passphrase, so nothing was invalidated")
	}
	if ok, err := Verify(dir, first.RecoveryPassphrase); err != nil {
		t.Fatalf("Verify: %v", err)
	} else if ok {
		t.Error("the previous passphrase still verifies after regenerating (ARC-114)")
	}
	if ok, err := Verify(dir, second.RecoveryPassphrase); err != nil {
		t.Fatalf("Verify: %v", err)
	} else if !ok {
		t.Error("the newly issued passphrase does not verify")
	}
}

// TestThePassphraseIsNotRecoverableFromDisk is the property everything else rests
// on. If the passphrase could be read back, regeneration would rotate a value an
// attacker with disk access could simply read again — and ARC-114's remedy would
// not be a remedy.
//
// It scans every byte this package wrote, in every form the passphrase could
// plausibly take, rather than checking the one file it expects: a future field
// that leaked it would be caught by the scan and missed by the check.
func TestThePassphraseIsNotRecoverableFromDisk(t *testing.T) {
	dir := t.TempDir()
	kit := issue(t, dir)

	forms := []string{
		kit.RecoveryPassphrase,
		strings.ReplaceAll(kit.RecoveryPassphrase, "-", ""),
		strings.ToLower(kit.RecoveryPassphrase),
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the kit directory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("Issue wrote nothing: there is no verifier to check a passphrase against")
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, form := range forms {
			if strings.Contains(string(raw), form) {
				t.Errorf("%s contains the recovery passphrase: it must persist a verifier, never the secret", e.Name())
			}
		}
	}
}

// TestAPassphraseIsAcceptedAsAHumanRetypesIt: this value is read off paper under
// the worst conditions the platform has. Refusing a correct passphrase over a
// missing dash or lowercase letters would fail an operator at exactly the moment
// the kit exists for.
func TestAPassphraseIsAcceptedAsAHumanRetypesIt(t *testing.T) {
	dir := t.TempDir()
	kit := issue(t, dir)

	for _, variant := range []struct{ name, value string }{
		{"as printed", kit.RecoveryPassphrase},
		{"lowercased", strings.ToLower(kit.RecoveryPassphrase)},
		{"no dashes", strings.ReplaceAll(kit.RecoveryPassphrase, "-", "")},
		{"spaces for dashes", strings.ReplaceAll(kit.RecoveryPassphrase, "-", " ")},
		{"surrounding whitespace", "  " + kit.RecoveryPassphrase + "\n"},
	} {
		ok, err := Verify(dir, variant.value)
		if err != nil {
			t.Fatalf("%s: Verify: %v", variant.name, err)
		}
		if !ok {
			t.Errorf("%s: a correct passphrase was refused", variant.name)
		}
	}
}

// TestAWrongPassphraseIsRefused is the other side of the leniency above:
// normalizing must not normalize two different secrets into one.
func TestAWrongPassphraseIsRefused(t *testing.T) {
	dir := t.TempDir()
	kit := issue(t, dir)

	for _, wrong := range []string{
		"",
		"NOTTHERIGHTPASSPHRASEATALL",
		// One character changed: the closest possible miss.
		strings.Replace(kit.RecoveryPassphrase, string(kit.RecoveryPassphrase[0]), "Z", 1),
	} {
		ok, err := Verify(dir, wrong)
		if err != nil {
			t.Fatalf("Verify(%q): %v", wrong, err)
		}
		if ok {
			t.Errorf("Verify(%q) accepted a passphrase that is not the issued one", wrong)
		}
	}
}

// TestNoKitIsDistinctFromAWrongPassphrase: the two have different remedies —
// print a kit versus re-read the one you hold — and an operator told the wrong
// one goes looking for a page that does not exist.
func TestNoKitIsDistinctFromAWrongPassphrase(t *testing.T) {
	if _, err := Verify(t.TempDir(), "anything"); err != ErrNoKit {
		t.Errorf("Verify against a workspace with no kit = %v, want ErrNoKit", err)
	}
	if _, _, _, err := Current(t.TempDir()); err != ErrNoKit {
		t.Errorf("Current against a workspace with no kit = %v, want ErrNoKit", err)
	}
}

// TestTheKitCarriesEnoughToRecoverWithoutAnotherDocument is ARC-111: the kit must
// be sufficient on its own. Asserted on the SUBSTANCE — what it recovers, what it
// does not, what to do, and what to do when it is lost — because a kit that says
// "see the manual" fails in exactly the situation it exists for.
func TestTheKitCarriesEnoughToRecoverWithoutAnotherDocument(t *testing.T) {
	kit := issue(t, t.TempDir())

	if !strings.Contains(kit.Instructions, testWorkspaceID) {
		t.Error("the kit does not identify what it recovers (ARC-111)")
	}
	if kit.WorkspaceID != testWorkspaceID || kit.KitID == "" {
		t.Errorf("kit identity = %q/%q, want the workspace and a kit id", kit.WorkspaceID, kit.KitID)
	}
	// ARC-112: the kit must not leave an operator believing this passphrase opens
	// an export archive. That is the confusion the two-passphrase design invites,
	// so the printed page addresses it directly.
	if !strings.Contains(kit.Instructions, "export") {
		t.Error("the kit does not distinguish the recovery passphrase from an export passphrase (ARC-112)")
	}
	// ARC-114's remedy is only useful to someone who knows it exists.
	if !strings.Contains(strings.ToLower(kit.Instructions), "new kit") {
		t.Error("the kit does not say what to do when it is lost or exposed (ARC-114)")
	}
}

// TestTwoIssuesProduceDifferentPassphrases guards the randomness itself. A mint
// that returned a constant would satisfy every other test here — verification
// would pass, and the rotation test compares the two values, but only this one
// fails for the right reason if the source is fixed.
func TestTwoIssuesProduceDifferentPassphrases(t *testing.T) {
	seen := map[string]bool{}
	for range 8 {
		kit := issue(t, t.TempDir())
		if seen[kit.RecoveryPassphrase] {
			t.Fatalf("a passphrase repeated across issues: %q", kit.RecoveryPassphrase)
		}
		seen[kit.RecoveryPassphrase] = true
	}
}

// TestAKitWithNoWorkspaceIsRefused: half of ARC-111 is identifying what the kit
// recovers, and a blank identifier cannot. Refused at mint rather than printed
// useless.
func TestAKitWithNoWorkspaceIsRefused(t *testing.T) {
	if _, err := Issue(t.TempDir(), "", testNow, testIDs()); err == nil {
		t.Error("a kit was issued with no workspace identifier")
	}
}
