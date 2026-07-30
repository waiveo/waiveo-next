// Package emergencykit implements archive/1's emergency kit (ARC-110-114): the
// printed artifact that recovers a workspace when its ordinary credentials are
// gone and its hardware may be too.
//
// # The one property everything here is built around
//
// A kit's recovery passphrase is returned EXACTLY ONCE, by the call that mints
// it, and is never recoverable afterwards — not from this package, not from
// disk. What persists is a verifier: an argon2id hash the passphrase can be
// checked against and from which it cannot be recovered.
//
// That shape is not a precaution, it is what makes ARC-114 mean anything.
// Regenerating a kit "MUST invalidate the previously issued recovery
// passphrase", so a lost or exposed kit is neutralized by regenerating it. If
// the passphrase were retrievable, regeneration would rotate a value an attacker
// holding disk access could simply read again, and the remedy would not be one.
//
// It also decides the API's shape by removing a function: there is no Get. A
// caller that needs to show an operator their passphrase has exactly one option —
// issue a new kit, invalidating the old — which is the correct answer to "I lost
// my kit" and the only answer this package can honestly give.
//
// # What this package does NOT do, stated because the gap is the interesting part
//
// It does not deliver the kit. ARC-113 forbids transmitting a kit's content
// electronically and calls a kit "a printed or otherwise physically-delivered
// artifact by design", which makes delivery a product decision about a surface —
// where an operator is present, what they are shown, and for how long — rather
// than a library one. Issue hands its content to a caller that has decided; this
// package has no opinion beyond refusing to keep a copy.
//
// Nothing calls Issue yet. The natural caller is first-boot: the workspace data
// key is established in internal/app/workspacekey.LoadOrCreate, which is
// ARC-110's "the point a workspace's data key is first established", and the
// operator is present at the setup surface. Wiring it there needs the delivery
// decision above.
//
// # Two passphrases, and they are not interchangeable
//
// ARC-112 makes the recovery passphrase (this package) and the export passphrase
// (internal/archive) distinct secrets protecting distinct recoveries: a recovery
// passphrase must not by itself decrypt any archive container, and an export
// passphrase must not by itself recover a workspace's data key on its original
// hardware. Nothing here reads or derives from an export passphrase, and
// internal/archive has no reference to this package — the separation is that
// neither can reach the other, rather than a rule either is asked to follow.
package emergencykit

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/maaxton/waiveo-next/internal/shared/secretfile"
)

// verifierFile is the only thing this package persists.
const verifierFile = "emergency-kit.json"

// Passphrase shape. Base32 without padding over 20 random bytes gives 160 bits
// in 32 characters, grouped for transcription.
//
// Grouping matters more than it looks: this is a value a human reads off paper
// and types back under the worst conditions the platform has — hardware dead,
// credentials gone, probably in a hurry. Base32's alphabet has no 0/O or 1/I/l
// confusion, and the groups give the eye somewhere to rest.
const (
	passphraseBytes = 20
	groupSize       = 4
)

// KDF cost for the verifier. Deliberately lighter than an archive's export KDF:
// this hash guards a 160-bit random value rather than a human-chosen secret, so
// there is no dictionary to slow down — the work factor exists to make a stolen
// verifier file useless rather than to compensate for a weak input.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16
)

// Kit is a kit's content (ARC-111): the recovery passphrase, an identifier for
// what it recovers, and instructions sufficient to complete recovery using only
// the kit itself and the platform's ordinary recovery tooling — no other
// document.
//
// It is returned by Issue and by nothing else. A caller that lets one escape into
// a log, an email, or a page left rendered has broken ARC-113 on this package's
// behalf; a caller that prints it and drops the value has satisfied it.
type Kit struct {
	// KitID identifies this issuance, so an operator holding two printouts can
	// tell which is current without either carrying a passphrase to compare.
	KitID string
	// WorkspaceID is what this kit recovers (ARC-111's "identifier for the
	// workspace or hardware it recovers").
	WorkspaceID string
	// RecoveryPassphrase is the secret itself, in transcription-friendly groups.
	RecoveryPassphrase string
	IssuedAt           int64
	// Instructions is the recovery procedure, carried IN the kit because ARC-111
	// requires the kit alone to be sufficient. A kit that says "see the manual" is
	// a kit that fails in exactly the situation it exists for.
	Instructions string
}

// record is the persisted verifier. It holds no passphrase and no material from
// which one can be derived.
type record struct {
	KitID       string `json:"kit_id"`
	WorkspaceID string `json:"workspace_id"`
	IssuedAt    int64  `json:"issued_at"`
	Salt        []byte `json:"salt"`
	Verifier    []byte `json:"verifier"`
}

// ErrNoKit reports that this workspace has never had a kit issued.
var ErrNoKit = errors.New("emergencykit: no kit has been issued for this workspace")

// Issue mints a fresh recovery passphrase, replaces any previously stored
// verifier, and returns the kit content — once.
//
// Replacing the verifier IS ARC-114's invalidation: the previous passphrase stops
// verifying at the moment this returns, so a lost or exposed kit is neutralized
// by issuing another one, without a factory reset of the workspace it protects.
func Issue(dir, workspaceID string, nowMs int64, newID func() string) (Kit, error) {
	if dir == "" {
		return Kit{}, errors.New("emergencykit: empty kit directory")
	}
	if workspaceID == "" {
		// A kit whose identifier is blank cannot tell an operator what it recovers,
		// which is half of ARC-111. Refused at mint rather than printed useless.
		return Kit{}, errors.New("emergencykit: empty workspace id: a kit must identify what it recovers (ARC-111)")
	}
	if err := secretfile.EnsureDir(dir); err != nil {
		return Kit{}, fmt.Errorf("emergencykit: prepare kit directory: %w", err)
	}

	passphrase, err := newPassphrase()
	if err != nil {
		return Kit{}, err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return Kit{}, fmt.Errorf("emergencykit: salt: %w", err)
	}

	rec := record{
		KitID:       newID(),
		WorkspaceID: workspaceID,
		IssuedAt:    nowMs,
		Salt:        salt,
		Verifier:    derive(passphrase, salt),
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return Kit{}, fmt.Errorf("emergencykit: encode verifier: %w", err)
	}
	// Written before the kit is returned, so a caller that prints a passphrase is
	// always printing one this workspace will actually accept. The other order —
	// return, then persist — can hand an operator a kit that verifies against
	// nothing if the write fails.
	if err := secretfile.Write(filepath.Join(dir, verifierFile), encoded); err != nil {
		return Kit{}, fmt.Errorf("emergencykit: write verifier: %w", err)
	}

	return Kit{
		KitID:              rec.KitID,
		WorkspaceID:        workspaceID,
		RecoveryPassphrase: passphrase,
		IssuedAt:           nowMs,
		Instructions:       instructions(workspaceID),
	}, nil
}

// Verify reports whether passphrase is the one the current kit carries.
//
// A workspace with no kit answers ErrNoKit rather than false: "wrong passphrase"
// and "no kit was ever issued" are different situations with different remedies,
// and collapsing them would tell an operator to re-read a printout that does not
// exist.
func Verify(dir, passphrase string) (bool, error) {
	rec, err := loadRecord(dir)
	if err != nil {
		return false, err
	}
	// Compared in constant time. The verifier is a hash rather than the secret, so
	// a timing leak here reveals less than it would against the passphrase itself —
	// but "less" is not "nothing", and an early-exit compare on a value an attacker
	// can submit repeatedly is the habit worth not forming.
	got := derive(normalize(passphrase), rec.Salt)
	return subtle.ConstantTimeCompare(got, rec.Verifier) == 1, nil
}

// Current returns the metadata of the kit in force — never its passphrase, which
// no longer exists anywhere by then. It answers "which printout is current" for
// an operator holding several.
func Current(dir string) (kitID, workspaceID string, issuedAt int64, err error) {
	rec, err := loadRecord(dir)
	if err != nil {
		return "", "", 0, err
	}
	return rec.KitID, rec.WorkspaceID, rec.IssuedAt, nil
}

func loadRecord(dir string) (record, error) {
	raw, err := os.ReadFile(filepath.Join(dir, verifierFile))
	if errors.Is(err, os.ErrNotExist) {
		return record{}, ErrNoKit
	}
	if err != nil {
		return record{}, fmt.Errorf("emergencykit: read verifier: %w", err)
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return record{}, fmt.Errorf("emergencykit: decode verifier: %w", err)
	}
	if len(rec.Salt) == 0 || len(rec.Verifier) == 0 {
		return record{}, fmt.Errorf("emergencykit: verifier file is present but carries no verifier material")
	}
	return rec, nil
}

func derive(passphrase string, salt []byte) []byte {
	return argon2.IDKey([]byte(passphrase), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
}

// newPassphrase mints a grouped base32 passphrase over passphraseBytes of
// randomness.
func newPassphrase() (string, error) {
	raw := make([]byte, passphraseBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("emergencykit: passphrase: %w", err)
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	var b strings.Builder
	for i, r := range encoded {
		if i > 0 && i%groupSize == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String(), nil
}

// normalize accepts a passphrase the way a human retypes one from paper:
// whatever case they used, with or without the grouping dashes, and with stray
// whitespace. None of that changes the secret, and refusing a correct passphrase
// over a missing dash would fail an operator at the worst possible moment.
func normalize(passphrase string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(passphrase) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '2' && r <= '7':
			b.WriteRune(r)
		}
	}
	// Re-group, so the normalized form matches what Issue hashed.
	grouped := b.String()
	var out strings.Builder
	for i, r := range grouped {
		if i > 0 && i%groupSize == 0 {
			out.WriteByte('-')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// instructions is the recovery procedure printed on the kit. ARC-111 requires the
// kit alone to be sufficient, so this says what to do rather than where to read
// about it.
func instructions(workspaceID string) string {
	return strings.Join([]string{
		"WAIVEO EMERGENCY RECOVERY KIT",
		"",
		"Keep this page. Store it somewhere physical and separate from the hardware",
		"it recovers. Anyone holding it can recover this workspace's data.",
		"",
		"Workspace: " + workspaceID,
		"",
		"WHAT THIS RECOVERS",
		"  The workspace data key, on this workspace's own hardware, when its",
		"  ordinary credentials are unavailable.",
		"",
		"WHAT THIS DOES NOT DO",
		"  It does not open a workspace export archive. An export is protected by a",
		"  separate export passphrase chosen when the export is taken. Neither",
		"  passphrase substitutes for the other.",
		"",
		"TO RECOVER",
		"  1. Reach the appliance's local console.",
		"  2. Choose recovery, then enter the passphrase above exactly as printed.",
		"     Case and dashes do not matter.",
		"  3. Set new credentials when prompted.",
		"",
		"IF THIS PAGE IS LOST OR SEEN BY SOMEONE ELSE",
		"  Generate a new kit. Doing so cancels the passphrase above immediately,",
		"  and no factory reset is needed. The passphrase cannot be looked up or",
		"  re-printed - a new kit is the only way to obtain a working one.",
	}, "\n")
}
