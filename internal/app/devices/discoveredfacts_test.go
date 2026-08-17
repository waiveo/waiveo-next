package devices

import (
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// discoveredfacts_test.go covers the two things a candidate report gained
// alongside its original members: the reachability/identification facts an
// operator and a command both need, and the ADOPTION flag no relay may touch.
//
// It sits beside intake_test.go rather than inside it because the subject is
// different in kind: intake_test pins what a hostile relay cannot do to
// IDENTITY, and these pin what a well-formed report contributes, plus the one
// row member whose authority is entirely on this side.

// TestReportedAddressAndIdentityReachTheRow: the discovered facts must survive
// intake rather than being silently dropped — a listed device with no address
// is one nothing can be dispatched to.
func TestReportedAddressAndIdentityReachTheRow(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	c := candidate("roku-ecp", "X1")
	c.Address = "192.168.50.31:8060"
	c.Model = "Roku Ultra"
	c.Serial = "X00500ABC123"
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}

	d := r.Devices()[0]
	if d.Address != "192.168.50.31:8060" || d.Model != "Roku Ultra" || d.Serial != "X00500ABC123" {
		t.Errorf("row = {address %q, model %q, serial %q}, want the reported values", d.Address, d.Model, d.Serial)
	}
	if d.Adopted {
		t.Error("a reported device arrived adopted — discovery is not adoption, and nothing a relay sends may set it")
	}
}

// TestOversizedLearnedFieldsRefuseTheReport keeps the new members under the same
// bound every other string off this wire is: they are rendered into a response
// an operator reads, and "merely descriptive" is not a reason to accept an
// unbounded or non-UTF-8 value from an untrusted relay.
func TestOversizedLearnedFieldsRefuseTheReport(t *testing.T) {
	for _, tc := range []struct {
		field string
		set   func(*wire.DeviceCandidate)
	}{
		{"address", func(c *wire.DeviceCandidate) { c.Address = strings.Repeat("a", maxIdentityFieldBytes+1) }},
		{"model", func(c *wire.DeviceCandidate) { c.Model = strings.Repeat("m", maxIdentityFieldBytes+1) }},
		{"serial", func(c *wire.DeviceCandidate) { c.Serial = strings.Repeat("s", maxIdentityFieldBytes+1) }},
		{"non-utf8 address", func(c *wire.DeviceCandidate) { c.Address = "\xff\xfe" }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			r := New(testSite, func() int64 { return 0 })
			c := candidate("roku-ecp", "X1")
			tc.set(&c)
			if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err == nil {
				t.Fatalf("ApplyCandidates accepted an unacceptable %s, want a refusal", tc.field)
			}
			if got := len(r.Devices()); got != 0 {
				t.Errorf("a refused report left %d row(s), want 0", got)
			}
		})
	}
}

// TestMarkAdoptedOutlivesAReport is the reason adoption is held beside the relay
// views rather than on their rows: a report is a full-set REPLACE, and an
// adoption stored inside one would be erased by the next report — including a
// report that simply no longer mentions a device that is powered off.
func TestMarkAdoptedOutlivesAReport(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	id := deviceid.Device(testSite, "roku-ecp", "X1")

	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("first report: %v", err)
	}
	r.MarkAdopted(id)
	if d, _ := r.Device(id); !d.Adopted {
		t.Fatal("MarkAdopted did not take effect")
	}

	// The device re-reports, then drops off the LAN entirely, then comes back.
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("second report: %v", err)
	}
	if d, _ := r.Device(id); !d.Adopted {
		t.Error("adoption was erased by a re-report")
	}
	if err := r.ApplyCandidates(relayA, nil); err != nil {
		t.Fatalf("empty report: %v", err)
	}
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("return report: %v", err)
	}
	if d, _ := r.Device(id); !d.Adopted {
		t.Error("adoption was erased while the device was off the network — a powered-off device is not an un-adopted one")
	}
}

// TestMarkIgnoredOutlivesAReport is the same requirement for the ignore flag,
// and it matters MORE here than for adoption: an ignored device keeps being
// reported (that is the point — it is not adopted, so the relay has no reason to
// stop listing it), so a re-report that erased the ignore is the COMMON path,
// not the rare powered-off one. If ignore did not outlive a report, a device an
// operator set aside would reappear on the next sweep, seconds later.
func TestMarkIgnoredOutlivesAReport(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	id := deviceid.Device(testSite, "roku-ecp", "X1")

	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("first report: %v", err)
	}
	r.MarkIgnored(id)
	if d, _ := r.Device(id); !d.Ignored {
		t.Fatal("MarkIgnored did not take effect")
	}

	// The device re-reports — the ordinary case for an ignored device — and the
	// ignore must still be there.
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("second report: %v", err)
	}
	if d, _ := r.Device(id); !d.Ignored {
		t.Error("the ignore decision was erased by a re-report — a re-sighting must not un-ignore")
	}
}

// TestUnmarkIgnoredIsReversible pins spec §7's "reversible": un-ignoring returns
// the device to plain discovered, and the flag clears on the next read without a
// report having to arrive.
func TestUnmarkIgnoredIsReversible(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	id := deviceid.Device(testSite, "roku-ecp", "X1")

	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("report: %v", err)
	}
	r.MarkIgnored(id)
	if d, _ := r.Device(id); !d.Ignored {
		t.Fatal("MarkIgnored did not take effect")
	}
	r.UnmarkIgnored(id)
	if d, _ := r.Device(id); d.Ignored {
		t.Error("UnmarkIgnored left the device ignored — the decision must be reversible")
	}
	// And ignore is independent of adoption: a device can be adopted while never
	// having been ignored, and clearing ignore does not touch adoption.
	r.MarkAdopted(id)
	r.MarkIgnored(id)
	r.UnmarkIgnored(id)
	if d, _ := r.Device(id); !d.Adopted || d.Ignored {
		t.Errorf("after adopt+ignore+unignore: adopted=%v ignored=%v, want adopted=true ignored=false", d.Adopted, d.Ignored)
	}
}
