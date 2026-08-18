package extensions_test

import (
	"encoding/json"
	"testing"

	"github.com/maaxton/waiveo-next/extensions"
	"github.com/maaxton/waiveo-next/internal/manifest"
)

// roku_test.go asserts the division the owner drew for this extension
// (2026-08-17): "Identifying something as a Roku, we should consider that a
// pattern, and the Roku should supply that pattern." So what this extension
// ships is the KNOWLEDGE — which sighting is a Roku — while the ECP driver stays
// in core on the relay, because only a relay can reach a LAN and extensions
// never run there.

func rokuManifest(t *testing.T) manifest.PackManifest {
	t.Helper()
	b, err := extensions.File("roku", "manifest.json")
	if err != nil {
		t.Fatalf("File(manifest.json): %v", err)
	}
	var m manifest.PackManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return m
}

func TestRokuIsPublishedFirstParty(t *testing.T) {
	if id := rokuManifest(t).ID; id != "waiveo/roku" {
		t.Fatalf("id = %q, want waiveo/roku", id)
	}
}

// TestRokuSuppliesTheDiscoveryPattern is the whole point of the extension: the
// pattern that recognises a Roku is DECLARED here rather than compiled into the
// relay, so a deployment that does not install it simply does not classify
// Rokus — which is what makes Discovery pattern-blind by construction.
func TestRokuSuppliesTheDiscoveryPattern(t *testing.T) {
	m := rokuManifest(t)
	if len(m.Devices) != 1 {
		t.Fatalf("devices = %+v, want exactly one contribution", m.Devices)
	}
	d := m.Devices[0]
	if d.DeviceClass != "media-player" {
		t.Errorf("deviceClass = %q, want media-player (the class whose command vocabulary a Roku answers)", d.DeviceClass)
	}
	if len(d.Match) != 1 {
		t.Fatalf("match = %+v, want one pattern", d.Match)
	}
	// MAN-071 admits exactly {ssdp}|{mdns}|{macOui}. A Roku answers an SSDP
	// search target, which is the form the relay's control point already speaks.
	if got, ok := d.Match[0]["ssdp"].(string); !ok || got != "roku:ecp" {
		t.Fatalf("match = %+v, want {ssdp: roku:ecp} — the search target a Roku answers", d.Match[0])
	}
}

// TestRokuShipsNoDriver: the ECP protocol lives in core on the relay. An
// extension that tried to carry it could not work — extensions do not run on the
// relay — so this asserts the extension stays declarative rather than growing a
// runtime that would have nowhere to run.
func TestRokuShipsNoDriver(t *testing.T) {
	m := rokuManifest(t)
	if m.Runtime != nil {
		t.Fatalf("runtime = %+v, want none: the ECP driver is core-on-relay, and an extension process cannot reach a LAN", m.Runtime)
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0].Capability != "device.read" {
		t.Fatalf("capabilities = %+v, want only device.read — recognising a device is not driving one", m.Capabilities)
	}
}
