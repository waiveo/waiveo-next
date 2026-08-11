package diskspace

import (
	"errors"
	"testing"
)

// TestOfReadsARealFilesystem drives the actual syscall against a directory the
// test framework guarantees exists. It asserts SHAPE, not values — a total of
// zero or an available count above the total means the platform read is wrong,
// and both are the kind of wrong that would make the health page lie.
func TestOfReadsARealFilesystem(t *testing.T) {
	u, err := Of(t.TempDir())
	if errors.Is(err, ErrUnsupported) {
		t.Skip("no statfs on this platform; usage_other.go's honest refusal is what runs here")
	}
	if err != nil {
		t.Fatalf("Of(tempdir): %v", err)
	}
	if u.TotalBytes <= 0 {
		t.Fatalf("total = %d; a real filesystem is not zero-sized, and a zero here would render as a full disk", u.TotalBytes)
	}
	if u.AvailBytes < 0 || u.AvailBytes > u.TotalBytes {
		t.Fatalf("available = %d of total %d — nonsense; check the Bsize conversion order", u.AvailBytes, u.TotalBytes)
	}
	if p := u.UsedPercent(); p < 0 || p > 100 {
		t.Fatalf("used = %.1f%%", p)
	}
}

// TestAMissingPathIsAnErrorNotAZero. "The data directory is gone" is a finding
// the health page must show, and a zero-valued Usage would render it as a
// perfectly full disk instead — the wrong emergency.
func TestAMissingPathIsAnErrorNotAZero(t *testing.T) {
	u, err := Of(t.TempDir() + "/definitely/not/here")
	if err == nil {
		t.Fatalf("Of on a missing path succeeded with %+v, want an error", u)
	}
}

func TestUsedPercent(t *testing.T) {
	cases := []struct {
		total, avail int64
		want         float64
	}{
		{100, 100, 0},
		{100, 0, 100},
		{100, 25, 75},
		{1000, 333, 66.7},
		// A zero-sized filesystem must not divide by zero.
		{0, 0, 0},
		// available > total is nonsense from a broken read; it must clamp to 0
		// used rather than report a negative percentage.
		{100, 200, 0},
	}
	for _, tc := range cases {
		u := Usage{TotalBytes: tc.total, AvailBytes: tc.avail}
		if got := u.UsedPercent(); got != tc.want {
			t.Errorf("Usage{%d,%d}.UsedPercent() = %v, want %v", tc.total, tc.avail, got, tc.want)
		}
	}
}

// TestClassifyGradesOnAbsoluteHeadroom pins the decision the thresholds encode:
// what matters is whether the next image deploy fits, which is a number of
// gigabytes and not a fraction of a disk whose size varies by a factor of ten
// across deployments.
func TestClassifyGradesOnAbsoluteHeadroom(t *testing.T) {
	const gib = int64(1) << 30
	cases := []struct {
		name         string
		total, avail int64
		want         Status
	}{
		{"a roomy appliance disk", 39 * gib, 20 * gib, StatusOK},
		{"exactly at the low threshold", 39 * gib, LowBytes, StatusOK},
		{"one byte below low", 39 * gib, LowBytes - 1, StatusLow},
		{"exactly at critical", 39 * gib, CriticalBytes, StatusLow},
		{"one byte below critical", 39 * gib, CriticalBytes - 1, StatusCritical},
		{"the box that died", 39 * gib, 40 << 20, StatusCritical},
		// A BIG disk that is 95% full still has room, and must not be reported
		// as an emergency: this is the case a percentage threshold gets wrong.
		{"95% of a large disk", 2000 * gib, 100 * gib, StatusOK},
		// A SMALL disk that is only 80% full is genuinely nearly out — the other
		// case a percentage threshold gets wrong, in the other direction.
		{"80% of a small disk", 4 * gib, 800 << 20, StatusCritical},
		{"unmeasured", 0, 0, StatusUnknown},
	}
	for _, tc := range cases {
		u := Usage{TotalBytes: tc.total, AvailBytes: tc.avail}
		if got := u.Classify(); got != tc.want {
			t.Errorf("%s: Classify() = %q, want %q (avail %d)", tc.name, got, tc.want, tc.avail)
		}
	}
}
