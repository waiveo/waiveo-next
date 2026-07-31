package vitals

import "golang.org/x/sys/unix"

// freeBytes reports the bytes available to an unprivileged writer on the
// filesystem holding path.
//
// Bavail, not Bfree: Bfree counts blocks the kernel reserves for root, which the
// relay is not writing as. Reporting those as headroom would tell an operator
// they have space the relay cannot actually use — the one number this field
// exists to make actionable.
func freeBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
