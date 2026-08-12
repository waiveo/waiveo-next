package examplepacks_test

import (
	"encoding/json"
	"testing"

	examplepacks "github.com/maaxton/waiveo-next/examples/packs"
	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/deviceclass"
	"github.com/maaxton/waiveo-next/internal/manifest"
)

// The example pack is the living proof of the whole wave, so it must itself be a
// clean artifact: a safe, well-formed zip whose manifest the REAL manifest engine
// accepts against genuinely-populated host registries — the same gate a POST
// /api/v1/packs install applies. If the on-disk pack ever drifts out of contract,
// this fails at build time, before the smoke or the e2e ever run.

func TestMenuBoardZipReadsAsASafeBundle(t *testing.T) {
	art, err := examplepacks.MenuBoardZip()
	if err != nil {
		t.Fatalf("MenuBoardZip: %v", err)
	}
	bundle, err := packs.ReadBundle(art, packs.DefaultLimits)
	if err != nil {
		t.Fatalf("the example pack is not a safe artifact: %v", err)
	}
	// The four declarative files ship, at their contract locations — and nothing
	// else (no code, no stray files).
	for _, want := range []string{"manifest.json", "ui/menu-items.json", "ui/settings.json", "messages/en.json"} {
		if !bundle.Has(want) {
			t.Fatalf("example pack bundle is missing %q", want)
		}
	}
	if got := len(bundle.Names()); got != 4 {
		t.Fatalf("example pack bundle has %d entries, want exactly 4 (the four declarative files)", got)
	}
}

func TestMenuBoardManifestPassesTheRealEngine(t *testing.T) {
	raw, err := examplepacks.MenuBoardFile("manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m manifest.PackManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest.json is not valid JSON: %v", err)
	}
	if m.ID != "waiveo/menu-board" || m.Version != "1.0.0" {
		t.Fatalf("manifest identity = %q %q, want waiveo/menu-board 1.0.0", m.ID, m.Version)
	}

	// Validate against the SAME host registries the install pipeline builds: the
	// renderer's page types, the built-in device classes, the pack's own bundle
	// file set, and the dev memory floor — so a passing check here means a passing
	// install.
	pageTypes := map[string]bool{}
	for _, t := range packs.PageTypes {
		pageTypes[t] = true
	}
	deviceClasses := map[string]bool{}
	for _, name := range deviceclass.Builtin().ClassNames() {
		deviceClasses[name] = true
	}
	host := manifest.HostRegistries{
		Capabilities:   map[string]bool{},
		Features:       map[string]bool{},
		PageTypes:      pageTypes,
		DeviceClasses:  deviceClasses,
		Providers:      map[string]bool{},
		BundleFiles:    map[string]bool{"messages/en.json": true},
		MemoryFloorMiB: 32,
	}
	if errs := manifest.Validate(m, host); len(errs) > 0 {
		t.Fatalf("the example manifest was refused by the engine: %+v", errs)
	}

	// Two pages + two collections: the lifecycle-participating menu_items list,
	// and the singleton `settings` record the settings-form page binds. The
	// second is not decoration — MAN-064 refuses a settings-form whose source
	// names no declared singleton, so a pack with this page cannot install
	// without it.
	if len(m.UI.Pages) != 2 {
		t.Fatalf("manifest declares %d pages, want 2", len(m.UI.Pages))
	}
	if len(m.DataModel.Collections) != 2 ||
		m.DataModel.Collections[0].Name != "menu_items" ||
		m.DataModel.Collections[1].Name != "settings" {
		t.Fatalf("manifest collections = %+v, want [menu_items settings]", m.DataModel.Collections)
	}
	if m.DataModel.Collections[0].Singleton {
		t.Fatalf("menu_items must not be a singleton — it is the list the list-detail page pages through")
	}
	if !m.DataModel.Collections[1].Singleton {
		t.Fatalf("settings must declare singleton: true (MAN-056), or the settings-form page cannot save")
	}
}
