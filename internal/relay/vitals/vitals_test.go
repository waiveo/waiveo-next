package vitals

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func f64(v float64) *float64 { return &v }
func u64(v uint64) *uint64   { return &v }
func i64(v int64) *int64     { return &v }

// TestPayloadMatchesTheFrozenCase drives the mapping the EVT-070 corpus case
// pins: its `input` is the raw reader shape and its `expected.envelope.payload`
// is the schema shape, so the case IS the specification of this translation.
func TestPayloadMatchesTheFrozenCase(t *testing.T) {
	// The case's input, verbatim: cpu_temp_c 46.5, throttled false,
	// disk_headroom_bytes 2147483648, undervoltage false.
	got := Payload("01J8Z3K4N5P6Q7R8S9T0V1W2ZF", Reading{
		CPUTempC:      f64(46.5),
		ThrottledWord: u64(0),
		DiskFreeBytes: i64(2147483648),
	})

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The case's expected payload, member for member.
	want := map[string]any{
		"relay_id":        "01J8Z3K4N5P6Q7R8S9T0V1W2ZF",
		"cpu_temp":        46.5,
		"throttled_flags": []any{},
		"undervoltage":    false,
		"disk_headroom":   float64(2147483648),
	}
	if len(decoded) != len(want) {
		t.Fatalf("payload members = %v, want exactly %v", keys(decoded), keys(want))
	}
	for k, v := range want {
		if !equal(decoded[k], v) {
			t.Errorf("%s = %#v, want %#v", k, decoded[k], v)
		}
	}
}

// TestThrottledFlagsIsAlwaysPresent is EVT-071 stated as a test: empty, never
// absent, "a subscriber checks for emptiness, never for the field's presence".
// An omitempty on that member would satisfy every other test here.
func TestThrottledFlagsIsAlwaysPresent(t *testing.T) {
	for _, name := range []string{"no readings at all", "a zero throttle word"} {
		r := Reading{}
		if name == "a zero throttle word" {
			r.ThrottledWord = u64(0)
		}
		raw, err := json.Marshal(Payload("01J8Z3K4N5P6Q7R8S9T0V1W2ZF", r))
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		got, present := decoded["throttled_flags"]
		if !present {
			t.Errorf("%s: throttled_flags is absent (EVT-071)", name)
			continue
		}
		if string(got) != "[]" {
			t.Errorf("%s: throttled_flags = %s, want []", name, got)
		}
	}
}

func TestEveryThrottleBitIsNamed(t *testing.T) {
	for _, tc := range []struct {
		word uint64
		want []string
	}{
		{0, []string{}},
		{1 << 0, []string{"undervoltage-now"}},
		{1 << 2, []string{"throttled-now"}},
		{1 << 16, []string{"undervoltage-since-boot"}},
		// Several at once, in bit order.
		{(1 << 0) | (1 << 2) | (1 << 18), []string{"undervoltage-now", "throttled-now", "throttled-since-boot"}},
		// A bit with no defined meaning is SURFACED, not dropped: a throttle
		// condition nobody has named yet is still one an operator should see.
		{1 << 7, []string{"unknown-bit-7"}},
	} {
		got := Payload("r", Reading{ThrottledWord: u64(tc.word)}).ThrottledFlags
		if len(got) != len(tc.want) {
			t.Errorf("word %#x: flags = %v, want %v", tc.word, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("word %#x: flags = %v, want %v", tc.word, got, tc.want)
				break
			}
		}
	}
}

// TestUndervoltageReportsNowNotSinceBoot: EVT-070 says "presently detected". An
// operator told `undervoltage: true` about a condition that cleared hours ago
// chases a fault that is not there — while the since-boot fact stays visible in
// throttled_flags, which is where history belongs.
func TestUndervoltageReportsNowNotSinceBoot(t *testing.T) {
	sinceBootOnly := Payload("r", Reading{ThrottledWord: u64(1 << 16)})
	if sinceBootOnly.Undervoltage {
		t.Error("undervoltage is true for a since-boot-only condition")
	}
	if len(sinceBootOnly.ThrottledFlags) != 1 || sinceBootOnly.ThrottledFlags[0] != "undervoltage-since-boot" {
		t.Errorf("the since-boot fact was lost: %v", sinceBootOnly.ThrottledFlags)
	}

	if !Payload("r", Reading{ThrottledWord: u64(1 << 0)}).Undervoltage {
		t.Error("undervoltage is false while the now-bit is set")
	}
}

// TestAnUnreadableSensorIsAbsentNotZero is the property the pointer types exist
// for. Zero is a legitimate reading for both members — 0 °C is a temperature and
// 0 bytes of headroom is an alarming disk — so a platform that cannot read one
// must be distinguishable from a box reporting it, or a fleet dashboard shows a
// wall of healthy-looking zeros for every dead sensor.
func TestAnUnreadableSensorIsAbsentNotZero(t *testing.T) {
	raw, err := json.Marshal(Payload("01J8Z3K4N5P6Q7R8S9T0V1W2ZF", Reading{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, absent := range []string{"cpu_temp", "disk_headroom", "sd_health"} {
		if v, present := decoded[absent]; present {
			t.Errorf("%s is present as %s for a reading that could not be taken", absent, v)
		}
	}

	// And a genuine zero IS reported, or "absent" and "zero" would have collapsed
	// the other way.
	raw, _ = json.Marshal(Payload("r", Reading{CPUTempC: f64(0), DiskFreeBytes: i64(0)}))
	_ = json.Unmarshal(raw, &decoded)
	if string(decoded["cpu_temp"]) != "0" || string(decoded["disk_headroom"]) != "0" {
		t.Errorf("a measured zero was dropped: cpu_temp=%s disk_headroom=%s", decoded["cpu_temp"], decoded["disk_headroom"])
	}
}

// TestReadSaysWhatItCouldNotRead: this repo is developed on macOS, where none of
// the Pi sysfs paths exist. A reader that silently returned zeros there would
// make every dev-machine payload look like a healthy box.
func TestReadSaysWhatItCouldNotRead(t *testing.T) {
	r := Read(t.TempDir())

	// Disk headroom is portable and must always be readable on a real directory.
	if r.DiskFreeBytes == nil {
		t.Error("disk headroom was not readable for an existing directory")
	}
	// The sysfs-backed ones are absent off a Pi, and Read must SAY so.
	for _, want := range []string{"cpu_temp", "throttled_flags"} {
		if r.CPUTempC != nil && want == "cpu_temp" {
			continue // running on a box that genuinely has one
		}
		if r.ThrottledWord != nil && want == "throttled_flags" {
			continue
		}
		if !contains(r.Unavailable(), want) {
			t.Errorf("%s is unreadable but Unavailable() does not say so: %v", want, r.Unavailable())
		}
	}
}

func TestReadReportsAMissingDiskPath(t *testing.T) {
	r := Read(filepath.Join(os.TempDir(), "no-such-directory-for-vitals"))
	if r.DiskFreeBytes != nil {
		t.Error("disk headroom was reported for a path that does not exist")
	}
	if !contains(r.Unavailable(), "disk_headroom") {
		t.Errorf("Unavailable() = %v, want it to name disk_headroom", r.Unavailable())
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func equal(got, want any) bool {
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	return string(gotJSON) == string(wantJSON)
}
