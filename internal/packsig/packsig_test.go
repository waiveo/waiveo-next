package packsig_test

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/packsig"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// zipOf builds an in-memory zip from a name→body map.
func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// extract reads a zip back into a name→bytes map (regular files only).
func extract(t *testing.T, artifact []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(artifact), int64(len(artifact)))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %q: %v", f.Name, err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatalf("read %q: %v", f.Name, err)
		}
		rc.Close()
		out[f.Name] = buf.Bytes()
	}
	return out
}

const testPackFilesManifest = `{"id":"acme/menu-board","version":"1.0.0"}`

func testArtifact(t *testing.T) []byte {
	t.Helper()
	return zipOf(t, map[string]string{
		"manifest.json":    testPackFilesManifest,
		"ui/main.json":     `{"pageType":"dashboard"}`,
		"messages/en.json": `{"k":"v"}`,
	})
}

func signedTestArtifact(t *testing.T) ([]byte, packsig.StaticAnchors) {
	t.Helper()
	pub, priv := signhash.GenerateKey()
	keyID := packsig.KeyIDFor(pub)
	signed, err := packsig.Sign(testArtifact(t), "acme/menu-board", "1.0.0", keyID, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	anchors := packsig.StaticAnchors{"acme": {{KeyID: keyID, PublicKey: pub}}}
	return signed, anchors
}

// TestSignVerifyRoundTrip: a signed artifact verifies, and the envelope carries
// the identity that was signed.
func TestSignVerifyRoundTrip(t *testing.T) {
	signed, anchors := signedTestArtifact(t)
	env, err := packsig.VerifyBundle(extract(t, signed), anchors)
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if env.ArtifactID != "acme/menu-board" || env.Version != "1.0.0" || env.Kind != packsig.KindPack {
		t.Fatalf("envelope identity = %+v", env)
	}
}

// TestVerifyUnsigned: no envelope entry is the unsigned refusal — never a pass.
func TestVerifyUnsigned(t *testing.T) {
	_, anchors := signedTestArtifact(t)
	_, err := packsig.VerifyBundle(extract(t, testArtifact(t)), anchors)
	assertReason(t, err, packsig.ReasonUnsigned)
}

// TestVerifyStrippedSignature: removing the envelope from a SIGNED artifact
// (signature stripping — "claim it's legacy") refuses unsigned, exactly like a
// never-signed artifact.
func TestVerifyStrippedSignature(t *testing.T) {
	signed, anchors := signedTestArtifact(t)
	files := extract(t, signed)
	delete(files, packsig.EnvelopeName)
	_, err := packsig.VerifyBundle(files, anchors)
	assertReason(t, err, packsig.ReasonUnsigned)
}

// TestVerifyTamperedPayload: altering any non-manifest entry after signing
// breaks the content digest — the signature covers the bytes that install, not
// just the manifest.
func TestVerifyTamperedPayload(t *testing.T) {
	signed, anchors := signedTestArtifact(t)
	files := extract(t, signed)
	files["ui/main.json"] = []byte(`{"pageType":"dashboard","evil":true}`)
	_, err := packsig.VerifyBundle(files, anchors)
	assertReason(t, err, packsig.ReasonInvalid)
}

// TestVerifyTamperedManifest: altering the manifest after signing breaks the
// content digest too.
func TestVerifyTamperedManifest(t *testing.T) {
	signed, anchors := signedTestArtifact(t)
	files := extract(t, signed)
	files["manifest.json"] = []byte(`{"id":"acme/menu-board","version":"9.9.9"}`)
	_, err := packsig.VerifyBundle(files, anchors)
	assertReason(t, err, packsig.ReasonInvalid)
}

// TestVerifyAddedEntry: ADDING an entry after signing is tampering just as much
// as altering one — the digest covers the whole extracted set.
func TestVerifyAddedEntry(t *testing.T) {
	signed, anchors := signedTestArtifact(t)
	files := extract(t, signed)
	files["ui/smuggled.json"] = []byte(`{}`)
	_, err := packsig.VerifyBundle(files, anchors)
	assertReason(t, err, packsig.ReasonInvalid)
}

// TestVerifyWrongKey: a formally valid signature by a key the anchors do not
// trust for the namespace refuses untrusted.
func TestVerifyWrongKey(t *testing.T) {
	_, anchors := signedTestArtifact(t)
	// A different keypair signs the same artifact — self-consistent, but unknown
	// to the anchor set.
	pub2, priv2 := signhash.GenerateKey()
	signed2, err := packsig.Sign(testArtifact(t), "acme/menu-board", "1.0.0", packsig.KeyIDFor(pub2), priv2)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	_, verr := packsig.VerifyBundle(extract(t, signed2), anchors)
	assertReason(t, verr, packsig.ReasonUntrusted)
}

// TestVerifyKeyIDSpoof: an envelope claiming a TRUSTED key id, with a signature
// actually produced by a different key, refuses invalid — the id is a lookup
// hint, the trusted public key is what verifies.
func TestVerifyKeyIDSpoof(t *testing.T) {
	signed, anchors := signedTestArtifact(t)
	files := extract(t, signed)
	var env packsig.Envelope
	if err := json.Unmarshal(files[packsig.EnvelopeName], &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	// Re-sign the same statement with a fresh key but keep the trusted key_id.
	_, priv2 := signhash.GenerateKey()
	spoofed, err := packsig.Sign(testArtifact(t), "acme/menu-board", "1.0.0", env.KeyID, priv2)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	_, verr := packsig.VerifyBundle(extract(t, spoofed), anchors)
	assertReason(t, verr, packsig.ReasonInvalid)
}

// TestVerifyNamespaceConfusion: a key trusted for one namespace cannot vouch
// for an artifact under a different namespace, however valid the signature.
func TestVerifyNamespaceConfusion(t *testing.T) {
	pub, priv := signhash.GenerateKey()
	keyID := packsig.KeyIDFor(pub)
	art := zipOf(t, map[string]string{
		"manifest.json":    `{"id":"waiveo/menu-board","version":"1.0.0"}`,
		"messages/en.json": `{}`,
	})
	signed, err := packsig.Sign(art, "waiveo/menu-board", "1.0.0", keyID, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// The key is anchored for "acme", not "waiveo".
	anchors := packsig.StaticAnchors{"acme": {{KeyID: keyID, PublicKey: pub}}}
	_, verr := packsig.VerifyBundle(extract(t, signed), anchors)
	assertReason(t, verr, packsig.ReasonUntrusted)
}

// TestVerifyNilAnchors: an unconfigured verifier fails closed — a validly
// signed artifact still refuses untrusted with no anchor source at all.
func TestVerifyNilAnchors(t *testing.T) {
	signed, _ := signedTestArtifact(t)
	_, err := packsig.VerifyBundle(extract(t, signed), nil)
	assertReason(t, err, packsig.ReasonUntrusted)
}

// TestVerifyCrossPurposeSignature: a signature the same trusted key produced
// over a DIFFERENT byte construction (here: the raw content digest, no
// domain-separated statement) does not verify as a pack signature — the
// domain-separation context is load-bearing.
func TestVerifyCrossPurposeSignature(t *testing.T) {
	pub, priv := signhash.GenerateKey()
	keyID := packsig.KeyIDFor(pub)
	anchors := packsig.StaticAnchors{"acme": {{KeyID: keyID, PublicKey: pub}}}

	files := extract(t, testArtifact(t))
	digest := packsig.ContentDigest(files)
	// Forge an envelope whose signature is over the bare digest — the kind of
	// bytes another subsystem might legitimately sign with this key.
	env := packsig.Envelope{
		Format: packsig.Format, ArtifactID: "acme/menu-board", Kind: packsig.KindPack,
		Version: "1.0.0", ContentDigest: digest, KeyID: keyID,
		Signature: base64.StdEncoding.EncodeToString(signhash.Sign(priv, []byte(digest))),
	}
	raw, _ := json.Marshal(env)
	files[packsig.EnvelopeName] = raw
	_, err := packsig.VerifyBundle(files, anchors)
	assertReason(t, err, packsig.ReasonInvalid)
}

// TestVerifyEnvelopeOnlyArtifact: an artifact holding NOTHING but a validly
// signed envelope over the empty entry set verifies at this layer (the digest
// of an empty set is well-defined) — proving the trivial-content edge is
// deterministic; the install pipeline still refuses it for having no manifest.
func TestVerifyEnvelopeOnlyArtifact(t *testing.T) {
	pub, priv := signhash.GenerateKey()
	keyID := packsig.KeyIDFor(pub)
	signed, err := packsig.Sign(zipOf(t, map[string]string{}), "acme/empty", "1.0.0", keyID, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	anchors := packsig.StaticAnchors{"acme": {{KeyID: keyID, PublicKey: pub}}}
	if _, err := packsig.VerifyBundle(extract(t, signed), anchors); err != nil {
		t.Fatalf("VerifyBundle over empty content: %v", err)
	}
}

// TestVerifyContentPackageKindCannotVouch: an envelope signing kind
// content-package cannot vouch for a pack install — kind is inside the signed
// statement and enforced at verification.
func TestVerifyContentPackageKindCannotVouch(t *testing.T) {
	pub, priv := signhash.GenerateKey()
	keyID := packsig.KeyIDFor(pub)
	anchors := packsig.StaticAnchors{"acme": {{KeyID: keyID, PublicKey: pub}}}

	files := extract(t, testArtifact(t))
	digest := packsig.ContentDigest(files)
	// Hand-build a content-package envelope with a fully valid signature over
	// its own (kind-bearing) statement.
	stmt := "waiveo-pack-signature/1\nacme/menu-board\ncontent-package\nrule-template\n1.0.0\n" + digest + "\n"
	env := packsig.Envelope{
		Format: packsig.Format, ArtifactID: "acme/menu-board", Kind: "content-package",
		Subtype: "rule-template", Version: "1.0.0", ContentDigest: digest, KeyID: keyID,
		Signature: base64.StdEncoding.EncodeToString(signhash.Sign(priv, []byte(stmt))),
	}
	raw, _ := json.Marshal(env)
	files[packsig.EnvelopeName] = raw
	_, err := packsig.VerifyBundle(files, anchors)
	assertReason(t, err, packsig.ReasonInvalid)
}

// TestContentDigestBoundaries: the length-prefixed canonical stream keeps entry
// boundaries unambiguous — moving a byte between an entry's name/body split
// point, or between two entries, changes the digest.
func TestContentDigestBoundaries(t *testing.T) {
	a := packsig.ContentDigest(map[string][]byte{"ab": []byte("c")})
	b := packsig.ContentDigest(map[string][]byte{"a": []byte("bc")})
	if a == b {
		t.Fatal("digest identical across a name/body boundary shift")
	}
	c := packsig.ContentDigest(map[string][]byte{"a": []byte("x"), "b": []byte("y")})
	d := packsig.ContentDigest(map[string][]byte{"a": []byte("xby")})
	if c == d {
		t.Fatal("digest identical across an entry-merge boundary shift")
	}
}

// TestFileAnchorsRoundTrip: DevProvision creates a keypair + anchors document;
// FileAnchors resolves the namespace to that key; a second provision reuses the
// SAME key (persistence), and a missing anchors file resolves to no anchors
// without error (fail closed at the verifier, loudly distinguishable from a
// malformed file).
func TestFileAnchorsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	anchorsPath := filepath.Join(dir, "trust", "anchors.json")

	keyID, priv, err := packsig.DevProvision(filepath.Join(dir, "keys"), anchorsPath, "acme")
	if err != nil {
		t.Fatalf("DevProvision: %v", err)
	}
	if len(priv) == 0 || keyID == "" {
		t.Fatalf("DevProvision returned empty identity: %q %d", keyID, len(priv))
	}

	keyID2, _, err := packsig.DevProvision(filepath.Join(dir, "keys"), anchorsPath, "acme")
	if err != nil {
		t.Fatalf("DevProvision (second): %v", err)
	}
	if keyID2 != keyID {
		t.Fatalf("second provision minted a different key: %q != %q", keyID2, keyID)
	}

	fa := packsig.FileAnchors{Path: anchorsPath}
	keys, err := fa.KeysFor("acme")
	if err != nil {
		t.Fatalf("KeysFor: %v", err)
	}
	if len(keys) != 1 || keys[0].KeyID != keyID {
		t.Fatalf("anchors resolve = %+v, want the provisioned key", keys)
	}
	if other, err := fa.KeysFor("waiveo"); err != nil || len(other) != 0 {
		t.Fatalf("unprovisioned namespace resolved keys: %v %v", other, err)
	}

	missing := packsig.FileAnchors{Path: filepath.Join(dir, "nope.json")}
	if keys, err := missing.KeysFor("acme"); err != nil || len(keys) != 0 {
		t.Fatalf("missing anchors file: keys=%v err=%v, want none/nil", keys, err)
	}
}

// TestFileAnchorsMalformed: a present-but-broken anchors file is an ERROR (the
// verifier fails closed loudly), never silently "no anchors".
func TestFileAnchorsMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "anchors.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (packsig.FileAnchors{Path: path}).KeysFor("acme"); err == nil {
		t.Fatal("malformed anchors file resolved without error")
	}
}

// TestSignEndToEndThroughFileAnchors: the full dev loop — provision, sign,
// verify through the file-backed anchors — holds together.
func TestSignEndToEndThroughFileAnchors(t *testing.T) {
	dir := t.TempDir()
	anchorsPath := filepath.Join(dir, "anchors.json")
	keyID, priv, err := packsig.DevProvision(filepath.Join(dir, "keys"), anchorsPath, "acme")
	if err != nil {
		t.Fatalf("DevProvision: %v", err)
	}
	signed, err := packsig.Sign(testArtifact(t), "acme/menu-board", "1.0.0", keyID, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := packsig.VerifyBundle(extract(t, signed), packsig.FileAnchors{Path: anchorsPath}); err != nil {
		t.Fatalf("VerifyBundle through file anchors: %v", err)
	}
}

// TestArtifactIdentity: the signer-side identity reader returns the manifest's
// id/version, and refuses an artifact with no manifest.
func TestArtifactIdentity(t *testing.T) {
	id, version, err := packsig.ArtifactIdentity(testArtifact(t))
	if err != nil || id != "acme/menu-board" || version != "1.0.0" {
		t.Fatalf("ArtifactIdentity = %q %q %v", id, version, err)
	}
	if _, _, err := packsig.ArtifactIdentity(zipOf(t, map[string]string{"x.json": "{}"})); err == nil {
		t.Fatal("ArtifactIdentity accepted an artifact with no manifest")
	}
}

func assertReason(t *testing.T, err error, want packsig.Reason) {
	t.Helper()
	verr, ok := err.(*packsig.VerifyError)
	if !ok {
		t.Fatalf("error = %v (%T), want *packsig.VerifyError", err, err)
	}
	if verr.Reason != want {
		t.Fatalf("refusal reason = %q (%s), want %q", verr.Reason, verr.Message, want)
	}
}

// TestVerifyEnvelopeWithUnknownMember: the envelope is the ONE entry the
// content digest cannot cover, so anything tolerated inside it is a byte region
// no signature vouches for. A mirror that rewrites only signature.json —
// leaving every defined member byte-identical and appending its own — is
// refused, not ignored.
func TestVerifyEnvelopeWithUnknownMember(t *testing.T) {
	signed, anchors := signedTestArtifact(t)
	files := extract(t, signed)

	var env map[string]json.RawMessage
	if err := json.Unmarshal(files[packsig.EnvelopeName], &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	env["smuggled"] = json.RawMessage(`"` + string(bytes.Repeat([]byte("A"), 4096)) + `"`)
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	files[packsig.EnvelopeName] = raw

	_, err = packsig.VerifyBundle(files, anchors)
	assertReason(t, err, packsig.ReasonInvalid)
}

// TestVerifyEnvelopeWithDuplicateMember: a duplicate member is refused because
// JSON parsers disagree on which one wins — an envelope that means one identity
// to this verifier and another to some later registry tool is exactly the
// relabelling confusion the signed statement exists to prevent.
func TestVerifyEnvelopeWithDuplicateMember(t *testing.T) {
	signed, anchors := signedTestArtifact(t)
	files := extract(t, signed)

	raw := files[packsig.EnvelopeName]
	dup := append([]byte(`{"artifact_id":"evil/relabelled",`), bytes.TrimPrefix(bytes.TrimSpace(raw), []byte("{"))...)
	files[packsig.EnvelopeName] = dup

	_, err := packsig.VerifyBundle(files, anchors)
	assertReason(t, err, packsig.ReasonInvalid)
}

// TestVerifyTriesEveryAnchoredKeyWithTheSameID: key_id is a truncated,
// publisher-supplied label, so two anchors may carry the same id. A shadow
// entry listed FIRST must not be able to refuse the genuine publisher's
// artifact — the signature is the gate, key_id only narrows the search.
func TestVerifyTriesEveryAnchoredKeyWithTheSameID(t *testing.T) {
	pub, priv := signhash.GenerateKey()
	keyID := packsig.KeyIDFor(pub)
	signed, err := packsig.Sign(testArtifact(t), "acme/menu-board", "1.0.0", keyID, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// A shadow anchor sharing the genuine key's id, listed ahead of it.
	shadowPub, _ := signhash.GenerateKey()
	anchors := packsig.StaticAnchors{"acme": {
		{KeyID: keyID, PublicKey: shadowPub},
		{KeyID: keyID, PublicKey: pub},
	}}

	if _, err := packsig.VerifyBundle(extract(t, signed), anchors); err != nil {
		t.Fatalf("a shadow anchor with a colliding key_id refused the genuine publisher's artifact: %v", err)
	}
}

// TestFileAnchorsRefuseWorldWritable: the anchors document IS the pack trust
// root — anyone who can write it can name themselves a trusted publisher. A
// file the group or world can write is refused outright rather than read, so a
// host that is not protecting its trust root fails closed instead of trusting
// whatever the file currently says.
func TestFileAnchorsRefuseWorldWritable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "anchors.json")
	pub, _ := signhash.GenerateKey()
	doc := `{"format":"pack-trust-anchors/1","anchors":[{"namespace":"acme","key_id":"` +
		packsig.KeyIDFor(pub) + `","public_key":"` + base64.StdEncoding.EncodeToString(pub) + `"}]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("write anchors: %v", err)
	}
	// Explicitly, after creation: the process umask would otherwise strip the
	// very bits this test is about.
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := (packsig.FileAnchors{Path: path}).KeysFor("acme"); err == nil {
		t.Fatal("a world-writable anchors file was accepted as a trust root")
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	keys, err := (packsig.FileAnchors{Path: path}).KeysFor("acme")
	if err != nil || len(keys) != 1 {
		t.Fatalf("a 0644 anchors file resolved to (%d keys, %v), want (1, nil)", len(keys), err)
	}
}

// TestFileAnchorsRefuseAWritableDirectory closes the half a file-mode check
// alone cannot: the trust root is only as protected as the directory holding it.
//
// Anyone who can write that directory can rename their OWN well-moded anchors
// document over the path, and the file's mode check then passes on a file they
// authored — so the pack-provenance trust root becomes whatever they say it is.
// Refusing a group- or world-writable directory is what makes the file check
// mean something.
//
// The sticky bit is honoured, and that is not a loophole: a sticky directory
// already forbids non-owners from renaming or unlinking files they do not own,
// which is precisely the attack being closed, so refusing there would be a
// false refusal.
func TestFileAnchorsRefuseAWritableDirectory(t *testing.T) {
	pub, _ := signhash.GenerateKey()
	doc := `{"format":"pack-trust-anchors/1","anchors":[{"namespace":"acme","key_id":"` +
		packsig.KeyIDFor(pub) + `","public_key":"` + base64.StdEncoding.EncodeToString(pub) + `"}]}`

	write := func(t *testing.T, dir string, dirMode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, "anchors.json")
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatalf("write anchors: %v", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatalf("chmod anchors: %v", err)
		}
		if err := os.Chmod(dir, dirMode); err != nil {
			t.Fatalf("chmod dir: %v", err)
		}
		return path
	}

	// A world-writable, non-sticky directory: refused even though the FILE is
	// 0600, because the file is replaceable.
	loose := t.TempDir()
	path := write(t, loose, 0o777)
	if _, err := (packsig.FileAnchors{Path: path}).KeysFor("acme"); err == nil {
		t.Fatal("a trust root in a world-writable directory was accepted — an attacker can rename their own document over it")
	}

	// Sticky: accepted, because renaming another owner's file is already
	// forbidden there.
	sticky := t.TempDir()
	path = write(t, sticky, os.ModeSticky|0o777)
	if keys, err := (packsig.FileAnchors{Path: path}).KeysFor("acme"); err != nil || len(keys) != 1 {
		t.Fatalf("a sticky directory was refused: (%d keys, %v) — that is a false refusal", len(keys), err)
	}

	// The ordinary case still works.
	tight := t.TempDir()
	path = write(t, tight, 0o700)
	if keys, err := (packsig.FileAnchors{Path: path}).KeysFor("acme"); err != nil || len(keys) != 1 {
		t.Fatalf("a 0700 directory resolved to (%d keys, %v), want (1, nil)", len(keys), err)
	}
}
