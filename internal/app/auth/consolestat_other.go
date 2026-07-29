//go:build !unix

package auth

import "io/fs"

// fileOwnerUID has no answer on a platform with no unix file ownership. It
// reports that, and the caller skips the ownership half of the directory gate —
// which is correct, since a platform with no uid also has no SEC-072 admission
// rule to gate (see readPeerUID's own unsupported-platform build).
func fileOwnerUID(fs.FileInfo) (int, bool) { return 0, false }
