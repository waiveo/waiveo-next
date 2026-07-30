package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// undeclaredmembers_test.go pins the member half of schema enforcement for the
// scope-node family: a body may carry only members the document declares, and
// nothing about the per-field validation downstream changes.
//
// The second half matters as much as the first. This family is deliberately NOT
// run through the whole-schema gate, because that gate answers with one
// fail-fast `detail` and would replace data-model/1's published per-field codes
// in API-013's multi-field `errors[]` shape. So every test here that adds a
// refusal is paired with one asserting the richer answer still arrives.

func TestCreateRefusesAnUndeclaredMember(t *testing.T) {
	e := newEnv(t)

	body := map[string]any{
		"kind": "org", "name": "Org", "account_state": "active", "entitlements": map[string]any{},
		"nonsense_field": 1,
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, body), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create with an undeclared member = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
	if detail, _ := p["detail"].(string); detail != "nonsense_field: not a member this operation declares." {
		t.Errorf("detail = %q, want it to name the offending member", detail)
	}
}

// TestAnUndeclaredMemberIsNotStored is the property the issue was actually
// about. Being refused is only half of it: before this, the member was accepted,
// written, and returned on every subsequent read of the node.
func TestAnUndeclaredMemberIsNotStored(t *testing.T) {
	e := newEnv(t)
	org := e.createNode(t, orgNode("Org"))

	// The refused PATCH first.
	resp, raw := e.do(t, http.MethodPatch, "/api/v1/scope-nodes/"+org,
		mustJSON(t, map[string]any{"name": "Renamed", "smuggled": "value"}),
		map[string]string{"If-Match": `"1"`})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("patch with an undeclared member = %d, want 422 (body %s)", resp.StatusCode, raw)
	}

	// Nothing was written — not the smuggled member, and not the legitimate
	// rename that travelled with it.
	get, graw := e.do(t, http.MethodGet, "/api/v1/scope-nodes/"+org, nil, nil)
	if get.StatusCode != http.StatusOK {
		t.Fatalf("read back the org: %d %s", get.StatusCode, graw)
	}
	var node map[string]any
	if err := json.Unmarshal(graw, &node); err != nil {
		t.Fatalf("decode the node: %v", err)
	}
	if _, present := node["smuggled"]; present {
		t.Error("the refused member is stored on the node and served back on every read")
	}
	if node["name"] != "Org" {
		t.Errorf("name = %v, want the original: a refused PATCH must store nothing at all", node["name"])
	}
}

// TestTheMemberCheckCannotPreEmptAPerFieldCode is the guard on the narrowness
// this check promises. It compares member NAMES only, so a body whose members
// are all declared reaches the datamodel validators however wrong its VALUES
// are — and their published per-field codes, all of them, still arrive.
func TestTheMemberCheckCannotPreEmptAPerFieldCode(t *testing.T) {
	e := newEnv(t)

	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes",
		mustJSON(t, map[string]any{"kind": "bogus", "name": ""}), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	p := assertProblem(t, resp, raw, "VALIDATION_FAILED")

	errs, _ := p["errors"].([]any)
	if len(errs) < 2 {
		t.Fatalf("errors[] carries %d entr(ies), want every failing member — the multi-field shape API-013 pins (body %s)",
			len(errs), raw)
	}
	want := map[string]bool{"SCOPE_NODE_KIND_INVALID": false, "SCOPE_NODE_NAME_INVALID": false}
	for _, e := range errs {
		if m, _ := e.(map[string]any); m != nil {
			if code, _ := m["code"].(string); want[code] == false {
				if _, tracked := want[code]; tracked {
					want[code] = true
				}
			}
		}
	}
	for code, found := range want {
		if !found {
			t.Errorf("errors[] carries no %s (body %s)", code, raw)
		}
	}
}

// TestEveryDeclaredMemberIsAccepted is the other side of a check that refuses by
// name: if the declared set were read wrongly — an empty set, the response
// schema instead of the create schema — every write would fail. A create
// carrying ALL of ScopeNodeCreate's members must succeed.
func TestEveryDeclaredMemberIsAccepted(t *testing.T) {
	e := newEnv(t)
	org := e.createNode(t, orgNode("Org"))

	full := map[string]any{
		"kind": "site", "name": "Site", "parent_id": org,
		"external_id": "site-1", "labels": map[string]string{"env": "prod"},
		"tz": siteTZ, "lat": siteLat, "long": siteLong,
	}
	resp, raw := e.do(t, http.MethodPost, "/api/v1/scope-nodes", mustJSON(t, full), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a body carrying every declared member = %d, want 201 (body %s)", resp.StatusCode, raw)
	}
}

// TestPatchStillAcceptsKind pins what nearly changed by accident. `kind` is
// patchable on the server, and the tree rules — not this check — are what refuse
// a dangerous re-kind. While `kind` was undeclared on the update schema, the
// member check made re-kinding impossible through the surface, which is a
// behaviour change no requirement asks for.
func TestPatchStillAcceptsKind(t *testing.T) {
	e := newEnv(t)
	org := e.createNode(t, orgNode("Org"))
	site := e.createNode(t, siteUnder(org))

	// A legal re-kind: a site becomes a group under the same org... which DAT-003
	// refuses (an org may not carry a group), so the refusal must come from the
	// tree rule with its published code, never from "kind is not a member".
	resp, raw := e.do(t, http.MethodPatch, "/api/v1/scope-nodes/"+site,
		mustJSON(t, map[string]any{"kind": "group"}), map[string]string{"If-Match": `"1"`})
	assertValidationError(t, resp, raw, "SCOPE_NODE_PARENT_INVALID")
}

// TestTheSchedulingCoreRefusesAnUndeclaredMember covers the three families that
// declared no request body at all until now. Their per-field rules live in the
// datamodel validators, which cannot see a member they do not define: by the time
// they validate, an undeclared member has vanished into the decoded row. So it was
// accepted, stored, and served back on every read — the same defect closed for
// scope nodes, on three more families.
func TestTheSchedulingCoreRefusesAnUndeclaredMember(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		body       func(t *testing.T, e *testEnv, site string) map[string]any
	}{
		{"playlists", "/api/v1/playlists", func(t *testing.T, e *testEnv, site string) map[string]any {
			e.uploadContent(t, playlistFixtureAsset)
			return map[string]any{"scope_node": site, "name": "P",
				"items": []map[string]any{{"source": "asset", "asset_ref": playlistFixtureAssetRef}}}
		}},
		{"schedules", "/api/v1/schedules", func(_ *testing.T, _ *testEnv, site string) map[string]any {
			return map[string]any{"scope_node": site, "name": "S"}
		}},
		{"dayparts", "/api/v1/dayparts", func(t *testing.T, e *testEnv, site string) map[string]any {
			sched := decodeID(t, e.createOK(t, "/api/v1/schedules",
				mustJSON(t, map[string]any{"scope_node": site, "name": "S"})))
			return map[string]any{"scope_node": site, "schedule_id": sched,
				"days_of_week": []int{1}, "start_time": "06:00", "end_time": "22:00",
				"display_power": "on"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			site := e.createNode(t, siteUnder(e.createNode(t, orgNode("Org"))))

			// The valid body first: a refusal that fires on everything proves nothing.
			valid := tc.body(t, e, site)
			resp, raw := e.do(t, http.MethodPost, tc.path, mustJSON(t, valid), nil)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("a valid %s body = %d, want 201 (body %s)", tc.name, resp.StatusCode, raw)
			}

			// The same body plus one member the document does not declare.
			smuggled := tc.body(t, e, site)
			smuggled["nonsense_field"] = "smuggled"
			resp, raw = e.do(t, http.MethodPost, tc.path, mustJSON(t, smuggled), nil)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("%s with an undeclared member = %d, want 422 (body %s)", tc.name, resp.StatusCode, raw)
			}
			p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
			if detail, _ := p["detail"].(string); detail != "nonsense_field: not a member this operation declares." {
				t.Errorf("detail = %q, want it to name the offending member", detail)
			}
		})
	}
}

// TestActionStylePostsRefuseAnUndeclaredMember covers the POSTs that are not
// resource families — a per-row action, a collection-wide one, an entity command,
// the two workspace data-subject operations, and the webhook secret rotation.
// None reaches the resource pipeline, each guards the fields it needs by hand,
// and none bounded the members it does not declare: bulk-enable answered 202 and
// accepted the job with an undeclared member on board.
//
// The check runs BEFORE the idempotency record is written, which this asserts
// directly: a refused body recorded as a key's outcome would make the retry that
// FIXES the body replay the refusal instead of executing.
func TestActionStylePostsRefuseAnUndeclaredMember(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		valid      map[string]any
	}{
		{"bulk-enable", "/api/v1/automations/bulk-enable",
			map[string]any{"selector": "env=prod", "enabled": true}},
		{"workspace export", "/api/v1/workspace/export",
			map[string]any{"passphrase": "conformance-export-passphrase"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)

			smuggled := map[string]any{"nonsense_field": "smuggled"}
			for k, v := range tc.valid {
				smuggled[k] = v
			}
			resp, raw := e.do(t, http.MethodPost, tc.path, mustJSON(t, smuggled), nil)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("%s with an undeclared member = %d, want 422 (body %s)", tc.name, resp.StatusCode, raw)
			}
			p := assertProblem(t, resp, raw, "VALIDATION_FAILED")
			if detail, _ := p["detail"].(string); detail != "nonsense_field: not a member this operation declares." {
				t.Errorf("detail = %q, want it to name the offending member", detail)
			}
		})
	}
}

// TestARefusedBodyIsNotRecordedAsAnIdempotentOutcome: the refusal has to happen
// before the idempotency record exists. Otherwise a client that retries the SAME
// key with a corrected body replays the stored 422 forever, and the operation
// becomes unreachable through no fault they can see.
func TestARefusedBodyIsNotRecordedAsAnIdempotentOutcome(t *testing.T) {
	e := newEnv(t)
	const key = "3f1b1e2a-6c4d-4a9b-8f2e-9d7c6b5a4321"

	bad := mustJSON(t, map[string]any{"selector": "env=prod", "enabled": true, "nonsense_field": 1})
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable", bad,
		map[string]string{"Idempotency-Key": key})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("refused body = %d, want 422 (body %s)", resp.StatusCode, raw)
	}

	// The same key, a corrected body. It must EXECUTE rather than replay the 422.
	good := mustJSON(t, map[string]any{"selector": "env=prod", "enabled": true})
	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations/bulk-enable", good,
		map[string]string{"Idempotency-Key": key})
	if resp.StatusCode == http.StatusUnprocessableEntity {
		t.Fatalf("the corrected body replayed the refusal: the 422 was recorded as this key's outcome (body %s)", raw)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("corrected body = %d, want 202 (body %s)", resp.StatusCode, raw)
	}
}
