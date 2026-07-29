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
// first-boot setup window (SEC-120) remains open on a box that has never been
// claimed. That is a property of a fresh checkout and not a standing guarantee —
// a tree that has run the web e2e suite already holds a real owner, since the
// suite claims the workspace and `make dev-down` does not wipe the auth state.
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
	"strings"
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
	// The feeder reads WAIVEO_FEEDER_AUTH_DIR for exactly this directory. Without
	// honouring it here, `make dev` provisions into one store while the feeder
	// authenticates against another: every probe 401s, and the refusal tells you
	// to run the command you just ran. It also leaves an orphan store holding a
	// live admin principal.
	def := auth.DefaultAuthDir
	if v := strings.TrimSpace(os.Getenv("WAIVEO_FEEDER_AUTH_DIR")); v != "" {
		def = v
	}
	dir := flag.String("auth-dir", def,
		"the app's auth state directory — the one the feeder opens its credential store in (defaults to $WAIVEO_FEEDER_AUTH_DIR when set)")
	flag.Parse()

	if err := refuseOutsideRepo(*dir); err != nil {
		fmt.Fprintf(os.Stderr, "DEV KEY FAIL: %v\n", err)
		os.Exit(1)
	}
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

// refuseOutsideRepo confines this tool to an auth directory under the repository
// it is run from.
//
// It opens the credential store DIRECTLY, so it answers to no authorization
// surface by construction — that is why it can provision a key on a box with no
// owner and no way to log in, and it is also why an un-confined `-auth-dir` is a
// one-command path to minting an admin credential on a REAL deployment. The
// capability argument ("root could write auth.db with sqlite3 anyway") is true
// and beside the point: the difference between a capability and a convenience is
// how likely someone is to use it by accident, and `-auth-dir /var/lib/waiveo`
// is one flag away from a paste that looks like a dev command.
//
// The confinement is the repository root, resolved through symlinks on both
// sides so neither a link nor a `..` walks out of it.
func refuseOutsideRepo(dir string) error {
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve the working directory: %w", err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return fmt.Errorf("resolve the working directory: %w", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", dir, err)
	}
	// The directory may not exist yet, so resolve the nearest existing ancestor
	// rather than requiring the leaf.
	probe := abs
	for {
		if resolved, err := filepath.EvalSymlinks(probe); err == nil {
			abs = filepath.Join(resolved, strings.TrimPrefix(abs, probe))
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("-auth-dir %s is outside the repository at %s — this tool writes an admin credential straight into a credential store, so it is confined to a development checkout; provision a real deployment through its own setup flow", dir, root)
	}
	return nil
}
