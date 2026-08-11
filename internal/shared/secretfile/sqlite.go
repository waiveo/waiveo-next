package secretfile

import (
	"fmt"
	"os"
)

// The suffixes SQLite derives its companion files' names with (database path
// plus suffix — not configurable), declared ONCE for the whole tree.
//
// # Why they live here and not beside each consumer
//
// They were written out by hand in three places, and the third one is why this
// block exists. internal/app/restoreswap kept a list because it MOVES store
// files; this file kept a separate `{-wal, -shm}` because it CHMODs them. When
// `-journal` turned out to be missing from the mover's list — a `VACUUM INTO`
// snapshot is a rollback-mode database, so a `-journal` really does appear
// beside a live store — one list was fixed and this one was not. A fact about
// SQLite that every consumer restates is a fact that gets corrected in one
// consumer at a time.
//
// So this is the list. A package that touches SQLite's companion files reads it
// from here, and adding a fourth is one edit.
const (
	// SQLiteWALSuffix names the write-ahead log: COMMITTED TRANSACTIONS not yet
	// folded into the database file (WAL mode).
	SQLiteWALSuffix = "-wal"
	// SQLiteSHMSuffix names the shared-memory index over the WAL. Rebuildable,
	// and the only genuinely disposable companion.
	SQLiteSHMSuffix = "-shm"
	// SQLiteJournalSuffix names the ROLLBACK journal, which holds the ORIGINAL
	// page images of a database being written in rollback mode. Its presence is
	// how SQLite knows a database was interrupted mid-transaction.
	SQLiteJournalSuffix = "-journal"
)

// SQLiteSidecarSuffixes is every companion SQLite can leave beside a database,
// in both journal modes. An array rather than a slice so a caller cannot mutate
// the tree's one copy of it.
var SQLiteSidecarSuffixes = [...]string{SQLiteWALSuffix, SQLiteSHMSuffix, SQLiteJournalSuffix}

// TightenSQLiteSidecars brings every companion file beside a database to the
// same mode as the database itself.
//
// It exists because the pure-Go driver this tree uses creates those companions
// at the process umask — 0644 in practice — while the database is deliberately
// 0600. The `-wal` file holds recently-written page images, which is to say the
// same credential, session and grant rows the 0600 mode on the database is
// protecting. A reader who cannot open the database can read what was just
// written to it.
//
// `-journal` is on the list for the mirror-image reason, and it was missing:
// a rollback journal holds the ORIGINAL page images of the rows being
// overwritten. That is the same secret material pointed backwards in time, and
// it is not hypothetical for this tree — a restored store arrives as `VACUUM
// INTO` output, which is a ROLLBACK-mode database, so the first boot after a
// restore writes a `<db>-journal` at the umask before SQLite converts the file
// to WAL.
//
// The exposure is contained today by the enclosing directory being 0700, so this
// is defence in depth rather than a live hole — but it is the kind of containment
// that evaporates quietly: a backup or copy tool that preserves file modes and
// not directory modes carries the companions out at 0644, and nothing would
// announce it.
//
// Call it after a write has forced the companions into existence (opening alone
// does not create them). An absent one is not an error: WAL and SHM are removed
// on a clean close, a journal is removed on commit, and this runs again on the
// next open.
func TightenSQLiteSidecars(dbPath string, mode os.FileMode) error {
	for _, suffix := range SQLiteSidecarSuffixes {
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
