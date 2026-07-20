package manifest

import "strconv"

// validateResources enforces MAN-040/042: the resources block declares an
// enforced memory ceiling, a relative CPU scheduling weight in the range
// 1-10000 (default 100 when omitted), a storage quota, and a schedule-timer
// ceiling. A resources.memory below the host-configured minimum floor MUST fail
// validation at install time with a typed RESOURCE_BELOW_FLOOR error; a cpuWeight
// outside 1-10000 is a schema violation.
func validateResources(m PackManifest, host HostRegistries) []Error {
	var errs []Error
	r := m.Resources

	if r.Memory < host.MemoryFloorMiB {
		errs = append(errs, Error{"RESOURCE_BELOW_FLOOR", "resources.memory",
			"resources.memory " + strconv.Itoa(r.Memory) + " MiB is below the host minimum floor of " +
				strconv.Itoa(host.MemoryFloorMiB) + " MiB (MAN-042)"})
	}

	// cpuWeight is 1-10000, defaulting to 100 when omitted (an omitted integer
	// unmarshals to 0, which this treats as "use the default", not an error).
	if r.CPUWeight != 0 && (r.CPUWeight < 1 || r.CPUWeight > 10000) {
		errs = append(errs, Error{"MANIFEST_SCHEMA_INVALID", "resources.cpuWeight",
			"resources.cpuWeight MUST be an integer in the range 1-10000 (MAN-040)"})
	}

	return errs
}
