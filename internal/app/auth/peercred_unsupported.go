//go:build !linux && !darwin

package auth

import (
	"errors"
	"net"
)

// readPeerUID has no implementation on this platform, and says so rather than
// returning a plausible value.
//
// SEC-072 admits a peer only after verifying its effective uid. A build that
// cannot verify one has exactly two honest options — refuse every connection, or
// refuse to start — and this returns the error that produces the first. What it
// must never do is return 0: a stub that "defaults to root" would make every
// connection on an unsupported platform an admitted one, which is the single
// worst failure this file could have.
func readPeerUID(*net.UnixConn) (int, error) {
	return -1, errors.New("auth: this platform has no peer-credential mechanism; the console binding cannot verify SEC-072's uid-0 admission and admits nobody")
}
