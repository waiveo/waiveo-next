package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
)

// scope_write_test.go is scope_visibility_test.go's other half: which rows a
// caller may WRITE, and where they may place them. It drives the same fixture
// tree over the same real /api/v1 mux, and every case here varies only WHO is
// asking or WHERE the write points — so a difference in outcome can only be the
// authorization, never the data.
//
// The read half's posture (an out-of-reach row 404s indistinguishably from an
// id that was never minted) is deliberately NOT copied here. A write names its
// own scope node, so the refusal answers a question about a node the caller
// supplied, and api/1's taxonomy makes that FORBIDDEN — "the principal is
// authenticated but not authorized for this operation". What the read half
// bought with a status code, this half buys with ORDERING: the placement is
// authorized before anything that consults stored state, so an unauthorized
// node and a node that does not exist produce the identical refusal, and no
// later stage ever gets to tell them apart. TestUnauthorizedPlacementIsNotAn
// ExistenceOracle below is that claim, asserted rather than asserted-about.

// roleAt is one (scope node, role) binding a fixture principal is seeded with.
type roleAt struct {
	node string
	role auth.Role
}

// principalWith seeds a NEW principal holding exactly the named bindings and
// returns its credential. It is principalAt's general form: that helper binds
// `owner` everywhere because visibility cases must not be explicable by the
// role floor, whereas these cases turn on the role as much as on the node.
func (e *testEnv) principalWith(t *testing.T, bindings ...roleAt) authtest.Credential {
	t.Helper()
	if len(bindings) == 0 {
		t.Fatal("principalWith: a principal with no binding is refused 403 by the middleware; name at least one")
	}
	cred, err := e.auth.AddPrincipal(authtest.Config{ScopeNode: bindings[0].node, Role: bindings[0].role})
	if err != nil {
		t.Fatalf("AddPrincipal %s at %s: %v", bindings[0].role, bindings[0].node, err)
	}
	for _, b := range bindings[1:] {
		if _, err := e.auth.Store.PutRoleBinding(context.Background(), cred.PrincipalID, b.node, b.role); err != nil {
			t.Fatalf("PutRoleBinding %s at %s: %v", b.role, b.node, err)
		}
	}
	return cred
}

// playlistAt returns the create body for a playlist placed at node, optionally
// carrying an external_id.
func playlistAt(t *testing.T, node, externalID string) []byte {
	t.Helper()
	p := playlistFixture(node, nil)
	p.ExternalID = externalID
	return mustJSON(t, p)
}

// seedPlaylistAt creates a playlist at node AS THE ROOT-BOUND SEEDER and returns
// its id. Seeding through the env's own identity rather than through the caller
// under test is what makes a later refusal meaningful: the row provably exists
// before anyone is refused anything about it.
func seedPlaylistAt(t *testing.T, e *testEnv, node, externalID string) string {
	t.Helper()
	return decodeID(t, e.createOK(t, "/api/v1/playlists", playlistAt(t, node, externalID)))
}

// scopeNodeOf reads a row's stored scope_node back AS THE SEEDER, who can see
// every row. A case proving a refused write changed nothing has to look at the
// row through an identity that could have seen the change.
func (e *testEnv) scopeNodeOf(t *testing.T, path string) string {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s as the seeder = %d, want 200 (body %s)", path, resp.StatusCode, raw)
	}
	var row struct {
		ScopeNode string `json:"scope_node"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode %s: %v (body %s)", path, err, raw)
	}
	return row.ScopeNode
}

// countPlaylists returns how many playlists exist AT ALL, read as the root-bound
// seeder. A create that was refused must leave this unchanged; a create that was
// silently accepted and merely hidden would not.
func (e *testEnv) countPlaylists(t *testing.T) int {
	t.Helper()
	resp, raw := e.do(t, http.MethodGet, "/api/v1/playlists?limit=200", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list playlists as the seeder = %d (body %s)", resp.StatusCode, raw)
	}
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode playlist page: %v (body %s)", err, raw)
	}
	return len(page.Items)
}

// etagOf reads a row's current ETag as who, so a patch/delete under test carries
// a genuinely valid If-Match and can only fail on authorization.
func (e *testEnv) etagOf(t *testing.T, who authtest.Credential, path string) string {
	t.Helper()
	resp, raw := e.as(t, who, http.MethodGet, path, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body %s)", path, resp.StatusCode, raw)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("GET %s returned no ETag (body %s)", path, raw)
	}
	return etag
}

// assertForbidden asserts a response is the 403 an unauthorized write draws, and
// returns the Problem minus its two per-request members so callers can compare
// two refusals member for member.
func assertForbidden(t *testing.T, resp *http.Response, raw []byte, what string) map[string]any {
	t.Helper()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("%s = %d, want 403 — an unauthorized write is refused, never default-permitted (SEC-005) (body %s)",
			what, resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "FORBIDDEN")
	// trace_id is per-request by construction (API-010) and instance echoes the
	// request path (API-015); neither says anything about a scope node.
	delete(p, "trace_id")
	delete(p, "instance")
	return p
}

// TestCreateAtUnauthorizedScopeNodeIsRefused is the first half of the gap: a
// create may name ANY scope node, and nothing about the caller's own bindings
// constrains which. Before this check the row was simply written — and, being
// placed outside its author's visible set, became invisible to its own author
// the instant it landed.
//
// SEC-005 binds a write exactly as it binds a read: "every api/1 route MUST
// authorize its caller against a role bound at a scope node ... before
// executing; a route that cannot resolve an authorization decision for its
// caller MUST refuse the request rather than default-permit."
func TestCreateAtUnauthorizedScopeNodeIsRefused(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	e.uploadContent(t, playlistFixtureAsset)
	alice := e.principalAt(t, tr.siteA)

	before := e.countPlaylists(t)

	// The control comes FIRST, and it is what makes the refusal below mean
	// "wrong node" rather than "this caller cannot create playlists at all".
	resp, raw := e.as(t, alice, http.MethodPost, "/api/v1/playlists", playlistAt(t, tr.screensA[0], ""), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create inside the caller's own subtree = %d, want 201 (body %s)", resp.StatusCode, raw)
	}

	for name, node := range map[string]string{
		"a screen in the other site": tr.screensB[0],
		"the other site itself":      tr.siteB,
		"the org above the binding":  tr.org,
	} {
		resp, raw := e.as(t, alice, http.MethodPost, "/api/v1/playlists", playlistAt(t, node, ""), nil)
		assertForbidden(t, resp, raw, "create placed at "+name)
	}

	// Exactly one playlist was written — the control. A refused create must
	// leave nothing behind, and the count is read as the root-bound seeder, who
	// would see a row placed anywhere.
	if got, want := e.countPlaylists(t), before+1; got != want {
		t.Fatalf("playlist count after one permitted and three refused creates = %d, want %d; "+
			"a refused write must not store the row it was refused", got, want)
	}
}

// TestScopeNodeCreateIsAuthorizedAtItsParent pins WHERE a scope node's own write
// authority comes from. A scope node's placement — what a subtree selector
// evaluates against — is ITSELF, but on a create that id has never existed, so
// authorizing against it would resolve through nothing but the workspace-root
// fallback and would lock every subtree-bound admin out of adding a screen.
// SEC-010's inheritance makes the parent the right anchor: a node hangs under
// its parent, and authority there inherits to the child being created.
func TestScopeNodeCreateIsAuthorizedAtItsParent(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	alice := e.principalAt(t, tr.siteA)

	resp, raw := e.as(t, alice, http.MethodPost, "/api/v1/scope-nodes",
		createBody(t, screenNode("", tr.siteA, "")), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create a screen under the caller's OWN site = %d, want 201 — authority at a node inherits to "+
			"the children created under it (SEC-010) (body %s)", resp.StatusCode, raw)
	}
	created := decodeID(t, raw)

	// And it is genuinely reachable by its author afterwards, which is the whole
	// property an unauthorized placement destroys.
	if resp, raw := e.as(t, alice, http.MethodGet, "/api/v1/scope-nodes/"+created, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the created node read back by its author = %d, want 200 (body %s)", resp.StatusCode, raw)
	}

	resp, raw = e.as(t, alice, http.MethodPost, "/api/v1/scope-nodes",
		createBody(t, screenNode("", tr.siteB, "")), nil)
	assertForbidden(t, resp, raw, "create a screen under the OTHER site")

	// A node with no parent is a new top of the tree: a platform-wide act, which
	// a subtree binding does not reach.
	resp, raw = e.as(t, alice, http.MethodPost, "/api/v1/scope-nodes",
		createBody(t, orgNode("Alice's Own Org")), nil)
	assertForbidden(t, resp, raw, "create a parentless org")
}

// TestPatchCannotMoveARowOutOfTheAuthorizedSet is the second half of the gap,
// and the half a check on the row's CURRENT placement alone would miss
// entirely. The row is one the caller may read and write; the patch is what
// carries it somewhere they may not.
func TestPatchCannotMoveARowOutOfTheAuthorizedSet(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	e.uploadContent(t, playlistFixtureAsset)
	alice := e.principalAt(t, tr.siteA)

	id := seedPlaylistAt(t, e, tr.screensA[0], "")
	path := "/api/v1/playlists/" + id

	// A move WITHIN the authorized set still works — the check narrows the
	// destination, it does not freeze it.
	resp, raw := e.as(t, alice, http.MethodPatch, path,
		mustJSON(t, map[string]string{"scope_node": tr.screensA[1]}),
		map[string]string{"If-Match": e.etagOf(t, alice, path)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("move a row between two nodes the caller is authorized at = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	if got := e.scopeNodeOf(t, path); got != tr.screensA[1] {
		t.Fatalf("after a permitted move the row's scope_node = %q, want %q", got, tr.screensA[1])
	}

	// A move OUT of it is refused, and refused for the destination — the row's
	// current placement is one this caller is fully authorized at, so nothing
	// but the target can explain the 403.
	for name, dest := range map[string]string{
		"a screen in the other site":   tr.screensB[0],
		"a node that was never minted": absentScopeNodeID,
	} {
		resp, raw := e.as(t, alice, http.MethodPatch, path,
			mustJSON(t, map[string]string{"scope_node": dest}),
			map[string]string{"If-Match": e.etagOf(t, alice, path)})
		assertForbidden(t, resp, raw, "move a row to "+name)
	}

	// Nothing moved. Read as the seeder, who would see the row wherever it went.
	if got := e.scopeNodeOf(t, path); got != tr.screensA[1] {
		t.Fatalf("after two refused moves the row's scope_node = %q, want it unchanged at %q; "+
			"a refused write must not execute", got, tr.screensA[1])
	}
}

// TestUnauthorizedPlacementIsNotAnExistenceOracle is the case the whole ordering
// exists for.
//
// API-101 scopes external_id uniqueness by (resource type, scope node), so the
// uniqueness guard MUST scan every row under the target node — including rows
// the caller cannot read. Narrowing it to the visible subset would break the
// rule it enforces (two callers could then each claim one external_id under one
// node, leaving every API-103 cross-reference with two candidates). So the guard
// is left exactly as API-101 requires, and the PLACEMENT is refused before the
// guard is ever reached.
//
// The assertion is indistinguishability, not merely "the status is 403", the
// same bar TestOutOfReachRowReadsAsNonexistent sets for the read side: three
// creates that differ ONLY in facts the caller is not entitled to learn — a
// hidden row with the colliding external_id, a hidden node with no such row, and
// a node that does not exist — must produce the same Problem member for member.
func TestUnauthorizedPlacementIsNotAnExistenceOracle(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	e.uploadContent(t, playlistFixtureAsset)
	alice := e.principalAt(t, tr.siteA)

	const takenExternalID = "shared-external-id"
	hidden := tr.screensB[0]
	seedPlaylistAt(t, e, hidden, takenExternalID)

	// The collision is REAL, and this is what proves it: the root-bound seeder,
	// who is entitled to the answer, gets the 400 the same create draws for
	// anyone authorized at that node. Without this arm the case below could pass
	// over a store where the hidden row had silently failed to be written — the
	// oracle would be closed because there was nothing to disclose.
	resp, raw := e.do(t, http.MethodPost, "/api/v1/playlists", playlistAt(t, hidden, takenExternalID), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the seeder's colliding create = %d, want 400 — the external_id collision under %s must be genuine, "+
			"or this case proves nothing (body %s)", resp.StatusCode, hidden, raw)
	}
	assertProblem(t, resp, raw, "EXTERNAL_ID_CONFLICT")

	// Three creates by the caller who may NOT write at the target. They differ
	// only in what is true at a node she cannot see.
	probes := []struct {
		name       string
		node       string
		externalID string
	}{
		{"a hidden node where that external_id IS taken", hidden, takenExternalID},
		{"a hidden node where that external_id is free", hidden, "an-external-id-nobody-has"},
		{"a node that was never minted", absentScopeNodeID, takenExternalID},
	}
	var first map[string]any
	for _, probe := range probes {
		resp, raw := e.as(t, alice, http.MethodPost, "/api/v1/playlists",
			playlistAt(t, probe.node, probe.externalID), nil)
		body := assertForbidden(t, resp, raw, "create at "+probe.name)
		if first == nil {
			first = body
			continue
		}
		if !reflect.DeepEqual(body, first) {
			t.Fatalf("a create aimed at %s answered differently from the first probe; every refusal of an "+
				"unauthorized placement must be INDISTINGUISHABLE, or the difference is an existence oracle\n"+
				" this %v\nfirst %v", probe.name, body, first)
		}
	}

	// The same equivalence on the patch path: moving a row onto a hidden node
	// where the external_id is taken must answer exactly as moving it onto one
	// where nothing is.
	id := seedPlaylistAt(t, e, tr.screensA[0], takenExternalID)
	path := "/api/v1/playlists/" + id
	var firstMove map[string]any
	for _, dest := range []string{hidden, tr.screensB[1], absentScopeNodeID} {
		resp, raw := e.as(t, alice, http.MethodPatch, path,
			mustJSON(t, map[string]string{"scope_node": dest}),
			map[string]string{"If-Match": e.etagOf(t, alice, path)})
		body := assertForbidden(t, resp, raw, "move a row onto "+dest)
		if firstMove == nil {
			firstMove = body
			continue
		}
		if !reflect.DeepEqual(body, firstMove) {
			t.Fatalf("moving a row onto %s answered differently from the first destination; "+
				"an unauthorized destination must disclose nothing about what is already there\n this %v\nfirst %v",
				dest, body, firstMove)
		}
	}
	// And the create refusal and the patch refusal agree too: one detail string
	// for every unauthorized write on this surface (api.go's
	// unauthorizedWriteDetail), so no member of the Problem varies with anything
	// the caller may not know.
	if !reflect.DeepEqual(firstMove, first) {
		t.Fatalf("a refused create and a refused move produced different Problems\ncreate %v\n  move %v", first, firstMove)
	}
}

// TestViewerCannotWriteAnywhereItCanRead pins the coarse role→verb mapping the
// contract leaves to this surface. security-model/1's draft-note beneath SEC-012
// defers "the complete permission matrix for admin/operator/viewer against every
// individual api/1 operation", so nothing finer than this is invented: a viewer
// reads and does not write; operator and above write.
//
// The reads are asserted too, and they carry the case: every refusal below is
// about the VERB, at a node this caller demonstrably reaches.
func TestViewerCannotWriteAnywhereItCanRead(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	e.uploadContent(t, playlistFixtureAsset)
	viewer := e.principalWith(t, roleAt{tr.siteA, auth.RoleViewer})

	id := seedPlaylistAt(t, e, tr.screensA[0], "")
	path := "/api/v1/playlists/" + id

	// Reading works, and the ETag it yields makes every write below fail on
	// authorization rather than on a missing precondition.
	etag := e.etagOf(t, viewer, path)

	resp, raw := e.as(t, viewer, http.MethodPost, "/api/v1/playlists", playlistAt(t, tr.screensA[1], ""), nil)
	assertForbidden(t, resp, raw, "a viewer's create inside its own subtree")

	resp, raw = e.as(t, viewer, http.MethodPatch, path,
		mustJSON(t, map[string]string{"name": "Renamed By A Viewer"}), map[string]string{"If-Match": etag})
	assertForbidden(t, resp, raw, "a viewer's patch of a row it can read")

	resp, raw = e.as(t, viewer, http.MethodDelete, path, nil, map[string]string{"If-Match": etag})
	assertForbidden(t, resp, raw, "a viewer's delete of a row it can read")

	// The row is untouched and still there — read as the seeder.
	if got := e.scopeNodeOf(t, path); got != tr.screensA[0] {
		t.Fatalf("the row a viewer was refused three writes on = %q, want it unchanged at %q", got, tr.screensA[0])
	}
}

// TestWriteAuthorityIsResolvedPerNodeNotPerPrincipal is the case the middleware
// structurally cannot catch, and therefore the one that proves the check is
// where it needs to be.
//
// The middleware's floor is auth.Effective — the strongest role held ANYWHERE —
// because a request is addressed by a resource, not by a scope node. This caller
// is `owner` at site A, so every mutating method clears that floor. They are
// `viewer` at site B, and SEC-010 makes the nearer binding authoritative there:
// "a role bound at a scope node applies to that node and, absent a more specific
// binding, to its descendants." A narrower binding therefore genuinely NARROWS,
// including when it is weaker — which only means anything if the write path
// resolves authority per node.
func TestWriteAuthorityIsResolvedPerNodeNotPerPrincipal(t *testing.T) {
	e := newEnv(t)
	tr := seedScopedTree(t, e)
	e.uploadContent(t, playlistFixtureAsset)
	mixed := e.principalWith(t, roleAt{tr.siteA, auth.RoleOwner}, roleAt{tr.siteB, auth.RoleViewer})

	inB := seedPlaylistAt(t, e, tr.screensB[0], "")
	pathB := "/api/v1/playlists/" + inB

	// Site B is readable — the viewer binding reaches it — so nothing below can
	// be explained by the row being invisible.
	etagB := e.etagOf(t, mixed, pathB)

	// Site A: full authority, every verb.
	resp, raw := e.as(t, mixed, http.MethodPost, "/api/v1/playlists", playlistAt(t, tr.screensA[0], ""), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create where this caller is owner = %d, want 201 (body %s)", resp.StatusCode, raw)
	}
	inA := decodeID(t, raw)
	pathA := "/api/v1/playlists/" + inA

	// Site B: none of them, though the very same principal just cleared the
	// middleware's floor for the identical method one request ago.
	resp, raw = e.as(t, mixed, http.MethodPost, "/api/v1/playlists", playlistAt(t, tr.screensB[1], ""), nil)
	assertForbidden(t, resp, raw, "create where this caller is only a viewer")

	resp, raw = e.as(t, mixed, http.MethodPatch, pathB,
		mustJSON(t, map[string]string{"name": "Renamed"}), map[string]string{"If-Match": etagB})
	assertForbidden(t, resp, raw, "patch where this caller is only a viewer")

	resp, raw = e.as(t, mixed, http.MethodDelete, pathB, nil, map[string]string{"If-Match": etagB})
	assertForbidden(t, resp, raw, "delete where this caller is only a viewer")

	// And the row they DO own cannot be shipped into the subtree they merely
	// read — the placement check covers the destination, not only the origin.
	resp, raw = e.as(t, mixed, http.MethodPatch, pathA,
		mustJSON(t, map[string]string{"scope_node": tr.screensB[1]}),
		map[string]string{"If-Match": e.etagOf(t, mixed, pathA)})
	assertForbidden(t, resp, raw, "move an owned row into a viewer-only subtree")

	if got := e.scopeNodeOf(t, pathB); got != tr.screensB[0] {
		t.Fatalf("the viewer-side row = %q, want it unchanged at %q", got, tr.screensB[0])
	}
	if got := e.scopeNodeOf(t, pathA); got != tr.screensA[0] {
		t.Fatalf("the owner-side row = %q, want it unchanged at %q after a refused move", got, tr.screensA[0])
	}
}
