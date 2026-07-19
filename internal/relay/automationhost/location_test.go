package automationhost

import (
	"path/filepath"
	"testing"
)

// TestSetLocationAdoptsSiteBinding proves the relay adopts a hello-ack's
// authoritative site_binding into its running engine (relay/1 REL-036 →
// rules/1 engine.SetLocation): a valid IANA zone loads, and an unknown zone
// surfaces the loader's error rather than silently succeeding.
func TestSetLocationAdoptsSiteBinding(t *testing.T) {
	host, _ := newTestHost(t, filepath.Join(t.TempDir(), "relay.db"), &recordController{})

	if err := host.SetLocation("America/Chicago", 41.8781, -87.6298); err != nil {
		t.Fatalf("SetLocation(America/Chicago): %v", err)
	}
	if err := host.SetLocation("Not/AZone", 1, 2); err == nil {
		t.Fatalf("SetLocation accepted an unknown zone")
	}
}
