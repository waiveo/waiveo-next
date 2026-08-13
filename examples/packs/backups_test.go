package examplepacks_test

import (
	"encoding/json"
	"testing"

	examplepacks "github.com/maaxton/waiveo-next/examples/packs"
	"github.com/maaxton/waiveo-next/internal/manifest"
)

// The backups pack is the platform's PILOT extension — the first one that runs
// code — so these assertions are about the claims that make it a pilot, not
// about it being well-formed.
//
// It exists to prove the whole chain with the fewest possible walls: identity,
// the actions plane, and a settings page. If it ever grows a second page type, a
// device class, a widget kind or an HTTP route, it stops being the thing that
// isolates those two mechanisms and this test should be the thing that says so.

func backupsManifest(t *testing.T) manifest.PackManifest {
	t.Helper()
	raw, err := examplepacks.PackZip("backups")
	if err != nil {
		t.Fatalf("PackZip(backups): %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("the backups pack assembled to nothing")
	}
	b, err := examplepacks.PackFile("backups", "manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m manifest.PackManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return m
}

// The pack declares exactly the two things the pilot is proving, and nothing
// that would drag another wall in.
func TestTheBackupsPilotHitsOnlyTheWallsItIsProving(t *testing.T) {
	m := backupsManifest(t)

	if len(m.UI.Pages) != 1 || m.UI.Pages[0].PageType != "settings-form" {
		t.Fatalf("pages = %+v; the pilot is ONE settings-form and no other page type", m.UI.Pages)
	}
	if len(m.Devices) != 0 {
		t.Fatalf("the pilot declares device classes (%+v) — that is a wall it exists to avoid", m.Devices)
	}
	if len(m.UI.Surfaces) != 0 {
		t.Fatalf("the pilot declares a frontend surface — surface/1 has no implementation, and the pilot must not need one")
	}
	if len(m.Actions) != 1 || m.Actions[0].Name != "run-backup" {
		t.Fatalf("actions = %+v, want exactly run-backup", m.Actions)
	}
}

// The action is safe-to-retry, and that is a deliberate property of the PILOT
// rather than an accident: a not-idempotent pilot could not be re-run freely
// while debugging the plane it exists to exercise.
func TestThePilotActionIsSafeToRetry(t *testing.T) {
	m := backupsManifest(t)
	if got := m.Actions[0].IdempotencyClass; got != "safe-to-retry" {
		t.Fatalf("run-backup idempotency = %q, want safe-to-retry", got)
	}
}

// Its settings collection is a SINGLETON. The settings-form page binds one
// record (MAN-064), so a non-singleton here would be refused at install — the
// pack would not be installable at all.
func TestThePilotsSettingsCollectionIsASingleton(t *testing.T) {
	m := backupsManifest(t)
	if len(m.DataModel.Collections) != 1 {
		t.Fatalf("collections = %+v, want exactly one", m.DataModel.Collections)
	}
	c := m.DataModel.Collections[0]
	if c.Name != "settings" || !c.Singleton {
		t.Fatalf("collection = %+v, want a singleton named settings (MAN-056/064)", c)
	}
}

// The page's Save and its Run button are BOTH wired, to different verbs. This is
// the pilot's whole shape — `submit` persists policy the pack owns, and
// `call-action` reaches code the pack runs — and a page with only one of them
// would exercise only half the chain.
func TestThePilotPageWiresBothPersistenceAndAnAction(t *testing.T) {
	raw, err := examplepacks.PackFile("backups", "ui/settings.json")
	if err != nil {
		t.Fatalf("read page: %v", err)
	}
	var page struct {
		Source  string `json:"source"`
		Actions []struct {
			On struct {
				Press struct {
					Verb   string `json:"verb"`
					Target string `json:"target"`
				} `json:"press"`
			} `json:"on"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if page.Source != "settings" {
		t.Fatalf("page source = %q, want the singleton collection", page.Source)
	}
	verbs := map[string]string{}
	for _, a := range page.Actions {
		verbs[a.On.Press.Verb] = a.On.Press.Target
	}
	if _, ok := verbs["submit"]; !ok {
		t.Fatalf("the page has no submit; policy would render and not persist. verbs=%v", verbs)
	}
	if verbs["call-action"] != "run-backup" {
		t.Fatalf("call-action targets %q, want run-backup — the button is how the pilot reaches pack CODE", verbs["call-action"])
	}
}

// The engine stays in core. The pack declares no egress and asks for storage
// only: it computes policy and calls the platform's export, so the archive
// encryption and the data key never enter an extension.
func TestThePilotOwnsPolicyAndNotTheEngine(t *testing.T) {
	m := backupsManifest(t)
	if len(m.Egress) != 0 {
		t.Fatalf("the pilot declares egress %v; it calls the local API and nothing else", m.Egress)
	}
	for _, c := range m.Capabilities {
		if c.Capability != "storage.write" {
			t.Fatalf("the pilot asks for %q; policy storage is all it needs", c.Capability)
		}
	}
}
