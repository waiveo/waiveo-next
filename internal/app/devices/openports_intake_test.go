package devices

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// openports_intake_test.go exists because the field was added to every struct
// and to none of the code that COPIES between them — so the relay attached ports
// to 38 real devices and the API served none. Every hop is asserted at the FAR
// side here: a struct field that is never carried looks exactly like a device
// nothing scanned.

func TestReportedPortsReachTheRow(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	c := candidate("net", "c4:8b:66:68:21:25")
	c.OpenPorts = []int{80, 8060}
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	d := r.Devices()[0]
	if len(d.OpenPorts) != 2 || d.OpenPorts[0] != 80 || d.OpenPorts[1] != 8060 {
		t.Fatalf("row open ports = %v, want [80 8060] — a field the intake drops is invisible", d.OpenPorts)
	}
}

// A device nothing scanned carries NO list, not an empty one: absent means
// nobody looked, and only a scan can say "looked and found nothing".
func TestAnUnscannedDeviceCarriesNoPorts(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("net", "c4:8b:66:68:21:26")}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	if got := r.Devices()[0].OpenPorts; len(got) != 0 {
		t.Fatalf("open ports = %v on an unscanned device, want none", got)
	}
}

// Ports arrive from an untrusted relay, so a nonsense one refuses the WHOLE
// report exactly as every other invalid field does (REL-111 all-or-nothing).
func TestANonsensePortRefusesTheReport(t *testing.T) {
	for _, bad := range [][]int{{0}, {-1}, {70000}} {
		r := New(testSite, func() int64 { return 0 })
		c := candidate("net", "c4:8b:66:68:21:27")
		c.OpenPorts = bad
		if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err == nil {
			t.Errorf("a candidate reporting port %v was accepted", bad)
		}
		if n := len(r.Devices()); n != 0 {
			t.Errorf("a refused report left %d row(s)", n)
		}
	}
}

func TestTooManyPortsRefusesTheReport(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	c := candidate("net", "c4:8b:66:68:21:28")
	for i := 0; i < maxOpenPortsPerCandidate+1; i++ {
		c.OpenPorts = append(c.OpenPorts, 1000+i)
	}
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err == nil {
		t.Fatal("a candidate reporting more ports than the cap was accepted")
	}
}
