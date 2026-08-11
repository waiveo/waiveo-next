//go:build unix

package diskspace

import "syscall"

// usage reads the filesystem `path` lives on, via statfs(2).
//
// Bavail — blocks available to an UNPRIVILEGED process — is used deliberately
// in preference to Bfree; see Usage.AvailBytes for why the difference matters on
// an appliance that does not run the feeder as root.
//
// The products are computed in uint64 and converted once, because Bsize's type
// differs between linux (int64) and darwin (int32): a conversion done in the
// wrong order can overflow and report a nearly-full disk as enormous, which is
// the one wrong answer this whole package exists to prevent.
func usage(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{}, err
	}
	bsize := uint64(st.Bsize)
	return Usage{
		TotalBytes: int64(uint64(st.Blocks) * bsize),
		AvailBytes: int64(uint64(st.Bavail) * bsize),
	}, nil
}
