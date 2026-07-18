package tlsboot

import (
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden fixture under testdata/ is the ONE committed cross-implementation
// vector for the PLY-052 pairing-pin commitment: a fixed relay bootstrap
// certificate and the 16-byte SPKI commitment every conforming player/1 client
// — this Go code and the BrightScript player-v3 on real Roku hardware — must
// reproduce byte-for-byte from the same inputs. It is minted once by
// gen_fixture_temp (since deleted) and never regenerated; regenerating would
// mint a new random cert and silently break the cross-impl contract. These
// tests lock the vector: they fail loudly if the Go commitment algorithm ever
// drifts from the committed bytes the BrightScript unit test also asserts
// against (player-1.md:162 draft-note — confirm SHA-256-at-truncation on real
// client hardware before PLY-052 leaves draft; findings.md:294 — one on-device
// digest against a published vector).
const (
	goldenCertPEM   = "testdata/relay-cert.pem"
	goldenCertDER   = "testdata/relay-cert.der"
	goldenSPKIDER   = "testdata/relay-spki.der"
	goldenCommitHex = "testdata/relay-spki-commitment.hex"
)

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(name))
	if err != nil {
		t.Fatalf("read golden fixture %s: %v", name, err)
	}
	return b
}

// TestGoldenFixtureInternallyConsistent checks the committed artifacts agree
// with each other under the real code path: the PEM decodes to exactly the
// committed DER, the DER's SPKI is exactly the committed SPKI, and the
// commitment over that SPKI is exactly the committed hex — via both the raw
// Commitment and the CommitmentForCertDER convenience form.
func TestGoldenFixtureInternallyConsistent(t *testing.T) {
	certPEM := readGolden(t, goldenCertPEM)
	certDER := readGolden(t, goldenCertDER)
	spkiDER := readGolden(t, goldenSPKIDER)
	wantHex := strings.TrimSpace(string(readGolden(t, goldenCommitHex)))

	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("golden PEM is not a CERTIFICATE block")
	}
	if !equalBytes(block.Bytes, certDER) {
		t.Fatalf("golden PEM decodes to different DER than relay-cert.der (%d vs %d bytes)", len(block.Bytes), len(certDER))
	}

	gotSPKI, err := SPKIFromCertDER(certDER)
	if err != nil {
		t.Fatalf("SPKIFromCertDER(golden der): %v", err)
	}
	if !equalBytes(gotSPKI, spkiDER) {
		t.Fatalf("SPKI extracted from golden DER != committed relay-spki.der")
	}

	if got := hex.EncodeToString(Commitment(spkiDER)); got != wantHex {
		t.Fatalf("Commitment(golden spki) = %s, want committed %s", got, wantHex)
	}

	gotCommit, err := CommitmentForCertDER(certDER)
	if err != nil {
		t.Fatalf("CommitmentForCertDER(golden der): %v", err)
	}
	if got := hex.EncodeToString(gotCommit); got != wantHex {
		t.Fatalf("CommitmentForCertDER(golden der) = %s, want committed %s", got, wantHex)
	}
}

// TestGoldenCommitmentIsSixteenBytes pins the committed vector's width (16
// bytes / 128 bits, above PLY-052's >=80-bit floor) so a change to
// commitmentBytes can't silently pass while the BrightScript side still
// truncates to the old width.
func TestGoldenCommitmentIsSixteenBytes(t *testing.T) {
	wantHex := strings.TrimSpace(string(readGolden(t, goldenCommitHex)))
	raw, err := hex.DecodeString(wantHex)
	if err != nil {
		t.Fatalf("decode committed commitment hex: %v", err)
	}
	if len(raw) != commitmentBytes {
		t.Fatalf("committed commitment is %d bytes, want %d (commitmentBytes)", len(raw), commitmentBytes)
	}
}

// TestGoldenVerifyRoundTrip proves a player fetching this exact cert and
// checking it against the OOB commitment (VerifyCommitmentForCertDER) accepts
// it — the positive on-device path — while a one-bit corruption of the
// commitment is rejected (fail-closed), the same behavior the BrightScript
// compare must exhibit.
func TestGoldenVerifyRoundTrip(t *testing.T) {
	certDER := readGolden(t, goldenCertDER)
	commit, err := hex.DecodeString(strings.TrimSpace(string(readGolden(t, goldenCommitHex))))
	if err != nil {
		t.Fatalf("decode committed commitment hex: %v", err)
	}

	ok, err := VerifyCommitmentForCertDER(certDER, commit)
	if err != nil {
		t.Fatalf("VerifyCommitmentForCertDER(golden): %v", err)
	}
	if !ok {
		t.Fatalf("golden cert did not verify against its own committed commitment")
	}

	tampered := append([]byte(nil), commit...)
	tampered[0] ^= 0x01
	ok, err = VerifyCommitmentForCertDER(certDER, tampered)
	if err != nil {
		t.Fatalf("VerifyCommitmentForCertDER(golden, tampered): unexpected error %v", err)
	}
	if ok {
		t.Fatalf("a one-bit-corrupted commitment verified against the golden cert — pin is not fail-closed")
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
