package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
)

// ChangeWitness answers ONE question about a store file: has any other
// connection committed to it since this witness was opened?
//
// # Why a file stat cannot answer it
//
// `waiveo-feeder -store-check` is a torn read by construction and advertised for
// exactly that use — the flag's help says it is safe against a live store, and
// the first-photon runbook says to run it with the old build still up. Each of
// its sections is its own unsnapshotted query set (the schema pass opens and
// closes a handle of its own; the rest run on a second), so the report can
// describe several different states of one file. That is legitimate. Not being
// able to TELL the reader is not.
//
// The first attempt at telling them compared `os.Stat` of the database file
// before and after the report. That watches the one file a WAL-mode writer does
// not touch. Measured: hold a live handle open, commit 200 rows through it, and
// the `.db` is byte-identical at nanosecond mtime resolution — every commit is in
// the `-wal` — while a read-only witness alongside it reads all 200 new rows. The
// guard was therefore silent for precisely the live-store invocation it was
// written for, and fired only on a checkpoint or a non-WAL write, which is the
// case that did not need it.
//
// # What answers it
//
// `PRAGMA data_version`. SQLite maintains it per connection and changes it when
// ANOTHER connection commits — which is this question verbatim — and it is
// documented to behave the same in WAL mode as in any other. It has to be read
// on one connection throughout, so this type holds its own single-connection
// read-only handle for the life of the report rather than borrowing one of the
// sections'.
//
// The file's size and mtime are kept as a SECOND signal rather than replaced: a
// writer that checkpoints, a rollback-journal store, and an out-of-WAL write all
// move the file, and a witness that only watched data_version would answer for a
// narrower question than the report needs. Either signal moving is a moved store.
//
// It opens the database `mode=ro`, so it cannot write to it; like every other
// reader of a WAL database it may leave the `-shm`/`-wal` sidecars beside it (see
// OpenReadOnly's caveat). It deliberately does NOT apply the schema-epoch gate:
// "did this file change" is answerable about a store written by a build this one
// cannot otherwise interpret, and the report wants the answer in that case too.
type ChangeWitness struct {
	path string
	db   *sql.DB

	dataVersion int64
	size        int64
	modTimeNs   int64
	statOK      bool
}

// OpenChangeWitness starts watching path. The returned witness must be closed.
//
// It fails only if the store cannot be opened for reading at all; a stat that
// fails is carried rather than raised, because the data_version half still
// answers and half an answer beats none in a diagnostic.
func OpenChangeWitness(path string) (*ChangeWitness, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("store: watch %s for concurrent writes: %w", path, err)
	}
	// PRAGMA data_version is a property of ONE connection. A pool free to hand
	// out a second would compare two different connections' counters, which is
	// not a comparison of anything.
	db.SetMaxOpenConns(1)

	w := &ChangeWitness{path: path, db: db}
	if w.dataVersion, err = w.readDataVersion(); err != nil {
		_ = db.Close()
		return nil, err
	}
	w.size, w.modTimeNs, w.statOK = statSignal(path)
	return w, nil
}

// Moved reports whether anything has committed to the store, or otherwise moved
// its file, since the witness was opened. why is a short phrase naming which
// signal moved, empty when nothing did.
//
// A failure to re-read is reported as an error, never as "nothing moved": this
// exists to keep a report from silently claiming a quiescent store.
func (w *ChangeWitness) Moved() (moved bool, why string, err error) {
	now, err := w.readDataVersion()
	if err != nil {
		return false, "", err
	}
	size, modTimeNs, ok := statSignal(w.path)
	switch {
	case now != w.dataVersion:
		return true, "another connection committed to it", nil
	case w.statOK && ok && (size != w.size || modTimeNs != w.modTimeNs):
		return true, fmt.Sprintf("its file changed (%d byte(s) then, %d now)", w.size, size), nil
	default:
		return false, "", nil
	}
}

// Close releases the witness's handle.
func (w *ChangeWitness) Close() error {
	if w == nil || w.db == nil {
		return nil
	}
	return w.db.Close()
}

func (w *ChangeWitness) readDataVersion() (int64, error) {
	var v int64
	if err := w.db.QueryRowContext(context.Background(), `PRAGMA data_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read %s's data_version: %w", w.path, err)
	}
	return v, nil
}

// statSignal reads the file's size and mtime, reporting whether it could.
func statSignal(path string) (size, modTimeNs int64, ok bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	return info.Size(), info.ModTime().UnixNano(), true
}
