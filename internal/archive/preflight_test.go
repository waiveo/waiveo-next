package archive

import "testing"

// preflight_test.go covers the four pre-apply refusals and, just as importantly,
// the cases that must NOT refuse: an older epoch, a healthy pack, an embedded
// asset. A preflight that refused everything would pass any test that only
// checked refusals.

func lockedPack(id, version, channel string) PackLock {
	return PackLock{PackID: id, Version: version, Channel: channel, Source: "https://index.example", SchemaEpoch: 1}
}

// trusting is a destination that resolves everything and yanks nothing — the
// baseline every case varies ONE thing from, so a refusal can only come from the
// thing that case changed.
func trusting() Destination {
	return Destination{
		SchemaEpoch:   4,
		DeveloperMode: false,
		PackTrust:     func(PackLock) PackTrust { return PackTrust{} },
		HasAsset:      func(string) bool { return true },
	}
}

func fatalCodes(r PreflightResult) []string {
	var out []string
	for _, e := range r.Fatal {
		out = append(out, e.ErrCode)
	}
	return out
}

func TestEpochNewerThanTheDestinationIsFatal(t *testing.T) {
	m := Manifest{PlatformSchemaEpoch: 9}
	r := Preflight(m, trusting())

	if r.OK() {
		t.Fatal("a newer schema epoch did not stop the restore (ARC-041/104)")
	}
	if got := fatalCodes(r); len(got) != 1 || got[0] != CodeEpochTooNew {
		t.Fatalf("fatal codes = %v, want exactly [%s]", got, CodeEpochTooNew)
	}
}

// TestAnOlderEpochIsNotRefused is the half a refusal-only test would miss.
// ARC-042 sends an older archive through the destination's normal
// migrate-on-open path, so refusing it here would break every restore of an
// archive taken before the last migration — the common case, not an edge one.
func TestAnOlderEpochIsNotRefused(t *testing.T) {
	for _, epoch := range []int{1, 3, 4} {
		r := Preflight(Manifest{PlatformSchemaEpoch: epoch}, trusting())
		if !r.OK() {
			t.Errorf("archive epoch %d against destination epoch 4: refused %v, want accepted (ARC-042)", epoch, fatalCodes(r))
		}
	}
}

func TestAnUnresolvableByReferenceAssetIsFatal(t *testing.T) {
	m := Manifest{Assets: []AssetEntry{{AssetRef: "sha256:aa", Storage: StorageByReference}}}
	dst := trusting()
	dst.HasAsset = func(string) bool { return false }

	r := Preflight(m, dst)
	if got := fatalCodes(r); len(got) != 1 || got[0] != CodeAssetUnavailable {
		t.Fatalf("fatal codes = %v, want exactly [%s] (ARC-063)", got, CodeAssetUnavailable)
	}
}

// TestOnlyByReferenceAssetsAreResolvedAtTheDestination pins the boundary rather
// than the rule. An embedded entry's bytes travel inside the container and are
// checked against their own asset_ref on the way past, and an inherited entry
// belongs to the base archive — asking the destination to resolve either would
// refuse a perfectly restorable archive.
func TestOnlyByReferenceAssetsAreResolvedAtTheDestination(t *testing.T) {
	m := Manifest{Assets: []AssetEntry{
		{AssetRef: "sha256:embedded", Storage: StorageEmbedded},
		{AssetRef: "sha256:inherited", Storage: StorageInherited},
	}}
	dst := trusting()
	dst.HasAsset = func(ref string) bool {
		t.Errorf("the destination was asked to resolve %s, which is not a by-reference asset", ref)
		return false
	}

	if r := Preflight(m, dst); !r.OK() {
		t.Fatalf("refused %v, want accepted: neither asset is by-reference", fatalCodes(r))
	}
}

func TestAYankedPackWithNoSubstituteIsBlocked(t *testing.T) {
	pack := lockedPack("acme/weather-widget", "1.2.0", "verified")
	dst := trusting()
	dst.PackTrust = func(PackLock) PackTrust { return PackTrust{Yanked: true} }

	r := Preflight(Manifest{Packs: []PackLock{pack}}, dst)

	// ARC-102 blocks the PACK, not the restore.
	if !r.OK() {
		t.Fatalf("a yanked pack stopped the whole restore (%v); ARC-102 blocks that one pack", fatalCodes(r))
	}
	if len(r.Packs) != 1 {
		t.Fatalf("got %d pack outcome(s), want 1", len(r.Packs))
	}
	out := r.Packs[0]
	if out.Restored {
		t.Error("the yanked version was restored — ARC-102 says that is not a legal outcome of either path")
	}
	if out.Code != CodePackYankedBlocked {
		t.Errorf("code = %q, want %s", out.Code, CodePackYankedBlocked)
	}
	if !out.OperatorSignal {
		t.Error("no operator signal raised; ARC-102 requires one on both of its paths")
	}
}

// TestAYankedPackWithASubstituteRestoresAndStillSignals is ARC-102's other path,
// and the signal is the part most easily dropped: the pack restores, so it looks
// like success — but a different version is running than the one the archive
// locked, and an operator who is not told cannot know that.
func TestAYankedPackWithASubstituteRestoresAndStillSignals(t *testing.T) {
	pack := lockedPack("acme/weather-widget", "1.2.0", "verified")
	dst := trusting()
	dst.PackTrust = func(PackLock) PackTrust { return PackTrust{Yanked: true, Substitute: "1.3.0"} }

	r := Preflight(Manifest{Packs: []PackLock{pack}}, dst)
	out := r.Packs[0]

	if !out.Restored {
		t.Error("a substituted pack was not restored; ARC-102 admits substitution as a legal path")
	}
	if out.SubstitutedVersion != "1.3.0" {
		t.Errorf("substituted version = %q, want 1.3.0", out.SubstitutedVersion)
	}
	if out.Code != "" {
		t.Errorf("code = %q, want none: the pack IS restored, just not at the locked version", out.Code)
	}
	if !out.OperatorSignal {
		t.Error("a silent substitution: ARC-102 requires a signal on this path too")
	}
	signalled := 0
	for _, p := range r.Packs {
		if p.OperatorSignal {
			signalled++
		}
	}
	if signalled != 1 {
		t.Errorf("%d outcome(s) carry an operator signal, want 1", signalled)
	}
}

func TestADevChannelPackIsRefusedWithoutDeveloperMode(t *testing.T) {
	pack := lockedPack("dev/scratch-widget", "0.0.1", ChannelDev)
	r := Preflight(Manifest{Packs: []PackLock{pack}}, trusting())

	// ARC-103: refuse that one pack "rather than failing the entire restore".
	if !r.OK() {
		t.Fatalf("a dev-channel pack failed the whole restore (%v); ARC-103 refuses just that pack", fatalCodes(r))
	}
	out := r.Packs[0]
	if out.Restored || out.Code != CodeDevChannelRefused || !out.OperatorSignal {
		t.Errorf("outcome = %+v, want refused with %s and a signal", out, CodeDevChannelRefused)
	}
}

func TestADevChannelPackRestoresWhenDeveloperModeIsOn(t *testing.T) {
	pack := lockedPack("dev/scratch-widget", "0.0.1", ChannelDev)
	dst := trusting()
	dst.DeveloperMode = true

	r := Preflight(Manifest{Packs: []PackLock{pack}}, dst)
	if !r.Packs[0].Restored {
		t.Errorf("outcome = %+v, want restored: developer mode is exactly the condition ARC-103 gates on", r.Packs[0])
	}
}

// TestABootCriticalDevPackEscalatesToFatal is ARC-103's single exception, and it
// is the one clause a per-pack-only implementation silently drops: refusing a
// pack the destination cannot boot without would otherwise leave a "successful"
// restore that cannot start.
func TestABootCriticalDevPackEscalatesToFatal(t *testing.T) {
	pack := lockedPack("dev/scratch-widget", "0.0.1", ChannelDev)
	dst := trusting()
	dst.BootCritical = func(p PackLock) bool { return p.PackID == "dev/scratch-widget" }

	r := Preflight(Manifest{Packs: []PackLock{pack}}, dst)
	if r.OK() {
		t.Fatal("a boot-critical dev-channel pack did not stop the restore (ARC-103's own exception)")
	}
	if got := fatalCodes(r); len(got) != 1 || got[0] != CodeDevChannelRefused {
		t.Fatalf("fatal codes = %v, want [%s]", got, CodeDevChannelRefused)
	}
}

// TestAMissingTrustSourceFailsClosed: ARC-101 makes the destination's present-day
// trust state authoritative. A destination that cannot answer is not a
// destination where everything is trusted — restoring silently there is exactly
// the outcome ARC-102 calls illegal.
func TestAMissingTrustSourceFailsClosed(t *testing.T) {
	dst := trusting()
	dst.PackTrust = nil

	r := Preflight(Manifest{Packs: []PackLock{lockedPack("acme/x", "1.0.0", "verified")}}, dst)
	if r.Packs[0].Restored {
		t.Error("a pack restored with no trust state consulted (ARC-101)")
	}
	if r.Packs[0].Code != CodePackYankedBlocked {
		t.Errorf("code = %q, want %s", r.Packs[0].Code, CodePackYankedBlocked)
	}
}

// TestNilHasAssetFailsClosed is the same fail-closed property for assets: a
// destination that cannot resolve anything must not be treated as one that
// resolves everything.
func TestNilHasAssetFailsClosed(t *testing.T) {
	m := Manifest{Assets: []AssetEntry{{AssetRef: "sha256:aa", Storage: StorageByReference}}}
	dst := trusting()
	dst.HasAsset = nil

	if r := Preflight(m, dst); r.OK() {
		t.Error("a nil asset resolver was treated as resolving everything (ARC-063 says fail closed)")
	}
}

// TestDevChannelIsDecidedBeforeTrust pins the order. A dev-channel pack on a
// destination without developer mode is refused whatever its trust state says,
// and reporting a yank instead would send an operator to fix a pack that was
// never going to install here.
func TestDevChannelIsDecidedBeforeTrust(t *testing.T) {
	pack := lockedPack("dev/scratch-widget", "0.0.1", ChannelDev)
	dst := trusting()
	dst.PackTrust = func(PackLock) PackTrust { return PackTrust{Yanked: true} }

	r := Preflight(Manifest{Packs: []PackLock{pack}}, dst)
	if got := r.Packs[0].Code; got != CodeDevChannelRefused {
		t.Errorf("code = %q, want %s: the channel gate decides first", got, CodeDevChannelRefused)
	}
}

// TestEveryProblemIsReportedFromOneAttempt: an operator who fixes one refusal
// and re-runs only to meet the next has been made to discover the state of their
// own destination one restore at a time.
func TestEveryProblemIsReportedFromOneAttempt(t *testing.T) {
	m := Manifest{
		PlatformSchemaEpoch: 9,
		Assets:              []AssetEntry{{AssetRef: "sha256:missing", Storage: StorageByReference}},
		Packs: []PackLock{
			lockedPack("acme/yanked", "1.0.0", "verified"),
			lockedPack("dev/scratch", "0.0.1", ChannelDev),
		},
	}
	dst := trusting()
	dst.HasAsset = func(string) bool { return false }
	dst.PackTrust = func(PackLock) PackTrust { return PackTrust{Yanked: true} }

	r := Preflight(m, dst)
	if got := fatalCodes(r); len(got) != 2 {
		t.Errorf("fatal codes = %v, want both the epoch and the asset refusal", got)
	}
	if len(r.Packs) != 2 {
		t.Fatalf("got %d pack outcome(s), want both packs judged even though the restore cannot proceed", len(r.Packs))
	}
	if r.Packs[0].Code != CodePackYankedBlocked || r.Packs[1].Code != CodeDevChannelRefused {
		t.Errorf("pack codes = %q, %q; want %s then %s", r.Packs[0].Code, r.Packs[1].Code,
			CodePackYankedBlocked, CodeDevChannelRefused)
	}
}

// TestAHealthyArchiveIsAccepted: the case that makes every refusal above mean
// something. Without it, a Preflight that refused unconditionally would pass all
// of them.
func TestAHealthyArchiveIsAccepted(t *testing.T) {
	m := Manifest{
		PlatformSchemaEpoch: 4,
		Assets:              []AssetEntry{{AssetRef: "sha256:present", Storage: StorageByReference}},
		Packs:               []PackLock{lockedPack("waiveo/slidecast", "2.2.0", "first-party")},
	}
	r := Preflight(m, trusting())

	if !r.OK() {
		t.Fatalf("a healthy archive was refused: %v", fatalCodes(r))
	}
	if !r.Packs[0].Restored || r.Packs[0].OperatorSignal {
		t.Errorf("outcome = %+v, want restored with no signal", r.Packs[0])
	}
}
