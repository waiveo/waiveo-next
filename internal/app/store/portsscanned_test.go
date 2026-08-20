package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// "A scan looked and found nothing open" and "nobody has looked" are different
// facts, and until this change the mirror could not hold the difference: the
// column is TEXT NOT NULL DEFAULT '[]' and every value went through nonNilPorts,
// so both landed as the same four bytes. mergeDiscovered recorded that as a
// KNOWN LIMIT and predicted the day it would matter — the day a scan reports a
// scanned-but-closed host. The relay's port scan now does exactly that.
//
// Everything below is about the round trip, because that is where the fact was
// being lost: a value can be correct in memory and still come back from disk as
// the other one.

func mirrored(deviceID string, ports []int) store.DiscoveredDevice {
	return store.DiscoveredDevice{
		DeviceID:    deviceID,
		RelayID:     "relay-a",
		ScopeNode:   "01J8Z0ROOT0000000000000000",
		Driver:      "mac",
		NativeID:    deviceID,
		DeviceClass: "unclassified",
		FirstSeen:   1_700_000_000_000,
		LastSeen:    1_700_000_000_000,
		OpenPorts:   ports,
	}
}

func TestOpenPortsRoundTripKeepsAbsentApartFromEmpty(t *testing.T) {
	st := openFileStoreAt(t, filepath.Join(t.TempDir(), "app.db"), func() int64 { return ddAppNowMs })
	ctx := context.Background()

	if _, err := st.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		mirrored("dev-unscanned", nil),
		mirrored("dev-scanned-empty", []int{}),
		mirrored("dev-scanned-open", []int{22, 8060}),
	}); err != nil {
		t.Fatalf("ReplaceDiscoveredDevices: %v", err)
	}

	got := map[string][]int{}
	rows, err := st.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	for _, r := range rows {
		got[r.DeviceID] = r.OpenPorts
	}

	// The whole point: nil comes back nil, and empty comes back empty-and-non-nil.
	if got["dev-unscanned"] != nil {
		t.Errorf("a device nothing scanned came back as %#v, want nil — an empty list here claims a scan that never ran",
			got["dev-unscanned"])
	}
	if p := got["dev-scanned-empty"]; p == nil || len(p) != 0 {
		t.Errorf("a device a scan found nothing open on came back as %#v, want a non-nil empty list", p)
	}
	if p := got["dev-scanned-open"]; len(p) != 2 || p[0] != 22 || p[1] != 8060 {
		t.Errorf("findings did not survive: %#v", p)
	}
}

func TestAScanThatFindsNothingRETRACTSAClosedPort(t *testing.T) {
	// The reason an empty list has to be a first-class value rather than a
	// tidier spelling of nil. A device that answered 8060 last week and answers
	// nothing today must stop reporting 8060 — otherwise the platform serves a
	// finding about hardware that has been unplugged, forever, because the
	// don't-blank guard never lets a later report take it away.
	st := openFileStoreAt(t, filepath.Join(t.TempDir(), "app.db"), func() int64 { return ddAppNowMs })
	ctx := context.Background()

	if _, err := st.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		mirrored("dev-x", []int{8060}),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		mirrored("dev-x", []int{}),
	}); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	rows, err := st.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if p := rows[0].OpenPorts; p == nil || len(p) != 0 {
		t.Fatalf("a scan reporting nothing open left %#v — the closed port was never retracted", p)
	}
}

func TestAReportThatDidNotScanKeepsWhatIsKnown(t *testing.T) {
	// The other half of the same guard, and the reason it exists at all: the
	// passive lanes re-observe a device every 30s carrying no ports, so a nil
	// must never blank a scan's findings seconds after it made them.
	st := openFileStoreAt(t, filepath.Join(t.TempDir(), "app.db"), func() int64 { return ddAppNowMs })
	ctx := context.Background()

	if _, err := st.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		mirrored("dev-y", []int{22, 443}),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.ReplaceDiscoveredDevices(ctx, "relay-a", []store.DiscoveredDevice{
		mirrored("dev-y", nil),
	}); err != nil {
		t.Fatalf("passive re-observation: %v", err)
	}

	rows, err := st.DiscoveredDevices(ctx)
	if err != nil {
		t.Fatalf("DiscoveredDevices: %v", err)
	}
	if p := rows[0].OpenPorts; len(p) != 2 {
		t.Fatalf("a passive re-observation blanked a scan's findings: %#v", p)
	}
}

func TestTheREALTypesSerializeEmptyRatherThanVanishing(t *testing.T) {
	// The tag guard, and it marshals the PRODUCTION structs on purpose. An
	// earlier draft of this test declared its own probe struct with the tag
	// written out — which proves what `omitzero` does in Go and proves nothing
	// about this codebase: revert every real tag to `omitempty` and it still
	// passes. The distinction is carried by three specific struct fields, so
	// those three are what get marshalled.
	//
	// `omitempty` drops an empty slice and a nil one identically, which is
	// exactly the distinction these members exist to carry. `omitzero` omits only
	// the zero value — nil — so an empty list survives as `[]`.
	cases := []struct {
		what  string
		value any
	}{
		{"app Device (api/1 reader)", devices.Device{}},
		{"relay->app wire candidate", wire.DeviceCandidate{}},
	}
	for _, c := range cases {
		out, err := json.Marshal(c.value)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.what, err)
		}
		if bytes.Contains(out, []byte(`"open_ports"`)) {
			t.Errorf("%s: a value nothing scanned serialized open_ports at all (%s); the member must be ABSENT",
				c.what, out)
		}
	}

	// And the half that regressing to omitempty would silently break.
	dev := devices.Device{OpenPorts: []int{}}
	out, err := json.Marshal(dev)
	if err != nil {
		t.Fatalf("marshal Device: %v", err)
	}
	if !bytes.Contains(out, []byte(`"open_ports":[]`)) {
		t.Errorf("a scan that found nothing open serialized as %s — want an explicit empty list, "+
			"or every API reader is told nobody looked", out)
	}

	cand := wire.DeviceCandidate{OpenPorts: []int{}}
	out, err = json.Marshal(cand)
	if err != nil {
		t.Fatalf("marshal DeviceCandidate: %v", err)
	}
	if !bytes.Contains(out, []byte(`"open_ports":[]`)) {
		t.Errorf("the wire dropped a scan's empty finding: %s", out)
	}
}
