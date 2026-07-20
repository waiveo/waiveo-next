package manifest

import (
	"regexp"
	"strconv"
)

// macOuiRe is the MAN-071 macOui form: exactly 6 hex digits (a MAC OUI prefix).
var macOuiRe = regexp.MustCompile(`^[0-9A-Fa-f]{6}$`)

// validateDevices enforces MAN-070/071: every device contribution names a
// host-registered device class, and every discovery-match pattern uses one of
// the three recognized forms (ssdp, mdns, macOui). The devices-non-empty-
// requires-compat.relay cross-check (MAN-072) is enforced in validateCompat,
// which also enforces the reverse direction (MAN-011); this function only
// validates the devices-side shape.
func validateDevices(m PackManifest, host HostRegistries) []Error {
	var errs []Error
	for i, d := range m.Devices {
		at := "devices[" + strconv.Itoa(i) + "]"

		if !host.DeviceClasses[d.DeviceClass] {
			errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", at + ".deviceClass",
				`device class "` + d.DeviceClass + `" is not in the host device-class registry (MAN-070)`})
		}

		for mi, match := range d.Match {
			mat := at + ".match[" + strconv.Itoa(mi) + "]"
			if msg := deviceMatchProblem(match); msg != "" {
				errs = append(errs, Error{"UNKNOWN_DEVICE_MATCH_FORM", mat, msg})
			}
		}
	}
	return errs
}

// deviceMatchProblem returns a human message describing why match is not one
// of the three MAN-071 forms — {ssdp: <search-target>}, {mdns: <service-type>},
// or {macOui: <6-hex-digit prefix>} — or "" if it is valid.
func deviceMatchProblem(match map[string]any) string {
	if len(match) != 1 {
		return "devices[].match entries MUST declare exactly one of ssdp, mdns, or macOui (MAN-071)"
	}
	for k, v := range match {
		switch k {
		case "ssdp", "mdns":
			s, ok := v.(string)
			if !ok || s == "" {
				return `devices[].match.` + k + ` MUST be a non-empty string (MAN-071)`
			}
			return ""
		case "macOui":
			s, ok := v.(string)
			if !ok || !macOuiRe.MatchString(s) {
				return `devices[].match.macOui MUST be a 6-hex-digit prefix (MAN-071)`
			}
			return ""
		default:
			return `devices[].match form "` + k + `" is not one of ssdp, mdns, macOui (MAN-071)`
		}
	}
	return ""
}
