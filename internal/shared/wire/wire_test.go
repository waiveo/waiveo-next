package wire

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestHello asserts a round-trip JSON marshal of a placeholder Hello struct
// carries exactly the field names the relay/1 contract's Wire-shapes section
// requires for the relay -> app-peer `hello` message (REL-031), including the
// `clock_state` sub-shape's field names (REL-038). This is the shared vocabulary
// both the feeder and relay skeletons import; later tasks fill in behavior.
func TestHello(t *testing.T) {
	h := Hello{
		RelayID:         "01J8Z4K4N5P6Q7R8S9T0V1W3A1",
		ProtocolVersion: "1.0",
		Features:        []string{"telemetry.latest_only_v1"},
		SiteBinding: SiteBinding{
			ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
			TZ:        "America/Chicago",
			Lat:       41.8781,
			Long:      -87.6298,
		},
		SubnetMetadata: SubnetMetadata{
			AdvertisedAddress: "203.0.113.12",
		},
		ClockState: ClockState{
			State:  "trusted",
			Source: "ntp",
		},
		ChannelBindingSignature: "ed25519-sig:5f6e7a1b2c3d4e5f8091a2b3c4d5e6f7",
	}

	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}

	wantKeys := []string{
		"relay_id",
		"protocol_version",
		"features",
		"site_binding",
		"subnet_metadata",
		"clock_state",
		"channel_binding_signature",
	}
	if len(got) != len(wantKeys) {
		t.Fatalf("Hello marshaled to %d top-level keys, want exactly %d (%v); got %v", len(got), len(wantKeys), wantKeys, raw)
	}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("Hello JSON missing contract field %q; got %s", k, raw)
		}
	}

	var clockState map[string]json.RawMessage
	if err := json.Unmarshal(got["clock_state"], &clockState); err != nil {
		t.Fatalf("Unmarshal clock_state: %v", err)
	}
	for _, k := range []string{"state", "source"} {
		if _, ok := clockState[k]; !ok {
			t.Errorf("Hello.clock_state JSON missing contract field %q (REL-038); got %s", k, got["clock_state"])
		}
	}

	// Round-trip: unmarshal back into Hello and confirm it matches the original.
	var back Hello
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("round-trip Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back, h) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", back, h)
	}
}
