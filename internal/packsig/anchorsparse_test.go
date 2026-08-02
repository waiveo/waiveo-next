package packsig

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// anchorsparse_test.go pins the trust-anchor document parser.
//
// These refusals are load-bearing rather than tidy: VerifyBundle fails CLOSED on
// an anchors resolution error — "never treated as 'no anchors, admit anyway'" —
// so each of these is what turns a misconfigured or tampered trust root into a
// refusal instead of a silently different set of trusted publishers. A mutation
// sweep found them pinned by nothing.
//
// The distinction the file's own doc draws is the one worth keeping: an ABSENT
// document means "not provisioned" and yields no keys quietly, while a PRESENT
// but malformed one is an error, so misconfiguration stays distinguishable from
// a host that was never given a trust root.

// writeAnchors writes an anchors document with owner-only permissions, which the
// parser requires before it will read a trust root at all.
func writeAnchors(t *testing.T, doc any) string {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal anchors: %v", err)
	}
	path := filepath.Join(t.TempDir(), "anchors.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write anchors: %v", err)
	}
	return path
}

func anchorDoc(format string, entries ...map[string]string) map[string]any {
	as := make([]map[string]string, 0, len(entries))
	as = append(as, entries...)
	return map[string]any{"format": format, "anchors": as}
}

func b64Key(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

// TestAnchorsDocumentFormatIsChecked: a document of another format is refused,
// not read as if it were this one. The format string is the only thing
// distinguishing this trust root from any other JSON file with an "anchors" key.
func TestAnchorsDocumentFormatIsChecked(t *testing.T) {
	path := writeAnchors(t, anchorDoc("pack-trust-anchors/2",
		map[string]string{"namespace": "acme", "key_id": "k1", "public_key": b64Key(t)}))

	keys, err := FileAnchors{Path: path}.KeysFor("acme")
	if err == nil {
		t.Fatalf("a document declaring another format was accepted, yielding %d key(s) — the verifier would "+
			"trust publishers named by a file it does not understand", len(keys))
	}
	if !strings.Contains(err.Error(), "format") {
		t.Errorf("refused with %q, want the format rule", err)
	}
}

// TestAnchorsPublicKeyMustBeAWellFormedEd25519Key covers both halves of the key
// check: it must decode as base64, AND be exactly an ed25519 public key's
// length. The length half matters on its own — a truncated key still decodes,
// and ed25519.Verify on a wrong-length key does not report a helpful failure, it
// simply never verifies, which would look like a signature problem rather than a
// provisioning one.
func TestAnchorsPublicKeyMustBeAWellFormedEd25519Key(t *testing.T) {
	for _, tc := range []struct{ name, key string }{
		{"not base64 at all", "!!!!not base64!!!!"},
		{"valid base64, too short", base64.StdEncoding.EncodeToString([]byte("short"))},
		{"valid base64, too long", base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize+1))},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeAnchors(t, anchorDoc(AnchorsFormat,
				map[string]string{"namespace": "acme", "key_id": "k1", "public_key": tc.key}))
			keys, err := FileAnchors{Path: path}.KeysFor("acme")
			if err == nil {
				t.Fatalf("an anchor carrying %s was accepted, yielding %d key(s)", tc.name, len(keys))
			}
			if !strings.Contains(err.Error(), "invalid public key") {
				t.Errorf("refused with %q, want the public-key rule", err)
			}
		})
	}

	// The control: a real key is returned, so none of the above is a parser that
	// refuses every document.
	key := b64Key(t)
	path := writeAnchors(t, anchorDoc(AnchorsFormat,
		map[string]string{"namespace": "acme", "key_id": "k1", "public_key": key}))
	keys, err := FileAnchors{Path: path}.KeysFor("acme")
	if err != nil {
		t.Fatalf("a well-formed anchors document was refused: %v", err)
	}
	if len(keys) != 1 || keys[0].KeyID != "k1" || len(keys[0].PublicKey) != ed25519.PublicKeySize {
		t.Fatalf("a well-formed document yielded %+v, want one 32-byte key named k1", keys)
	}
}

// TestAnchorsAreScopedToTheirNamespace pins the filter every anchor passes
// through, including the case that makes it a real scoping rule rather than an
// optimisation: a MALFORMED anchor belonging to another namespace must not
// refuse this one's resolution. The decode happens after the namespace check for
// exactly that reason, and a reordering would let any publisher's bad entry deny
// service to every other publisher on the box.
func TestAnchorsAreScopedToTheirNamespace(t *testing.T) {
	good := b64Key(t)
	path := writeAnchors(t, anchorDoc(AnchorsFormat,
		map[string]string{"namespace": "acme", "key_id": "k1", "public_key": good},
		map[string]string{"namespace": "other", "key_id": "k2", "public_key": "!!!not base64!!!"},
	))

	keys, err := FileAnchors{Path: path}.KeysFor("acme")
	if err != nil {
		t.Fatalf("a broken anchor in ANOTHER namespace denied resolution for acme: %v", err)
	}
	if len(keys) != 1 || keys[0].KeyID != "k1" {
		t.Fatalf("acme resolved to %+v, want only its own k1", keys)
	}

	// And a namespace with no anchors resolves to nothing, without error: an
	// unprovisioned publisher is untrusted, not a failure.
	if keys, err := (FileAnchors{Path: path}).KeysFor("nobody"); err != nil || len(keys) != 0 {
		t.Fatalf("an unprovisioned namespace = (%+v, %v), want no keys and no error", keys, err)
	}
}

// TestAbsentAnchorsDocumentIsNotAnError pins the distinction the parser's own
// doc draws: an absent document means "not provisioned" and yields no keys
// quietly, while a present-but-malformed one is loud. Collapsing the two would
// make a host that lost its trust root indistinguishable from one that never had
// one.
func TestAbsentAnchorsDocumentIsNotAnError(t *testing.T) {
	keys, err := FileAnchors{Path: filepath.Join(t.TempDir(), "nothing-here.json")}.KeysFor("acme")
	if err != nil {
		t.Fatalf("an absent anchors document errored: %v — an unprovisioned host must fail closed quietly", err)
	}
	if len(keys) != 0 {
		t.Fatalf("an absent anchors document yielded %d key(s), want none", len(keys))
	}
}
