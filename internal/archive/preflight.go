package archive

// preflight.go decides, from a verified manifest and a description of the
// destination, every refusal archive/1 places BEFORE a restore applies anything:
// the schema-epoch refusal (ARC-041/104), unresolvable by-reference assets
// (ARC-063), and the two pack-lockfile refusals (ARC-102/103).
//
// # What this is, and what it is not
//
// It is the DECISION half of a restore, and only that half. It writes nothing,
// opens nothing, and holds no destination state of its own — every fact about
// the destination arrives through Destination's callbacks, so the decision is a
// pure function of (manifest, destination) and is testable without a store.
//
// It is NOT a restore. Nothing in either binary calls Preflight today: there is
// no apply path, no rollback (ARC-107), and no route. Stating that plainly
// because the shape is easy to mistake for a finished feature — the refusals are
// real and corpus-driven, but a refusal nobody consults refuses nothing. The
// conformance driver is currently Preflight's only caller, which is also why the
// dead-code baseline no longer reports it; that is a consequence of being driven,
// not evidence of being wired.
//
// # Why the fatal/per-pack split is the contract's rather than a design choice
//
// Two of these refusals stop the whole restore and two do not, and the contract
// is explicit about which:
//
//   - ARC-041/104 (epoch) refuses to OPEN at all, "exactly as the destination
//     would refuse to open a live workspace at that epoch". Nothing is restored.
//   - ARC-063 (by-reference asset) says "fail closed … rather than silently
//     proceed with a broken reference" — the workspace would be incomplete.
//   - ARC-102 (yanked pack) blocks or substitutes THAT ONE pack, and either path
//     raises an operator signal.
//   - ARC-103 (dev channel) refuses THAT ONE pack "rather than failing the entire
//     restore, unless that pack is one the destination cannot boot without".
//
// So a restore can legitimately succeed with a pack refused, which is exactly
// what the frozen ARC-103 case records ("succeeded, one pack refused"). A
// preflight collapsing all four into one error list would make that outcome
// unrepresentable.

// ChannelDev is the one channel value archive/1 gives meaning to. ARC-052 asks
// only that `channel` "distinguish a dev-channel lock from any other" — it fixes
// no vocabulary for the rest — so this is the single value restore-time gating
// (ARC-103) compares against, and every other channel string is simply "not dev".
//
// Declared here rather than in manifest.go because ARC-052's whole reason for
// existing is the gate in this file: the manifest only has to CARRY the value.
const ChannelDev = "dev"

// PackTrust is the destination's CURRENT trust state for one locked pack.
//
// ARC-101 is emphatic that this comes from the destination and never from the
// archive: "An archive recorded before a pack's trust status changed carries no
// special standing; the destination's present-day trust state is authoritative."
// That is why it arrives as a callback rather than being read from the manifest —
// there is nothing in the manifest that could answer it honestly.
type PackTrust struct {
	// Yanked reports that the destination marks this exact version revoked or
	// yanked. ARC-102 treats both the same way.
	Yanked bool
	// Substitute is the version the destination's own normal channel-resolution
	// rule chose in place of a yanked one, or "" if it offers none. The choice is
	// the destination's: this package neither resolves channels nor ranks versions.
	Substitute string
}

// Destination is what a restore needs to know about where it is restoring TO.
// Every member is a question answered by the destination, because every one of
// them is a fact the archive cannot carry honestly.
type Destination struct {
	// SchemaEpoch is the platform schema epoch this destination understands.
	SchemaEpoch int
	// DeveloperMode reports whether the destination has developer mode enabled
	// (ARC-103).
	DeveloperMode bool
	// PackTrust answers ARC-101's re-verification for one locked pack. A nil
	// callback is treated as "no trust state available", which fails closed: every
	// pack is refused rather than silently trusted, because a restore that cannot
	// consult trust state is precisely the case ARC-101 exists for.
	PackTrust func(PackLock) PackTrust
	// HasAsset reports whether the destination can resolve a by-reference asset —
	// already holding it, or able to obtain it independently of this container
	// (ARC-063). Nil means it can resolve nothing, which again fails closed.
	HasAsset func(assetRef string) bool
	// BootCritical reports whether the destination cannot boot without this pack,
	// which is ARC-103's one escalation from "refuse this pack" to "fail the
	// restore". Nil means no pack is boot-critical.
	BootCritical func(PackLock) bool
}

// PackOutcome is one locked pack's preflight verdict.
type PackOutcome struct {
	Pack PackLock
	// Restored reports whether this pack takes part in the restore at all.
	Restored bool
	// SubstitutedVersion is the version restored in place of the locked one when
	// the destination offered a substitute for a yanked version (ARC-102). Empty
	// when the locked version itself is restored, or when nothing is restored.
	SubstitutedVersion string
	// Code is the published refusal, or "" when the pack is restored. A
	// substitution carries no code: the pack IS restored, just not at the locked
	// version.
	Code string
	// OperatorSignal records that this outcome must reach an operator. ARC-102
	// requires it on BOTH of its paths — the substituted one included, since a
	// silent substitution is a different version running than the one recorded.
	OperatorSignal bool
}

// PreflightResult is the whole verdict: whether the restore may proceed at all,
// and what happens to each locked pack if it does.
type PreflightResult struct {
	// Fatal holds refusals that stop the restore. Non-empty means nothing is
	// applied and the destination is untouched.
	Fatal []*Error
	// Packs is one outcome per locked pack, in manifest order. Populated even when
	// Fatal is non-empty, so an operator sees every problem from one attempt
	// instead of discovering them one restore at a time.
	Packs []PackOutcome
}

// OK reports whether the restore may proceed. A refused pack does not make it
// false — ARC-103's own expected outcome is a restore that succeeded with one
// pack refused.
func (r PreflightResult) OK() bool { return len(r.Fatal) == 0 }

// Preflight decides every pre-apply refusal for restoring m into dst.
//
// It returns a verdict rather than an error, because "one pack refused, restore
// proceeds" is a legal and expected outcome that a single error cannot express.
func Preflight(m Manifest, dst Destination) PreflightResult {
	var res PreflightResult

	// ARC-041/104: newer than the destination understands refuses to open, on
	// fresh infrastructure and over a running destination alike. OLDER is not a
	// refusal — ARC-042 sends it through the destination's normal migrate-on-open
	// path, and refusing it here would break every restore of an older archive.
	if m.PlatformSchemaEpoch > dst.SchemaEpoch {
		res.Fatal = append(res.Fatal, codedf(CodeEpochTooNew,
			"archive platform_schema_epoch %d is newer than the destination's %d; a restore refuses to open it exactly as a live workspace at that epoch would be refused",
			m.PlatformSchemaEpoch, dst.SchemaEpoch))
	}

	// ARC-063: a by-reference entry the destination cannot resolve fails closed.
	// Only by-reference: an embedded entry's bytes travel in the container and are
	// checked against their own asset_ref on the way past (ARC-062), and an
	// inherited entry belongs to the base archive, whose availability is
	// BASE_ARCHIVE_UNAVAILABLE's question rather than this one.
	for _, a := range m.Assets {
		if a.Storage != StorageByReference {
			continue
		}
		if dst.HasAsset == nil || !dst.HasAsset(a.AssetRef) {
			res.Fatal = append(res.Fatal, codedf(CodeAssetUnavailable,
				"by-reference asset %s is not resolvable at the destination and its bytes are not carried in this container", a.AssetRef))
		}
	}

	for _, p := range m.Packs {
		res.Packs = append(res.Packs, preflightPack(p, dst))
	}
	// ARC-103's escalation: a refused pack the destination cannot boot without
	// stops the restore. Applied after the per-pack pass so the outcome list is
	// complete either way.
	for _, out := range res.Packs {
		if out.Restored || out.Code != CodeDevChannelRefused {
			continue
		}
		if dst.BootCritical != nil && dst.BootCritical(out.Pack) {
			res.Fatal = append(res.Fatal, codedf(CodeDevChannelRefused,
				"pack %s@%s is on the dev channel and this destination has developer mode disabled, and it is a pack the destination cannot boot without",
				out.Pack.PackID, out.Pack.Version))
		}
	}
	return res
}

func preflightPack(p PackLock, dst Destination) PackOutcome {
	out := PackOutcome{Pack: p}

	// ARC-103 before ARC-102, and the order is deliberate: a dev-channel pack on a
	// destination without developer mode is refused whatever its trust state says,
	// and reporting a yank on a pack that was never going to be installed here
	// would tell an operator to fix the wrong thing.
	if p.Channel == ChannelDev && !dst.DeveloperMode {
		out.Code = CodeDevChannelRefused
		out.OperatorSignal = true
		return out
	}

	// ARC-101: re-verify against the destination's CURRENT trust state. A missing
	// trust source is not "trusted" — it is "cannot verify", and this refuses,
	// because the alternative is restoring a possibly-yanked version silently,
	// which ARC-102 names as not a legal outcome of either path.
	if dst.PackTrust == nil {
		out.Code = CodePackYankedBlocked
		out.OperatorSignal = true
		return out
	}
	trust := dst.PackTrust(p)
	if !trust.Yanked {
		out.Restored = true
		return out
	}
	// ARC-102's two legal paths. Both signal; neither restores the yanked version.
	if trust.Substitute != "" {
		out.Restored = true
		out.SubstitutedVersion = trust.Substitute
		out.OperatorSignal = true
		return out
	}
	out.Code = CodePackYankedBlocked
	out.OperatorSignal = true
	return out
}
