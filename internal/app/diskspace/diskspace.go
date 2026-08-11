// Package diskspace reports the free space on the filesystem a given path lives
// on — the one health signal that kills this appliance for real.
//
// It exists because the failure it measures has already happened to the legacy
// box more than once: repeated image deploys fill the disk, the container can no
// longer be recreated, and the whole box goes down. From the outside that looks
// like an application fault and sends whoever is diagnosing it into the logs,
// which is the wrong place. A health page that names the disk turns a
// multi-hour misdiagnosis into a one-line answer.
//
// # Why it is its own package with a build-tagged implementation
//
// `statfs` is a syscall, and its result type differs per platform (the field is
// `Bavail` on both linux and darwin but with different integer widths, and it
// does not exist at all on windows). internal/app/auth/consolestat_{unix,other}
// sets the precedent this follows: the platform-specific read is isolated behind
// one function, and the platform that cannot answer says so rather than
// returning a plausible zero.
//
// A zero would be the dangerous answer. `free_bytes: 0` on a page whose whole
// job is to warn about a full disk reads as "the disk is full" — the health
// check would manufacture the emergency it exists to detect.
package diskspace

import "errors"

// ErrUnsupported is returned by Usage on a platform with no statfs. A caller
// reports "unknown", never a number.
var ErrUnsupported = errors.New("diskspace: not supported on this platform")

// Usage is one filesystem's capacity, as seen from a path on it.
type Usage struct {
	// TotalBytes is the filesystem's size, and AvailBytes is what is available
	// TO THIS PROCESS — not the raw free count.
	//
	// The distinction is not pedantry. A unix filesystem reserves a percentage
	// of its blocks for root, so `free` and `available` differ by gigabytes on a
	// box this size, and the feeder does not run as root. Reporting the reserved
	// blocks as headroom would tell an operator they have room for one more
	// image right up until the write fails.
	TotalBytes int64
	AvailBytes int64
}

// UsedPercent is how full the filesystem is, from the caller's point of view:
// the space it may NOT use, over the total. Returns 0 for a zero-sized
// filesystem rather than dividing by zero.
//
// Rounded to one decimal place, because a health page renders it and a
// full-precision float renders as noise.
func (u Usage) UsedPercent() float64 {
	if u.TotalBytes <= 0 {
		return 0
	}
	used := u.TotalBytes - u.AvailBytes
	if used < 0 {
		used = 0
	}
	pct := float64(used) * 100 / float64(u.TotalBytes)
	return float64(int64(pct*10+0.5)) / 10
}

// Status classifies headroom into the three answers an operator can act on.
type Status string

const (
	// StatusOK — there is room.
	StatusOK Status = "ok"
	// StatusLow — act soon. Prune images, clear old archives.
	StatusLow Status = "low"
	// StatusCritical — the box is about to stop working. This is the state the
	// legacy box died in.
	StatusCritical Status = "critical"
	// StatusUnknown — this platform, or this path, could not be measured. NEVER
	// conflated with ok: "we could not look" and "we looked and it is fine" send
	// an operator to different places.
	StatusUnknown Status = "unknown"
)

// The thresholds, as ABSOLUTE headroom rather than a percentage.
//
// A percentage is the wrong unit for this question. 10% of a 39 GB appliance
// disk is 3.9 GB — comfortably more than one container image — while 10% of a
// 256 GB disk is 25 GB, so one threshold expressed as a fraction means two
// completely different operational situations. What actually matters is whether
// the next image deploy fits, and that is a number of gigabytes.
const (
	// LowBytes — below this, an image deploy is getting tight.
	LowBytes int64 = 5 << 30 // 5 GiB
	// CriticalBytes — below this, ordinary operation (an upload, a snapshot, an
	// archive) can fail.
	CriticalBytes int64 = 1 << 30 // 1 GiB
)

// Classify grades headroom.
func (u Usage) Classify() Status {
	switch {
	case u.TotalBytes <= 0:
		return StatusUnknown
	case u.AvailBytes < CriticalBytes:
		return StatusCritical
	case u.AvailBytes < LowBytes:
		return StatusLow
	default:
		return StatusOK
	}
}

// Of reports the usage of the filesystem `path` lives on.
//
// A path that does not exist is an error, not a zero: on this box the path is
// the data directory, and "the data directory is missing" is itself a finding
// the health page must show rather than smooth over.
func Of(path string) (Usage, error) { return usage(path) }
