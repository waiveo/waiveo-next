package manifest

import "testing"

// TestValidateMemoryBelowFloor: a resources.memory below the host's minimum
// floor MUST fail validation at install time (MAN-042), named at resources.memory.
func TestValidateMemoryBelowFloor(t *testing.T) {
	m := loadManifest(t, man001File)
	host := testHost() // MemoryFloorMiB == 32
	m.Resources.Memory = host.MemoryFloorMiB - 1
	errs := Validate(m, host)
	if !hasCode(errs, "RESOURCE_BELOW_FLOOR") {
		t.Fatalf("memory below the host floor MUST be rejected, got %+v", errs)
	}
	if !hasField(errs, "resources.memory") {
		t.Fatalf("the floor error MUST name resources.memory, got %+v", errs)
	}
}

// TestValidateMemoryAtFloorOK: a resources.memory exactly at the floor is valid.
func TestValidateMemoryAtFloorOK(t *testing.T) {
	m := loadManifest(t, man001File)
	host := testHost()
	m.Resources.Memory = host.MemoryFloorMiB
	if errs := Validate(m, host); hasCode(errs, "RESOURCE_BELOW_FLOOR") {
		t.Fatalf("memory exactly at the floor must be accepted, got %+v", errs)
	}
}

// TestValidateCPUWeightOutOfRange: a cpuWeight beyond the 1-10000 range is a
// schema violation (MAN-040), named at resources.cpuWeight.
func TestValidateCPUWeightOutOfRange(t *testing.T) {
	m := loadManifest(t, man001File)
	m.Resources.CPUWeight = 20000
	errs := Validate(m, testHost())
	if !hasField(errs, "resources.cpuWeight") {
		t.Fatalf("a cpuWeight of 20000 MUST be rejected at resources.cpuWeight, got %+v", errs)
	}
}
