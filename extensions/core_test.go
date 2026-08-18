package extensions_test

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/extensions"
	"github.com/maaxton/waiveo-next/internal/manifest"
)

// core_test.go asserts what makes waiveo/core the BUNDLED core extension the
// owner asked for (2026-08-17): one first-party extension carrying the
// platform's own functions, each contributing its own page, collection and
// actions — rather than a tier of several small extensions in the operator's
// list. Backups is the first function; the shape must take the next one without
// a second extension appearing.

func coreManifest(t *testing.T) manifest.PackManifest {
	t.Helper()
	raw, err := extensions.ZipWithFiles("core", nil)
	if err != nil {
		t.Fatalf("ZipWithFiles(core): %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("the core extension assembled to nothing")
	}
	b, err := extensions.File("core", "manifest.json")
	if err != nil {
		t.Fatalf("File(manifest.json): %v", err)
	}
	var m manifest.PackManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return m
}

func TestCoreIsPublishedFirstParty(t *testing.T) {
	m := coreManifest(t)
	if m.ID != "waiveo/core" {
		t.Fatalf("id = %q, want waiveo/core — the `waiveo` namespace is what the first-party channel is reserved to", m.ID)
	}
}

// TestCoreCarriesBackupsAsAFunction is the absorption the owner asked for:
// backups is a FUNCTION INSIDE core (its own collection, page and action), not
// an extension standing beside it.
func TestCoreCarriesBackupsAsAFunction(t *testing.T) {
	m := coreManifest(t)

	if len(m.DataModel.Collections) != 1 || m.DataModel.Collections[0].Name != "backups" {
		t.Fatalf("collections = %+v, want a `backups` collection", m.DataModel.Collections)
	}
	fields := map[string]bool{}
	for _, f := range m.DataModel.Collections[0].Fields {
		fields[f.Name] = true
	}
	for _, want := range []string{"schedule", "keep_last", "keep_days", "notify_on_failure"} {
		if !fields[want] {
			t.Errorf("backup policy field %q is missing — when to run and how many to keep is what this function owns", want)
		}
	}

	if len(m.UI.Pages) != 1 || m.UI.Pages[0].Path != "backups" {
		t.Fatalf("pages = %+v, want a `backups` page", m.UI.Pages)
	}
	if m.UI.Pages[0].PageType != "settings-form" {
		t.Errorf("page type = %q, want settings-form", m.UI.Pages[0].PageType)
	}
	if _, err := extensions.File("core", "ui/backups.json"); err != nil {
		t.Fatalf("the declared backups page is not bundled: %v", err)
	}

	if len(m.Actions) != 1 || m.Actions[0].Name != "run-backup" {
		t.Fatalf("actions = %+v, want run-backup", m.Actions)
	}
}

// TestCoreIsShapedToTakeASecondFunction. The bundled decision only pays off if
// adding the next platform function does NOT mean a second extension appearing
// in the operator's list — which requires the per-function members to be plural
// in the manifest. This asserts the SHAPE (arrays, one page per function, named
// after its collection) rather than a count, so it keeps holding as functions
// land.
func TestCoreIsShapedToTakeASecondFunction(t *testing.T) {
	m := coreManifest(t)

	byName := map[string]bool{}
	for _, c := range m.DataModel.Collections {
		byName[c.Name] = true
	}
	for _, p := range m.UI.Pages {
		if !byName[p.Path] {
			t.Errorf("page %q has no collection of the same name — each function owns a page and the collection behind it, so a page with no collection is a function with nowhere to keep its policy", p.Path)
		}
	}
	// Every action's capability must be one the manifest actually declares; a
	// bundle makes it easy for a new function to reach for a power the extension
	// never asked the operator about.
	declared := map[string]bool{}
	for _, c := range m.Capabilities {
		declared[c.Capability] = true
	}
	for _, a := range m.Actions {
		if a.CapabilityScope != "" && !declared[a.CapabilityScope] {
			t.Errorf("action %q is scoped to capability %q, which this extension never declares", a.Name, a.CapabilityScope)
		}
	}
}

func TestCoreBundlesItsMessages(t *testing.T) {
	raw, err := extensions.File("core", "messages/en.json")
	if err != nil {
		t.Fatalf("messages/en.json: %v", err)
	}
	var msgs map[string]string
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	for _, key := range []string{
		"pack.displayName", "page.backups.title", "cap.storage.reason",
		"backups.schedule", "backups.keepLast", "backups.run",
	} {
		if msgs[key] == "" {
			t.Errorf("message %q is missing — the surface would render blank", key)
		}
	}
}
