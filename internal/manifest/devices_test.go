package manifest

import "testing"

// TestValidateUnknownDeviceClass: a devices[].deviceClass absent from the host
// device-class registry is refused with a typed error naming it (MAN-070),
// never a silent no-op grant.
func TestValidateUnknownDeviceClass(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Devices[0].DeviceClass = "printer"
	errs := Validate(m, testHost())
	if !hasField(errs, "devices[0].deviceClass") {
		t.Fatalf("expected a deviceClass error for %q, got %+v", m.Devices[0].DeviceClass, errs)
	}
}

// TestValidateUnknownDeviceMatchForm: a match entry using an unrecognized form
// (e.g. upnp, not one of ssdp/mdns/macOui) fails with UNKNOWN_DEVICE_MATCH_FORM
// (MAN-071).
func TestValidateUnknownDeviceMatchForm(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Devices[0].Match = []map[string]any{{"upnp": "urn:some:device"}}
	errs := Validate(m, testHost())
	if !hasCode(errs, "UNKNOWN_DEVICE_MATCH_FORM") {
		t.Fatalf("expected an UNKNOWN_DEVICE_MATCH_FORM error, got %+v", errs)
	}
	if !hasField(errs, "devices[0].match[0]") {
		t.Fatalf("expected the error to name devices[0].match[0], got %+v", errs)
	}
}

// TestValidateDeviceMatchMacOuiValid: a well-formed 6-hex-digit macOui match
// validates clean (MAN-071).
func TestValidateDeviceMatchMacOuiValid(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Devices[0].Match = []map[string]any{{"macOui": "AABBCC"}}
	errs := Validate(m, testHost())
	if hasField(errs, "devices[0].match[0]") {
		t.Fatalf("expected a valid macOui match to validate clean, got %+v", errs)
	}
}

// TestValidateDeviceMatchMacOuiInvalid: a macOui value that is not exactly 6 hex
// digits fails MAN-071.
func TestValidateDeviceMatchMacOuiInvalid(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Devices[0].Match = []map[string]any{{"macOui": "not-hex"}}
	errs := Validate(m, testHost())
	if !hasCode(errs, "UNKNOWN_DEVICE_MATCH_FORM") {
		t.Fatalf("expected an UNKNOWN_DEVICE_MATCH_FORM error for a malformed macOui, got %+v", errs)
	}
}

// TestValidateDeviceMatchMdnsValid: a well-formed mdns match validates clean
// (MAN-071).
func TestValidateDeviceMatchMdnsValid(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Devices[0].Match = []map[string]any{{"mdns": "_roku._tcp"}}
	errs := Validate(m, testHost())
	if hasField(errs, "devices[0].match[0]") {
		t.Fatalf("expected a valid mdns match to validate clean, got %+v", errs)
	}
}

// TestValidateDevicesRequireCompatRelay: a non-empty devices array with no
// compat.relay declared violates MAN-072 (finalizing the Task 1 cross-check
// from the devices side).
func TestValidateDevicesRequireCompatRelay(t *testing.T) {
	m := loadManifest(t, man020File)
	m.Compat.Relay = ""
	errs := Validate(m, testHost())
	if !hasField(errs, "compat.relay") {
		t.Fatalf("expected a compat.relay error for non-empty devices with no relay range (MAN-072), got %+v", errs)
	}
}
