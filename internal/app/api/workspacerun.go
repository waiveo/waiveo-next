package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/app/workspacekey"
	"github.com/maaxton/waiveo-next/internal/archive"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/apijob"
)

// workspacerun.go is the EXECUTION half of the two data-subject operations
// workspace.go accepts — the part that makes each 202 + Job a promise the
// surface keeps (API-123), exactly as jobrun.go does for the bulk-enable path.
//
// Both executions drive the SAME persisted apijob state machine every other Job
// does: `running` is committed BEFORE the work is attempted, so a crash
// mid-execution leaves a target visibly `running` — the state API-116's resume
// is defined over — rather than a `pending` target whose workspace may already be
// half-exported or half-destroyed.
//
// Neither re-authorizes. The acceptance already established `owner` at the
// workspace (workspace.go), and that decision is fixed by the world the job was
// accepted in; re-deriving authority here, off the request and with no principal
// in hand, would be a second and weaker answer to a question already answered.

// WorkspaceArchive is the deployment collaborator the export operation writes
// through: where the `archive/1` container lands, and the workspace signing key
// (SEC-046) whose private half produces the header signature ARC-021 requires.
//
// It is a wired collaborator rather than something this package constructs
// because both halves are DEPLOYMENT facts: an archive destination is a
// filesystem location the operator chooses, and key material is persisted state
// with a custody story (SEC-047) that belongs nowhere near an HTTP handler. A
// handler built without one still mounts the route — it answers UNAVAILABLE
// rather than 404, so "this deployment is not configured for it" is
// distinguishable from "this operation does not exist" (workspace.go).
type WorkspaceArchive struct {
	// Dir is the directory containers are written to. It is created 0700 on
	// first use.
	Dir string
	// Key is the workspace signing key. Required: ARC-021 makes an unsigned
	// container unreadable by a conformant restorer, so an export with nothing
	// to sign with is a failed export, not an unsigned one.
	Key *workspacekey.Key
	// KDF are the argon2id parameters recorded in the container header
	// (ARC-010). The zero value means archive.DefaultKDFParams() — a test may
	// substitute lighter ones so a suite does not spend a second per export
	// stretching a passphrase it does not care about.
	KDF archive.KDFParams
	// Rand is the randomness source for the per-archive KDF salt and base nonce
	// (ARC-010/017). Nil means crypto/rand.
	Rand io.Reader
}

// WithWorkspaceArchive wires the export operation's archive destination and
// signing key. Without it the route still mounts and still authorizes, and
// answers UNAVAILABLE — never 202 for work nothing can perform.
func WithWorkspaceArchive(a *WorkspaceArchive) Option {
	return func(srv *server) { srv.workspaceArchive = a }
}

// runWorkspaceExport executes one accepted export Job: it drives the single
// target (the workspace itself) through API-113's progression and, in between,
// produces exactly the container `archive/1` defines (API-121).
func (srv *server) runWorkspaceExport(ctx context.Context, jobID, workspaceID, passphrase string) {
	if _, err := srv.store.AdvanceJob(ctx, jobID, func(j *apijob.Job) error {
		return j.StartTarget(workspaceID)
	}); err != nil {
		// The job is gone, or the target is not pending (already canceled, or
		// already run). The state machine has refused verbatim and there is no
		// legal transition to make.
		return
	}
	code, detail := srv.writeWorkspaceArchive(ctx, jobID, workspaceID, passphrase)
	srv.finishWorkspaceTarget(ctx, jobID, workspaceID, code, detail)
}

// runWorkspaceDelete executes one accepted delete Job: SEC-121's key-material
// destruction, which API-122 makes this operation's whole meaning.
func (srv *server) runWorkspaceDelete(ctx context.Context, jobID, workspaceID string) {
	if _, err := srv.store.AdvanceJob(ctx, jobID, func(j *apijob.Job) error {
		return j.StartTarget(workspaceID)
	}); err != nil {
		return
	}
	code, detail := srv.destroyWorkspace(ctx)
	srv.finishWorkspaceTarget(ctx, jobID, workspaceID, code, detail)
}

// finishWorkspaceTarget commits the single target's terminal state: succeeded
// when code is empty, otherwise failed with a code from api/1's OWN error-code
// registry (API-115 — never a parallel vocabulary; apijob.FailTarget refuses
// anything outside the closed set).
func (srv *server) finishWorkspaceTarget(ctx context.Context, jobID, workspaceID, code, detail string) {
	_, _ = srv.store.AdvanceJob(ctx, jobID, func(j *apijob.Job) error {
		if code == "" {
			return j.SucceedTarget(workspaceID)
		}
		return j.FailTarget(workspaceID, code, detail)
	})
}

// writeWorkspaceArchive produces the container and returns "" on success, or the
// registry code and detail the target's failure is typed with.
//
// The container is assembled and written by internal/archive, which owns every
// byte of archive/1's format. This function's whole job is to collect what that
// format carries — the consistent relational snapshot (ARC-083), the pack
// lockfile (ARC-050), the asset references (ARC-060) — and hand it over. It
// makes no format decision of its own, which is exactly what API-121 forbids
// ("not a distinct export format or a second code path").
func (srv *server) writeWorkspaceArchive(ctx context.Context, jobID, workspaceID, passphrase string) (code, detail string) {
	cfg := srv.workspaceArchive
	if cfg == nil || cfg.Key == nil {
		return "UNAVAILABLE", "This deployment is not configured with a workspace archive destination."
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return "INTERNAL", "The archive destination could not be prepared."
	}

	// The relational snapshot is taken through the store's own consistent-
	// snapshot path (ARC-083) into a scratch file beside the destination, then
	// streamed into the container and removed. It is scratch, not a second
	// artifact: it exists only because a tar entry needs its size up front.
	snapPath := filepath.Join(cfg.Dir, "."+jobID+".snapshot")
	_ = os.Remove(snapPath)
	if err := srv.store.SnapshotInto(ctx, snapPath); err != nil {
		return "INTERNAL", "The workspace snapshot could not be taken."
	}
	defer func() { _ = os.Remove(snapPath) }()

	packs, err := srv.workspacePackLocks(ctx)
	if err != nil {
		return "INTERNAL", "The installed packs could not be read."
	}
	assets, err := srv.workspaceAssets(ctx)
	if err != nil {
		return "INTERNAL", "The referenced assets could not be enumerated."
	}

	// A single file, named by the Job that produced it, so the artifact an
	// operator finds on disk is traceable back to the request that asked for it
	// and two exports can never collide.
	outPath := filepath.Join(cfg.Dir, "workspace-"+jobID+".waiveo-archive")
	out, err := os.OpenFile(outPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "INTERNAL", "The archive file could not be created."
	}
	src := archive.Source{
		CreatedAt:           srv.nowMs(),
		WorkspaceID:         workspaceID,
		PlatformSchemaEpoch: store.PlatformSchemaEpoch,
		Packs:               packs,
		Assets:              assets,
		// The workspace holds no wrapped secret stubs: nothing in this tree mints
		// one yet. The field is still present and empty rather than omitted —
		// ARC-031 makes it a required member of the manifest shape, and a reader
		// distinguishes "carries no stubs" from "is not a conformant manifest" by
		// its presence.
		SecretStubs: nil,
		// The data key IS carried, re-wrapped under the sub-key archive/1 derives
		// for that purpose (ARC-011/071). The wrap itself is performed by the key
		// hierarchy that owns the key (workspacekey.Key.WrapDataKey), never by
		// the archive package — archive/1's Scope puts the wrap/unwrap algorithm
		// explicitly outside it.
		WrapDataKey: cfg.Key.WrapDataKey,
		Snapshot:    func() (io.ReadCloser, int64, error) { return openSized(snapPath) },
		Asset:       srv.openWorkspaceAsset,
	}
	writeErr := archive.Create(out, src, archive.Options{
		Passphrase:  passphrase,
		Signer:      cfg.Key.Private(),
		SignerKeyID: cfg.Key.KeyID(),
		KDF:         cfg.KDF,
		Rand:        cfg.Rand,
	})
	closeErr := out.Close()
	if writeErr != nil || closeErr != nil {
		// A partially written container is not an archive — it would fail its own
		// truncation check (ARC-016) on the first read. Removing it means the
		// failed target leaves no artifact behind that could be mistaken for one.
		_ = os.Remove(outPath)
		return "INTERNAL", "The workspace archive could not be written."
	}
	return "", ""
}

// openSized opens path for reading and reports its size — the two things a tar
// entry needs before its bytes can be streamed.
func openSized(path string) (io.ReadCloser, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// openWorkspaceAsset resolves one asset_ref to its bytes in the shared content
// origin — the source an `embedded` manifest entry's tar entry is written from
// (ARC-061).
func (srv *server) openWorkspaceAsset(assetRef string) (io.ReadCloser, int64, error) {
	if srv.content == nil {
		return nil, 0, fmt.Errorf("api: no content origin is wired")
	}
	b := srv.content.Serve(strings.TrimPrefix(assetRef, "sha256:"))
	if b == nil {
		return nil, 0, fmt.Errorf("api: asset %s is not present in the content origin", assetRef)
	}
	return io.NopCloser(strings.NewReader(string(b))), int64(len(b)), nil
}

// workspacePackLocks projects the installed packs onto archive/1's pack lockfile
// (ARC-050).
//
// Two of ARC-050's five fields are recorded honestly as UNKNOWN rather than
// guessed, and the direction of the guess matters:
//
//   - `source` (the registry the pack was resolved from) is not persisted by the
//     install pipeline, so it is empty. An invented value would be a claim about
//     provenance this deployment cannot substantiate.
//   - `channel` (the pack's trust channel at export time) is likewise not
//     persisted, and it is recorded as `dev` — the FAIL-CLOSED value. ARC-052
//     exists so restore-time gating can treat a dev-channel lock differently, and
//     ARC-103 makes a dev-channel lock refuse on a destination without developer
//     mode enabled. Recording `dev` therefore makes a restore of a pack whose
//     provenance we did not record require an explicit operator decision, where
//     recording `first-party` would assert a trust level nothing in this tree
//     verified. A false refusal is recoverable; a false assertion of trust is not.
//
// Persisting the resolved channel and source at install time is the fix, and it
// belongs in the install pipeline that actually knows them, not here.
func (srv *server) workspacePackLocks(ctx context.Context) ([]archive.PackLock, error) {
	rows, err := srv.store.ListPacks(ctx)
	if err != nil {
		return nil, err
	}
	locks := make([]archive.PackLock, 0, len(rows))
	for _, p := range rows {
		locks = append(locks, archive.PackLock{
			PackID:      p.ID,
			Version:     p.Version,
			Channel:     "dev",
			Source:      "",
			SchemaEpoch: p.DataModelVersion,
		})
	}
	return locks, nil
}

// workspaceAssets enumerates every asset the workspace's own relational data
// references, which is exactly the set ARC-064 requires the manifest to carry:
// "an asset referenced by workspace data but absent from the manifest entirely
// MUST cause `MANIFEST_INVALID` at manifest-validation time".
//
// The references come from the playlist rows' items — the same `asset_ref`
// projection validatePlaylistAssets already guards on write (scheduling.go), so
// the export enumerates precisely the set the authoring surface enforces is
// resolvable. Each ref appears once, in the order first encountered, so the
// manifest is stable across exports of an unchanged workspace.
//
// An asset present in the content origin is `embedded` and its bytes ride the
// container. One that is REFERENCED but not present is carried `by-reference`
// rather than dropped: ARC-063 defines that value for bytes not carried here,
// and a restore that cannot resolve one fails closed with ASSET_UNAVAILABLE.
// Dropping it instead would produce a manifest that is invalid by ARC-064's own
// rule, and silently so.
func (srv *server) workspaceAssets(ctx context.Context) ([]archive.AssetEntry, error) {
	rows, err := srv.store.List(ctx, store.KindPlaylist, store.ListFilter{})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	entries := []archive.AssetEntry{}
	for _, res := range rows {
		var pl struct {
			Items []datamodel.PlaylistItem `json:"items"`
		}
		if err := json.Unmarshal(res.Body, &pl); err != nil {
			continue
		}
		for _, item := range pl.Items {
			if item.AssetRef == "" || seen[item.AssetRef] {
				continue
			}
			seen[item.AssetRef] = true
			entry := archive.AssetEntry{AssetRef: item.AssetRef, Storage: archive.StorageByReference}
			if srv.content != nil {
				if b := srv.content.Serve(strings.TrimPrefix(item.AssetRef, "sha256:")); b != nil {
					entry.Storage = archive.StorageEmbedded
					entry.Size = int64(len(b))
				}
			}
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// destroyWorkspace performs SEC-121's key-material destruction and returns "" on
// success, or the registry code and detail the target's failure is typed with.
//
// SEC-121 enumerates what a factory reset destroys: "the workspace and its data
// key (SEC-040), the box key (SEC-041), the workspace signing key (SEC-046-047),
// and the relay's device identity plus its enrollment certificate/keypair". Of
// those, the data key, the box key and the relay's own identity do not exist in
// this tree — the wrapped-secret hierarchy is unimplemented, and the relay's
// enrollment material is the relay's own persisted state, not the app's. What
// this deployment actually holds is destroyed, and the gap is named here rather
// than left for a reader to infer from silence.
//
// The ORDER is the design. Credentials go LAST:
//
//	content assets -> relational rows -> workspace signing key -> principals
//
// Every step before the last is recoverable-by-retry if it fails, because the
// owner who submitted the request still holds a live session and can look at
// what happened. Destroying credentials first would mean a failure at any later
// step left a deployment that is simultaneously not erased and not
// administrable — the worst of both outcomes. Once credentials are gone the
// deployment re-opens its first-boot claim window (SEC-120) by construction, and
// the erasure is complete.
//
// One consequence is worth stating plainly rather than discovering: the client
// polling this Job (API-123) is holding a credential this operation destroys, so
// once the target reaches `succeeded` that client's own session no longer
// authenticates. That is the destruction working, not the poll failing — the Job
// record itself survives the purge (store.PurgeWorkspace keeps the jobs tables
// precisely so the operation's outcome stays readable), so the deployment's next
// owner can still read what happened.
func (srv *server) destroyWorkspace(ctx context.Context) (code, detail string) {
	if srv.content != nil {
		if err := srv.content.Purge(); err != nil {
			return "INTERNAL", "The workspace's content assets could not be destroyed."
		}
	}
	if err := srv.store.PurgeWorkspace(ctx); err != nil {
		if err == store.ErrNoWorkspace {
			return "NOT_FOUND", "The workspace no longer exists."
		}
		return "INTERNAL", "The workspace's data could not be destroyed."
	}
	if srv.workspaceArchive != nil && srv.workspaceArchive.Key != nil {
		if err := srv.workspaceArchive.Key.Destroy(); err != nil {
			return "INTERNAL", "The workspace signing key could not be destroyed."
		}
	}
	if srv.authn != nil {
		if err := srv.authn.Store().DestroyAllPrincipals(ctx); err != nil {
			return "INTERNAL", "The workspace's credentials could not be destroyed."
		}
	}
	return "", ""
}
