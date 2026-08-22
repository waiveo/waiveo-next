// Command storebackup takes a consistent backup of a feeder store.
//
// It exists because the deploy rule requires one on every deploy that can write
// to the store, and nothing provided it: the box has no sqlite3 binary and the
// feeder's only store flag is -store-check. Twenty-odd deploys were made with
// the rule on the books and no tool to satisfy it.
//
// VACUUM INTO from a mode=ro connection, never a cp of a live database: a copy
// taken while the writer holds a WAL is a torn file that restores as corruption,
// and the failure is invisible until the day it is needed.
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: storebackup <source.db> <dest.db>")
		os.Exit(2)
	}
	src, dst := os.Args[1], os.Args[2]
	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro")
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", src, err)
		os.Exit(1)
	}
	defer db.Close()
	if _, err := db.Exec("VACUUM INTO ?", dst); err != nil {
		fmt.Fprintf(os.Stderr, "VACUUM INTO %s: %v\n", dst, err)
		os.Exit(1)
	}
	var out string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&out); err != nil {
		fmt.Fprintf(os.Stderr, "integrity_check: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("backed up %s -> %s (source integrity_check: %s)\n", src, dst, out)
}
