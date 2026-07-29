//go:build linux

package auth

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// readPeerUID is SEC-072's peer-credential read on Linux: getsockopt(2) with
// SO_PEERCRED, which returns the connecting process's pid/uid/gid as recorded by
// the kernel AT CONNECT TIME.
//
// That timing is the property worth stating. The credential is a kernel fact
// about the peer at the moment it connected, not something the peer sends and
// not something it can change afterwards by execing a setuid binary — which is
// why SEC-071 can call the filesystem mode and this check "a defense-in-depth
// pair" rather than two spellings of the same trust.
//
// It is read through SyscallConn().Control, so the descriptor stays owned by the
// net package for the duration and cannot be closed underneath the getsockopt.
func readPeerUID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, fmt.Errorf("auth: reach the console peer's socket: %w", err)
	}
	var (
		cred    *unix.Ucred
		sockErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return -1, fmt.Errorf("auth: read the console peer's credential: %w", err)
	}
	if sockErr != nil {
		return -1, fmt.Errorf("auth: SO_PEERCRED: %w", sockErr)
	}
	return int(cred.Uid), nil
}
