// marketplace.go is the resolution half of the install pipeline: turning a
// marketplace reference (marketplace/1 MKT-060a) into the exact sealed artifact
// bytes the SAME signature-gated, manifest-gated install path already verifies,
// and recording what was resolved (MKT-094/094a/094b).
//
// The one property everything here is built around: **resolution never grants
// trust**. A registry index is a LOCATOR. It says where bytes live and what
// version label a publisher put on them; it does not, and structurally cannot,
// make the host accept an artifact the direct-upload path would refuse. Every
// resolved artifact goes through Installer.install — the same packsig envelope
// verification against the same trust anchors, the same manifest engine, the
// same atomic store write — and an index that names an unsigned, tampered, or
// wrong-key artifact simply produces the same refusal a hand-uploaded one would.
// What resolution ADDS is a set of checks that constrain WHICH validly-signed
// artifact an index may steer a host to:
//
//   - the entry's own digest/size must match the bytes fetched (CHI-021/023) —
//     an index cannot point at bytes it did not describe;
//   - the entry's claimed identity must equal the artifact's SIGNED identity
//     (MKT-040a) — an index cannot relabel a signed artifact as another pack;
//   - a channel pointer may not name a version below the persisted high-water
//     mark (MKT-050) — a validly-signed newer index cannot walk a channel back
//     onto a superseded version, and neither can a replayed older one;
//   - a yanked entry is not resolvable (CHI-072/MKT-048), a held entry is not
//     yet eligible (CHI-029/030 via MKT-042), and a `security_flagged` early
//     selection authenticated by nothing beyond the routine publish path does
//     NOT zero that hold (MKT-043).
//
// # What is NOT implemented, stated plainly
//
// The signed-index trust chain — channel-index/1's root/targets/snapshot/
// timestamp roles and CHI-050 steps 1-6, the revocation feed (CHI-070-074), and
// MKT-061's per-source index-signer key binding — does not exist anywhere in
// this repository yet (conformance/traceability/channel-index.md marks those
// requirements TBD-wave1), exactly as scripts/install-from-channel-index.sh
// states for its own run. This resolver therefore treats an index document as
// UNTRUSTED transport: whoever can serve the index chooses which of the
// artifacts a host would accept anyway it is offered, and can withhold ones it
// would prefer the host not see. It cannot cause acceptance of anything the
// packsig anchors do not vouch for, and it cannot cause a downgrade past the
// persisted MKT-050 mark. Those two are what a stale-index replay reduces to
// here: an old index can only ever offer an old version, which the mark refuses.
//
// CHI-060's monotonic index-version high-water mark is deliberately NOT
// persisted. Without index signature verification it would defend nothing an
// attacker who can serve the index cannot trivially step around (serve a higher
// version number), while handing that same attacker a permanent denial of
// service against the client (one absurd version number locks out every real
// index thereafter). The defense that does hold without a trust root is the one
// keyed on the pack version actually installed, and that is persisted.
package packs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/maaxton/waiveo-next/internal/packsig"
)

// ---- the reference (MKT-060a) ----------------------------------------------

// TrustChannels is MKT-020's closed four-member trust-channel enum. Growth is a
// marketplace/1 minor, never a host decision.
var TrustChannels = map[string]bool{
	"first-party": true,
	"verified":    true,
	"community":   true,
	"dev":         true,
}

// firstPartyNamespace is the sole publisher namespace that carries — and MUST
// carry — the `first-party` trust channel (MKT-001/MKT-021).
const firstPartyNamespace = "waiveo"

// devNamespace and devNamespacePrefix are the only publisher identities a
// `dev`-channel artifact may claim (MKT-025).
const (
	devNamespace       = "dev"
	devNamespacePrefix = "local-"
)

// reservedNamespacePrefixes / reservedNamespaceNames are MKT-062's reserved set:
// an entry under one of these resolves ONLY from a source the host has
// explicitly authorized for that namespace.
var (
	reservedNamespaceNames    = []string{"waiveo"}
	reservedNamespacePrefixes = []string{"waiveo-", "official-", "system-"}
)

// Ref is a marketplace reference (MKT-060a): the input a host resolves an
// install from.
//
// TrustChannel is required and never defaulted. It is the provenance tier the
// install is accepted under — MKT-022's production-install bar and MKT-090's
// auto-tracking pin are both stated per channel — so a host that filled it in
// would be choosing how much review the code it is about to install has had, in
// whatever direction the registry currently points.
//
// Version empty means "resolve this source's channel pointer for (PackID,
// TrustChannel)" (MKT-046/047), under MKT-050's anti-rollback. Version set means
// "resolve exactly that entry" — MKT-044's same-version re-resolution path —
// which selects a different entry but relaxes no check applied to it.
type Ref struct {
	Source       string
	PackID       string
	TrustChannel string
	Version      string
}

// String renders the reference's canonical text form: `<pack_id>@<version>`
// when pinned, `<pack_id>` when channel-tracked. The trust channel is
// deliberately NOT part of it — a published version carries exactly one channel
// (MKT-020), so the channel selects which version, it does not name the pack.
func (r Ref) String() string {
	if r.Version == "" {
		return r.PackID
	}
	return r.PackID + "@" + r.Version
}

// validate applies MKT-060a(a)-(c) plus the namespace/channel agreement rules
// MKT-001/MKT-021/MKT-025 fix, before any source is consulted. A reference that
// cannot possibly be legitimate is refused without a fetch.
func (r Ref) validate() error {
	ns, err := packsig.Namespace(r.PackID)
	if err != nil {
		return artifactErr("CROSS_PACK_REFERENCE_UNQUALIFIED",
			"the pack reference %q is not a fully publisher-qualified pack id (<publisher>/<name>, marketplace/1 MKT-008)", r.PackID)
	}
	if !TrustChannels[r.TrustChannel] {
		return artifactErr("TRUST_CHANNEL_UNKNOWN",
			"the pack reference must name one of the four trust channels (first-party, verified, community, dev); got %q — a host never defaults it (marketplace/1 MKT-060a)",
			r.TrustChannel)
	}
	// MKT-001/MKT-021: `waiveo` carries first-party, and nothing else may.
	if ns == firstPartyNamespace && r.TrustChannel != "first-party" {
		return artifactErr("FIRST_PARTY_CHANNEL_NAMESPACE_MISMATCH",
			"the %q publisher namespace carries the first-party trust channel; %q was requested", ns, r.TrustChannel)
	}
	if ns != firstPartyNamespace && r.TrustChannel == "first-party" {
		return artifactErr("FIRST_PARTY_CHANNEL_NAMESPACE_MISMATCH",
			"the first-party trust channel belongs to the %q publisher namespace alone; %q claimed it", firstPartyNamespace, ns)
	}
	// MKT-025: a dev-channel identity is the reserved `dev` publisher or a
	// `local-` name — never a registered third-party or first-party namespace.
	if r.TrustChannel == "dev" && ns != devNamespace && !strings.HasPrefix(ns, devNamespacePrefix) {
		return artifactErr("DEV_CHANNEL_RESERVED_NAMESPACE",
			"a dev-channel pack's publisher must be %q or start with %q; got %q", devNamespace, devNamespacePrefix, ns)
	}
	if r.Version != "" && !ValidVersion(r.Version) {
		return artifactErr("MARKETPLACE_REF_INVALID",
			"the pinned version %q is not a three-component MAJOR.MINOR.PATCH version (manifest/1 MAN-002, marketplace/1 MKT-050a)", r.Version)
	}
	return nil
}

// ---- transport seam ---------------------------------------------------------

// Fetcher is the transport an index document and an artifact are retrieved
// through — the seam that keeps this resolver independent of whether the
// registry is a directory on the box or a host on the internet. A self-hosted
// deployment must be able to install a pack with no internet connection at all,
// so the only implementation here is the local one; a network fetcher slots in
// at this interface and changes nothing else, because every check that decides
// whether an artifact is acceptable runs on the BYTES after they arrive, never
// on where they came from.
//
// What a real remote fetcher would add on top of this interface: TLS and
// redirect policy, a byte cap and timeout per object, retry/backoff, and — the
// substantive one — the channel-index/1 role-chain and revocation-feed fetches
// (CHI-050 steps 1-6 and 9) that make a remote index's CONTENTS trustworthy
// rather than merely reachable. None of those change what this resolver does
// with the bytes.
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) ([]byte, error)
}

// FileFetcher serves `file://` URLs (MKT-060's explicitly legal registry-source
// scheme) from a confined root directory.
//
// The confinement is not decoration. `download_url` is a value an index author
// chooses, and this fetcher is the one place it becomes a filesystem path, so an
// unconfined implementation would hand any party who can write an index a
// read-any-file primitive on the host. Root is resolved once, every target is
// resolved against it, and anything landing outside is refused — symlink
// traversal included, because the check is made on the RESOLVED path.
type FileFetcher struct {
	Root string
	// MaxBytes caps a single fetched object. Zero means DefaultLimits'
	// artifact cap, which is also the cap the install pipeline enforces.
	MaxBytes int64
}

func (f FileFetcher) Fetch(_ context.Context, rawURL string) ([]byte, error) {
	target, err := f.resolve(rawURL)
	if err != nil {
		return nil, err
	}
	limit := f.MaxBytes
	if limit <= 0 {
		limit = DefaultLimits.MaxArtifactBytes
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("packs: fetch %s: %w", rawURL, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("packs: fetch %s: not a regular file", rawURL)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("packs: fetch %s: %d bytes exceeds the %d-byte cap", rawURL, info.Size(), limit)
	}
	return os.ReadFile(target)
}

// resolve maps a file:// URL (or a bare relative path) to a real path inside
// Root, refusing anything that escapes it.
func (f FileFetcher) resolve(rawURL string) (string, error) {
	rel := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Scheme != "" {
		if u.Scheme != "file" {
			return "", fmt.Errorf("packs: fetch %s: only the file:// scheme is served by this deployment", rawURL)
		}
		rel = u.Path
		if u.Opaque != "" {
			rel = u.Opaque
		}
	}
	root, err := filepath.Abs(f.Root)
	if err != nil {
		return "", fmt.Errorf("packs: resolve registry root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("packs: resolve registry root: %w", err)
	}
	target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	// Resolve symlinks on the target too: a link inside Root pointing outside it
	// would otherwise pass a purely lexical prefix check.
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("packs: fetch %s: resolves outside the configured registry root", rawURL)
	}
	return target, nil
}

// ---- registry sources (MKT-060/061/062) -------------------------------------

// Source is one configured registry source. The ORDER of the configured list is
// a resolution preference only and never a trust decision (MKT-061): every
// source's entries face identical checks regardless of position, and no
// source's position lets it serve an artifact another source could not.
type Source struct {
	// ID is the stable identifier recorded in the install record's `source`
	// (MKT-094). It may not be the SourceDirect sentinel.
	ID string
	// Channel is the channel-index/1 channel identifier this source's index is
	// expected to name; a document naming a different channel is refused rather
	// than resolved (CHI-040/CHI-050 step 5's channel binding, degraded to a
	// document-level agreement check because the signature chain that would make
	// it authoritative does not exist yet).
	Channel string
	// IndexURL is the source's stable index location (CHI-080).
	IndexURL string
	// Fetcher retrieves this source's index and artifacts.
	Fetcher Fetcher
	// ReservedNamespaces are the reserved publisher namespaces (MKT-062) the
	// HOST has explicitly authorized this source to serve. Empty — the default —
	// means none, so a reserved-namespace pack refuses from this source. This is
	// the same host-provisioned stand-in MKT-009b sanctions for its trust
	// anchors: it sits behind the seam the root-signed CHI-012 delegation
	// replaces, and it fails closed rather than defaulting to permitted.
	ReservedNamespaces []string
	// StaleSource marks resolutions from this source as pending a
	// revocation-feed re-check (MKT-063), set for a `file://` source.
	StaleSource bool
}

// authorizesReserved reports whether this source may serve namespace when
// namespace is in MKT-062's reserved set.
func (s Source) authorizesReserved(namespace string) bool {
	for _, ns := range s.ReservedNamespaces {
		if ns == namespace {
			return true
		}
	}
	return false
}

// isReservedNamespace reports whether ns is in MKT-062's reserved set.
func isReservedNamespace(ns string) bool {
	for _, n := range reservedNamespaceNames {
		if ns == n {
			return true
		}
	}
	for _, p := range reservedNamespacePrefixes {
		if strings.HasPrefix(ns, p) {
			return true
		}
	}
	return false
}

// Market is the configured registry: an ordered source list plus the
// resolution-time clock `hold_hours` eligibility is evaluated against (MKT-042,
// CHI-029/030 — a resolution-time check, injectable so it is exercised against a
// fixed instant rather than a sleep).
type Market struct {
	sources []Source
	nowMs   func() int64
}

// NewMarket builds a Market over an ordered source list. nowMs supplies
// resolution time; nil is a programming error rather than a silent wall clock,
// because every timing decision here must be reproducible under test.
func NewMarket(nowMs func() int64, sources ...Source) *Market {
	return &Market{sources: sources, nowMs: nowMs}
}

// Sources returns the configured source list (introspection; the slice is
// copied so a caller cannot reorder the resolution preference in place).
func (m *Market) Sources() []Source {
	out := make([]Source, len(m.sources))
	copy(out, m.sources)
	return out
}

// ---- index documents --------------------------------------------------------

// indexDoc is the channel-index/1 signed-index envelope (its Wire shapes,
// `ChannelIndex`) plus marketplace/1's channel-pointer records (MKT-046), which
// ride INSIDE the same signed document rather than in a second one.
//
// `signatures` is parsed but not verified: the trust bundle it would be verified
// against does not exist in this repository yet (see this file's package
// comment). It is read so the document shape stays the one the eventual verifier
// will consume, not so anything here is authenticated by it.
type indexDoc struct {
	Signed struct {
		Role            string           `json:"role"`
		FormatVersion   string           `json:"format_version"`
		Channel         string           `json:"channel"`
		Version         int64            `json:"version"`
		SigningTime     int64            `json:"signing_time"`
		Artifacts       []indexEntry     `json:"artifacts"`
		ChannelPointers []channelPointer `json:"channel_pointers"`
	} `json:"signed"`
	Signatures []struct {
		KeyID string `json:"key_id"`
		Sig   string `json:"sig"`
	} `json:"signatures"`
}

// indexEntry is one artifact entry (CHI-020 + MKT-040/041/044/045).
type indexEntry struct {
	ArtifactID         string            `json:"artifact_id"`
	Kind               string            `json:"kind"`
	Subtype            string            `json:"subtype"`
	Version            string            `json:"version"`
	Status             string            `json:"status"`
	Digest             string            `json:"digest"`
	Size               int64             `json:"size"`
	DownloadURL        string            `json:"download_url"`
	Compression        string            `json:"compression"`
	Parts              []json.RawMessage `json:"parts"`
	PublishedAt        int64             `json:"published_at"`
	HoldHours          int               `json:"hold_hours"`
	SecurityFlagged    bool              `json:"security_flagged"`
	PublisherSignature string            `json:"publisher_signature"`
	SupersededBy       string            `json:"superseded_by"`
}

// channelPointer maps a trust channel to the version a source currently
// resolves that (pack_id, trust channel) pair to (MKT-046).
type channelPointer struct {
	PackID   string            `json:"pack_id"`
	Channels map[string]string `json:"channels"`
}

// entry statuses (CHI-020 + MKT-044).
const (
	statusActive     = "active"
	statusDeprecated = "deprecated"
	statusYanked     = "yanked"
	statusArchived   = "archived"
)

// artifact kinds (CHI-020 + MKT-041).
var knownKinds = map[string]bool{
	"platform-release": true,
	"relay-release":    true,
	"pack":             true,
	"content-package":  true,
}

const hourMs = int64(3600_000)

// ---- resolution -------------------------------------------------------------

// resolution is one successfully resolved candidate: the source it came from,
// the entry that named it, and the artifact bytes that matched that entry's own
// digest and size. Nothing here is trusted yet — Installer.install is still the
// gate.
type resolution struct {
	source   Source
	entry    indexEntry
	artifact []byte
}

// InstallRef resolves a marketplace reference and installs what it resolves to,
// through the SAME path a direct artifact upload takes.
//
// The ordering is deliberate and is the whole "resolution never grants trust"
// property in one place: resolve (which may refuse), then install (which
// verifies the signature envelope against the host's trust anchors and refuses
// on its own terms), then — only inside the install transaction — record and
// advance the anti-rollback mark. A refusal at any step leaves no row, no file,
// no record, no mark, and no generation bump.
func (in *Installer) InstallRef(ctx context.Context, ref Ref) (Result, error) {
	if in.market == nil {
		return Result{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"this deployment has no registry sources configured, so a marketplace reference cannot be resolved")
	}
	if err := ref.validate(); err != nil {
		return Result{}, err
	}

	res, err := in.market.resolve(ctx, ref, in.channelMarkReader(ctx))
	if err != nil {
		return Result{}, err
	}

	prov := provenance{
		Source:       res.source.ID,
		TrustChannel: ref.TrustChannel,
		EntryID:      res.entry.ArtifactID,
		EntryKind:    res.entry.Kind,
		EntrySubtype: res.entry.Subtype,
		EntryVersion: res.entry.Version,
		EntryDigest:  res.entry.Digest,
		StaleSource:  res.source.StaleSource,
	}
	return in.install(ctx, res.artifact, &prov)
}

// channelMarkReader returns the persisted MKT-050 high-water lookup the resolver
// consults. It reads through the store, so the mark it enforces is the one that
// survived the last restart — and the last uninstall (MKT-094b).
func (in *Installer) channelMarkReader(ctx context.Context) func(packID, channel string) (string, bool, error) {
	return func(packID, channel string) (string, bool, error) {
		return in.store.PackChannelMark(ctx, packID, channel)
	}
}

// resolve walks the configured sources in order and returns the first
// resolution that passes every check. Order is preference, not trust (MKT-061):
// a later source's entry faces exactly the checks an earlier one's would, and an
// earlier source cannot vouch for a later one.
//
// A refusal that is ABOUT THE REFERENCE (a rollback attempt, a yanked version, a
// reserved namespace this source may not serve) is returned rather than swallowed
// into "try the next source": silently falling through would let a second source
// satisfy a request the first one refused for a security reason, which is the
// source-order-as-trust behaviour MKT-061 forbids.
func (m *Market) resolve(ctx context.Context, ref Ref, markOf func(packID, channel string) (string, bool, error)) (resolution, error) {
	sources := m.sources
	if ref.Source != "" {
		var picked []Source
		for _, s := range m.sources {
			if s.ID == ref.Source {
				picked = append(picked, s)
			}
		}
		if len(picked) == 0 {
			return resolution{}, artifactErr("MARKETPLACE_REF_INVALID",
				"no registry source named %q is configured on this deployment", ref.Source)
		}
		sources = picked
	}
	if len(sources) == 0 {
		return resolution{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"this deployment has no registry sources configured, so %s cannot be resolved", ref)
	}

	ns, _ := packsig.Namespace(ref.PackID) // ref.validate already proved this splits

	var lastMiss error
	for _, src := range sources {
		// MKT-062: a reserved namespace resolves only from a source the host has
		// authorized for it. Defense in depth over the per-artifact trust anchors,
		// and fail-closed: an unconfigured source serves no reserved namespace.
		if isReservedNamespace(ns) && !src.authorizesReserved(ns) {
			lastMiss = artifactErr("REGISTRY_SOURCE_NOT_DELEGATED",
				"registry source %q is not authorized to serve the reserved publisher namespace %q (marketplace/1 MKT-062)", src.ID, ns)
			continue
		}
		res, err := m.resolveFrom(ctx, src, ref, markOf)
		if err == nil {
			return res, nil
		}
		var aerr *ArtifactError
		if ok := asArtifactError(err, &aerr); ok && !isSourceMiss(aerr.Code) {
			// A security refusal, not a miss: stop here rather than letting the
			// next source answer a question this one refused.
			return resolution{}, err
		}
		lastMiss = err
	}
	if lastMiss != nil {
		return resolution{}, lastMiss
	}
	return resolution{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
		"no configured registry source resolves %s on the %s trust channel", ref, ref.TrustChannel)
}

// sourceMissCodes are the refusal codes that mean "this source has nothing for
// you", and so may be retried against the next configured source. Everything
// else is a refusal ABOUT the reference and stops the walk.
var sourceMissCodes = map[string]bool{
	"MARKETPLACE_REF_UNRESOLVED": true,
}

func isSourceMiss(code string) bool { return sourceMissCodes[code] }

// resolveFrom resolves ref against one source: fetch the index, select the
// entry, apply every resolution-time check, then fetch and integrity-check the
// artifact bytes.
func (m *Market) resolveFrom(ctx context.Context, src Source, ref Ref, markOf func(packID, channel string) (string, bool, error)) (resolution, error) {
	doc, err := m.fetchIndex(ctx, src)
	if err != nil {
		return resolution{}, err
	}

	version := ref.Version
	viaPointer := version == ""
	if viaPointer {
		v, ok := pointerVersion(doc, ref.PackID, ref.TrustChannel)
		if !ok {
			return resolution{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
				"registry source %q publishes no %s channel pointer for %s (marketplace/1 MKT-046)", src.ID, ref.TrustChannel, ref.PackID)
		}
		// MKT-047: a pointer's value is a LOOKUP KEY, never a trust decision —
		// and MKT-050a: a value outside MAN-002's grammar has no position in the
		// version order, so it is not resolvable rather than compared some other way.
		if !ValidVersion(v) {
			return resolution{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
				"the %s channel pointer for %s names %q, which is not a three-component MAJOR.MINOR.PATCH version (marketplace/1 MKT-050a)",
				ref.TrustChannel, ref.PackID, v)
		}
		version = v
	}

	// MKT-050: a channel pointer may not walk a channel backward. Checked BEFORE
	// the artifact is fetched — a refused resolution should not even download.
	// An explicitly pinned version is outside this rule by MKT-050's own scoping
	// (it governs channel-pointer resolution), which is what keeps MKT-044's
	// same-version re-resolution of an archived version possible.
	if viaPointer {
		mark, found, err := markOf(ref.PackID, ref.TrustChannel)
		if err != nil {
			return resolution{}, err
		}
		if found && VersionLower(version, mark) {
			return resolution{}, artifactErr("POINTER_ROLLBACK_REJECTED",
				"the %s channel pointer for %s names version %s, below the %s this deployment has already resolved for that pair (marketplace/1 MKT-050)",
				ref.TrustChannel, ref.PackID, version, mark)
		}
	}

	entry, err := selectEntry(doc, ref.PackID, version, viaPointer, m.nowMs(), src.ID)
	if err != nil {
		return resolution{}, err
	}

	artifact, err := m.fetchArtifact(ctx, src, entry)
	if err != nil {
		return resolution{}, err
	}
	return resolution{source: src, entry: entry, artifact: artifact}, nil
}

// fetchIndex retrieves and structurally checks one source's index document.
func (m *Market) fetchIndex(ctx context.Context, src Source) (indexDoc, error) {
	if src.Fetcher == nil {
		return indexDoc{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"registry source %q has no configured transport", src.ID)
	}
	raw, err := src.Fetcher.Fetch(ctx, src.IndexURL)
	if err != nil {
		// The underlying error names host paths and OS failures — operator
		// detail. The client sees only that the source did not answer.
		return indexDoc{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"registry source %q could not be read", src.ID)
	}
	var doc indexDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return indexDoc{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"registry source %q did not serve a readable index document", src.ID)
	}
	// CHI-090: refuse a format_version major this client does not implement,
	// rather than parsing a schema it does not understand.
	major, _, _ := strings.Cut(doc.Signed.FormatVersion, ".")
	if major != "1" {
		return indexDoc{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"registry source %q serves index format_version %q; only major 1 is understood (channel-index/1 CHI-090)",
			src.ID, doc.Signed.FormatVersion)
	}
	if src.Channel != "" && doc.Signed.Channel != src.Channel {
		return indexDoc{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"registry source %q serves an index naming channel %q, not the configured %q",
			src.ID, doc.Signed.Channel, src.Channel)
	}
	return doc, nil
}

// pointerVersion reads the channel pointer for (packID, trustChannel).
func pointerVersion(doc indexDoc, packID, trustChannel string) (string, bool) {
	for _, p := range doc.Signed.ChannelPointers {
		if p.PackID != packID {
			continue
		}
		v, ok := p.Channels[trustChannel]
		if ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// selectEntry locates (packID, version)'s own artifact entry and applies every
// resolution-time check the entry itself decides: kind, status, version grammar,
// staged-rollout hold, and the shape this deployment can actually handle.
//
// viaPointer distinguishes the two paths where they genuinely differ: an
// `archived` version stays resolvable for an explicit same-version reinstall
// (MKT-044) but is withdrawn from ordinary discovery, so a channel pointer may
// not land on one.
func selectEntry(doc indexDoc, packID, version string, viaPointer bool, nowMs int64, sourceID string) (indexEntry, error) {
	var found *indexEntry
	for i := range doc.Signed.Artifacts {
		e := &doc.Signed.Artifacts[i]
		if e.ArtifactID != packID || e.Version != version {
			continue
		}
		if found != nil {
			// Two entries for one (artifact_id, version) is an ambiguous index
			// and this client has no basis to prefer either (MAN-002 forbids two
			// artifacts sharing an id+version in the first place).
			return indexEntry{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
				"registry source %q publishes more than one entry for %s version %s", sourceID, packID, version)
		}
		found = e
	}
	if found == nil {
		return indexEntry{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"registry source %q publishes no entry for %s version %s", sourceID, packID, version)
	}
	e := *found

	// MKT-041: a kind outside the four-member set is refused by a client that
	// implements this contract (a client that does not skips the entry instead,
	// CHI-031).
	if !knownKinds[e.Kind] {
		return indexEntry{}, artifactErr("PACK_KIND_UNKNOWN",
			"the entry for %s version %s carries kind %q, outside the set channel-index/1 and marketplace/1 define", packID, version, e.Kind)
	}
	if e.Kind != packsig.KindPack {
		return indexEntry{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"the entry for %s version %s is a %q, which this install pipeline does not install (it installs manifest/1 packs)", packID, version, e.Kind)
	}
	if !ValidVersion(e.Version) {
		return indexEntry{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"the entry for %s carries version %q, which is not a three-component MAJOR.MINOR.PATCH version (marketplace/1 MKT-050a)", packID, e.Version)
	}

	switch e.Status {
	case statusActive, statusDeprecated:
		// resolvable; `deprecated` is informational only (CHI-020).
	case statusYanked:
		// CHI-072/MKT-048: checked at RESOLUTION time, for a new install and for
		// a channel-tracking advance alike.
		return indexEntry{}, artifactErr("ARTIFACT_YANKED",
			"%s version %s is yanked and MUST NOT be resolved for a new install (channel-index/1 CHI-072)", packID, version)
	case statusArchived:
		// MKT-044: withdrawn from discovery, still resolvable for a reinstall of
		// that exact version — so an explicit pin may land here and a pointer
		// may not.
		if viaPointer {
			return indexEntry{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
				"%s version %s is archived: it remains resolvable by explicit version, not through a channel pointer (marketplace/1 MKT-044)", packID, version)
		}
	default:
		// CHI-031: an unrecognized status is NEVER treated as if it were active.
		return indexEntry{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"the entry for %s version %s carries an unrecognized status %q, so it is not resolvable (channel-index/1 CHI-031)", packID, version, e.Status)
	}

	// CHI-029/030 via MKT-042: a staged-rollout hold is evaluated at RESOLUTION
	// time. MKT-043 then tightens CHI-030's `security_flagged` escape hatch for a
	// pack entry: a bare publish-path-signed flag is NOT sufficient to zero the
	// hold, and the further authentication it requires (a matching
	// revocation-feed entry under the separately-delegated revocation role, or an
	// owner/revocation-role countersignature) does not exist on this deployment —
	// so the flag never zeroes a hold here, which is MKT-043's stated outcome
	// rather than an unimplemented case.
	if e.HoldHours > 0 {
		eligibleAt := e.PublishedAt + int64(e.HoldHours)*hourMs
		if nowMs < eligibleAt {
			if e.SecurityFlagged {
				return indexEntry{}, artifactErr("HOLD_HOURS_ZERO_UNAUTHENTICATED",
					"%s version %s marks security_flagged to skip its %d-hour hold, authenticated by nothing beyond the routine publish signature (marketplace/1 MKT-043); the hold remains in force",
					packID, version, e.HoldHours)
			}
			return indexEntry{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
				"%s version %s is inside its %d-hour staged-rollout hold and is not yet install-eligible (channel-index/1 CHI-030)", packID, version, e.HoldHours)
		}
	}

	// Shapes this deployment does not handle are refused, never mishandled: a
	// split artifact (CHI-024) or a transport-compressed one (CHI-026) needs the
	// bounded streaming decompressor CHI-028 requires, which is not built here.
	if len(e.Parts) > 0 {
		return indexEntry{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"%s version %s is published split (channel-index/1 CHI-024); split artifacts are not supported by this deployment", packID, version)
	}
	if e.Compression != "" {
		return indexEntry{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"%s version %s is published with %q transport compression (channel-index/1 CHI-026); compressed artifacts are not supported by this deployment", packID, version, e.Compression)
	}
	if !strings.HasPrefix(e.Digest, "sha256:") || e.Size <= 0 || e.DownloadURL == "" {
		return indexEntry{}, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"the entry for %s version %s is missing a sha256 digest, a positive size, or a download url (channel-index/1 CHI-020)", packID, version)
	}
	return e, nil
}

// fetchArtifact downloads the entry's bytes and checks them against the entry's
// own `size` and `digest` (CHI-023, CHI-021) — two checks, not one, because they
// catch different failure modes and a fault isolated to either is never a reason
// to accept the artifact on the strength of the other.
//
// Passing these proves only that these are the bytes THIS ENTRY described. It
// proves nothing about whether the entry may be trusted — that is the signature
// envelope's job, run next by the install pipeline, against the host's own trust
// anchors rather than anything the index said.
func (m *Market) fetchArtifact(ctx context.Context, src Source, e indexEntry) ([]byte, error) {
	raw, err := src.Fetcher.Fetch(ctx, e.DownloadURL)
	if err != nil {
		return nil, artifactErr("MARKETPLACE_REF_UNRESOLVED",
			"the artifact for %s version %s could not be downloaded from registry source %q", e.ArtifactID, e.Version, src.ID)
	}
	if int64(len(raw)) != e.Size {
		return nil, artifactErr("SIZE_MISMATCH",
			"the artifact for %s version %s is %d bytes; the index entry says %d (channel-index/1 CHI-023)",
			e.ArtifactID, e.Version, len(raw), e.Size)
	}
	sum := sha256.Sum256(raw)
	got := "sha256:" + hex.EncodeToString(sum[:])
	if got != e.Digest {
		return nil, artifactErr("DIGEST_MISMATCH",
			"the artifact for %s version %s does not match the digest its index entry names (channel-index/1 CHI-021)",
			e.ArtifactID, e.Version)
	}
	return raw, nil
}

// asArtifactError is errors.As specialized to *ArtifactError, kept local so this
// file's control flow reads as one predicate rather than a two-line dance.
func asArtifactError(err error, out **ArtifactError) bool {
	for err != nil {
		if aerr, ok := err.(*ArtifactError); ok {
			*out = aerr
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
