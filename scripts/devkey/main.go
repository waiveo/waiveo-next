// Command devkey provisions the local dev API key the repo's HTTP probes present
// to a running feeder. It is `make dev-key`, and `make dev-up` runs it too.
//
// It opens the app's auth store DIRECTLY — the same SQLite file the feeder opens
// — rather than going through HTTP, because there is no HTTP route that mints an
// api-key: the surface offers login, first-boot claim, and credential-reset
// redemption, and none of the three produces the bearer credential a
// non-browser client needs. Direct access is also what makes this work in both
// states a dev box is ever in: an unclaimed box (no owner exists yet, so nothing
// can log in) and a claimed one (the setup code is gone, so nothing can claim).
//
// It grants nothing a developer with write access to that file could not already
// write by hand — the reasoning SEC-076 applies to the console binding's verbs —
// and it deliberately does NOT claim the box: the principal it creates is bound
// `admin`, never `owner`, so the deployment's owner count stays 0 and the real
// first-boot setup window (SEC-120) remains open.
//
// The full argument for the authority it grants, the on-disk discipline, and
// what happens when the key is missing lives in ONE place: scripts/devcred's
// package doc.
//
// Ordering: `make dev-up` runs this BEFORE it starts the feeder, so the two
// never write the auth database at the same time. Run by hand against a live
// stack it still works — the store is WAL with a busy timeout — but the feeder
// will not notice a revoked key until its next lookup, which is per-request.
//
// It prints where it wrote and what the key authorizes. It never prints the key.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/scripts/devcred"
)

// authStoreFile mirrors cmd/waiveo-feeder's own unexported `authStoreFile`. The
// feeder owns that name; this is a dev tool reading the file the feeder writes,
// so the literal is repeated here rather than exported from the auth package for
// a tool's convenience.
const authStoreFile = "auth.db"

func main() {
	dir := flag.String("auth-dir", auth.DefaultAuthDir,
		"the app's auth state directory — the one the feeder opens its credential store in")
	flag.Parse()

	if err := run(*dir); err != nil {
		fmt.Fprintf(os.Stderr, "DEV KEY FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	ctx := context.Background()

	// The store's clock is the app's persisted monotonic floor, not the bare
	// host clock — the same wiring the feeder uses, and for the reason
	// auth.Open's own doc gives: a credential store that timestamps rows from a
	// clock the rest of the deployment does not share is a store whose
	// time-windowed checks can disagree with everyone else's. Opening the floor
	// only READS it; nothing here advances or persists it.
	floor, err := auth.OpenClockFloor(dir, func() int64 { return time.Now().UnixMilli() })
	if err != nil {
		return fmt.Errorf("open clock floor in %s: %w", dir, err)
	}
	store, err := auth.Open(filepath.Join(dir, authStoreFile), floor.Now, ulid.New, auth.WithClockFloor(floor))
	if err != nil {
		return fmt.Errorf("open auth store in %s: %w", dir, err)
	}
	defer store.Close()

	// A key already on disk is an input, not a precondition: an unreadable,
	// absent, or wrongly-permissioned file simply means there is nothing to
	// re-confirm and a fresh key gets minted. devcred.Load's refusal is for a
	// PROBE, which cannot proceed without one; this tool exists to fix exactly
	// that state.
	existing, _ := devcred.Load()

	key, err := store.EnsureDevScriptKey(ctx, existing)
	if err != nil {
		return err
	}

	path, err := devcred.Write(key.Token)
	if err != nil {
		return err
	}

	disposition := "minted credential " + key.Label + ", revoking any prior one"
	if key.Reused {
		disposition = "existing key still live, left as it was"
	}
	fmt.Printf("DEV KEY OK (%s; principal %s %q, %s at %s; %s)\n",
		path, key.PrincipalID, auth.DevScriptPrincipalName, key.Role, key.ScopeNode, disposition)
	return nil
}
