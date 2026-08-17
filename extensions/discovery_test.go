package extensions_test

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/extensions"
	"github.com/maaxton/waiveo-next/internal/manifest"
)

// discovery_test.go asserts the claims that make waiveo/discovery a FIRST-PARTY
// extension rather than merely a well-formed one: who publishes it, what power
// it asks for, and that the division of labour the owner set is actually
// encoded — the extension owns policy, core owns the engine.

func discoveryManifest(t *testing.T) manifest.PackManifest {
	t.Helper()
	raw, err := extensions.ZipWithFiles("discovery", nil)
	if err != nil {
		t.Fatalf("ZipWithFiles(discovery): %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("the discovery extension assembled to nothing")
	}
	b, err := extensions.File("discovery", "manifest.json")
	if err != nil {
		t.Fatalf("File(manifest.json): %v", err)
	}
	var m manifest.PackManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return m
}

// TestDiscoveryIsPublishedFirstParty is the "signed by us" claim in its only
// enforceable form: marketplace/1 MKT-001/021 reserves the first-party trust
// channel to the sole publisher namespace `waiveo`, so the id's namespace is
// what makes this a core extension rather than a community one.
func TestDiscoveryIsPublishedFirstParty(t *testing.T) {
	m := discoveryManifest(t)
	if m.ID != "waiveo/discovery" {
		t.Fatalf("id = %q, want waiveo/discovery — the `waiveo` namespace is what the first-party channel is reserved to", m.ID)
	}
}

// TestDiscoveryAsksOnlyForTheTwoPowersItNeeds. A core extension is trusted by
// virtue of its channel, which makes over-asking cheap and therefore worth
// pinning: it may trigger scans and record its own policy, and nothing else.
func TestDiscoveryAsksOnlyForTheTwoPowersItNeeds(t *testing.T) {
	m := discoveryManifest(t)
	want := map[string]bool{"discovery.scan": true, "storage.write": true}
	if len(m.Capabilities) != len(want) {
		t.Fatalf("capabilities = %+v, want exactly %v", m.Capabilities, want)
	}
	for _, c := range m.Capabilities {
		if !want[c.Capability] {
			t.Errorf("unexpected capability %q — a first-party extension is trusted by channel, so its ask must stay minimal", c.Capability)
		}
	}
}

// TestDiscoveryOwnsPolicyNotTheEngine is the architectural claim the owner set:
// the extension carries the POLICY (which subnets, what schedule, how hard) and
// a scan ACTION, while the enumeration engine stays in core on the relay —
// extensions never run on the relay, so a manifest that tried to own the engine
// could not work even if it declared it.
func TestDiscoveryOwnsPolicyNotTheEngine(t *testing.T) {
	m := discoveryManifest(t)

	if len(m.DataModel.Collections) != 1 || m.DataModel.Collections[0].Name != "settings" {
		t.Fatalf("collections = %+v, want a single `settings` policy collection", m.DataModel.Collections)
	}
	fields := map[string]bool{}
	for _, f := range m.DataModel.Collections[0].Fields {
		fields[f.Name] = true
	}
	for _, want := range []string{"subnets", "schedule", "max_concurrent", "probe_timeout_ms"} {
		if !fields[want] {
			t.Errorf("policy field %q is missing — scope, schedule and rate budget are what this extension exists to own", want)
		}
	}

	if len(m.Actions) != 1 || m.Actions[0].Name != "scan-now" {
		t.Fatalf("actions = %+v, want exactly scan-now", m.Actions)
	}
	if m.Actions[0].CapabilityScope != "discovery.scan" {
		t.Errorf("scan-now capability scope = %q, want discovery.scan", m.Actions[0].CapabilityScope)
	}
	if m.Actions[0].IdempotencyClass != "safe-to-retry" {
		t.Errorf("scan-now idempotency = %q, want safe-to-retry — a replayed scan must not double the probe traffic on a segment", m.Actions[0].IdempotencyClass)
	}
}

// TestDiscoverySettingsPageIsRenderable pins that the operator surface is a page
// type the renderer actually supports. It matters because the Discovery
// INVENTORY console is blocked on renderer gaps (the table widget has no search,
// filter or row action) — a settings-form is not, which is why the policy half
// can ship as an extension today while the inventory half waits.
func TestDiscoverySettingsPageIsRenderable(t *testing.T) {
	m := discoveryManifest(t)
	if len(m.UI.Pages) != 1 || m.UI.Pages[0].Path != "settings" {
		t.Fatalf("ui.pages = %+v, want a single settings page", m.UI.Pages)
	}
	if m.UI.Pages[0].PageType != "settings-form" {
		t.Fatalf("page type = %q, want settings-form", m.UI.Pages[0].PageType)
	}
	// The declared page must actually be bundled at ui/<path>.json (UIS-001) —
	// a manifest naming a page nothing ships fails install, not review.
	if _, err := extensions.File("discovery", "ui/settings.json"); err != nil {
		t.Fatalf("the declared settings page is not bundled: %v", err)
	}
}

// TestDiscoveryBundlesItsMessages: every label the manifest and page reference is
// a msg: key, so a missing catalogue is an extension that installs and renders
// blank.
func TestDiscoveryBundlesItsMessages(t *testing.T) {
	raw, err := extensions.File("discovery", "messages/en.json")
	if err != nil {
		t.Fatalf("messages/en.json: %v", err)
	}
	var msgs map[string]string
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	for _, key := range []string{
		"pack.displayName", "page.settings.title",
		"cap.scan.reason", "cap.storage.reason",
		"settings.subnets", "settings.schedule", "settings.scanNow",
	} {
		if msgs[key] == "" {
			t.Errorf("message %q is missing — the surface would render blank", key)
		}
	}
}
