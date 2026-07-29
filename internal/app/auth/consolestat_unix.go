//go:build unix

package auth

import (
	"io/fs"
	"syscall"
)

// fileOwnerUID reports the owning uid of a stat result.
//
// It is split per platform because os.FileInfo.Sys() is an untyped escape hatch:
// on unix it is a *syscall.Stat_t, and elsewhere it is something else entirely.
// The boolean is what a caller must branch on — an ownership check that cannot
// be performed reports that it could not, rather than reporting uid 0.
func fileOwnerUID(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
