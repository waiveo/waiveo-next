package secretseal

import (
	"bytes"
	"errors"
	"testing"
)

func testKey(b byte) []byte {
	k := make([]byte, KeySize)
	for i := range k {
		k[i] = b ^ byte(i)
	}
	return k
}

func TestSealRoundTrips(t *testing.T) {
	s, err := New(testKey(0x11))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	secret := []byte("a TOTP shared secret's raw bytes")
	sealed, err := s.Seal(secret, []byte("credential:01J8"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains([]byte(sealed), secret) {
		t.Fatal("the sealed form contains the plaintext verbatim")
	}
	got, err := s.Open(sealed, []byte("credential:01J8"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("Open returned %q, want %q", got, secret)
	}
}

// TestSealIsNonDeterministic proves the nonce is fresh per call: two seals of
// the same plaintext under the same key and context must differ, or a database
// reader could tell that two principals enrolled the same secret.
func TestSealIsNonDeterministic(t *testing.T) {
	s, _ := New(testKey(0x22))
	a, err := s.Seal([]byte("same"), []byte("ctx"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b, err := s.Seal([]byte("same"), []byte("ctx"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if a == b {
		t.Fatal("two seals of identical plaintext produced identical ciphertext — the nonce is not fresh per call")
	}
}

// TestOpenRefusesAForeignContext is the row-binding property the package doc
// calls unskippable: a blob lifted onto another row does not open.
func TestOpenRefusesAForeignContext(t *testing.T) {
	s, _ := New(testKey(0x33))
	sealed, err := s.Seal([]byte("victim secret"), []byte("credential:VICTIM"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := s.Open(sealed, []byte("credential:ATTACKER")); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("opening under a different context returned %v; want ErrOpenFailed — a copied row must not authenticate", err)
	}
}

func TestOpenRefusesAForeignKey(t *testing.T) {
	mine, _ := New(testKey(0x44))
	theirs, _ := New(testKey(0x45))
	sealed, err := mine.Seal([]byte("secret"), []byte("ctx"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := theirs.Open(sealed, []byte("ctx")); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("opening under a different key returned %v; want ErrOpenFailed", err)
	}
}

func TestOpenRefusesTamperedCiphertext(t *testing.T) {
	s, _ := New(testKey(0x55))
	sealed, err := s.Seal([]byte("secret"), []byte("ctx"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Flip one base64 symbol; any bit change must fail authentication.
	b := []byte(sealed)
	if b[len(b)-2] == 'A' {
		b[len(b)-2] = 'B'
	} else {
		b[len(b)-2] = 'A'
	}
	if _, err := s.Open(string(b), []byte("ctx")); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("opening tampered ciphertext returned %v; want ErrOpenFailed", err)
	}
}

func TestEmptyContextIsRefused(t *testing.T) {
	s, _ := New(testKey(0x66))
	if _, err := s.Seal([]byte("secret"), nil); err == nil {
		t.Fatal("Seal accepted an empty context; binding is mandatory")
	}
	if _, err := s.Open("AAAA", nil); err == nil {
		t.Fatal("Open accepted an empty context; binding is mandatory")
	}
}

func TestWrongKeySizeIsRefused(t *testing.T) {
	if _, err := New(make([]byte, KeySize-1)); err == nil {
		t.Fatal("New accepted a short key")
	}
}

func TestOpenRefusesGarbage(t *testing.T) {
	s, _ := New(testKey(0x77))
	for _, in := range []string{"", "not base64 !!!", "AAAA"} {
		if _, err := s.Open(in, []byte("ctx")); !errors.Is(err, ErrOpenFailed) {
			t.Fatalf("Open(%q) returned %v; want ErrOpenFailed", in, err)
		}
	}
}
