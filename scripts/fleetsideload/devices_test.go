package main

import "testing"

// The roster is the input a human types while standing in a hallway with a
// laptop, so every shape they plausibly type is pinned here — and, more
// importantly, so is the shape that must FAIL. A parser that quietly drops a
// malformed entry turns "6/6 installed" into a lie about a seven-screen wall.
func TestParseDeviceListShapes(t *testing.T) {
	devices, err := parseDeviceList(" hanger=192.168.50.21, 192.168.50.22 ,lobby=roku-lobby.local:8888 , ")
	if err != nil {
		t.Fatalf("parseDeviceList: %v", err)
	}
	want := []device{
		{Name: "hanger", Host: "192.168.50.21", Port: devInstallerPort},
		// No name given: the host IS the label, so the report still identifies
		// the row rather than printing an empty column.
		{Name: "192.168.50.22", Host: "192.168.50.22", Port: devInstallerPort},
		// An explicitly typed port is honoured — the only way one gets here is
		// deliberately (a tunnel, a lab proxy).
		{Name: "lobby", Host: "roku-lobby.local", Port: 8888},
	}
	if len(devices) != len(want) {
		t.Fatalf("parsed %d device(s), want %d: %+v", len(devices), len(want), devices)
	}
	for i := range want {
		if devices[i] != want[i] {
			t.Errorf("device %d = %+v, want %+v", i, devices[i], want[i])
		}
	}
}

func TestParseDeviceListRejectsMalformed(t *testing.T) {
	for _, raw := range []string{
		"hanger=",             // named, but no address at all
		"192.168.50.21:",      // colon with no port
		"192.168.50.21:0",     // port 0 is not dialable
		"192.168.50.21:99999", // out of range
	} {
		if devices, err := parseDeviceList(raw); err == nil {
			t.Errorf("parseDeviceList(%q) accepted a malformed entry as %+v; a dropped screen must never be silent", raw, devices)
		}
	}
}

// An entry whose name is empty ("=host") is a typo, not a failure: the host
// becomes the label and the device is still reachable. Refusing it would trade
// a cosmetic slip for a screen that does not get the build.
func TestParseDeviceListEmptyNameFallsBackToHost(t *testing.T) {
	devices, err := parseDeviceList("=192.168.50.21")
	if err != nil {
		t.Fatalf("parseDeviceList: %v", err)
	}
	if len(devices) != 1 || devices[0].Name != "192.168.50.21" {
		t.Fatalf("parsed %+v, want the host used as the label", devices)
	}
}

func TestParseDeviceListEmpty(t *testing.T) {
	devices, err := parseDeviceList("  , ,")
	if err != nil {
		t.Fatalf("parseDeviceList: %v", err)
	}
	if len(devices) != 0 {
		t.Fatalf("an all-separator list parsed to %+v, want no devices", devices)
	}
}

// The ECP roster carries ECP ports (:8060). Sideloading to :8060 reaches a
// service that has never heard of /plugin_install, so the port MUST be
// dropped — this is the one behavioural difference between the two roster
// parsers and the whole reason they are separate entry points.
func TestParseECPTargetDevicesDropsECPPort(t *testing.T) {
	devices, err := parseECPTargetDevices("screen.hanger=192.168.50.21:8060,screen.lobby=192.168.50.22")
	if err != nil {
		t.Fatalf("parseECPTargetDevices: %v", err)
	}
	want := []device{
		{Name: "screen.hanger", Host: "192.168.50.21", Port: devInstallerPort},
		{Name: "screen.lobby", Host: "192.168.50.22", Port: devInstallerPort},
	}
	for i := range want {
		if devices[i] != want[i] {
			t.Errorf("device %d = %+v, want %+v", i, devices[i], want[i])
		}
	}
	if got := devices[0].Addr(); got != "192.168.50.21:80" {
		t.Errorf("dialled address = %q, want the dev installer port, not the ECP one", got)
	}
}

func TestParseECPTargetDevicesRejectsBareHost(t *testing.T) {
	// The relay's own grammar requires entity=host; a bare host has no entity
	// id, and inventing one would make the report name a screen something the
	// relay and the console do not.
	if _, err := parseECPTargetDevices("192.168.50.21"); err == nil {
		t.Fatal("parseECPTargetDevices accepted a bare host; the relay grammar is entity=host[:port]")
	}
}
