package api

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Archive retention (archive/1 ARC-124): bounding how many export containers a
// deployment keeps, so a disk does not fill with backups nobody pruned.
//
// The failure this answers is one this project has already had on a box: a disk
// filling until the container could not be recreated. diskspace.go's low-disk
// remedy literally advises "clear old archives", which until the delete
// operation existed was advice an operator had no way to follow, and which
// until now nothing followed on their behalf.

// RetentionPolicy bounds the retained set. A zero value retains everything,
// which is what every deployment does today — gaining this feature must not
// delete an archive nobody asked it to delete.
type RetentionPolicy struct {
	// KeepLast retains the N newest containers. Zero means "no count bound".
	KeepLast int
	// KeepDays retains anything younger than N days. Zero means "no age bound".
	KeepDays int
}

// Unset reports a policy that retains everything.
func (p RetentionPolicy) Unset() bool { return p.KeepLast <= 0 && p.KeepDays <= 0 }

// archiveFile is one container the sweep may consider.
type archiveFile struct {
	Name    string
	ModTime int64 // epoch ms
}

// sweepable decides which containers a policy releases, given the set present
// and the current time (ARC-124).
//
// Pure, and separate from the deletion it authorizes, because this is the half
// that is easy to get wrong and impossible to check by reading the filesystem
// afterwards — by then the answer has already been acted on.
//
// # Both bounds RETAIN, they do not both have to agree
//
// Where a policy gives keep-last AND keep-days, a container is retained if
// EITHER would retain it. The two express different fears — "I want the last
// five whatever happens" and "I want a fortnight of cover" — and intersecting
// them would silently defeat whichever one the operator was relying on: a quiet
// box would lose its five because they aged out, and a busy box would lose its
// fortnight because six newer ones exist.
//
// # The newest is never swept
//
// Under any policy, including a keep-last of zero. A policy that can empty the
// archive directory is a policy that can destroy the only copy of a workspace,
// and no retention setting is worth that. A deployment that wants no archives
// at all deletes them itself — deliberately, one at a time, through an
// operation that says what it is doing.
func sweepable(files []archiveFile, p RetentionPolicy, nowMs int64) []string {
	if p.Unset() || len(files) <= 1 {
		return nil
	}
	// Newest first, so "the first KeepLast" is the retained set and index 0 is
	// the one that is never swept. Name breaks a modtime tie so the answer is
	// stable across two runs over the same directory.
	sorted := append([]archiveFile(nil), files...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].ModTime != sorted[j].ModTime {
			return sorted[i].ModTime > sorted[j].ModTime
		}
		return sorted[i].Name > sorted[j].Name
	})

	var out []string
	for i, f := range sorted {
		if i == 0 {
			continue // never the newest
		}
		if p.KeepLast > 0 && i < p.KeepLast {
			continue // within the count bound
		}
		if p.KeepDays > 0 && nowMs-f.ModTime < int64(p.KeepDays)*24*60*60*1000 {
			continue // within the age bound
		}
		out = append(out, f.Name)
	}
	return out
}

// readArchiveFiles lists the containers in dir. A missing directory is an empty
// set, not an error: a deployment that has never exported has nothing to sweep.
func readArchiveFiles(dir string) []archiveFile {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]archiveFile, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), archiveSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, archiveFile{Name: e.Name(), ModTime: info.ModTime().UnixMilli()})
	}
	return out
}

// sweepArchives applies p to dir and returns what it removed, by name.
//
// The names are returned rather than logged and forgotten because ARC-124
// requires the sweep to report them: a backup that vanishes silently is
// indistinguishable from one that was never taken, which is exactly the doubt
// an archive exists to remove.
//
// A container that cannot be removed is skipped rather than failing the sweep.
// This runs after a successful export, and an export that reported failure
// because it could not tidy up afterwards would be a lie about the thing the
// operator actually asked for.
func sweepArchives(dir string, p RetentionPolicy, nowMs int64) []string {
	names := sweepable(readArchiveFiles(dir), p, nowMs)
	removed := make([]string, 0, len(names))
	for _, n := range names {
		if err := os.Remove(filepath.Join(dir, n)); err == nil {
			removed = append(removed, n)
		}
	}
	return removed
}
