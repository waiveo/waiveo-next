package store

import (
	"database/sql"
	"fmt"
)

// The platform schema epoch (workspace.go PlatformSchemaEpoch) is recorded in
// the SQLite file itself as its `user_version`. Keeping it in the file — rather
// than in a row a query would have to open the schema to read — is what lets
// Open decide the ARC-041/104 refusal from the file alone, before running a
// single migration against a shape a newer build may have written.

// EpochTooNewError is returned by Open when the on-disk workspace records a
// platform schema epoch newer than this build understands. Per archive/1
// ARC-041/104 the workspace MUST NOT be opened — "exactly as the destination
// would refuse to open a live workspace at that epoch" — and this build's
// migrations MUST NOT run over it. The feeder boot maps this to the destination
// entering maintenance mode rather than crash-looping; the error carries both
// epochs so that surface can report which build is behind.
type EpochTooNewError struct {
	OnDisk     int // the epoch the file was written at
	Understood int // the highest epoch this build can open (PlatformSchemaEpoch)
}

func (e *EpochTooNewError) Error() string {
	return fmt.Sprintf("store: workspace platform schema epoch %d is newer than this build understands (%d); "+
		"refusing to open it (archive/1 ARC-041/104)", e.OnDisk, e.Understood)
}

// readSchemaEpoch reads the file's recorded platform schema epoch. A file that
// predates the marker reads 0 (SQLite's default user_version), which is treated
// as "older than epoch 1" and upgraded on open rather than refused — an
// existing box must not be bricked by the marker's introduction.
func readSchemaEpoch(db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read schema epoch: %w", err)
	}
	return v, nil
}

// writeSchemaEpoch stamps the file with an epoch. user_version takes an integer
// literal (it cannot be parameterized), so epoch is rendered directly — it is an
// in-process constant (PlatformSchemaEpoch), never caller input.
func writeSchemaEpoch(db *sql.DB, epoch int) error {
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", epoch)); err != nil {
		return fmt.Errorf("store: stamp schema epoch %d: %w", epoch, err)
	}
	return nil
}
