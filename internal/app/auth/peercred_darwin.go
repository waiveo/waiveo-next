//go:build darwin

package auth

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// readPeerUID is SEC-072's peer-credential read on Darwin, through
// LOCAL_PEERCRED — which the requirement itself allows for ("via SO_PEERCRED, or
// the platform's equivalent peer-credential mechanism").
//
// The mechanism differs from Linux's in name and struct, not in the property it
// establishes: getsockopt(SOL_LOCAL, LOCAL_PEERCRED) returns an `xucred` the
// kernel filled in from the peer process AT CONNECT TIME, so it is a kernel fact
// about the connecting process rather than anything the peer asserts.
//
// This file exists because the deployment target is Linux and the development
// machine is Darwin. Building only the Linux half would leave the whole socket
// path — bind, permission, accept, admit, dispatch, encode — untestable
// anywhere it is actually being written, which is how a listener ships having
// never run.
func readPeerUID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, fmt.Errorf("auth: reach the console peer's socket: %w", err)
	}
	var (
		cred    *unix.Xucred
		sockErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return -1, fmt.Errorf("auth: read the console peer's credential: %w", err)
	}
	if sockErr != nil {
		return -1, fmt.Errorf("auth: LOCAL_PEERCRED: %w", sockErr)
	}
	return int(cred.Uid), nil
}
