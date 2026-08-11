package api_test

// workspace_test.go drives api/1's two data-subject operations (API-120–124)
// through the real, authenticated mux: POST /api/v1/workspace/export and POST
// /api/v1/workspace/delete.
//
// It asserts three families of claim, and each is a distinct thing that could
// be wrong independently:
//
//   - ACCEPTANCE. Both answer 202 with a Job whose single target is the
//     workspace itself (API-123), pollable at GET /jobs/{job_id} (API-112).
//   - AUTHORIZATION. Neither is invocable by a non-owner, however broad that
//     principal's ordinary write authority is — the argument for `owner` is in
//     internal/app/api/workspace.go's header.
//   - EXECUTION. The export writes a REAL archive/1 container that reads back
//     and verifies; the delete really destroys, and refuses without its
//     confirmation.
//
// Completion is observed by asking the job runner (testEnv.runJobs), never by
// sleeping: the runner is wired stopped, so each case arranges the world the
// job will run against and then releases it.

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/app/workspacekey"
	"github.com/maaxton/waiveo-next/internal/archive"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"

	"net/http/httptest"
)

// workspaceKeyID is the fixed `signer_key_id` these tests' workspace signing key
// is minted under — a valid ULID (DAT-005a) from a pinned closure, never a
// package-level generator.
const workspaceKeyID = "01J8Z9TESTWSPACEKEY0000001"

// testExportPassphrase is the export passphrase every case here supplies. It is
// a fixture string, not a secret: it protects nothing but a container written
// into a directory the test itself deletes.
const testExportPassphrase = "conformance-export-passphrase"

// lightKDF is argon2id at parameters chosen to be FAST, not strong. Production
// uses archive.DefaultKDFParams(); a test that stretched a passphrase with 256
// MiB of memory per export would spend seconds proving nothing about the
// parameters it spent them on.
func lightKDF() archive.KDFParams {
	return archive.KDFParams{MemoryKiB: 8, Iterations: 1, Parallelism: 1}
}

// workspaceEnv is a testEnv plus the archive destination the export writes into
// and the workspace signing key it signs with — in SEPARATE directories, exactly
// as the feeder wires them (security-model.md SEC-047). A test that shared one
// directory could not notice key material leaking into the export output.
type workspaceEnv struct {
	*testEnv
	archiveDir string
	keyDir     string
	key        *workspacekey.Key
	// storePath is the live store a restore stages beside.
	storePath string
}

// newWorkspaceEnv builds an env whose export operation is fully wired: a scratch
// archive directory, and a real workspace signing key in a directory of its own.
func newWorkspaceEnv(t *testing.T) *workspaceEnv {
	t.Helper()
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	dir := t.TempDir()
	keyDir := t.TempDir()
	key, err := workspacekey.LoadOrCreate(keyDir, func() string { return workspaceKeyID })
	if err != nil {
		t.Fatalf("workspacekey.LoadOrCreate: %v", err)
	}

	storePath := filepath.Join(t.TempDir(), "app.db")
	clock := func() int64 { return fixedNowMs }
	idem := apihttp.NewIdempotencyStore(clock, 0)
	fixture := newAuthFixture(t)
	content := origin.New()
	jobs := api.NewJobRunner()
	ts := httptest.NewServer(api.New(st, idem, clock, ulid.Monotonic(), content, testContentBase, fixture.Auth,
		api.WithJobRunner(jobs),
		api.WithWorkspaceArchive(&api.WorkspaceArchive{Dir: dir, Key: key, KDF: lightKDF()}),
		api.WithStorePath(storePath)))
	t.Cleanup(ts.Close)

	return &workspaceEnv{
		testEnv:    &testEnv{ts: ts, store: st, content: content, contentBase: testContentBase, auth: fixture, jobs: jobs},
		archiveDir: dir,
		keyDir:     keyDir,
		key:        key,
		storePath:  storePath,
	}
}

// seedWorkspace creates the deployment's org node — the row that IS the
// workspace's identity (DAT-010/012/014) — and returns its server-minted id.
//
// It goes through the env's shared orgRoot rather than creating an org of its
// own, because DAT-002 admits exactly ONE org-kind node: a test that seeds the
// workspace AND builds a scope tree (the export case does both) would otherwise
// create a second one and be refused SCOPE_NODE_MULTIPLE_ORG. The workspace's
// org node and the tree's root are the same row in production too.
func (e *workspaceEnv) seedWorkspace(t *testing.T) string {
	t.Helper()
	return e.orgRoot(t)
}

// postWorkspace drives one data-subject operation as who and returns the
// response and body.
func (e *workspaceEnv) postWorkspace(t *testing.T, who authtest.Credential, op string, body any) (*http.Response, []byte) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw = mustJSON(t, body)
	}
	return e.as(t, who, http.MethodPost, "/api/v1/workspace/"+op, raw, nil)
}

// acceptedJob decodes a 202's Job body, failing the test if the status is not
// 202 — so every case below reads a Job it has already proved was ACCEPTED.
func acceptedJob(t *testing.T, resp *http.Response, raw []byte) jobBody {
	t.Helper()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", resp.StatusCode, raw)
	}
	return decodeJob(t, raw)
}

// polledJob reads one Job back through GET /jobs/{job_id} — API-112's only
// completion signal — as who.
func (e *workspaceEnv) polledJob(t *testing.T, who authtest.Credential, jobID string) jobBody {
	t.Helper()
	resp, raw := e.getJob(t, who, jobID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /jobs/%s = %d, want 200 (body %s)", jobID, resp.StatusCode, raw)
	}
	return decodeJob(t, raw)
}

// openArchive opens the container one export Job produced and reads it back
// through archive.Open, which is what makes every assertion below an assertion
// about a REAL archive/1 container: Open verifies the outer header's signature
// against the workspace signing key (ARC-021/023), recomputes the body digest
// over the bytes actually streamed (ARC-024), and refuses a frame sequence that
// does not terminate on exactly one final-marked frame (ARC-016). A stub file
// or an empty one cannot survive it.
func (e *workspaceEnv) openArchive(t *testing.T, jobID string) (archive.Manifest, []archive.Entry) {
	t.Helper()
	path := filepath.Join(e.archiveDir, "workspace-"+jobID+".waiveo-archive")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("the export reported success but wrote no container at %s: %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	key, err := workspacekey.LoadOrCreate(e.keyDir, func() string { return workspaceKeyID })
	if err != nil {
		t.Fatalf("reload workspace signing key: %v", err)
	}
	manifest, entries, err := archive.Open(f, testExportPassphrase, key.Public())
	if err != nil {
		t.Fatalf("archive.Open on the emitted container: %v", err)
	}
	return manifest, entries
}

// TestExportWorkspaceAcceptsWithSingleWorkspaceTargetJob is API-123's shape
// claim: the operation answers 202 with a Job whose target set is exactly the
// workspace itself — one entry, the org node's id — and every target pending,
// because a 202 represents work as ACCEPTED rather than begun.
//
// The target id is compared against the id the seed returned, so the assertion
// is anchored to a real row rather than to whatever the handler happened to
// emit.
func TestExportWorkspaceAcceptsWithSingleWorkspaceTargetJob(t *testing.T) {
	e := newWorkspaceEnv(t)
	orgID := e.seedWorkspace(t)

	resp, raw := e.postWorkspace(t, e.auth.Credential(), "export", map[string]any{"passphrase": testExportPassphrase})
	job := acceptedJob(t, resp, raw)

	if len(job.Targets) != 1 {
		t.Fatalf("targets = %d, want exactly 1 (the workspace itself, API-123): %s", len(job.Targets), raw)
	}
	if job.Targets[0].TargetID != orgID {
		t.Errorf("target_id = %q, want the workspace's org node %q", job.Targets[0].TargetID, orgID)
	}
	if job.Targets[0].State != "pending" {
		t.Errorf("target state = %q, want pending", job.Targets[0].State)
	}
	if job.State != "pending" {
		t.Errorf("job state = %q, want pending", job.State)
	}
	if job.CreatedBy != e.auth.PrincipalID {
		t.Errorf("created_by = %q, want the authenticated caller %q", job.CreatedBy, e.auth.PrincipalID)
	}
	if !ulid.Valid(job.ID) {
		t.Errorf("job id %q is not a valid ULID (DAT-005a)", job.ID)
	}
}

// TestExportWorkspaceJobIsPollable closes API-112's loop for this operation: the
// Job the 202 named is readable at GET /jobs/{job_id}, and after the accepted
// work runs it reports `succeeded` there — which is the ONLY completion signal
// the contract offers.
func TestExportWorkspaceJobIsPollable(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	who := e.auth.Credential()

	resp, raw := e.postWorkspace(t, who, "export", map[string]any{"passphrase": testExportPassphrase})
	job := acceptedJob(t, resp, raw)

	polled := e.polledJob(t, who, job.ID)
	if polled.State != "pending" {
		t.Errorf("polled state before execution = %q, want pending", polled.State)
	}

	e.runJobs()

	done := e.polledJob(t, who, job.ID)
	if done.State != "succeeded" {
		t.Fatalf("polled state after execution = %q, want succeeded (targets %+v)", done.State, done.Targets)
	}
}

// TestExportWorkspaceWritesReadableArchiveContainer is the claim that matters
// most, and the one a stub would fail: the export produces a REAL archive/1
// container — not an empty file, not a placeholder — and it is proved real by
// reading it back through archive.Open, which verifies the header signature
// against the workspace signing key (ARC-021/023), recomputes the body digest
// over the actual streamed bytes (ARC-024), and checks the frame sequence
// terminates on exactly one final-marked frame (ARC-016).
//
// The manifest is then checked to describe THIS workspace: its own org node id,
// the platform schema epoch, and — the part a hand-rolled export would get
// wrong — the workspace snapshot entry archive/1 names, carrying real SQLite
// bytes rather than nothing.
func TestExportWorkspaceWritesReadableArchiveContainer(t *testing.T) {
	e := newWorkspaceEnv(t)
	orgID := e.seedWorkspace(t)
	who := e.auth.Credential()

	resp, raw := e.postWorkspace(t, who, "export", map[string]any{"passphrase": testExportPassphrase})
	job := acceptedJob(t, resp, raw)
	e.runJobs()

	if got := e.polledJob(t, who, job.ID); got.State != "succeeded" {
		t.Fatalf("export job state = %q, want succeeded (targets %+v)", got.State, got.Targets)
	}

	path := filepath.Join(e.archiveDir, "workspace-"+job.ID+".waiveo-archive")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("the export reported success but wrote no container at %s: %v", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat container: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the export wrote an EMPTY container; API-121 requires exactly the container archive/1 defines")
	}

	key, err := workspacekey.LoadOrCreate(e.keyDir, func() string { return workspaceKeyID })
	if err != nil {
		t.Fatalf("reload workspace signing key: %v", err)
	}
	manifest, entries, err := archive.Open(f, testExportPassphrase, key.Public())
	if err != nil {
		t.Fatalf("archive.Open on the emitted container: %v", err)
	}

	if manifest.WorkspaceID != orgID {
		t.Errorf("manifest workspace_id = %q, want this deployment's org node %q", manifest.WorkspaceID, orgID)
	}
	if manifest.Mode != "full" {
		t.Errorf("manifest mode = %q, want full", manifest.Mode)
	}
	if manifest.PlatformSchemaEpoch != store.PlatformSchemaEpoch {
		t.Errorf("manifest platform_schema_epoch = %d, want %d (ARC-040)", manifest.PlatformSchemaEpoch, store.PlatformSchemaEpoch)
	}
	if manifest.CreatedAt != fixedNowMs {
		t.Errorf("manifest created_at = %d, want the injected clock's %d", manifest.CreatedAt, fixedNowMs)
	}

	names := make([]string, 0, len(entries))
	byName := map[string][]byte{}
	for _, en := range entries {
		names = append(names, en.Name)
		byName[en.Name] = en.Body
	}
	snap, ok := byName["workspace.sqlite"]
	if !ok {
		t.Fatalf("container carries no workspace.sqlite entry (ARC-085); entries = %v", names)
	}
	if len(snap) == 0 {
		t.Fatal("workspace.sqlite entry is empty; the relational snapshot did not enter the container")
	}
	// A SQLite database file always begins with this 16-byte magic. Checking it
	// is what distinguishes "a real consistent snapshot rode into the container"
	// (ARC-083) from "some bytes did".
	const sqliteMagic = "SQLite format 3\x00"
	if len(snap) < len(sqliteMagic) || string(snap[:len(sqliteMagic)]) != sqliteMagic {
		t.Errorf("workspace.sqlite does not begin with the SQLite file magic; it is not a database snapshot")
	}
}

// TestExportWorkspaceEmbedsReferencedAssets is ARC-064's claim at the api layer:
// an asset the workspace's own data references appears in the manifest, and its
// bytes ride the container under the name its own hash gives it (ARC-061/062).
func TestExportWorkspaceEmbedsReferencedAssets(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	who := e.auth.Credential()

	// The shared scheduling fixture: a site, a screen, and the uploaded asset a
	// playlist item references — the same rows the authoring surface's own tests
	// build, so the export enumerates a workspace shaped like a real one.
	screenID := seedSchedulingScope(t, e.testEnv)
	resp, raw := e.as(t, who, http.MethodPost, "/api/v1/playlists", rowCreateBody(t, playlistFixture(screenID, nil)), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create playlist: status %d, body %s", resp.StatusCode, raw)
	}
	wantHex := hexOf(playlistFixtureAsset)

	resp, raw = e.postWorkspace(t, who, "export", map[string]any{"passphrase": testExportPassphrase})
	job := acceptedJob(t, resp, raw)
	e.runJobs()
	if got := e.polledJob(t, who, job.ID); got.State != "succeeded" {
		t.Fatalf("export job state = %q, want succeeded (targets %+v)", got.State, got.Targets)
	}

	manifest, entries := e.openArchive(t, job.ID)

	var found bool
	for _, a := range manifest.Assets {
		if a.AssetRef != playlistFixtureAssetRef {
			continue
		}
		found = true
		if a.Storage != archive.StorageEmbedded {
			t.Errorf("asset storage = %q, want embedded — the origin holds these bytes", a.Storage)
		}
		if a.Size != int64(len(playlistFixtureAsset)) {
			t.Errorf("asset size = %d, want %d", a.Size, len(playlistFixtureAsset))
		}
	}
	if !found {
		t.Fatalf("manifest carries no entry for the referenced asset %s (ARC-064); assets = %+v", playlistFixtureAssetRef, manifest.Assets)
	}
	var carried bool
	for _, en := range entries {
		if en.Name == "assets/"+wantHex {
			carried = true
			if string(en.Body) != string(playlistFixtureAsset) {
				t.Errorf("embedded asset bytes differ from the uploaded content")
			}
		}
	}
	if !carried {
		t.Fatalf("container carries no assets/%s entry for the embedded asset (ARC-061)", wantHex)
	}
}

// TestExportWorkspaceCarriesCastReferencedAssets is ARC-064 for the family that
// carries most of a signage workspace's images.
//
// The row export loops every kind, so the container already carried cast rows —
// while the asset manifest enumerated playlist items only. The result is exactly
// the manifest this function's own doc forbids: "an asset referenced by
// workspace data but absent from the manifest entirely MUST cause
// MANIFEST_INVALID at manifest-validation time" — so the archive either fails on
// restore or restores image-less.
func TestExportWorkspaceCarriesCastReferencedAssets(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	who := e.auth.Credential()

	screenID := seedSchedulingScope(t, e.testEnv)
	castAsset := []byte("waiveo-next: the image a cast's slide draws")
	castRef := e.uploadContent(t, castAsset).AssetRef
	resp, raw := e.as(t, who, http.MethodPost, "/api/v1/casts", rowCreateBody(t, datamodel.Cast{
		ScopeNode: screenID, Name: "Exported Menu",
		Slides: []datamodel.CastSlide{{ID: "photo", Layers: []wire.Layer{
			{Kind: wire.LayerKindImage, X: 0, Y: 0, W: 1920, H: 1080, AssetRef: castRef},
		}}},
	}), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create cast: status %d, body %s", resp.StatusCode, raw)
	}

	resp, raw = e.postWorkspace(t, who, "export", map[string]any{"passphrase": testExportPassphrase})
	job := acceptedJob(t, resp, raw)
	e.runJobs()
	if got := e.polledJob(t, who, job.ID); got.State != "succeeded" {
		t.Fatalf("export job state = %q, want succeeded (targets %+v)", got.State, got.Targets)
	}

	manifest, entries := e.openArchive(t, job.ID)

	var found bool
	for _, a := range manifest.Assets {
		if a.AssetRef != castRef {
			continue
		}
		found = true
		if a.Storage != archive.StorageEmbedded {
			t.Errorf("cast asset storage = %q, want embedded — the origin holds these bytes", a.Storage)
		}
		if a.Size != int64(len(castAsset)) {
			t.Errorf("cast asset size = %d, want %d", a.Size, len(castAsset))
		}
	}
	if !found {
		t.Fatalf("the manifest omits the cast's image %s (ARC-064): the archive fails MANIFEST_INVALID on restore, "+
			"or restores a cast with no image; assets = %+v", castRef, manifest.Assets)
	}

	wantHex := hexOf(castAsset)
	var carried bool
	for _, en := range entries {
		if en.Name == "assets/"+wantHex {
			carried = true
			if string(en.Body) != string(castAsset) {
				t.Errorf("embedded cast asset bytes differ from the uploaded content")
			}
		}
	}
	if !carried {
		t.Fatalf("container carries no assets/%s entry for the cast's image (ARC-061)", wantHex)
	}
}

// TestExportWorkspaceRejectsMissingPassphrase: archive/1 derives a container's
// encryption key from the export passphrase (ARC-010), so an export with none
// cannot produce a conformant container. It is a BODY failure, so it is 422 and
// never 400 (API-013a).
func TestExportWorkspaceRejectsMissingPassphrase(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)

	for name, body := range map[string]any{
		"absent": map[string]any{},
		"empty":  map[string]any{"passphrase": ""},
		"blank":  map[string]any{"passphrase": "     "},
		"short":  map[string]any{"passphrase": "sixchars"},
	} {
		t.Run(name, func(t *testing.T) {
			resp, raw := e.postWorkspace(t, e.auth.Credential(), "export", body)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (API-013a: a body failure is 422, never 400); body %s", resp.StatusCode, raw)
			}
			assertProblem(t, resp, raw, "VALIDATION_FAILED")
		})
	}
}

// TestExportWithADestroyedKeyFailsRatherThanReportingSuccess is the regression
// for the worst outcome this operation can produce: a lie the operator acts on.
//
// SEC-121's destruction used to zero the workspace signing key IN PLACE while
// leaving it wired into the running process. Every consumer tested the key's
// LENGTH — which zeroing does not change — so the export signed with 64 zero
// bytes, wrapped a data key of 32 zero bytes, wrote a complete container, and
// reported the Job `succeeded`. The operator believes they hold a backup. No
// restorer can ever open it. The window is every export between a workspace
// delete and a process restart, because the key is loaded once at boot.
//
// A failed export is recoverable. A successful one that produced an unreadable
// artifact is not, because nobody goes looking.
func TestExportWithADestroyedKeyFailsRatherThanReportingSuccess(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	who := e.auth.Credential()

	// Exactly what the delete operation does to the key, in a process that keeps
	// running and keeps holding this same value.
	if err := e.key.Destroy(); err != nil {
		t.Fatalf("Destroy the workspace signing key: %v", err)
	}

	resp, raw := e.postWorkspace(t, who, "export", map[string]any{"passphrase": testExportPassphrase})
	job := acceptedJob(t, resp, raw)
	e.runJobs()

	done := e.polledJob(t, who, job.ID)
	if done.State == "succeeded" {
		t.Fatalf("export job reported %q with a destroyed signing key — the operator is being told they have a backup they do not have", done.State)
	}
	if len(done.Targets) != 1 {
		t.Fatalf("targets = %d, want exactly 1", len(done.Targets))
	}
	if done.Targets[0].State != "failed" {
		t.Errorf("target state = %q, want failed", done.Targets[0].State)
	}

	path := filepath.Join(e.archiveDir, "workspace-"+job.ID+".waiveo-archive")
	if _, err := os.Stat(path); err == nil {
		t.Errorf("a container was written at %s despite the signing key being destroyed; an artifact nobody can open must not be left where it can be mistaken for a backup", path)
	}
}

// TestExportLeavesOnlyTheContainerInTheArchiveDirectory covers two things an
// operator does with this directory: copies it, and backs it up.
//
//   - No KEY MATERIAL. The signing key's private half and the workspace data key
//     live in their own directory (SEC-047). They once shared this one, which
//     meant copying "the exports" copied the private key that signs them and the
//     cleartext data key that wraps the workspace's secrets — against precisely
//     the attacker the container's encryption assumes cannot have them.
//   - No SCRATCH SNAPSHOT. The relational snapshot an export streams is a
//     cleartext copy of the entire store. It must not outlive the export that
//     needed it.
func TestExportLeavesOnlyTheContainerInTheArchiveDirectory(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	who := e.auth.Credential()

	resp, raw := e.postWorkspace(t, who, "export", map[string]any{"passphrase": testExportPassphrase})
	job := acceptedJob(t, resp, raw)
	e.runJobs()
	if got := e.polledJob(t, who, job.ID); got.State != "succeeded" {
		t.Fatalf("export job state = %q, want succeeded (targets %+v)", got.State, got.Targets)
	}

	entries, err := os.ReadDir(e.archiveDir)
	if err != nil {
		t.Fatalf("read archive dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, en := range entries {
		names = append(names, en.Name())
	}
	want := "workspace-" + job.ID + ".waiveo-archive"
	if len(names) != 1 || names[0] != want {
		t.Errorf("archive directory holds %v, want exactly [%s]", names, want)
	}

	// Stated positively as well, so this case fails loudly rather than by
	// arithmetic if the naming above ever changes: the key material is where the
	// key directory is, and nowhere else.
	if stray := workspacekey.StrayKeyFiles(e.archiveDir); len(stray) != 0 {
		t.Errorf("workspace key material %v is sitting in the archive directory", stray)
	}
	if stray := workspacekey.StrayKeyFiles(e.keyDir); len(stray) == 0 {
		t.Error("the key directory holds no key material at all; the assertion above would pass vacuously")
	}
}

// TestWorkspaceOperationsRefuseNonOwner is the authorization claim, and the
// admin principal is what makes it mean something: an `admin` clears every
// ordinary write floor on this surface (auth.CanWrite's floor is `operator`),
// so a handler that authorized these two operations like any other write would
// let this caller through. It must not — see workspace.go for the argument.
func TestWorkspaceOperationsRefuseNonOwner(t *testing.T) {
	e := newWorkspaceEnv(t)
	orgID := e.seedWorkspace(t)

	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleOperator, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			who, err := e.auth.AddPrincipal(authtest.Config{Role: role, ScopeNode: auth.RootScopeNode})
			if err != nil {
				t.Fatalf("AddPrincipal(%s): %v", role, err)
			}
			resp, raw := e.postWorkspace(t, who, "export", map[string]any{"passphrase": testExportPassphrase})
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("export as %s: status = %d, want 403; body %s", role, resp.StatusCode, raw)
			}
			assertProblem(t, resp, raw, "FORBIDDEN")

			resp, raw = e.postWorkspace(t, who, "delete", map[string]any{"confirm_workspace_id": orgID})
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("delete as %s: status = %d, want 403; body %s", role, resp.StatusCode, raw)
			}
			assertProblem(t, resp, raw, "FORBIDDEN")
		})
	}

	// Nothing was destroyed by any of those refused deletes.
	if _, _, err := e.store.WorkspaceRoot(t.Context()); err != nil {
		t.Fatalf("the workspace is gone after refused deletes: %v", err)
	}
}

// TestDeleteWorkspaceRefusedWithoutConfirmation is the safety gate. No api/1 or
// security-model requirement imposes one — workspace.go says so explicitly —
// so this is a conservative default being held to, not a contract clause: a
// delete that does not name the workspace it destroys is refused, and nothing
// is destroyed.
//
// The wrong-id case is the important one: it proves the check compares the
// value against the real workspace rather than merely requiring the member to
// be present.
func TestDeleteWorkspaceRefusedWithoutConfirmation(t *testing.T) {
	e := newWorkspaceEnv(t)
	orgID := e.seedWorkspace(t)
	who := e.auth.Credential()
	siteID := e.createNode(t, datamodel.ScopeNode{Kind: "site", ParentID: &orgID, Name: "Survivor", TZ: strp("America/Chicago"), Lat: f64p(41.8781), Long: f64p(-87.6298)})

	for name, body := range map[string]any{
		"absent":   map[string]any{},
		"empty":    map[string]any{"confirm_workspace_id": ""},
		"wrong-id": map[string]any{"confirm_workspace_id": "01J8Z0WR0NGW0RKSPACE1D0001"},
	} {
		t.Run(name, func(t *testing.T) {
			resp, raw := e.postWorkspace(t, who, "delete", body)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body %s", resp.StatusCode, raw)
			}
			assertProblem(t, resp, raw, "VALIDATION_FAILED")
		})
	}

	// The refusals are refusals: the workspace and its rows are untouched.
	e.runJobs()
	resp, raw := e.as(t, who, http.MethodGet, "/api/v1/scope-nodes/"+siteID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("site node after refused deletes: status %d, want 200 (body %s)", resp.StatusCode, raw)
	}
}

// TestDeleteWorkspaceDestroysTheWorkspace is the execution claim: the accepted
// delete really performs SEC-121's destruction. Three independent things are
// asserted, because a partial destruction would satisfy any one of them alone:
//
//   - the workspace's rows are gone and its content assets with them;
//   - the org node survives as a `purged` tombstone (DAT-012);
//   - every principal, credential and session is gone, which is SEC-121's
//     "force fresh enrollment on every principal" and, because the claim gate
//     counts owner bindings, its "re-open the first-boot claim window" too.
func TestDeleteWorkspaceDestroysTheWorkspace(t *testing.T) {
	e := newWorkspaceEnv(t)
	orgID := e.seedWorkspace(t)
	who := e.auth.Credential()
	siteID := e.createNode(t, datamodel.ScopeNode{Kind: "site", ParentID: &orgID, Name: "Doomed", TZ: strp("America/Chicago"), Lat: f64p(41.8781), Long: f64p(-87.6298)})
	if _, err := e.content.Add([]byte("bytes that must not survive an erasure")); err != nil {
		t.Fatalf("content.Add: %v", err)
	}

	resp, raw := e.postWorkspace(t, who, "delete", map[string]any{"confirm_workspace_id": orgID})
	job := acceptedJob(t, resp, raw)
	if len(job.Targets) != 1 || job.Targets[0].TargetID != orgID {
		t.Fatalf("delete job targets = %+v, want exactly the workspace %q", job.Targets, orgID)
	}
	e.runJobs()

	ctx := t.Context()

	// The Job's own record survives the purge — it is the operation's only
	// completion signal (API-123), and destroying it would make the outcome
	// unobservable by construction.
	rec, found, err := e.store.GetJob(ctx, job.ID)
	if err != nil || !found {
		t.Fatalf("GetJob after the purge: found=%v err=%v — the delete Job's own record must survive", found, err)
	}
	if got := rec.Job.State(); string(got) != "succeeded" {
		t.Fatalf("delete job state = %q, want succeeded (targets %+v)", got, rec.Job.Targets())
	}

	// The workspace's rows are gone.
	if _, found, err := e.store.Get(ctx, store.KindScopeNode, siteID); err != nil {
		t.Fatalf("Get site node: %v", err)
	} else if found {
		t.Error("the site scope node survived the workspace delete")
	}

	// The org node survives as the `purged` tombstone DAT-012 names.
	gotID, state, err := e.store.WorkspaceRoot(ctx)
	if err != nil {
		t.Fatalf("WorkspaceRoot after the purge: %v", err)
	}
	if gotID != orgID {
		t.Errorf("org node id after the purge = %q, want the same %q", gotID, orgID)
	}
	if state != "purged" {
		t.Errorf("org node account_state = %q, want purged (DAT-012)", state)
	}

	// The content origin is empty.
	if e.content.Has(hexOf([]byte("bytes that must not survive an erasure"))) {
		t.Error("a content asset survived the workspace delete")
	}

	// Every principal is gone, so no previously-issued credential authenticates
	// and the claim window has re-opened (SEC-120/121).
	owners, err := e.auth.Store.CountOwnerBindings(ctx)
	if err != nil {
		t.Fatalf("CountOwnerBindings: %v", err)
	}
	if owners != 0 {
		t.Errorf("owner bindings after the purge = %d, want 0 — SEC-121 re-opens the first-boot claim window", owners)
	}
	resp, raw = e.as(t, who, http.MethodGet, "/api/v1/scope-nodes", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the caller's own session still authenticates after the destruction: status %d, want 401 (body %s)", resp.StatusCode, raw)
	}
}

// TestWorkspaceOperationsRefuseWithNoWorkspace: with no org node there is no
// workspace for either operation to act on, and api/1's own NOT_FOUND is what
// "no resource exists at the identifier named by the request" means — the
// identifier here being the path, which names the workspace implicitly
// (API-123).
func TestWorkspaceOperationsRefuseWithNoWorkspace(t *testing.T) {
	e := newWorkspaceEnv(t)
	who := e.auth.Credential()

	resp, raw := e.postWorkspace(t, who, "export", map[string]any{"passphrase": testExportPassphrase})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("export with no workspace: status = %d, want 404; body %s", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")

	resp, raw = e.postWorkspace(t, who, "delete", map[string]any{"confirm_workspace_id": "01J8Z0N0W0RKSPACEEX1STS001"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete with no workspace: status = %d, want 404; body %s", resp.StatusCode, raw)
	}
	assertProblem(t, resp, raw, "NOT_FOUND")
}

// TestExportWorkspaceIdempotencyKeyReplaysTheSameJob: both operations are
// mutating mcp:act POSTs, so a retry under the same Idempotency-Key replays the
// original 202 verbatim rather than starting a second export under a second Job
// (API-050/052). A second Job would mean a second container for one request.
func TestExportWorkspaceIdempotencyKeyReplaysTheSameJob(t *testing.T) {
	e := newWorkspaceEnv(t)
	e.seedWorkspace(t)
	who := e.auth.Credential()
	body := mustJSON(t, map[string]any{"passphrase": testExportPassphrase})
	headers := map[string]string{"Idempotency-Key": "export-workspace-retry-1"}

	resp1, raw1 := e.as(t, who, http.MethodPost, "/api/v1/workspace/export", body, headers)
	first := acceptedJob(t, resp1, raw1)
	resp2, raw2 := e.as(t, who, http.MethodPost, "/api/v1/workspace/export", body, headers)
	second := acceptedJob(t, resp2, raw2)

	if first.ID != second.ID {
		t.Fatalf("retry under the same Idempotency-Key minted a SECOND Job (%q then %q); it must replay the first", first.ID, second.ID)
	}
	if string(raw1) != string(raw2) {
		t.Errorf("replayed body differs from the original:\n first  = %s\n second = %s", raw1, raw2)
	}
}

// hexOf is the origin's own key for some bytes: their sha256, hex, unprefixed.
func hexOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
