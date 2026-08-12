package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// Rule version history over the real HTTP surface (rules/1 RUL-394).
//
// The capability answers one bad day: an operator edits a working rule, breaks
// it, and has no way back except retyping it from memory or restoring the whole
// workspace — which reverts everything else with it.

type automationVersionsResponse struct {
	Items []struct {
		Revision     int64           `json:"revision"`
		SupersededAt int64           `json:"superseded_at"`
		Definition   json.RawMessage `json:"definition"`
	} `json:"items"`
}

// createThenRename creates an automation and renames it once, so there is
// exactly one superseded definition to restore.
func createThenRename(t *testing.T, e *testEnv, name string) (id string, revision int64) {
	t.Helper()
	e.placementNode(t)
	resp, raw := e.do(t, http.MethodPost, "/api/v1/automations", edgeAutomationBody("", autoScopeNode, nil), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%s)", resp.StatusCode, raw)
	}
	var created struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id = decodeID(t, raw)
	resp, raw = e.do(t, http.MethodPatch, "/api/v1/automations/"+id,
		mustJSON(t, map[string]any{"name": name}),
		map[string]string{"Content-Type": "application/json", "If-Match": fmt.Sprintf(`"%d"`, created.Revision)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename = %d (%s)", resp.StatusCode, raw)
	}
	return id, created.Revision
}

func TestAutomationVersionsListWhatTheRuleUsedToSay(t *testing.T) {
	e := newEnv(t)
	id, _ := createThenRename(t, e, "broken")

	resp, raw := e.do(t, http.MethodGet, "/api/v1/automations/"+id+"/versions", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("versions = %d (%s)", resp.StatusCode, raw)
	}
	var out automationVersionsResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode versions: %v (%s)", err, raw)
	}
	if len(out.Items) != 1 {
		t.Fatalf("versions = %d, want the one superseded definition", len(out.Items))
	}
	// The revision it HELD, and the definition as it read then.
	if out.Items[0].Revision != 1 {
		t.Errorf("revision = %d, want 1", out.Items[0].Revision)
	}
	if out.Items[0].SupersededAt == 0 {
		t.Error("no superseded_at — the history cannot be read in order")
	}
}

// The rule the whole operation turns on: restoring the LOGIC must not restore
// the ENABLEMENT. An operator who disabled a rule while debugging it has not
// asked for it to start firing again by putting its old logic back.
func TestRestoringAVersionDoesNotReEnableADisabledRule(t *testing.T) {
	e := newEnv(t)
	id, createdRev := createThenRename(t, e, "broken")

	// Disable it — as an operator would while investigating.
	resp, raw := e.do(t, http.MethodPatch, "/api/v1/automations/"+id,
		mustJSON(t, map[string]any{"enabled": false}),
		map[string]string{"Content-Type": "application/json", "If-Match": fmt.Sprintf(`"%d"`, createdRev+1)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable = %d (%s)", resp.StatusCode, raw)
	}

	// Restore revision 1 — a definition that was ENABLED at the time.
	resp, raw = e.do(t, http.MethodPost, "/api/v1/automations/"+id+"/versions/1/restore", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restore = %d (%s)", resp.StatusCode, raw)
	}

	resp, raw = e.do(t, http.MethodGet, "/api/v1/automations/"+id, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get = %d (%s)", resp.StatusCode, raw)
	}
	var got struct {
		Enabled bool   `json:"enabled"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if got.Enabled {
		t.Fatal("restoring an earlier definition RE-ENABLED a rule the operator had switched off")
	}
	// And the logic really did come back: the rename is gone.
	if got.Name == "broken" {
		t.Fatal("the restore did not put the earlier definition back")
	}
}

// A restore is a NEW update, not a rewind: the history grows, so restoring the
// wrong version is itself undoable.
func TestRestoringRecordsItselfInTheHistory(t *testing.T) {
	e := newEnv(t)
	id, _ := createThenRename(t, e, "broken")

	if resp, raw := e.do(t, http.MethodPost, "/api/v1/automations/"+id+"/versions/1/restore", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("restore = %d (%s)", resp.StatusCode, raw)
	}

	_, raw := e.do(t, http.MethodGet, "/api/v1/automations/"+id+"/versions", nil, nil)
	var out automationVersionsResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("versions after a restore = %d, want 2 — a restore that rewound would leave 1, and an operator could not undo it", len(out.Items))
	}
}

func TestRestoringAVersionThatDoesNotExistIs404(t *testing.T) {
	e := newEnv(t)
	id, _ := createThenRename(t, e, "broken")

	if resp, raw := e.do(t, http.MethodPost, "/api/v1/automations/"+id+"/versions/99/restore", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", resp.StatusCode, raw)
	}
}
