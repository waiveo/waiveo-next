// Package vitals reads a relay's own physical and operational health and shapes
// it into events/1's `box.vitals` payload (EVT-070/071).
//
// # Two shapes, and the gap between them is the point
//
// What a box can READ and what the schema PUBLISHES are deliberately different.
// A Raspberry Pi reports its throttle state as a bit-packed word and its
// temperature in millidegrees; EVT-070 publishes named flags and degrees. The
// frozen EVT-070 corpus case pins exactly that translation — its input carries
// raw reader-shaped fields (`cpu_temp_c`, `throttled`, `disk_headroom_bytes`) and
// its expected envelope carries the schema's (`cpu_temp`, `throttled_flags`,
// `disk_headroom`) — so the mapping is specified, not invented here.
//
// Naming the flags rather than packing them is EVT-070's own reasoning: "a new
// flag is an additive schema change, not a bit-layout change".
//
// # What this does not do
//
// It does not emit anything. EVT-070's own draft-note says the emission cadence
// "is not fixed by any normative source yet" — proposed at least every 5 minutes
// plus immediately on a flag transition, "subject to revision once real fleet
// data exists". Reading is well-specified; when to send is not, so this package
// answers when asked and the scheduling decision stays where it belongs.
package vitals

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Snapshot is EVT-070's payload, field for field.
//
// The optional members are pointers rather than zero values because zero is a
// LEGITIMATE reading for both: 0 °C is a real temperature and 0 bytes of headroom
// is a real (alarming) disk state. A platform that cannot read one must be
// distinguishable from a box reporting it as zero, or the first fleet dashboard
// built on this will show a wall of healthy-looking zeros for every unreadable
// sensor.
type Snapshot struct {
	RelayID string `json:"relay_id"`
	// CPUTemp is degrees Celsius. Absent where no thermal sensor is readable.
	CPUTemp *float64 `json:"cpu_temp,omitempty"`
	// ThrottledFlags is EVT-071's array: empty, never absent, when nothing is
	// active. The json tag has no omitempty for exactly that reason, and Snapshot's
	// constructor never leaves it nil.
	ThrottledFlags []string `json:"throttled_flags"`
	Undervoltage   bool     `json:"undervoltage"`
	// DiskHeadroom is bytes free on the relay's own operational storage. Absent
	// where the path could not be stat'd.
	DiskHeadroom *int64 `json:"disk_headroom,omitempty"`
	// SDHealth is EVT-070's optional implementation-defined storage-wear detail,
	// "where the platform is able to read one". Omitted entirely otherwise.
	SDHealth map[string]any `json:"sd_health,omitempty"`
}

// Reading is what a platform actually managed to read — the raw side of the
// mapping, in the shape the frozen corpus case's input uses.
//
// Every member is optional because a reader that could not answer must say so
// rather than substitute a plausible number. That is the same rule DAT-033 states
// for timezone resolution, for the same reason: a substituted value is
// indistinguishable from a measured one once it is in a payload.
type Reading struct {
	CPUTempC       *float64
	ThrottledWord  *uint64
	DiskFreeBytes  *int64
	SDHealth       map[string]any
	unavailableFor []string
}

// throttleFlagNames maps a Raspberry Pi `get_throttled` bit to EVT-070's flag
// name. Only the bits with a defined meaning are named; an unknown bit becomes a
// generic named flag rather than being dropped, because a throttle condition
// nobody has a name for yet is still a throttle condition an operator should see.
//
// The low four bits are the NOW conditions and the high four are the
// since-boot ones. Both are reported: "it happened" and "it is happening" are
// different operational facts, and collapsing them loses the one an operator
// acts on.
var throttleFlagNames = map[uint]string{
	0:  "undervoltage-now",
	1:  "arm-frequency-capped-now",
	2:  "throttled-now",
	3:  "soft-temperature-limit-now",
	16: "undervoltage-since-boot",
	17: "arm-frequency-capped-since-boot",
	18: "throttled-since-boot",
	19: "soft-temperature-limit-since-boot",
}

// undervoltageNowBit is the bit EVT-070's own `undervoltage` boolean reports.
// Presently detected, not since-boot: the schema says "presently detected", and
// an operator reading "undervoltage: true" about a condition that cleared hours
// ago would chase a fault that is not there.
const undervoltageNowBit = 0

// Payload turns a Reading into the EVT-070 payload for relayID.
//
// It is a pure function of the reading, so the mapping the frozen corpus case
// pins can be tested without a Raspberry Pi.
func Payload(relayID string, r Reading) Snapshot {
	s := Snapshot{
		RelayID: relayID,
		CPUTemp: r.CPUTempC,
		// EVT-071: empty, never absent. Established here rather than left to the
		// caller, so no path can produce a payload missing it.
		ThrottledFlags: []string{},
		DiskHeadroom:   r.DiskFreeBytes,
		SDHealth:       r.SDHealth,
	}
	if r.ThrottledWord != nil {
		s.ThrottledFlags = flagsFromWord(*r.ThrottledWord)
		s.Undervoltage = *r.ThrottledWord&(1<<undervoltageNowBit) != 0
	}
	return s
}

// flagsFromWord names every set bit of a Raspberry Pi throttle word.
func flagsFromWord(word uint64) []string {
	flags := []string{}
	for bit := uint(0); bit < 32; bit++ {
		if word&(1<<bit) == 0 {
			continue
		}
		if name, ok := throttleFlagNames[bit]; ok {
			flags = append(flags, name)
			continue
		}
		// Unnamed but set: surfaced rather than dropped. A condition with no name
		// yet is still a condition, and dropping it would make the payload claim
		// nothing is wrong.
		flags = append(flags, fmt.Sprintf("unknown-bit-%d", bit))
	}
	return flags
}

// Unavailable reports which readings this platform could not take, so a caller
// can say "not readable here" instead of publishing a payload that looks
// complete.
func (r Reading) Unavailable() []string { return r.unavailableFor }

// Paths the Linux reader consults. Named so a test can point them elsewhere and
// so a reader on a board that puts them somewhere else is a data change.
const (
	thermalPath  = "/sys/class/thermal/thermal_zone0/temp"
	throttlePath = "/sys/devices/platform/soc/soc:firmware/get_throttled"
)

// Read takes a reading from this host.
//
// diskPath is the relay's own operational storage — the thing EVT-070's
// `disk_headroom` is about, not whatever filesystem the process happens to start
// in. Passed in rather than assumed, because the appliance's data directory is a
// deployment choice.
func Read(diskPath string) Reading {
	r := Reading{}

	if temp, err := readMilliDegrees(thermalPath); err == nil {
		r.CPUTempC = &temp
	} else {
		r.unavailableFor = append(r.unavailableFor, "cpu_temp")
	}

	if word, err := readThrottleWord(throttlePath); err == nil {
		r.ThrottledWord = &word
	} else {
		r.unavailableFor = append(r.unavailableFor, "throttled_flags")
	}

	if free, err := freeBytes(diskPath); err == nil {
		r.DiskFreeBytes = &free
	} else {
		r.unavailableFor = append(r.unavailableFor, "disk_headroom")
	}

	return r
}

// readMilliDegrees reads a thermal zone, which reports millidegrees Celsius.
func readMilliDegrees(path string) (float64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	milli, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("thermal zone %s: %w", path, err)
	}
	return float64(milli) / 1000, nil
}

// readThrottleWord reads the firmware throttle word, written as `0x0` hex.
func readThrottleWord(path string) (uint64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	text := strings.TrimSpace(string(raw))
	text = strings.TrimPrefix(strings.TrimPrefix(text, "throttled="), "0x")
	word, err := strconv.ParseUint(text, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("throttle word %s: %w", path, err)
	}
	return word, nil
}
