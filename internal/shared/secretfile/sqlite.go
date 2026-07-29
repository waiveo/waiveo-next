package secretfile

import (
	"fmt"
	"os"
)

// SQLite writes two sidecars beside a database in WAL mode: `<db>-wal` and
// `<db>-shm`. TightenSQLiteSidecars brings them to the same mode as the database
// itself.
//
// It exists because the pure-Go driver this tree uses creates those sidecars at
// the process umask — 0644 in practice — while the database is deliberately
// 0600. The `-wal` file holds recently-written page images, which is to say the
// same credential, session and grant rows the 0600 mode on the database is
// protecting. A reader who cannot open the database can read what was just
// written to it.
//
// The exposure is contained today by the enclosing directory being 0700, so this
// is defence in depth rather than a live hole — but it is the kind of containment
// that evaporates quietly: a backup or copy tool that preserves file modes and
// not directory modes carries the sidecars out at 0644, and nothing would
// announce it.
//
// Call it after a write has forced the WAL into existence (opening alone does
// not create it). A sidecar that is absent is not an error: WAL and SHM are
// removed on a clean close, and this runs again on the next open.
func TightenSQLiteSidecars(dbPath string, mode os.FileMode) error {
	for _, suffix := range []string{"-wal", "-shm"} {
		path := dbPath + suffix
		if err := os.Chmod(path, mode); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("tighten %s: %w", path, err)
		}
	}
	return nil
}
