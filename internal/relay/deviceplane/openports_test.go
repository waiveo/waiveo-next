package deviceplane

import "testing"

// openports_test.go pins open_ports' MERGE rule, which is the third time this
// store has had to learn the same lesson (Match thrash, then name quality, then
// device_class): a field the PASSIVE lanes do not carry must not be blanked by
// them. The passive lanes re-observe every device every 30s with no ports at
// all, so an overwrite rule would erase a scan's findings seconds after it made
// them — and the operator would see the column fill in and empty out.

func portsObservation(mac string, ports []int) Observation {
	driver, nativeID, match, _ := MACIdentity(mac)
	return Observation{
		Match:       match,
		Provenance:  ProvenanceDiscovered,
		Driver:      driver,
		NativeID:    nativeID,
		DeviceClass: "unclassified",
		Address:     "192.168.50.31",
		OpenPorts:   ports,
	}
}

func TestAPassiveResightingDoesNotBlankScannedPorts(t *testing.T) {
	s := NewStore("relay-1")
	mac := "c4:8b:66:68:21:25"

	// A scan finds ports.
	s.Observe(portsObservation(mac, []int{80, 8060}), 1000)
	// Then the passive neighbour lane re-observes the same device, as it does
	// every 30s, carrying NO ports.
	s.Observe(portsObservation(mac, nil), 2000)

	c := s.Report().Body.Candidates
	if len(c) != 1 {
		t.Fatalf("candidates = %d, want 1", len(c))
	}
	if len(c[0].OpenPorts) != 2 || c[0].OpenPorts[0] != 80 || c[0].OpenPorts[1] != 8060 {
		t.Fatalf("open ports = %v after a passive re-sighting, want [80 8060] kept — a lane that did not look must not erase what a scan learned", c[0].OpenPorts)
	}
}

// The other half of the same rule: a scan that DID look replaces the list
// wholesale, so a port that has closed disappears rather than lingering forever.
func TestARescanReplacesTheListWholesale(t *testing.T) {
	s := NewStore("relay-1")
	mac := "c4:8b:66:68:21:25"

	s.Observe(portsObservation(mac, []int{80, 8060, 9100}), 1000)
	s.Observe(portsObservation(mac, []int{80}), 2000) // 8060 and 9100 have closed

	got := s.Report().Body.Candidates[0].OpenPorts
	if len(got) != 1 || got[0] != 80 {
		t.Fatalf("open ports = %v after a re-scan, want [80] — a closed port must disappear", got)
	}
}

// An empty (but non-nil) result is a scan saying "I looked and nothing is open",
// which must be recorded as such rather than treated like "did not look".
func TestAScanFindingNothingClearsThePorts(t *testing.T) {
	s := NewStore("relay-1")
	mac := "c4:8b:66:68:21:25"

	s.Observe(portsObservation(mac, []int{80}), 1000)
	s.Observe(portsObservation(mac, []int{}), 2000)

	if got := s.Report().Body.Candidates[0].OpenPorts; len(got) != 0 {
		t.Fatalf("open ports = %v, want none — a scan that looked and found nothing says so", got)
	}
}
