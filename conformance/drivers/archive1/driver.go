// Package archive1 is the executable archive/1 conformance driver: the §10
// differential oracle for internal/archive's container — Create's output read
// back by Open, and each of the container's five refusals reached by
// constructing the condition the corpus case describes.
//
// The frozen archive-1 cases are DECLARATIVE rather than byte-level: a case says
// "the supplied passphrase was wrong" or "4 frames were written, 3 delivered",
// and names the code plus a few properties that must hold. So this driver's job
// is to build each described situation against the live package and diff the
// outcome — never to replay recorded bytes, which the corpus does not carry.
//
// # Two of the expectations need a construction, not an assertion
//
// Several cases assert a NEGATIVE about internal behaviour: `decryption_attempted:
// false` (ARC-023) and `frames_attempted_after_failure: 0` (ARC-014). A code
// comparison alone cannot see either — the reader returns the same code whether
// or not it stopped where the contract says it must. Instrumenting the package to
// count attempts would be a seam that exists only for a test.
//
// Instead each is driven by a DIFFERENTIAL construction: build an archive with
// two independent faults, arranged so that a reader doing the work in the
// contract's order reports one code and a reader doing it in any other order
// reports a different one. The code then carries the ordering claim.
//
//   - ARC-023: a tampered header (breaking the signature) AND a wrong passphrase.
//     Verify-then-decrypt reports ARCHIVE_SIGNATURE_INVALID; decrypt-first reports
//     DECRYPT_FAILED. Observing the former is what "decryption_attempted: false"
//     means from outside.
//   - ARC-014: a corrupted EARLY frame AND a truncated tail. Abort-at-first-failure
//     reports DECRYPT_FAILED; a reader that kept going past the bad frame reaches
//     EOF without a final frame and reports ARCHIVE_TRUNCATED.
//
// # What this driver does NOT drive, and why
//
// Three cases — ARC-041 (EPOCH_TOO_NEW with the destination in maintenance mode),
// ARC-102 (PACK_YANKED_BLOCKED) and ARC-103 (DEV_CHANNEL_REFUSED) — are
// RESTORE-time refusals. They belong to archive/1's "Restore is an install path"
// section, and no restore path exists yet: internal/archive.Open is called by
// nothing outside its own tests, and there is no route or install path to invoke.
// Each is reported PENDING with that reason rather than skipped silently, so
// conformance/driven-manifest.json records the gap as PENDING and no traceability
// row can claim `covered` on the strength of a case nobody executes.
package archive1

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/archive"
)

const contract = "archive/1"

// restorePathPending is the single reason string every restore-dependent case
// carries, written once so the three cannot drift into three different
// explanations of the same missing thing.
const restorePathPending = "restore is an install path (ARC-100-107) and no restore path exists: " +
	"internal/archive.Open has no caller outside its own tests, so there is nothing to drive this refusal against"

// Run loads the frozen archive-1 corpus and drives it against the live
// internal/archive implementation.
func Run() report.Report {
	rep := report.Report{Driver: "archive1", Target: "internal/archive (Create + Open)"}
	cases, err := LoadCorpus()
	if err != nil {
		rep.Fail("corpus", contract, fmt.Sprintf("load archive-1 corpus: %v", err))
		return rep
	}
	driveCases(&rep, cases)
	return rep
}

// RunCases drives the identical per-case logic Run uses against a
// caller-supplied case set — the seam the has-teeth test uses to corrupt one
// case's `expected` block in memory and confirm the SAME comparison reports FAIL.
func RunCases(cases map[string]corpus.Case) report.Report {
	rep := report.Report{Driver: "archive1", Target: "internal/archive (Create + Open)"}
	driveCases(&rep, cases)
	return rep
}

func driveCases(rep *report.Report, cases map[string]corpus.Case) {
	drivers := map[string]func(*report.Report, corpus.Case){
		"ARC-014-invalid-decrypt-failed-wrong-passphrase": driveWrongPassphrase,
		"ARC-016-invalid-truncated-tail-rejected":         driveTruncatedTail,
		"ARC-023-invalid-signature-verification-failed":   driveSignatureInvalid,
		"ARC-060-valid-assets-by-reference":               driveAssetsByReference,
		"ARC-041-invalid-epoch-mismatch":                  driveEpochTooNew,
		"ARC-102-invalid-yanked-pack-blocked":             driveYankedPackBlocked,
		"ARC-103-invalid-dev-channel-refused":             driveDevChannelRefused,
		"ARC-091-valid-manifest-incremental":              driveManifestIncremental,
	}
	// Each pending case names the SPECIFIC thing that does not exist, not a shared
	// "not implemented" — the three restore refusals and the incremental export are
	// different gaps with different work behind them, and a reader of
	// driven-manifest.json should be able to tell which is which.
	pending := map[string]string{
		"ARC-031-valid-manifest-full": "the case's first asset declares storage:embedded with asset_ref " +
			"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85, whose hex part is 63 characters — " +
			"not a valid sha256. No bytes can hash to it, and Open verifies every embedded asset's bytes against its own " +
			"asset_ref (ARC-062), so this case cannot be driven as a Create+Open round trip. The contract's own Wire shapes " +
			"example carries the same truncated value, so the case inherited it rather than introducing it",
	}

	for id, c := range cases {
		switch {
		case drivers[id] != nil:
			drivers[id](rep, c)
		case pending[id] != "":
			rep.Pending(id, contract, pending[id])
		default:
			// A case this driver has never heard of is a FAIL, not a skip: the corpus
			// grew and nobody taught the driver, which is exactly the silent gap the
			// driven-manifest exists to prevent.
			rep.Fail(id, contract, "this driver has no handler for the case and does not know why it would be pending")
		}
	}
}

// ---- the container cases -------------------------------------------------

// driveWrongPassphrase: ARC-014/015/074. A wrong export passphrase must abort
// with DECRYPT_FAILED, emit no plaintext, attempt no further frames, and be
// distinguishable from ARCHIVE_SIGNATURE_INVALID.
func driveWrongPassphrase(rep *report.Report, c corpus.Case) {
	supplied, _ := c.Input["supplied_export_passphrase"].(string)
	if supplied == "" {
		rep.Fail(c.CaseID, contract, "case declares no input.supplied_export_passphrase")
		return
	}

	f := newFixture()
	container, err := f.multiFrameArchive()
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("build the fixture archive: %v", err))
		return
	}

	// The archive is well-formed and validly signed — the case says so
	// (outer_header_signature_valid: true) — so the ONLY fault is the passphrase.
	manifest, entries, openErr := archive.Open(bytes.NewReader(container), supplied, f.pub)
	got := archive.Code(openErr)

	var diffs []report.Diff
	if want := expectedCode(c); got != want {
		diffs = append(diffs, report.Diff{Field: "error.code", Expected: want, Actual: got})
	}
	// "plaintext_emitted: false" — observable directly: neither the manifest nor a
	// single body entry may come back from a read that failed to authenticate.
	if wantEmitted, ok := c.Expected["plaintext_emitted"].(bool); ok && !wantEmitted {
		if !reflect.DeepEqual(manifest, archive.Manifest{}) || len(entries) != 0 {
			diffs = append(diffs, report.Diff{Field: "plaintext_emitted", Expected: false,
				Actual: fmt.Sprintf("manifest non-zero=%v, %d entr(ies)", !reflect.DeepEqual(manifest, archive.Manifest{}), len(entries))})
		}
	}
	// "distinguishable_from: ARCHIVE_SIGNATURE_INVALID" (ARC-015) — asserted by
	// reaching that OTHER code from this same fixture, so the two are shown to be
	// different answers rather than assumed to be.
	if wantFrom, ok := c.Expected["distinguishable_from"].(string); ok {
		tampered := tamperHeaderDigest(container)
		if other := archive.Code(mustFailOpen(tampered, f.passphrase, f.pub)); other != wantFrom {
			diffs = append(diffs, report.Diff{Field: "distinguishable_from", Expected: wantFrom, Actual: other})
		} else if other == got {
			diffs = append(diffs, report.Diff{Field: "distinguishable_from",
				Expected: "a code different from " + got, Actual: other})
		}
	}
	// "frames_attempted_after_failure: 0" — see the package doc: a corrupted early
	// frame PLUS a truncated tail. Aborting at the first failure yields
	// DECRYPT_FAILED; continuing past it reaches EOF with no final frame and yields
	// ARCHIVE_TRUNCATED.
	if wantAttempts, ok := c.Expected["frames_attempted_after_failure"].(float64); ok && wantAttempts == 0 {
		both := truncateFinalFrame(corruptFrame(container, 0))
		if code := archive.Code(mustFailOpen(both, f.passphrase, f.pub)); code != archive.CodeDecryptFailed {
			diffs = append(diffs, report.Diff{Field: "frames_attempted_after_failure",
				Expected: "DECRYPT_FAILED from aborting at the first bad frame", Actual: code})
		}
	}

	finish(rep, c, diffs)
}

// driveTruncatedTail: ARC-013/016. A frame sequence that ends without an
// authenticated final-marked frame is ARCHIVE_TRUNCATED — distinguishable from
// both DECRYPT_FAILED and ARCHIVE_SIGNATURE_INVALID, neither of which means
// content is missing.
func driveTruncatedTail(rep *report.Report, c corpus.Case) {
	f := newFixture()
	container, err := f.multiFrameArchive()
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("build the fixture archive: %v", err))
		return
	}

	truncated := truncateFinalFrame(container)
	got := archive.Code(mustFailOpen(truncated, f.passphrase, f.pub))

	var diffs []report.Diff
	if want := expectedCode(c); got != want {
		diffs = append(diffs, report.Diff{Field: "error.code", Expected: want, Actual: got})
	}
	// The case's `distinguishable_from` is a LIST here, and both members are
	// reachable from this same fixture: a wrong passphrase and a tampered header.
	if raw, ok := c.Expected["distinguishable_from"].([]any); ok {
		reach := map[string]string{
			archive.CodeDecryptFailed:           archive.Code(mustFailOpen(container, "not-the-passphrase", f.pub)),
			archive.CodeArchiveSignatureInvalid: archive.Code(mustFailOpen(tamperHeaderDigest(container), f.passphrase, f.pub)),
		}
		for _, want := range raw {
			w, _ := want.(string)
			if reach[w] != w {
				diffs = append(diffs, report.Diff{Field: "distinguishable_from[" + w + "]", Expected: w, Actual: reach[w]})
			}
			if w == got {
				diffs = append(diffs, report.Diff{Field: "distinguishable_from[" + w + "]",
					Expected: "a code different from " + got, Actual: w})
			}
		}
	}
	// "restore_completed: false" and "final_marked_frame_authenticated: false" are
	// both implied by the refusal returning an error rather than a manifest, which
	// mustFailOpen already asserted. Recorded as a note so the case's own fields are
	// accounted for rather than silently unread.
	notes := []string{"final_marked_frame_authenticated / restore_completed: observed as Open returning an error and no manifest"}

	finishWithNotes(rep, c, diffs, notes)
}

// driveSignatureInvalid: ARC-020/021/023. Verification happens BEFORE any
// decryption, and a header whose signed digest does not describe the body is
// refused ARCHIVE_SIGNATURE_INVALID.
func driveSignatureInvalid(rep *report.Report, c corpus.Case) {
	f := newFixture()
	container, err := f.multiFrameArchive()
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("build the fixture archive: %v", err))
		return
	}

	// The case's input describes a header whose `recomputed_digest` disagrees with
	// its signed `digest`, so the fixture tampers with exactly that field.
	got := archive.Code(mustFailOpen(tamperHeaderDigest(container), f.passphrase, f.pub))

	var diffs []report.Diff
	if want := expectedCode(c); got != want {
		diffs = append(diffs, report.Diff{Field: "error.code", Expected: want, Actual: got})
	}
	// "decryption_attempted: false" — the differential construction from the package
	// doc: tampered header AND wrong passphrase. Verify-first reports
	// ARCHIVE_SIGNATURE_INVALID; decrypt-first would report DECRYPT_FAILED.
	if wantAttempted, ok := c.Expected["decryption_attempted"].(bool); ok && !wantAttempted {
		code := archive.Code(mustFailOpen(tamperHeaderDigest(container), "not-the-passphrase", f.pub))
		if code != archive.CodeArchiveSignatureInvalid {
			diffs = append(diffs, report.Diff{Field: "decryption_attempted",
				Expected: "ARCHIVE_SIGNATURE_INVALID with a wrong passphrase too, proving the signature was checked first",
				Actual:   code})
		}
	}

	finish(rep, c, diffs)
}

// driveAssetsByReference: ARC-060/063/093. A `by-reference` asset carries no tar
// entry in the body — the destination resolves it from its own content-addressed
// store — so the observable claim is the ABSENCE of an entry for that asset_ref.
func driveAssetsByReference(rep *report.Report, c corpus.Case) {
	var entry archive.AssetEntry
	if err := remarshal(c.Input["asset_entry"], &entry); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode input.asset_entry: %v", err))
		return
	}

	f := newFixture()
	m := f.minimalManifest()
	m.Assets = []archive.AssetEntry{entry}
	container, err := f.createFrom(m, nil)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("create with a by-reference asset: %v", err))
		return
	}
	_, entries, err := archive.Open(bytes.NewReader(container), f.passphrase, f.pub)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("open: %v", err))
		return
	}

	var diffs []report.Diff
	if wantPresent, ok := c.Expected["tar_entry_present"].(bool); ok {
		// Asset entries are named `assets/<hex>` (the asset_ref minus its "sha256:"
		// prefix), so the comparison is against that name — matching on the raw ref
		// would find nothing and report "absent" for every asset, embedded ones
		// included, which would make this assertion pass for the wrong reason.
		wantName := "assets/" + strings.TrimPrefix(entry.AssetRef, "sha256:")
		present := false
		for _, e := range entries {
			if e.Name == wantName {
				present = true
			}
		}
		if present != wantPresent {
			diffs = append(diffs, report.Diff{Field: "tar_entry_present", Expected: wantPresent, Actual: present})
		}
	}
	// `resolved` and `resolution_source` describe what the DESTINATION does with the
	// reference, which is restore-path behaviour. Recorded as unobservable here
	// rather than quietly treated as satisfied.
	notes := []string{"resolved / resolution_source: not observable without a restore path — " + restorePathPending}

	finishWithNotes(rep, c, diffs, notes)
}

// ---- the restore-decision cases ------------------------------------------
//
// These three are decided by archive.Preflight against a described destination,
// before a restore applies anything. Each case's `input` says what the
// destination looks like; the driver builds that destination and diffs the
// verdict.
//
// Note what is NOT claimed by driving them: Preflight is the DECISION half of a
// restore and nothing in either binary calls it. These cases pin that the
// refusals are correct, not that a restore exists.

// driveEpochTooNew: ARC-041/104. An archive whose platform_schema_epoch is newer
// than the destination understands refuses to open — identically on fresh
// infrastructure and over an already-running destination, which is the variant
// this case names.
func driveEpochTooNew(rep *report.Report, c corpus.Case) {
	m, ok := c.Input["manifest"].(map[string]any)
	if !ok {
		rep.Fail(c.CaseID, contract, "case declares no input.manifest")
		return
	}
	archiveEpoch, ok := m["platform_schema_epoch"].(float64)
	if !ok {
		rep.Fail(c.CaseID, contract, "case declares no input.manifest.platform_schema_epoch")
		return
	}
	dstEpoch, ok := c.Input["destination_understood_epoch"].(float64)
	if !ok {
		rep.Fail(c.CaseID, contract, "case declares no input.destination_understood_epoch")
		return
	}

	res := archive.Preflight(
		archive.Manifest{PlatformSchemaEpoch: int(archiveEpoch)},
		archive.Destination{SchemaEpoch: int(dstEpoch), PackTrust: trustEverything, HasAsset: resolveEverything},
	)

	var diffs []report.Diff
	if want := expectedCode(c); !hasFatalCode(res, want) {
		diffs = append(diffs, report.Diff{Field: "error.code", Expected: want, Actual: fatalCodes(res)})
	}
	// "workspace_opened: false" is exactly what a fatal verdict means here, and it is
	// asserted rather than assumed: a refusal that still permitted the restore would
	// be the defect this case exists for.
	if wantOpened, ok := c.Expected["workspace_opened"].(bool); ok && !wantOpened && res.OK() {
		diffs = append(diffs, report.Diff{Field: "workspace_opened", Expected: false, Actual: true})
	}
	// "maintenance_mode: true" describes what the DESTINATION does after refusing —
	// a platform behaviour outside this decision's reach. Recorded rather than
	// quietly treated as satisfied.
	notes := []string{"maintenance_mode: not observable from the restore decision — it is destination behaviour after the refusal"}

	finishWithNotes(rep, c, diffs, notes)
}

// driveYankedPackBlocked: ARC-101/102. The destination's CURRENT trust state
// marks the locked version yanked and offers no substitute, so that one pack is
// blocked with an operator-facing signal — and the restore itself proceeds.
func driveYankedPackBlocked(rep *report.Report, c corpus.Case) {
	pack, err := lockedPackFrom(c)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	trust, _ := c.Input["destination_trust_state"].(map[string]any)
	substitute, _ := trust["substitute_available"].(bool)

	consulted := false
	res := archive.Preflight(
		archive.Manifest{Packs: []archive.PackLock{pack}},
		archive.Destination{
			SchemaEpoch: 4,
			HasAsset:    resolveEverything,
			PackTrust: func(p archive.PackLock) archive.PackTrust {
				// The flag is the case's own assertion that the DESTINATION's trust state
				// was consulted (ARC-101), rather than the archive's.
				consulted = true
				t := archive.PackTrust{Yanked: trustSaysYanked(trust, p)}
				if substitute {
					t.Substitute = "substituted-by-the-destination"
				}
				return t
			},
		},
	)

	var diffs []report.Diff
	out, found := packOutcome(res, pack.PackID)
	switch {
	case !found:
		diffs = append(diffs, report.Diff{Field: "packs[]", Expected: pack.PackID, Actual: "no outcome for this pack"})
	default:
		if want := expectedCode(c); out.Code != want {
			diffs = append(diffs, report.Diff{Field: "error.code", Expected: want, Actual: out.Code})
		}
		if wantRestored, ok := c.Expected["pack_restored"].(bool); ok && out.Restored != wantRestored {
			diffs = append(diffs, report.Diff{Field: "pack_restored", Expected: wantRestored, Actual: out.Restored})
		}
		if wantSignal, ok := c.Expected["operator_signal_raised"].(bool); ok && out.OperatorSignal != wantSignal {
			diffs = append(diffs, report.Diff{Field: "operator_signal_raised", Expected: wantSignal, Actual: out.OperatorSignal})
		}
	}
	// "trust_source_consulted: destination current trust state" — observed by the
	// destination's own callback having been invoked. A preflight that decided from
	// the archive's contents would never have called it.
	if _, ok := c.Expected["trust_source_consulted"]; ok && !consulted {
		diffs = append(diffs, report.Diff{Field: "trust_source_consulted",
			Expected: "the destination's trust state", Actual: "the destination was never asked"})
	}

	finish(rep, c, diffs)
}

// driveDevChannelRefused: ARC-052/103. A dev-channel lock on a destination
// without developer mode refuses THAT pack — and the restore overall still
// succeeds, which is the half a whole-restore refusal would get wrong.
func driveDevChannelRefused(rep *report.Report, c corpus.Case) {
	pack, err := lockedPackFrom(c)
	if err != nil {
		rep.Fail(c.CaseID, contract, err.Error())
		return
	}
	devMode, _ := c.Input["destination_developer_mode"].(bool)
	requiredToBoot, _ := c.Input["pack_required_to_boot"].(bool)

	res := archive.Preflight(
		archive.Manifest{Packs: []archive.PackLock{pack}},
		archive.Destination{
			SchemaEpoch: 4, DeveloperMode: devMode,
			PackTrust: trustEverything, HasAsset: resolveEverything,
			BootCritical: func(archive.PackLock) bool { return requiredToBoot },
		},
	)

	var diffs []report.Diff
	out, found := packOutcome(res, pack.PackID)
	if !found {
		diffs = append(diffs, report.Diff{Field: "packs[]", Expected: pack.PackID, Actual: "no outcome for this pack"})
	} else {
		if want := expectedCode(c); out.Code != want {
			diffs = append(diffs, report.Diff{Field: "error.code", Expected: want, Actual: out.Code})
		}
		if wantRestored, ok := c.Expected["pack_restored"].(bool); ok && out.Restored != wantRestored {
			diffs = append(diffs, report.Diff{Field: "pack_restored", Expected: wantRestored, Actual: out.Restored})
		}
	}
	// "restore_overall_result: succeeded, one pack refused" — the case says the
	// restore SUCCEEDS, so a fatal verdict here is a divergence even though the
	// refusal code matches. This is the assertion that catches an implementation
	// which refuses correctly but too widely.
	if want, ok := c.Expected["restore_overall_result"].(string); ok && strings.HasPrefix(want, "succeeded") && !res.OK() {
		diffs = append(diffs, report.Diff{Field: "restore_overall_result", Expected: want, Actual: fatalCodes(res)})
	}

	finish(rep, c, diffs)
}

// driveManifestIncremental: ARC-090/091/093.
//
// This case's `expected.manifest` is a PARTIAL manifest — it declares mode,
// base_archive, assets and workspace_snapshot_mode, and nothing else. It is an
// assertion about those members, not a document to round-trip whole: it carries
// no created_at, workspace_id or data_key_wrap, and a conformant archive cannot
// be built from it alone. So the driver starts from a complete minimal manifest,
// overlays what the case declares, and compares only what the case declared.
func driveManifestIncremental(rep *report.Report, c corpus.Case) {
	var want archive.Manifest
	if err := remarshal(c.Expected["manifest"], &want); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode expected.manifest: %v", err))
		return
	}

	// A case whose own asset table cannot be expressed is reported PENDING here
	// rather than from a hardcoded list, so the reason is DERIVED from the case and
	// the case starts driving the moment the corpus is corrected — with no change
	// to this driver.
	if bad, ok := unusableAssetRef(want.Assets); ok {
		rep.Pending(c.CaseID, contract, malformedRefPending(bad))
		return
	}

	f := newFixture()
	build := f.minimalManifest()
	build.Mode = want.Mode
	build.BaseArchive = want.BaseArchive
	build.Assets = want.Assets

	container, err := f.createFrom(build, nil)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("create an incremental archive: %v", err))
		return
	}
	got, entries, err := archive.Open(bytes.NewReader(container), f.passphrase, f.pub)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("open a round-tripped incremental archive: %v", err))
		return
	}

	var diffs []report.Diff
	if got.Mode != want.Mode {
		diffs = append(diffs, report.Diff{Field: "manifest.mode", Expected: want.Mode, Actual: got.Mode})
	}
	switch {
	case want.BaseArchive == nil:
	case got.BaseArchive == nil:
		diffs = append(diffs, report.Diff{Field: "manifest.base_archive", Expected: *want.BaseArchive, Actual: nil})
	case *got.BaseArchive != *want.BaseArchive:
		diffs = append(diffs, report.Diff{Field: "manifest.base_archive", Expected: *want.BaseArchive, Actual: *got.BaseArchive})
	}
	if !reflect.DeepEqual(got.Assets, want.Assets) {
		diffs = append(diffs, report.Diff{Field: "manifest.assets", Expected: want.Assets, Actual: got.Assets})
	}

	// ARC-091's economy claim: an inherited entry costs one manifest row and NO
	// bytes. Asserted rather than assumed — re-embedding an unchanged asset would
	// still round-trip cleanly and is exactly the waste incremental mode exists to
	// avoid.
	byName := map[string]bool{}
	for _, e := range entries {
		byName[e.Name] = true
	}
	for _, a := range want.Assets {
		name := "assets/" + strings.TrimPrefix(a.AssetRef, "sha256:")
		switch a.Storage {
		case archive.StorageInherited:
			if byName[name] {
				diffs = append(diffs, report.Diff{Field: "tar entry for inherited " + a.AssetRef,
					Expected: "absent — its bytes live in the base archive", Actual: "present"})
			}
		case archive.StorageEmbedded:
			// The other half: an entry the case says is freshly embedded must actually
			// carry its bytes here, or "inherited" would be indistinguishable from
			// "forgotten".
			if !byName[name] {
				diffs = append(diffs, report.Diff{Field: "tar entry for embedded " + a.AssetRef,
					Expected: "present", Actual: "absent"})
			}
		}
	}

	// "workspace_snapshot_mode": "full" — ARC-091 says the snapshot is always
	// embedded in full regardless of mode, never incrementally diffed. Observed as
	// its tar entry being present in an INCREMENTAL archive.
	if mode, ok := c.Expected["workspace_snapshot_mode"].(string); ok && mode == "full" && !byName[archive.SnapshotEntryName] {
		diffs = append(diffs, report.Diff{Field: "workspace_snapshot_mode",
			Expected: "the snapshot embedded in full even in an incremental archive", Actual: "no " + archive.SnapshotEntryName + " entry"})
	}

	finish(rep, c, diffs)
}

// unusableAssetRef reports the first asset_ref in assets that is not a
// well-formed `sha256:` URI with 64 lowercase hex digits — the shape every path
// through this package validates.
func unusableAssetRef(assets []archive.AssetEntry) (archive.AssetEntry, bool) {
	for _, a := range assets {
		hex, found := strings.CutPrefix(a.AssetRef, "sha256:")
		if !found || len(hex) != 64 {
			return a, true
		}
	}
	return archive.AssetEntry{}, false
}

// malformedRefPending is the shared reason for a case whose asset table carries a
// ref no implementation can accept. Written once because two frozen cases carry
// the SAME truncated value, and two differently-worded explanations of one corpus
// defect would read as two defects.
func malformedRefPending(a archive.AssetEntry) string {
	return fmt.Sprintf("the case declares an asset (%s, storage %q) whose asset_ref hex part is %d characters rather than 64, "+
		"so it is not a sha256 URI any path through internal/archive accepts and the case cannot be built. "+
		"The same truncated value appears in the contract's own Wire shapes example and in a second frozen case, "+
		"so it is one corpus defect rather than a property of this case",
		a.AssetRef, a.Storage, len(strings.TrimPrefix(a.AssetRef, "sha256:")))
}

// ---- comparison helpers --------------------------------------------------

// lockedPackFrom builds the case's locked pack. schema_epoch and source are not
// members these cases declare — they are manifest-validation concerns, not
// restore-decision ones — so they are filled with values that satisfy the shape
// without standing in for anything the case asserts.
func lockedPackFrom(c corpus.Case) (archive.PackLock, error) {
	raw, ok := c.Input["locked_pack"].(map[string]any)
	if !ok {
		return archive.PackLock{}, fmt.Errorf("case declares no input.locked_pack")
	}
	var p archive.PackLock
	if err := remarshal(raw, &p); err != nil {
		return archive.PackLock{}, fmt.Errorf("decode input.locked_pack: %w", err)
	}
	p.Source, p.SchemaEpoch = "https://index.example", 1
	return p, nil
}

// trustSaysYanked reads the case's own trust-state map, whose keys are
// "<pack_id>@<version>".
func trustSaysYanked(trust map[string]any, p archive.PackLock) bool {
	state, _ := trust[p.PackID+"@"+p.Version].(string)
	return state == "yanked" || state == "revoked"
}

func trustEverything(archive.PackLock) archive.PackTrust { return archive.PackTrust{} }
func resolveEverything(string) bool                      { return true }

func fatalCodes(r archive.PreflightResult) []string {
	var out []string
	for _, e := range r.Fatal {
		out = append(out, e.ErrCode)
	}
	return out
}

func hasFatalCode(r archive.PreflightResult, code string) bool {
	for _, e := range r.Fatal {
		if e.ErrCode == code {
			return true
		}
	}
	return false
}

func packOutcome(r archive.PreflightResult, packID string) (archive.PackOutcome, bool) {
	for _, p := range r.Packs {
		if p.Pack.PackID == packID {
			return p, true
		}
	}
	return archive.PackOutcome{}, false
}

func expectedCode(c corpus.Case) string {
	e, _ := c.Expected["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func finish(rep *report.Report, c corpus.Case, diffs []report.Diff) {
	finishWithNotes(rep, c, diffs, nil)
}

func finishWithNotes(rep *report.Report, c corpus.Case, diffs []report.Diff, notes []string) {
	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("%d expectation(s) diverged", len(diffs)), diffs...)
		return
	}
	rep.Pass(c.CaseID, contract, notes...)
}

func remarshal(from any, into any) error {
	b, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// mustFailOpen opens container and returns the error. A nil error is turned into
// a non-nil sentinel so a caller comparing codes never mistakes "it succeeded"
// for "no code matched" — the two look identical through archive.Code otherwise.
func mustFailOpen(container []byte, passphrase string, pub ed25519.PublicKey) error {
	_, _, err := archive.Open(bytes.NewReader(container), passphrase, pub)
	if err == nil {
		return fmt.Errorf("archive opened successfully where a refusal was expected")
	}
	return err
}

// ---- container surgery ---------------------------------------------------
//
// Each helper edits a real Create output rather than hand-building bytes, so the
// only thing wrong with the result is the one fault under test.

// bodyOffset returns where the encrypted body begins: past the 4-byte length
// prefix and the header it sizes (ARC-001).
func bodyOffset(container []byte) int {
	return 4 + int(binary.BigEndian.Uint32(container[:4]))
}

// tamperHeaderDigest flips a character of the header's `digest` field, breaking
// the signature that covers it. It edits the CLEARTEXT header in place, which is
// what makes this a header-integrity fault rather than a body one.
func tamperHeaderDigest(container []byte) []byte {
	out := append([]byte(nil), container...)
	header := out[4:bodyOffset(out)]
	var h map[string]any
	if json.Unmarshal(header, &h) != nil {
		return out
	}
	d, _ := h["digest"].(string)
	if d == "" {
		return out
	}
	// Same length, different value: a re-marshal must fit the original region, since
	// the length prefix sizes it.
	flipped := []byte(d)
	if flipped[0] == '0' {
		flipped[0] = '1'
	} else {
		flipped[0] = '0'
	}
	h["digest"] = string(flipped)
	replaced, err := json.Marshal(h)
	if err != nil || len(replaced) != len(header) {
		// Key order or escaping changed the length; fall back to editing the raw bytes
		// of the digest value in place, which cannot change the length.
		idx := bytes.Index(out, []byte(d))
		if idx < 0 {
			return out
		}
		out[idx] = flipped[0]
		return out
	}
	copy(header, replaced)
	return out
}

// truncateFinalFrame drops the last frame's bytes, leaving a body that reaches
// EOF without an authenticated final-marked frame (ARC-016). Every remaining
// frame still authenticates individually, which is exactly why per-frame
// authentication cannot catch this.
func truncateFinalFrame(container []byte) []byte {
	start := bodyOffset(container)
	last := start
	for at := start; at+4 <= len(container); {
		flen := int(binary.BigEndian.Uint32(container[at : at+4]))
		if flen <= 0 || at+4+flen > len(container) {
			break
		}
		last = at
		at += 4 + flen
	}
	return append([]byte(nil), container[:last]...)
}

// corruptFrame flips one ciphertext byte of frame n, so that frame fails AEAD
// authentication while every other frame remains intact.
func corruptFrame(container []byte, n int) []byte {
	out := append([]byte(nil), container...)
	at := bodyOffset(out)
	for i := 0; at+4 <= len(out); i++ {
		flen := int(binary.BigEndian.Uint32(out[at : at+4]))
		if flen <= 0 || at+4+flen > len(out) {
			return out
		}
		if i == n {
			out[at+4] ^= 0xff
			return out
		}
		at += 4 + flen
	}
	return out
}

// ---- corpus loading ------------------------------------------------------

func corpusDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "corpora", "archive-1")
}

// LoadCorpus reads every frozen archive-1 case.
func LoadCorpus() (map[string]corpus.Case, error) {
	return corpus.LoadDir(corpusDir())
}
