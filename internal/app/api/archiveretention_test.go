package api

import "testing"

// Archive retention (archive/1 ARC-124). The decision is tested apart from the
// deletion it authorizes, because this is the half that is impossible to check
// by reading the filesystem afterwards — by then it has already been acted on.

func files(names ...string) []archiveFile {
	// Descending age: the first name is the newest. 1 day apart.
	const day = int64(24 * 60 * 60 * 1000)
	out := make([]archiveFile, 0, len(names))
	for i, n := range names {
		out = append(out, archiveFile{Name: n, ModTime: 1_000_000_000_000 - int64(i)*day})
	}
	return out
}

const now = int64(1_000_000_000_000)

func TestAnUnsetPolicyRetainsEverything(t *testing.T) {
	// The behaviour every deployment has today. Gaining this feature must not
	// delete an archive nobody asked it to delete.
	if got := sweepable(files("a", "b", "c"), RetentionPolicy{}, now); got != nil {
		t.Fatalf("swept %v under an unset policy, want nothing", got)
	}
}

func TestKeepLastRetainsTheNewestN(t *testing.T) {
	got := sweepable(files("a", "b", "c", "d"), RetentionPolicy{KeepLast: 2}, now)
	if len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Fatalf("swept %v, want the two oldest [c d]", got)
	}
}

func TestKeepDaysRetainsAnythingYounger(t *testing.T) {
	// a=today, b=1d, c=2d, d=3d. Keep 2 days: c is exactly 2d (not younger) and
	// d is older, so both go.
	got := sweepable(files("a", "b", "c", "d"), RetentionPolicy{KeepDays: 2}, now)
	if len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Fatalf("swept %v, want [c d]", got)
	}
}

// The rule that decides whether the feature is trustworthy: both bounds RETAIN.
// Intersecting them would silently defeat whichever the operator relied on — a
// quiet box loses its five because they aged out, a busy box loses its
// fortnight because six newer ones exist.
func TestBothBoundsRetainRatherThanIntersect(t *testing.T) {
	// Keep the last 3 OR anything under 2 days. d is 4th-newest and 3 days old,
	// so neither bound saves it; b and c are each saved by at least one.
	got := sweepable(files("a", "b", "c", "d"), RetentionPolicy{KeepLast: 3, KeepDays: 2}, now)
	if len(got) != 1 || got[0] != "d" {
		t.Fatalf("swept %v, want only [d] — a container either bound retains must survive", got)
	}
}

// No policy may empty the directory. A retention setting that can destroy the
// only copy of a workspace is not worth having.
func TestTheNewestIsNeverSwept(t *testing.T) {
	if got := sweepable(files("a"), RetentionPolicy{KeepLast: 0, KeepDays: 1}, now+999*24*60*60*1000); got != nil {
		t.Fatalf("swept %v — the only archive must survive any policy", got)
	}
	got := sweepable(files("a", "b"), RetentionPolicy{KeepDays: 1}, now+999*24*60*60*1000)
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("swept %v, want only [b]: everything is ancient, and the newest still stays", got)
	}
}
