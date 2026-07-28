// Package packsig implements the sealed pack artifact's own signature envelope
// (marketplace/1 MKT-009a) and its verification (MKT-009b): the reserved
// signature.json entry a sealed artifact carries, the canonical content digest
// over the artifact's EXTRACTED entry set, the domain-separated ed25519
// statement binding {artifact_id, kind, subtype, version, content_digest}, and
// the trust-anchor seam a verifier resolves a publisher namespace's authorized
// signing keys through.
//
// Three properties are load-bearing and deliberate:
//
//   - The content digest covers the artifact's extracted entries — the exact
//     name→bytes map the install pipeline consumes — excluding only the envelope
//     entry itself, which cannot cover itself because it carries the digest. A
//     zip whose container-level encoding could present an entry differently
//     than an extractor yields cannot carry content the signature did not
//     cover, because the verifier digests what IT extracted, not what some
//     other parser might have seen.
//
//     The envelope exclusion is the one hole in "what is verified is what
//     installs", and it is closed on the far side rather than here: the
//     envelope is decoded STRICTLY (decodeEnvelope — no unknown members, no
//     duplicates), and the install pipeline DROPS it from the bundle the
//     moment verification returns (packs.Install), so no entry the digest did
//     not cover survives into anything downstream. Left in, it would be an
//     attacker-editable byte region inside a "verified" artifact that a
//     manifest could legally point a UI surface at (MAN-063).
//
//   - The signed statement binds the artifact identity (artifact_id, kind,
//     subtype, version), not the content alone — the same signed set
//     marketplace/1 MKT-009 puts inside an index entry's publisher_signature —
//     so a valid signature for one (artifact_id, kind, version) can never vouch
//     for byte-identical content re-labelled as a different pack, kind, or
//     version.
//
//   - The statement carries a domain-separation prefix, so a signature the same
//     ed25519 key produced for any OTHER construction in this platform (a
//     relay/1 desired-state snapshot, an archive/1 export header, a
//     channel-index/1 role document) can never verify as a pack-artifact
//     signature. This package can only enforce that direction; whether a
//     pack-artifact signature could verify under some other construction is
//     that construction's business, and not every one of them is
//     domain-separated (internal/relay/hello signs a bare nonce). The
//     directions are kept separate here rather than claiming both, and in
//     practice the key roots are separate too (security-model/1 SEC-044).
//
// The crypto primitives are internal/shared/signhash's (ed25519 + sha256) — no
// new dependency, per security-model SEC-044's trust-root separation this key
// material belongs to channel-index/1's software-artifact root, root (2), and is
// never the feeder's relay/1 key (root 3) nor the workspace signing key (root 1).
package packsig

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// EnvelopeName is the reserved artifact-root entry the signature envelope lives
// at (MKT-009a). It is excluded from the content digest (it cannot cover
// itself) and is never persisted as pack content.
const EnvelopeName = "signature.json"

// Format is the envelope's format discriminant (MKT-009a).
const Format = "pack-signature/1"

// KindPack is the artifact kind a manifest/1 sealed pack signs as; a
// content-package signs as its own kind and cannot vouch for a pack (the kind
// is inside the signed statement).
const KindPack = "pack"

// statementContext is the domain-separation prefix every pack-artifact
// signature statement opens with (MKT-009a). Frozen: changing it invalidates
// every existing artifact signature.
const statementContext = "waiveo-pack-signature/1"

// digestPrefix is the content digest's required prefix — the same
// "sha256:<lowercase-hex>" form the platform's asset_ref and channel-index/1
// CHI-021 digests use (signhash.ContentID's own form).
const digestPrefix = "sha256:"

// Envelope is the parsed signature.json entry (MKT-009a; marketplace/1 Wire
// shapes, PackSignatureEnvelope).
type Envelope struct {
	Format        string `json:"format"`
	ArtifactID    string `json:"artifact_id"`
	Kind          string `json:"kind"`
	Subtype       string `json:"subtype,omitempty"`
	Version       string `json:"version"`
	ContentDigest string `json:"content_digest"`
	KeyID         string `json:"key_id"`
	Signature     string `json:"signature"`
}

// ContentDigest computes MKT-009a's canonical digest over an artifact's
// extracted entry set: entries in byte-wise lexicographic name order, each
// contributing uint64-BE(len(name)) || name || uint64-BE(len(body)) || body to
// one sha256 stream. The envelope entry itself is excluded — it cannot cover
// itself. The length prefixes make entry boundaries unambiguous: no
// name/body split ambiguity, no concatenation collision between adjacent
// entries.
func ContentDigest(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for n := range files {
		if n == EnvelopeName {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	h := sha256.New()
	var lenBuf [8]byte
	for _, n := range names {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(n)))
		h.Write(lenBuf[:])
		h.Write([]byte(n))
		body := files[n]
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(body)))
		h.Write(lenBuf[:])
		h.Write(body)
	}
	return digestPrefix + hex.EncodeToString(h.Sum(nil))
}

// decodeEnvelope parses the signature envelope STRICTLY: exactly the members
// MKT-009a defines, each at most once.
//
// The envelope is the one entry the content digest cannot cover (it carries
// the digest), so anything this decoder tolerates is a byte region inside a
// "verified" artifact that no signature vouches for. An unknown member is
// therefore a refusal rather than something to ignore, and a duplicate member
// is a refusal because JSON parsers disagree on which one wins — a document
// that means different things to this verifier and to some later registry tool
// is exactly the identity-confusion MKT-009a exists to close. Together with
// Install dropping the envelope from the bundle after verification, an
// artifact cannot carry attacker-chosen bytes past this point at all.
func decodeEnvelope(raw []byte) (Envelope, error) {
	var seen map[string]bool
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return Envelope{}, verifyErr(ReasonInvalid, "the signature envelope is not valid JSON: %v", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return Envelope{}, verifyErr(ReasonInvalid, "the signature envelope is not a JSON object")
	}
	seen = map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return Envelope{}, verifyErr(ReasonInvalid, "the signature envelope is not valid JSON: %v", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return Envelope{}, verifyErr(ReasonInvalid, "the signature envelope is not valid JSON")
		}
		if seen[key] {
			return Envelope{}, verifyErr(ReasonInvalid,
				"the signature envelope declares member %q more than once", key)
		}
		seen[key] = true
		var discard json.RawMessage
		if err := dec.Decode(&discard); err != nil {
			return Envelope{}, verifyErr(ReasonInvalid, "the signature envelope is not valid JSON: %v", err)
		}
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return Envelope{}, verifyErr(ReasonInvalid, "the signature envelope is not valid JSON: %v", err)
	}
	if dec.More() {
		return Envelope{}, verifyErr(ReasonInvalid, "the signature envelope carries trailing content")
	}

	var env Envelope
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&env); err != nil {
		return Envelope{}, verifyErr(ReasonInvalid, "the signature envelope is malformed: %v", err)
	}
	return env, nil
}

// statement renders the domain-separated signing statement (MKT-009a):
// context LF artifact_id LF kind LF subtype LF version LF content_digest LF.
// No field may contain a LF — fieldsSane enforces that before any sign or
// verify, so the line structure is unambiguous.
func statement(artifactID, kind, subtype, version, contentDigest string) []byte {
	return []byte(statementContext + "\n" + artifactID + "\n" + kind + "\n" +
		subtype + "\n" + version + "\n" + contentDigest + "\n")
}

// fieldsSane refuses any statement field that would break the line-oriented
// statement encoding or is structurally empty. artifact_id needs only the
// coarse <publisher>/<name> split here — the manifest engine remains the
// authority on the full MAN-001 grammar; this check exists so namespace
// extraction and the statement encoding are sound, not to duplicate that
// grammar.
func fieldsSane(e Envelope) error {
	for name, v := range map[string]string{
		"artifact_id":    e.ArtifactID,
		"kind":           e.Kind,
		"version":        e.Version,
		"content_digest": e.ContentDigest,
		"key_id":         e.KeyID,
	} {
		if v == "" {
			return fmt.Errorf("envelope field %s is empty", name)
		}
	}
	for name, v := range map[string]string{
		"artifact_id":    e.ArtifactID,
		"kind":           e.Kind,
		"subtype":        e.Subtype,
		"version":        e.Version,
		"content_digest": e.ContentDigest,
	} {
		if strings.ContainsAny(v, "\n\r") {
			return fmt.Errorf("envelope field %s contains a line break", name)
		}
	}
	if _, err := Namespace(e.ArtifactID); err != nil {
		return err
	}
	if !strings.HasPrefix(e.ContentDigest, digestPrefix) {
		return fmt.Errorf("content_digest %q does not carry the %q prefix", e.ContentDigest, digestPrefix)
	}
	return nil
}

// Namespace returns the publisher namespace of an artifact id — the segment
// before the first "/" of <publisher>/<name> — or an error when the id does not
// split into two non-empty segments. Deliberately coarse (see fieldsSane).
func Namespace(artifactID string) (string, error) {
	pub, rest, ok := strings.Cut(artifactID, "/")
	if !ok || pub == "" || rest == "" {
		return "", fmt.Errorf("artifact_id %q is not <publisher>/<name>", artifactID)
	}
	return pub, nil
}

// ---- verification ----------------------------------------------------------

// Reason classifies a verification refusal into the three MKT-009b outcomes the
// install pipeline renders as its typed artifact errors.
type Reason string

const (
	// ReasonUnsigned: no envelope entry at all (→ PACK_UNSIGNED).
	ReasonUnsigned Reason = "unsigned"
	// ReasonInvalid: malformed envelope, content/digest mismatch, or a
	// signature that does not verify (→ PACK_SIGNATURE_INVALID).
	ReasonInvalid Reason = "invalid"
	// ReasonUntrusted: no trust anchor authorizes the named key for the
	// artifact's publisher namespace (→ PACK_SIGNER_UNTRUSTED).
	ReasonUntrusted Reason = "untrusted"
)

// VerifyError is a typed verification refusal: the MKT-009b outcome class plus
// a human message. It never carries artifact content.
type VerifyError struct {
	Reason  Reason
	Message string
	// Err is OPERATOR detail — a host path, an OS error, a parse failure —
	// that must NEVER reach a client. The api layer renders Message only; a
	// caller logging server-side may unwrap this for the real cause.
	Err error
}

func (e *VerifyError) Error() string {
	if e.Err != nil {
		return string(e.Reason) + ": " + e.Message + ": " + e.Err.Error()
	}
	return string(e.Reason) + ": " + e.Message
}

// Unwrap exposes the operator-detail cause to errors.Is/As without ever
// putting it in the client-visible Message.
func (e *VerifyError) Unwrap() error { return e.Err }

func verifyErr(reason Reason, format string, args ...any) *VerifyError {
	return &VerifyError{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// TrustedKey is one anchor entry: a stable key id and its ed25519 public key.
type TrustedKey struct {
	KeyID     string
	PublicKey ed25519.PublicKey
}

// TrustAnchors is the replaceable trust-anchor seam MKT-009b requires: it
// resolves a publisher namespace to the signing keys currently authorized for
// it. Today's implementations are a host-provisioned set (StaticAnchors,
// FileAnchors); when the root-signed channel-index/1 CHI-012
// publisher-namespace delegation exists (the pending external trust root), an
// implementation backed by that delegation slots in HERE without the verifier
// or the install pipeline changing. An error is a resolution failure the
// verifier fails closed on — never treated as "no anchors, admit anyway".
type TrustAnchors interface {
	KeysFor(namespace string) ([]TrustedKey, error)
}

// StaticAnchors is an in-memory anchor set: namespace → authorized keys.
type StaticAnchors map[string][]TrustedKey

func (a StaticAnchors) KeysFor(namespace string) ([]TrustedKey, error) {
	return a[namespace], nil
}

// VerifyBundle verifies an artifact's extracted entry set against MKT-009a/b:
// the envelope is present and well-formed, the content digest over files
// (excluding the envelope entry) matches the signed digest, the named key is
// one the anchors authorize for the artifact's own publisher namespace, and
// the ed25519 signature verifies over the domain-separated statement. files
// MUST be the map the caller actually extracted and will consume — that is the
// property that makes "what is verified is what installs" true.
//
// A nil anchors source refuses everything signed (ReasonUntrusted): an
// unconfigured verifier fails closed, never open.
func VerifyBundle(files map[string][]byte, anchors TrustAnchors) (Envelope, error) {
	raw, ok := files[EnvelopeName]
	if !ok {
		return Envelope{}, verifyErr(ReasonUnsigned,
			"the artifact carries no %s signature envelope (marketplace/1 MKT-009a)", EnvelopeName)
	}

	env, err := decodeEnvelope(raw)
	if err != nil {
		return Envelope{}, err
	}
	if env.Format != Format {
		return Envelope{}, verifyErr(ReasonInvalid,
			"the signature envelope's format is %q, want %q", env.Format, Format)
	}
	if err := fieldsSane(env); err != nil {
		return Envelope{}, verifyErr(ReasonInvalid, "the signature envelope is malformed: %v", err)
	}
	if env.Kind != KindPack {
		return Envelope{}, verifyErr(ReasonInvalid,
			"the signature envelope signs kind %q, which cannot vouch for a pack artifact", env.Kind)
	}
	if env.Subtype != "" {
		return Envelope{}, verifyErr(ReasonInvalid,
			"a pack artifact's signature envelope must carry no subtype (got %q)", env.Subtype)
	}

	if got := ContentDigest(files); got != env.ContentDigest {
		return Envelope{}, verifyErr(ReasonInvalid,
			"the artifact's content does not match the signed content digest — the payload or manifest was altered after signing")
	}

	ns, err := Namespace(env.ArtifactID)
	if err != nil {
		return Envelope{}, verifyErr(ReasonInvalid, "the signature envelope is malformed: %v", err)
	}
	var candidates []ed25519.PublicKey
	if anchors != nil {
		keys, err := anchors.KeysFor(ns)
		if err != nil {
			// The underlying error names a host path and an OS/parse failure —
			// operator detail that must not cross the trust boundary into a
			// client-visible refusal. It rides Err (which the api layer does
			// not render) while the client sees only that trust could not be
			// resolved.
			e := verifyErr(ReasonUntrusted,
				"the trust-anchor set for publisher namespace %q could not be resolved; check the host's pack trust configuration", ns)
			e.Err = err
			return Envelope{}, e
		}
		// EVERY anchored key whose id matches is a candidate, not just the
		// first: key_id is a truncated, publisher-supplied label, so two
		// anchors can carry the same id. Letting the first shadow the rest
		// would let a colliding entry both admit artifacts the real publisher
		// never signed and refuse the ones it did. The signature is the gate;
		// key_id only narrows the search.
		for _, k := range keys {
			if k.KeyID == env.KeyID {
				candidates = append(candidates, k.PublicKey)
			}
		}
	}
	if len(candidates) == 0 {
		return Envelope{}, verifyErr(ReasonUntrusted,
			"no trust anchor authorizes signing key %q for publisher namespace %q", env.KeyID, ns)
	}

	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil {
		return Envelope{}, verifyErr(ReasonInvalid, "the signature is not valid base64: %v", err)
	}
	msg := statement(env.ArtifactID, env.Kind, env.Subtype, env.Version, env.ContentDigest)
	for _, pub := range candidates {
		if signhash.Verify(pub, msg, sig) {
			return env, nil
		}
	}
	return Envelope{}, verifyErr(ReasonInvalid,
		"the signature does not verify under key %q for the signed statement", env.KeyID)
}

// ---- signing (publisher tooling) -------------------------------------------

// Sign rewrites a pack artifact zip with a fresh signature envelope for
// (artifactID, version) under priv, replacing any envelope already present.
// The content digest is computed over the artifact's extracted entries exactly
// as a verifier will recompute it. This is the publisher-side half; nothing in
// the install pipeline calls it.
func Sign(artifact []byte, artifactID, version, keyID string, priv ed25519.PrivateKey) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("packsig: signing key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	files, err := extractEntries(artifact)
	if err != nil {
		return nil, err
	}
	delete(files, EnvelopeName)

	env := Envelope{
		Format:        Format,
		ArtifactID:    artifactID,
		Kind:          KindPack,
		Version:       version,
		ContentDigest: ContentDigest(files),
		KeyID:         keyID,
	}
	if err := fieldsSane(env); err != nil {
		return nil, fmt.Errorf("packsig: refusing to sign: %v", err)
	}
	msg := statement(env.ArtifactID, env.Kind, env.Subtype, env.Version, env.ContentDigest)
	env.Signature = base64.StdEncoding.EncodeToString(signhash.Sign(priv, msg))

	envJSON, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("packsig: marshal envelope: %w", err)
	}
	files[EnvelopeName] = envJSON
	return writeZip(files)
}

// ArtifactIdentity reads manifest.json out of a pack artifact zip and returns
// its id and version — the identity a publisher tool signs the artifact as, so
// the signed statement and the bundled manifest can never disagree by
// tool construction.
func ArtifactIdentity(artifact []byte) (id, version string, err error) {
	files, err := extractEntries(artifact)
	if err != nil {
		return "", "", err
	}
	raw, ok := files["manifest.json"]
	if !ok {
		return "", "", fmt.Errorf("packsig: the artifact carries no manifest.json to read an identity from")
	}
	var m struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", "", fmt.Errorf("packsig: manifest.json is not valid JSON: %w", err)
	}
	if m.ID == "" || m.Version == "" {
		return "", "", fmt.Errorf("packsig: manifest.json carries no id/version to sign as")
	}
	return m.ID, m.Version, nil
}

// extractEntries reads a zip's regular-file entries into a name→bytes map —
// the same skip-directories, refuse-duplicates posture the install pipeline's
// own reader takes, so a signer digests the same set a verifier will.
func extractEntries(artifact []byte) (map[string][]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(artifact), int64(len(artifact)))
	if err != nil {
		return nil, fmt.Errorf("packsig: not a readable zip: %w", err)
	}
	files := make(map[string][]byte, len(zr.File))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if _, dup := files[f.Name]; dup {
			return nil, fmt.Errorf("packsig: duplicate entry %q", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("packsig: open entry %q: %w", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("packsig: read entry %q: %w", f.Name, err)
		}
		files[f.Name] = data
	}
	return files, nil
}

// writeZip serializes a name→bytes map into a deterministic zip (sorted entry
// order, no timestamps beyond archive/zip's zero defaults) — reproducible
// output for a reproducible artifact.
func writeZip(files map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			return nil, fmt.Errorf("packsig: zip create %q: %w", n, err)
		}
		if _, err := w.Write(files[n]); err != nil {
			return nil, fmt.Errorf("packsig: zip write %q: %w", n, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("packsig: zip close: %w", err)
	}
	return buf.Bytes(), nil
}
