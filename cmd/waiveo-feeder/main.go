// Command waiveo-feeder is the Wave-1 first-photon feeder: the relay/1
// server role. It signs one desired-state generation with a persistent
// make-dev identity, serves that generation's image directly by content
// hash, and exposes loopback enrollment so a co-located relay can obtain
// its certificate and learn the feeder's own desired-state signing key —
// the trust anchor it then verifies every pulled snapshot against
// (relay/1 REL-012/071, `#28` enrollment-anchored trust).
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/maaxton/waiveo-next/internal/app/restoreswap"
	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/eventingest"
	"github.com/maaxton/waiveo-next/internal/app/eventsse"
	"github.com/maaxton/waiveo-next/internal/app/packs"
	"github.com/maaxton/waiveo-next/internal/app/screens"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/app/webhookdeliver"
	"github.com/maaxton/waiveo-next/internal/app/webui"
	"github.com/maaxton/waiveo-next/internal/app/workspacekey"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/feeder/enroll"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/packsig"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// retentionSweepInterval is how often the feeder retires state that has outlived
// its use: the events/1 retention policy over the persisted event log (deleting
// rows past their class's window, trimming a class past its row cap), the pending
// second-factor enrollments past their ttl (SEC-004,
// auth.PendingTOTPEnrollmentTTLMs), and the content assets no live desired-state
// generation references any more (internal/feeder/contentgc).
//
// It is a SWEEP cadence, not a guarantee, and that is why one cadence can serve
// three windows of very different sizes. For the event log the guarantee is the
// policy's per-class window, which is a floor — "retained for a bounded window"
// (EVT-010) — so a row that outlives it by up to one sweep is still inside the
// guarantee, and the shortest window the policy configures is measured in days.
// For a pending enrollment the guarantee is not the sweep at all: an expired
// enrollment stops arming anything the instant it expires, checked against the
// clock on every read and inside the arming transaction, so the sweep only keeps
// the abandoned rows from piling up. For content there is no window to miss in
// the first place: nothing promises an unreferenced asset is gone by any
// deadline, and every one of that sweep's guards fails toward keeping the bytes.
// None of the three is weakened by being coarse.
//
// The content arm has NO boot counterpart, unlike the other two. Their boot
// sweeps exist because a box that was off for a month comes back holding rows
// that expired while it slept. Content has no such backlog: reclaiming needs the
// fleet to be connected and converged (nothing is, at boot) and needs an asset
// observed unreferenced by an EARLIER sweep (there is none, at boot), so a
// boot-time content sweep could only ever reclaim nothing.
const retentionSweepInterval = time.Hour

// firstPhotonSite is the app peer's authoritative site_binding for Wave-1
// first-photon (relay/1 REL-036): the site a relay is bound to, and that
// site's effective timezone and coordinates, reported as canonical in every
// hello-ack so the relay adopts it into its edge engine's schedule/sun
// evaluation. A real IANA zone, so a relay can feed it straight into
// rules/1's engine.SetLocation. The persisted per-site record this stands in
// for lands with the data-model site source in a later wave.
var firstPhotonSite = hello.SiteBinding{
	ScopeNode: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
	TZ:        "America/Chicago",
	Lat:       41.8781,
	Long:      -87.6298,
}

// firstPhotonRecognizedFeatures are the relay/1 capability flags this app peer
// understands, for the hello-ack shared-feature subset (REL-035); a relay flag
// outside this set is dropped from the subset silently, never a refusal.
var firstPhotonRecognizedFeatures = []string{"telemetry.latest_only_v1"}

// config is the feeder's deployment-time addressing. Defaults keep the Wave-1
// loopback dev/CI behavior byte-identical; the on-box deployment overrides them
// so the content URL a screen fetches direct (contentBaseURL, baked into the
// signed snapshot -> lease) resolves to a LAN-reachable host rather than the
// box's own loopback.
type config struct {
	listen         string // TCP bind address for the HTTPS listener
	contentBaseURL string // scheme+host the direct-fetch content URL is built from
	storePath      string // SQLite file the app store persists scope-nodes + scheduling rows to
	authDir        string // directory the security-model/1 auth state (database + setup code) lives in
	contentPath    string // directory the content origin persists uploaded asset bytes to
	archiveDir     string // directory the data-subject export writes archive/1 containers into
	keyDir         string // directory the workspace signing key + data key persist in — NEVER the archive dir
	demoCast       string // "" (default, single first-photon image) or "multi" (the real 3-item demo cast)
	packTrustPath  string // trust-anchors document pack-artifact signatures verify against (marketplace/1 MKT-009b)
	// requiredPacksPath is the required-pack roster document this deployment
	// declares its floors in (marketplace/1 MKT-093a). Absent ⇒ no pack is
	// required; unreadable/malformed ⇒ the feeder refuses to start.
	requiredPacksPath string
}

// demoCastModeMulti is loadConfig's WAIVEO_FEEDER_DEMO_CAST value that swaps
// the single first-photon image for snapshot.DemoCastItems' real 3-item
// ordered demo cast. Any other value (including unset/"") keeps today's exact
// single-image behavior — unset is the make-dev/CI default, so neither is
// affected by this flag's existence.
//
// The cast is seeded as the demo PLAYLIST's items, in order (store.SeedDemo),
// which is the only way content reaches a screen: every screen_programs entry
// is resolved from the playlist the screen's effective daypart selects
// (snapshot.DeriveScreenPrograms). A multi-item cast therefore rides the real
// playlist/daypart engine end to end rather than a parallel direct path — the
// caveat this doc used to carry, that a governing schedule superseded the flag,
// no longer applies because there is no longer a second path for it to lose to.
//
// Because it is seeded, the flag takes effect only on an EMPTY store (SeedDemo
// runs when the generation is still 0). A store that already has an authored
// playlist keeps it; edit that playlist instead.
const demoCastModeMulti = "multi"

// defaultStorePath is the make-dev-local SQLite file the feeder's app store lives
// in (git-ignored under .dev/, alongside the signing keys). store.Open creates
// the parent dir if absent, so no separate mkdir is needed.
const defaultStorePath = ".dev/feeder-store.db"

// defaultContentPath is the make-dev-local directory the content origin persists
// uploaded asset bytes into (git-ignored under .dev/, a sibling of the store DB).
// It MUST persist alongside storePath: the playlists in the store reference
// content by asset_ref, so if the bytes did not survive a restart the store's
// resolved content urls would 404 and re-authoring would be spuriously rejected.
// origin.Open creates the dir if absent, so no separate mkdir is needed.
const defaultContentPath = ".dev/feeder-content"

// defaultEnrollDir is the make-dev-local directory the enrollment server's
// registry (internal/feeder/enroll.Server.EnablePersistence) persists into —
// git-ignored under .dev/, a sibling of the signing keys and the store DB. It
// MUST persist alongside signing.DefaultDir: both together are what let an
// already-enrolled relay's hello keep verifying (REL-032) across a feeder
// restart, rather than the app peer forgetting the relay ever enrolled.
const defaultEnrollDir = ".dev/feeder-enroll"

// defaultAuthDir is the make-dev-local directory the security-model/1 auth state
// lives in: the principal/credential/session/role/grant database and the
// first-boot setup code. Git-ignored under .dev/, a sibling of the signing keys.
//
// It is a SEPARATE directory (and a separate SQLite file) from the authoring
// store deliberately. Every write to the authoring store bumps the desired-state
// generation and nudges every live relay connection to re-pull (relay/1
// REL-057); a login has no business advancing desired state, and folding the two
// together would make every session issuance look like an authored change.
//
// The value comes from the auth package rather than being spelled here, because
// the console binding's socket (SEC-070) lives in this same directory and a
// second process looking for it must resolve the same path this one serves it
// from.
const defaultAuthDir = auth.DefaultAuthDir

// authStoreFile is the auth database's filename inside defaultAuthDir.
const authStoreFile = "auth.db"

// defaultArchiveDir is the make-dev-local directory the data-subject export
// operation (api/1 API-120/121) writes its archive/1 containers into.
//
// It is git-ignored under .dev/. A container is an ENCRYPTED export of the whole
// workspace, so it does not belong anywhere a repo or a backup sweep would pick
// it up by accident — but it IS the artifact an operator is meant to take away,
// copy, and hand to someone. See defaultWorkspaceKeyDir for what that makes
// impossible to keep here.
const defaultArchiveDir = ".dev/feeder-archive"

// defaultWorkspaceKeyDir is the make-dev-local directory the workspace signing
// key (security-model.md SEC-046) and the per-workspace data key (SEC-040)
// persist in.
//
// It is a SEPARATE directory from defaultArchiveDir, and the separation is the
// requirement rather than tidiness. SEC-047 places the signing key's private
// half in "the same root-owned keyfile as the box key (SEC-041)" — key custody,
// deliberately apart from workspace content. An export directory is the opposite
// of that by design: its whole purpose is to hold files an operator copies off
// the box, mails to a data subject, or sweeps into a backup. Key material and
// export output in one directory means every one of those actions silently
// carries the private key that signs the exports and the cleartext data key that
// wraps the workspace's secrets — and the archive's encryption assumes the
// attacker does NOT have them.
//
// The directory is 0700 and every file in it 0600, enforced on every boot by
// workspacekey.LoadOrCreate rather than assumed from the mode it was created
// with.
const defaultWorkspaceKeyDir = ".dev/feeder-keys"

// defaultPackTrustPath is the make-dev-local trust-anchors document the pack
// install pipeline verifies artifact signature envelopes against — the
// host-provisioned local anchor set the contract requires while the external,
// root-signed software-artifact trust root is still a pending owner ceremony
// (marketplace/1 MKT-009b; the root-signed publisher-namespace delegation
// replaces this file behind the same packsig.TrustAnchors seam when it lands).
// The dev publisher tooling (scripts/examplepack, scripts/packsmoke) provisions
// it; it carries PUBLIC key material only. The file is read per verification,
// so provisioning after boot takes effect with no restart — and an ABSENT file
// refuses every install rather than admitting unsigned packs (fail closed).
const defaultPackTrustPath = ".dev/pack-trust/anchors.json"

// defaultRequiredPacksPath is the make-dev-local required-pack roster document
// (marketplace/1 MKT-093a): the deployment's own declaration of which packs it
// cannot run without, and the floor version each may not be uninstalled or
// regressed below.
//
// It is git-ignored under .dev/ and, unlike every other .dev path here, it is
// NOT created by anything. That is deliberate: MKT-093a's default is that an
// absent roster makes no pack required, so a dev checkout, a CI run, and a box
// nobody has provisioned a roster for all behave exactly as they did before this
// file existed. A deployment opts IN by authoring the document.
//
// It is a SEPARATE file from the trust anchors, not another section of them. The
// anchors are a trust root — who may sign a pack — and the roster is a policy —
// which packs this deployment refuses to be without. They fail in opposite
// directions (an empty anchor set refuses everything, an empty roster requires
// nothing), they are provisioned by different parties, and folding them together
// would mean an operator editing deployment policy inside the file that decides
// who is trusted.
const defaultRequiredPacksPath = ".dev/feeder-required-packs.json"

// absHostFilePath pins a host-provisioned file's path to an absolute one, ONCE,
// at config load. Used for both the trust anchors and the required-pack roster.
//
// The anchors file is read per verification so that provisioning and
// revocation take effect without a restart. Resolved per read, a relative path
// would mean the trust root follows the process's working directory — so the
// same deployment launched from a different cwd would silently consult a
// different (possibly attacker-planted) trust root, and "fail closed when
// absent" would not save it, because a substituted file is present. Resolving
// once at boot makes the trust root a fixed location for the process's life
// while keeping the re-read behavior.
//
// The roster is read once rather than per call, so the substitution window is
// narrower — but the failure is worse, because a roster that resolves to
// nothing lifts every floor instead of refusing every install. Pinning it costs
// the same one call, and leaves no path on which "which file is the roster"
// depends on where the process was launched from.
func absHostFilePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// loadConfig reads the feeder config from env (via `env`, os.Getenv in main),
// falling back to the loopback defaults. contentBaseURL defaults to the listen
// address so an unconfigured feeder behaves exactly as before.
func loadConfig(env func(string) string) config {
	listen := envOr(env, "WAIVEO_FEEDER_LISTEN", "127.0.0.1:7420")
	return config{
		listen:         listen,
		contentBaseURL: envOr(env, "WAIVEO_FEEDER_CONTENT_URL", "https://"+listen),
		storePath:      envOr(env, "WAIVEO_FEEDER_STORE", defaultStorePath),
		authDir:        envOr(env, "WAIVEO_FEEDER_AUTH_DIR", defaultAuthDir),
		contentPath:    envOr(env, "WAIVEO_FEEDER_CONTENT_DIR", defaultContentPath),
		archiveDir:     envOr(env, "WAIVEO_FEEDER_ARCHIVE_DIR", defaultArchiveDir),
		keyDir:         envOr(env, "WAIVEO_FEEDER_KEY_DIR", defaultWorkspaceKeyDir),
		demoCast:       envOr(env, "WAIVEO_FEEDER_DEMO_CAST", ""),
		packTrustPath:  absHostFilePath(envOr(env, "WAIVEO_FEEDER_PACK_TRUST", defaultPackTrustPath)),

		requiredPacksPath: absHostFilePath(envOr(env, "WAIVEO_FEEDER_REQUIRED_PACKS", defaultRequiredPacksPath)),
	}
}

func envOr(env func(string) string, key, def string) string {
	if v := env(key); v != "" {
		return v
	}
	return def
}

// reportStoreIDs backs -store-check: it opens the store, reports every row id
// the next boot would rewrite, and writes nothing. It is what an operator runs
// against a box BEFORE restarting it onto a build that requires a canonical ULID
// for every row id, so the restart holds no surprises — an empty report means the
// store is already conforming, a listed one is exactly what the boot will change,
// and a refusal means the boot would decline to start and the store needs a look
// first.
//
// It also reports what the durable event log is holding, because the same store
// now carries it and the canonicalization pass reaches into it: a stored event's
// scope_node follows a renamed scope node, so an operator deciding whether to
// restart should be able to see how much audit history is in scope for that.
//
// The return value is the process exit code: 0 when the store is fine or merely
// needs a rewrite the boot can perform, 1 when it cannot be opened or the
// rewrite would be refused.
func reportStoreIDs(storePath string, out io.Writer) int {
	// The HOST clock deliberately, and the only place in this binary that is the
	// right answer. This subcommand stamps no ROW — it plans and reports, and the
	// migration planner takes a read lock and writes nothing — so there is no
	// stamp for a floor to keep consistent with anything, and reaching for the
	// floor would mean creating the auth-state directory it lives in as a side
	// effect of a check that runs before the server does.
	//
	// "Stamps no row" rather than "writes nothing", because the open itself is
	// not inert: against a path with no store, store.Open creates the directory
	// and file and runs every migration, so -store-check on a fresh path reports
	// a conforming store it just created. That is a surprising-but-harmless
	// property of the subcommand, not of this clock choice.
	st, err := store.Open(storePath, store.WallClockMs)
	if err != nil {
		fmt.Fprintf(out, "cannot open %s: %v\n", storePath, err)
		return 1
	}
	defer st.Close()

	m, err := st.PlanRowIDMigration(context.Background())
	if err != nil {
		fmt.Fprintf(out, "%s CANNOT be canonicalized: %v\n", storePath, err)
		fmt.Fprintf(out, "the feeder will refuse to start against this store; nothing has been changed.\n")
		return 1
	}
	if len(m.Rewrites) == 0 {
		fmt.Fprintf(out, "%s: every row id is already a canonical ULID; the next boot will not touch it.\n", storePath)
	} else {
		fmt.Fprintf(out, "%s: %d row id(s) will be canonicalized at the next boot:\n", storePath, len(m.Rewrites))
		for _, rw := range m.Rewrites {
			fmt.Fprintf(out, "  %-16s %s -> %s\n", rw.Kind, rw.From, rw.To)
		}
		fmt.Fprintf(out, "references to them are rewritten in the same transaction; nothing has been changed by this check.\n")
	}
	reportEventLog(st, out)
	return 0
}

// reportEventLog prints what the durable event log holds, by retention class. A
// failure to read it is reported and not fatal: this check exists to tell an
// operator what a restart will do to the ROW IDS, and it should still answer
// that question if the event tables cannot be read.
func reportEventLog(st *store.Store, out io.Writer) {
	log, err := st.EventLog(events.DefaultRetentionPolicy(), nil, func(error) {})
	if err != nil {
		fmt.Fprintf(out, "the durable event log could not be opened: %v\n", err)
		return
	}
	total, byClass, err := log.Count()
	if err != nil {
		fmt.Fprintf(out, "the durable event log could not be counted: %v\n", err)
		return
	}
	if total == 0 {
		fmt.Fprintf(out, "durable event log: empty.\n")
		return
	}
	fmt.Fprintf(out, "durable event log: %d event(s) retained", total)
	for _, class := range sortedClasses(byClass) {
		fmt.Fprintf(out, ", %s=%d", class, byClass[class])
	}
	fmt.Fprintf(out, "\n")
}

// sortedClasses orders a per-class count map so the report is stable run to run.
func sortedClasses(byClass map[string]int) []string {
	out := make([]string, 0, len(byClass))
	for c := range byClass {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func main() {
	// The one flag the feeder takes, checked before any state is opened, the
	// same shape the relay's -version uses. Everything else stays env-only.
	storeCheck := flag.Bool("store-check", false,
		"report any stored row id that is not a canonical ULID (and would be rewritten at the next boot), then exit without writing")
	flag.Parse()

	cfg := loadConfig(os.Getenv)

	if *storeCheck {
		os.Exit(reportStoreIDs(cfg.storePath, os.Stdout))
	}

	// The deployment's required-pack roster (marketplace/1 MKT-093a): which packs
	// this deployment declares it cannot run without, and the floor version each
	// may not be uninstalled or regressed below. It is handed to api.New below,
	// which installs it on the store, where MKT-093b's refusal runs inside the
	// install and uninstall transactions.
	//
	// It is resolved HERE — first, before the signing identity, the content
	// store, the app store or any other state is opened — for two reasons. It
	// needs nothing else, so nothing else should have been created by the time
	// this decides whether the process may run at all; and "refuses to start" is
	// only honest if the refusal happens before the process has begun writing.
	//
	// READ ONCE, not per call, which is the opposite of the trust anchors beside
	// it. The anchors are re-read so a revocation takes effect without a restart:
	// their danger direction is a stale PERMISSION. The roster's danger direction
	// is the reverse — deleting or emptying it lifts every floor — so re-reading
	// would mean anyone who could touch the file could disarm the protection on a
	// running box with no restart. Reading once makes the roster fixed for the
	// process's life; changing it is a deliberate edit plus a restart, which is
	// what changing a deployment's policy should cost.
	//
	// AN UNRESOLVABLE ROSTER IS FATAL, and that is a choice worth defending in a
	// process that also serves relays, screens and content. MKT-093a permits
	// either refusing to start or refusing every pack removal while running, and
	// this takes the first:
	//
	//   - the second option only exists for a host that learns of unresolvability
	//     after boot, which reading once makes impossible here;
	//   - a box that runs on with its pack surface dead reports the failure as
	//     REQUIRED_PACK_FLOOR on packs that are not required, which reads as a
	//     policy refusal rather than as a broken file, so it can sit unnoticed
	//     indefinitely;
	//   - a deployment only reaches this branch if it AUTHORED a roster and
	//     authored it wrong. It has declared that it cannot run without certain
	//     packs; carrying on with those floors lifted runs the configuration the
	//     operator explicitly forbade. An unprovisioned host does not reach it at
	//     all — an absent roster resolves to "nothing required" and boots.
	//
	// It is the same posture the auth store, the workspace key, the clock floor
	// and the console binding above/below already take: a deployment-state
	// problem an operator must see does not get to be a warning.
	requiredPacks, err := packs.LoadRoster(cfg.requiredPacksPath)
	if err != nil {
		log.Fatalf("waiveo-feeder: %v\n"+
			"    an unresolvable roster is NOT an empty one: the deployment declared packs it cannot run without and this host could not learn them,\n"+
			"    so it refuses to start rather than serve with every required-pack floor silently lifted (marketplace/1 MKT-093a).\n"+
			"    Fix or remove %s. An ABSENT roster is valid and makes no pack required.", err, cfg.requiredPacksPath)
	}
	// Absent and authored-empty are both permissive and are NOT the same event to
	// an operator: a typo'd env var, an edited-out unit line, a wrong working
	// directory or a late-mounting filesystem all produce ABSENT, and because the
	// roster is read once that disables the feature for the process's life. A
	// report that prints one line for both cannot confirm the deployment loaded
	// the roster it meant to, which is the only thing the report is for.
	switch declared := requiredPacks.Declared(); {
	case packs.RosterAbsent(cfg.requiredPacksPath):
		log.Printf("waiveo-feeder: NO required-pack roster at %s — no pack is protected from uninstall or downgrade. If this deployment authored one, it is not being read (marketplace/1 MKT-093a)", cfg.requiredPacksPath)
	case len(declared) == 0:
		log.Printf("waiveo-feeder: required-pack roster %s declares no required pack — no pack is protected from uninstall or downgrade (marketplace/1 MKT-093a)", cfg.requiredPacksPath)
	default:
		log.Printf("waiveo-feeder: required-pack roster %s declares %d required pack(s): %v", cfg.requiredPacksPath, len(declared), declared)
	}

	id, err := signing.LoadOrCreate(signing.DefaultDir)
	if err != nil {
		log.Fatalf("waiveo-feeder: load identity: %v", err)
	}
	log.Printf("waiveo-feeder identity loaded (signing pub %s)", hex.EncodeToString(id.SigningPub()))

	// The content origin is dir-backed so operator-uploaded assets survive a
	// restart in lock-step with the persisted playlists that reference them
	// (cfg.contentPath, a sibling of the store DB). The placeholder is re-added
	// every boot — content-addressed, so re-adding it is a no-op on disk.
	// The app-tier persisted monotonic clock floor (security-model/1
	// SEC-066-068). It is opened HERE, ahead of every component in this process
	// that reads a clock, because nowMs — the floor's floor-aware reading — is
	// the app's ONE notion of current time for every component wired below it:
	// a process that stamped an audit record from one clock, a grant's expiry
	// from a second and a screen's schedule from a third would be three
	// deployments wearing one binary, and the difference between them would
	// only ever be visible on the day the host clock was wrong, which is the
	// day it matters.
	//
	// EVERY APP-TIER COMPONENT THIS PROCESS STAMPS A RECORD WITH NOW READS THIS
	// ONE CLOCK. The two that did not — internal/app/store, which hardcoded
	// time.Now with no injection point, and internal/app/eventingest, which
	// stamped an ingested envelope's ts the same way — now take it as a required
	// argument, so a store row's created_at, a durable event's ts, a grant's
	// expiry, an idempotency record's window and an audit event's timestamp all
	// come from the reading taken below. The old note said those two agreed with
	// the floor anyway; that was true only because the floor is 0, which is
	// precisely the condition wiring a time source removes.
	//
	// WHAT STILL READS THE HOST CLOCK DIRECTLY, named rather than left to be
	// discovered, because "one notion of time" is a claim that has to survive a
	// grep:
	//
	//   - internal/shared/ulid. Every id this process mints — a row id, an event
	//     id, a grant id — carries a 48-bit host-clock millisecond in its prefix.
	//     It is a SORT KEY, not a timestamp anything is evaluated against: no
	//     window, expiry or retention decision reads it. It is the one worth
	//     revisiting, since a floor established above the host clock would make
	//     a new id sort BELOW ids already stored, and EVT-011's recording order
	//     is an id ordering.
	//   - internal/feeder/enroll and internal/shared/tlsboot stamp X.509
	//     NotBefore/NotAfter. Those are verified by PEERS against THEIR clocks,
	//     so issuing them from a floor this box alone believes would mint
	//     certificates its own relays reject. The host clock is correct there.
	//   - internal/app/auth's console socket sets a net.Conn deadline, which is
	//     host monotonic time by construction and not an app timestamp at all.
	//   - internal/feeder/grant stamps an enrollment grant's issued_at, and
	//     internal/feeder/enroll's re-enrollment rate limiter measures its window
	//     from the host clock. Both are relay-tier, both are short-lived, and
	//     neither is left unexamined on purpose — they are simply not this
	//     wiring, which is app-tier (SEC-066 is explicit that the app-side floor
	//     is "distinct from relay/1's relay-side floor").
	//
	// READ THE LIMIT BEFORE RELYING ON THIS. SEC-066 exists so a time-windowed
	// check — a TOTP step, a grant ttl — cannot be defeated by turning the host
	// clock back, and the mechanism is now genuinely load-bearing: the auth
	// store below is opened ON this clock, so grant expiry and every TOTP step
	// read the floor-clamped reading rather than the bare host clock. What is
	// still missing is the OTHER half: no authenticated time SOURCE is wired, so
	// nothing calls Advance, the floor stays at 0, Now() equals the wall clock,
	// and the restart clamp never actually fires on this deployment. Which
	// authenticated-time sources to trust is a deployment-tier decision SEC-067
	// leaves open ("implementation-defined per deployment tier"), and inventing
	// one here would be inventing the trust decision with it. So: the clamp is
	// real and wired to the checks that need it, and it currently clamps against
	// a floor of zero. No traceability row claims SEC-066 until a source exists.
	//
	// THE RISK THIS WIRING TAKES ON, named where it is taken: TOTP steps are
	// derived from this clock, and the skew tolerance is one step. A floor
	// established meaningfully above the host clock therefore makes every
	// authenticator code fail, for everyone, at once.
	//
	// The escape hatch that risk demanded NOW EXISTS, and it did not when this
	// note was first written. startConsoleBinding below constructs
	// auth.NewConsole over this same floor and serves it on the console socket,
	// and the console's clock_floor.reset verb (SEC-075) calls ClockFloor.Reset —
	// the one sanctioned way a one-way ratchet ever moves down. So a box driven
	// into the future by a bad reading is recoverable by an operator with console
	// access, which is the shape of recovery SEC-075 chose deliberately (uid-0
	// can already edit the file; the verb adds an audit record, not new reach).
	// The other two doors named here are still shut and still worth knowing
	// about: workspace delete needs an authenticated owner — the person now
	// locked out — and break-glass recovery is unimplemented.
	//
	// That was the precondition on wiring a time source, and it is now met. It is
	// not permission to wire one: the remaining question is which sources to
	// TRUST, and inventing that answer here would be inventing the deployment's
	// trust decision with it.
	clockFloor, err := auth.OpenClockFloor(cfg.authDir, func() int64 { return time.Now().UnixMilli() })
	if err != nil {
		log.Fatalf("waiveo-feeder: open the app clock floor: %v", err)
	}
	nowMs := clockFloor.Now

	// The content origin stamps each asset's stored-at from ITS clock, and the
	// retention sweep measures the minimum-asset-age guard against that stamp —
	// so both must read the same clock or the guard measures a skew rather than
	// an age, in the direction that DEFEATS it. That is why the floor is opened
	// here rather than beside the auth store it also serves.
	// The content-URL signing key: generated once, persisted, and reloaded on
	// every later boot. A key regenerated at boot would invalidate every URL a
	// relay is still serving from its cached snapshot (REL-050), turning an
	// ordinary restart into a site-wide content outage.
	contentURLKey, err := contenturl.LoadOrCreateKey(filepath.Join(cfg.authDir, "content-url.key"))
	if err != nil {
		log.Fatalf("waiveo-feeder: content-url key: %v", err)
	}
	// Verification is ON: an unsigned or expired fetch is refused before a byte
	// of content is written. The key reaches every party that constructs a URL —
	// this process for what it serves, and every enrolled relay through the
	// signed snapshot (REL-066a) — so there is no longer a constructor left that
	// cannot sign.
	contentStore, err := origin.Open(cfg.contentPath, origin.WithClock(nowMs), origin.WithSigningKey(contentURLKey))
	if err != nil {
		log.Fatalf("waiveo-feeder: open content store: %v", err)
	}
	img := placeholderImage()
	if _, err := contentStore.Add(img); err != nil {
		log.Fatalf("waiveo-feeder: persist placeholder image: %v", err)
	}

	// The demo cast this feeder SEEDS as the demo playlist's items: the single
	// placeholder image by default, or the real 3-item demo cast
	// (snapshot.DemoCastItems) when WAIVEO_FEEDER_DEMO_CAST=multi. Every item's
	// bytes are added to the SAME contentStore the placeholder above uses, so
	// each is immediately servable at its own content-addressed URL — each item
	// keeps its own digest, independently verifiable, not just the first
	// (CastItem's doc).
	castItems := []snapshot.CastItem{{Bytes: img}}
	if cfg.demoCast == demoCastModeMulti {
		items, err := snapshot.DemoCastItems()
		if err != nil {
			log.Fatalf("waiveo-feeder: load demo cast items: %v", err)
		}
		for i, item := range items {
			if _, err := contentStore.Add(item.Bytes); err != nil {
				log.Fatalf("waiveo-feeder: persist demo cast item %d: %v", i, err)
			}
		}
		castItems = items
		log.Printf("waiveo-feeder: multi-item demo cast enabled (%d items) — seeded as the demo playlist's items on an empty store only (demoCastModeMulti's doc)", len(items))
	}

	contentBaseURL := cfg.contentBaseURL

	// The app-side store (scope-nodes + scheduling-core rows) the api authors into
	// and the desired-state is derived from. Seeded with the make-dev demo only
	// when empty, so a first run resolves a program while a persisted store keeps
	// whatever was authored.
	ctx := context.Background()
	// On nowMs, not the host clock: a resource's created_at baseline and the
	// credential row written in the same request must come from ONE reading, or
	// the two disagree the day the host clock is wrong — the only day it matters.
	// A restore staged by a previous run is adopted HERE, before the store is
	// opened, because the swap is a rename of the file this line is about to
	// open — doing it after would leave the process serving the store the
	// restore replaced (archive/1 ARC-107, the offline swap).
	//
	// A failure is fatal rather than degraded. Every state Adopt can resolve, it
	// resolves; the one it cannot is a marker with neither store present, which
	// means something outside this process deleted a file mid-swap. Booting on
	// whatever is left would pick, silently, between two stores an operator has
	// a real stake in telling apart — and the pre-restore copy is on disk for
	// them to choose from.
	if adopted, err := restoreswap.Adopt(cfg.storePath); err != nil {
		log.Fatalf("waiveo-feeder: adopt a pending restore: %v", err)
	} else if adopted {
		log.Printf("waiveo-feeder: adopted a staged restore; the pre-restore store is kept beside it")
	}
	st, err := store.Open(cfg.storePath, nowMs)
	if err != nil {
		// A workspace at a newer schema epoch than this build understands is not
		// a crash — the box stays up in maintenance mode so an operator can see
		// why it will not serve (archive/1 ARC-041/104), instead of flapping
		// under a supervisor. Every other open failure remains fatal.
		if maintenanceOnStoreOpenError(cfg, id, err) {
			return
		}
		log.Fatalf("waiveo-feeder: open store: %v", err)
	}
	defer st.Close()

	// A store written before a row's id had to be a canonical ULID is READABLE
	// but write-dead: every write validates the resulting full row set, so one
	// stale id fails every later create and update for rows the caller never
	// touched. That is invisible until an operator tries to author something, so
	// it is repaired here, at boot, rather than left for a command someone has to
	// know to run — a box that skipped it would look healthy and quietly refuse
	// every edit.
	//
	// It earns that position by being inert: a conforming store is not written to
	// at all, nothing is ever deleted, and the whole rewrite is one transaction
	// that re-runs the write path's own validators before it commits. If it
	// cannot proceed it declines to start, in the same spirit as the
	// desired-state check below — a feeder that cannot accept a write is not
	// something to discover later. `waiveo-feeder -store-check` reports what this
	// would do, without writing, before the restart.
	// renamedScopeNodes carries the scope-node half of the rewrite to the auth
	// store, which is a separate file opened much later in this function and
	// holds role bindings and grants that name these nodes.
	renamedScopeNodes := map[string]string{}
	if m, err := st.MigrateRowIDs(ctx); err != nil {
		log.Fatalf("waiveo-feeder: canonicalize store row ids: %v\n"+
			"    run `waiveo-feeder -store-check` to inspect %s; nothing was changed", err, cfg.storePath)
	} else if len(m.Rewrites) > 0 {
		for _, rw := range m.Rewrites {
			log.Printf("waiveo-feeder: canonicalized %s id %q -> %q", rw.Kind, rw.From, rw.To)
			if rw.Kind == store.KindScopeNode {
				renamedScopeNodes[rw.From] = rw.To
			}
		}
		log.Printf("waiveo-feeder: canonicalized %d store row id(s) in %s", len(m.Rewrites), cfg.storePath)
	}

	// The demo cast's asset refs, in play order — the items the seeded demo
	// PLAYLIST points at. The cast reaches a screen the only way content reaches
	// a screen now: through an authored playlist a daypart selects, resolved per
	// screen (snapshot.DeriveScreenPrograms). There is no second, direct path for
	// it to take and therefore nothing left for a governing schedule to supersede
	// — the caveat demoCastModeMulti's own doc used to carry is gone with it.
	assetRefs := make([]string, 0, len(castItems))
	for _, item := range castItems {
		assetRefs = append(assetRefs, signhash.ContentID(item.Bytes))
	}
	if gen, err := st.Generation(ctx); err != nil {
		log.Fatalf("waiveo-feeder: read store generation: %v", err)
	} else if gen == 0 {
		if err := st.SeedDemo(ctx, assetRefs...); err != nil {
			log.Fatalf("waiveo-feeder: seed demo: %v", err)
		}
		log.Printf("waiveo-feeder seeded make-dev demo into %s (%d playlist item(s))", cfg.storePath, len(assetRefs))
	}

	// The desired-state source: rebuilds the signed snapshot from the store,
	// cached by generation and invalidated when an api write advances it — so each
	// pull serves the current generation (the authoring loop's serving half).
	src := &desiredStateSource{
		store: st, content: contentStore, contentBaseURL: contentBaseURL, id: id,
		nowMs: nowMs,
	}
	// Fail fast if the seeded/persisted store cannot derive a signed snapshot
	// at all — better a boot-time exit than every relay pull failing later.
	if _, err := src.current(); err != nil {
		log.Fatalf("waiveo-feeder: build initial desired state: %v", err)
	}

	enrollSrv, err := enroll.NewServer(id)
	if err != nil {
		log.Fatalf("waiveo-feeder: enrollment server: %v", err)
	}
	// Persist the enrollment registry (relay_id -> enrollment key, issuance
	// records, claim-token bookkeeping, CA) across restarts: without this, a
	// fresh process forgets every relay it ever enrolled and the very next
	// hello from an already-enrolled relay fails channel-binding verification
	// (CHANNEL_BINDING_INVALID, REL-032) even though nothing about that
	// relay's own identity changed. See enroll.Server.EnablePersistence's doc.
	if err := enrollSrv.EnablePersistence(defaultEnrollDir); err != nil {
		log.Fatalf("waiveo-feeder: enable enrollment persistence: %v", err)
	}

	// The api/1 authoring surface (scope-nodes + scheduling-core CRUD), mounted
	// under /api/v1 on the same TLS listener. Auth is DEFERRED for this dev-lab
	// POC (unauthenticated — the api/1 conventions treat the principal as a given;
	// the idempotency principal is a fixed POC ULID inside the api package),
	// documented, not silent. The clock is injected so no wall-clock read lives in
	// the api/idempotency layers; likewise the id source (ulid.New — plain, not
	// Monotonic: unlike eventingest's tight reconstruction loop, api/1 mints at
	// most one id per HTTP request, so there is no same-millisecond ordering
	// concern to guard against here) is injected so the api layer mints no id
	// from a generator of its own.
	// The api's content upload writes into the SAME contentStore the /content/
	// handler below serves from (one origin.Store instance), so an uploaded asset
	// is immediately servable; contentBaseURL is this feeder's own content-origin
	// base the upload's returned url is built from (REL-061/140).
	// The relay/1 persistent-connection endpoint (REL-001's stable path,
	// upgraded to a WS carrying one JSON message per frame, REL-002): the
	// ONLY route desired state and the connection handshake move over. The
	// store-derived snapshot provider answers state.pull (REL-050/051), the
	// enrollment key is looked up by the mTLS client-certificate identity
	// (REL-041), and the enrollment registry's revocation record is checked
	// at every connection attempt and before every steady-state frame
	// (REL-016). The bootstrap HTTP routes below (enrollment +
	// re-enrollment) are the only pre-mTLS surface left — they exist to
	// bootstrap the identity this connection authenticates with.
	//
	// It is built here, ahead of the api handler, because the api's device
	// plane dispatches its commands down this server's live connections.
	// The device plane's read model: the devices and entities the
	// devices/entities list operations serve, and the resolver a command's
	// target entity is looked up in to find the relay that owns it (REL-112).
	// It starts empty and is populated by each relay's own `device.candidates`
	// report over the connection below (REL-110/111) — no operator registration
	// step stands between a relay seeing a device and this app peer listing it.
	// Its rows are placed at this deployment's site node, the same site_binding
	// every hello-ack hands the relay (REL-036), which is also the third member
	// of the identity tuple both peers derive a device_id from (REL-153/110b).
	deviceRegistry := devices.New(firstPhotonSite.ScopeNode, nowMs)

	// Pre-populate it from the durable mirror BEFORE the API starts serving, so
	// a restart does not blank the device list until the relays next report
	// (candidatemirror.go). Non-fatal: a feeder that cannot read its own cache
	// still serves correctly the moment a relay connects, and refusing to boot
	// over a cache would turn a cosmetic degradation into an outage.
	if restored, err := restoreDeviceRegistry(context.Background(), st, deviceRegistry); err != nil {
		log.Printf("waiveo-feeder: restoring the discovered-device mirror failed (the list fills as relays report): %v", err)
	} else if restored > 0 {
		log.Printf("waiveo-feeder: restored %d discovered device(s) from the last run", restored)
	}

	// The LIVE SCREEN STATUS read model (parity row 5.8): what each relay reports
	// its screens have actually been observed doing. Deliberately NOT persisted
	// and not restored across a restart, unlike the discovered-device mirror
	// above: a device sighting stays true while the app is down, but "this screen
	// polled 4 seconds ago" does not, and serving a restored one would state an
	// observation nobody made since the process started. It fills within one
	// report interval of a relay connecting.
	screenRegistry, err := screens.NewRegistry(nowMs)
	if err != nil {
		log.Fatalf("waiveo-feeder: build the screen-status read model: %v", err)
	}

	relayConnSrv := relayconn.New(
		src.current,
		enrollSrv.RelayEnrollmentKey,
		enrollSrv.IsRevoked,
		firstPhotonSite,
		hello.AppPeerImplementedMinors(1, 1),
		firstPhotonRecognizedFeatures,
		// The read model plus its durable mirror, applied in that order: the
		// live view answers `GET /devices` now, the mirror answers it after a
		// restart before any relay has reconnected (candidatemirror.go).
		relayconn.WithCandidateSink(candidateMirror{registry: deviceRegistry, st: st}),
		// REL-124's upstream redemption report, retiring the reported grant
		// from later generations (redemptionsink.go). Retirement TRAILS the
		// redemption and enforces nothing — the site-wide at-most-once
		// property is the grant's own relay binding (REL-121b/REL-124c).
		relayconn.WithRedemptionSink(storeRedemptionSink{st: st}),
		// The screen-liveness read model (parity row 5.8). It takes the app
		// peer's own clock alongside the sink so each report is anchored to the
		// instant it ARRIVED here, which is what lets every age it carries keep
		// growing while a relay is disconnected instead of freezing at whatever
		// the last report said.
		relayconn.WithScreenStatusSink(screenRegistry, nowMs),
	)

	idem := apihttp.NewIdempotencyStore(nowMs, 0)

	// The live observability plane (events/1 EVT-010/013/100/130-144): ONE shared
	// event log the relay-telemetry ingest and the auth plane's auditor write
	// into and the /events/v1 SSE server streams from, bridged by an eventsse.Hub
	// — the concurrent-safe live-transport boundary the log delegates its
	// synchronization to. The relay pushes each fired rule's automation.run to
	// /telemetry/v1/push; eventingest reconstructs a full events/1 envelope
	// (origin: relay, the site scope_node, a recording-order id, the schema's
	// cost/retention class), events.Validates it (EVT-013), and appends it THROUGH
	// the Hub, whose Append wakes every connected /events/v1 subscriber so a
	// telemetry-derived event pushes live (REL-090/092 -> EVT-010/100).
	// firstPhotonSite.ScopeNode is the authoritative site node stamped onto every
	// ingested event (the REL-090 wire record carries no per-record scope).
	//
	// The log is the PERSISTENT one, in the app store beside the rows it
	// describes. EVT-010 defines a durable event as "retained for a bounded window
	// regardless of whether any subscriber is connected", and EVT-141 makes a
	// resume from a point past that window a marked gap rather than a silent hole
	// — neither is satisfiable by a log whose window is a process lifetime. It
	// matters most for the audit trail: security-model/1 SEC-150 makes audit.event
	// the platform's SOLE audit mechanism, so every login, session and API-key
	// issuance and revocation, and every mutating api/1 request files its only
	// permanent record here.
	//
	// A boot-time sweep applies the retention policy before anything reads the
	// log, so a box that was off past a window does not serve expired records
	// until the first tick, and a failing sweep is reported rather than assumed.
	//
	// eventIDs is the ONE recording-order id source (EVT-011) both producers mint
	// from — the relay ingest below and the auditor further down. One source
	// because the ordering guarantee is over the LOG, not over a producer: two
	// independent generators can invert against each other inside a millisecond
	// just as easily as one non-monotonic generator can invert against itself, and
	// an inverted id is appended behind a connected subscriber's cursor and so
	// never delivered to it at all (REL-094/097, EVT-135/143). It is seeded from
	// the stored head, so a restart — including one where the box's clock came
	// back low, which is exactly what SEC-066-068 exists for — cannot mint an id
	// sorting under an event already in the log.
	//
	// Both routes are authenticated, by the credential each caller actually
	// holds. /events/v1 takes a platform session or API key (EVT-110-114, wired
	// below). /telemetry/v1/push takes the pushing relay's own enrollment-issued
	// mTLS client certificate: this listener already verifies a presented client
	// certificate against the enrollment CA (see ClientAuth/ClientCAs below), so
	// the ingest reads the identity off that certificate and asks the enrollment
	// registry the same two questions the /relay/v1 connection asks — did we
	// enroll this relay (REL-041), and is this serial revoked (REL-016). It is
	// the relay/1 channel's own peer authentication applied to the HTTP transport
	// that stands in for a telemetry.push frame, not a second, weaker credential
	// minted for the same peer.
	eventLog, err := st.EventLog(events.DefaultRetentionPolicy(), nowMs, func(err error) {
		// A storage failure the events.Log surface cannot return. On the append
		// path it means a durable record was NOT written, which on the audit
		// path is the platform failing to record something SEC-150 says it must
		// — so it is logged at the loudest thing a log line has.
		log.Printf("waiveo-feeder: EVENT LOG FAILURE: %v", err)
	})
	if err != nil {
		log.Fatalf("waiveo-feeder: open the durable event log: %v", err)
	}
	if pruned, err := eventLog.Prune(); err != nil {
		log.Printf("waiveo-feeder: WARNING — the event-log retention sweep failed at boot: %v", err)
	} else if pruned.Rows > 0 {
		log.Printf("waiveo-feeder: retired %d event(s) past their retention window %v", pruned.Rows, pruned.ByClass)
	}
	eventHead, err := eventLog.Head()
	if err != nil {
		log.Fatalf("waiveo-feeder: read the durable event log's head: %v\n"+
			"    every event id must sort above it (EVT-011); starting without knowing it could append behind a live subscriber", err)
	}
	eventIDs := ulid.MonotonicFrom(eventHead)
	eventHub := eventsse.NewHub(eventLog)
	telemetryIngest := eventingest.New(eventHub, firstPhotonSite.ScopeNode, eventIDs, nowMs,
		func(relayID, serial string) bool {
			if _, enrolled := enrollSrv.RelayEnrollmentKey(relayID); !enrolled {
				return false
			}
			return !enrollSrv.IsRevoked(relayID, serial)
		})

	// The workspace signing key (SEC-046) and the destination the data-subject
	// export writes its archive/1 container to (API-120/121). Establishing the
	// key here, at boot, rather than lazily inside the export means a deployment
	// that cannot hold key material fails loudly at startup instead of at the
	// moment an owner asks for their data.
	//
	// It is established BEFORE the auth store because the auth store now depends
	// on it: a TOTP shared secret (SEC-004) is the one credential secret that
	// must be recoverable rather than hashed, and it is held sealed under a
	// sub-key of this key's data key (SEC-040). Ordering the two this way is what
	// makes "no key, no second-factor enrollment" a startup fact rather than a
	// surprise at the moment an owner tries to enroll.
	//
	// Two directories, never one: cfg.keyDir holds custody, cfg.archiveDir holds
	// output an operator is expected to copy away (see defaultWorkspaceKeyDir).
	wsKey, err := workspacekey.LoadOrCreate(cfg.keyDir, ulid.New)
	if err != nil {
		log.Fatalf("waiveo-feeder: load workspace signing key: %v", err)
	}
	// A deployment that ran a build where the two shared a directory still has
	// the old key material sitting among its exports. Nothing here can safely
	// delete it — it may be the key that signed archives an operator still holds
	// — so this says so, loudly, every boot until someone moves or destroys it.
	if stray := workspacekey.StrayKeyFiles(cfg.archiveDir); len(stray) > 0 {
		log.Printf("waiveo-feeder: WARNING — workspace key material is sitting in the archive directory %s: %v. "+
			"Anyone who copies that directory copies the private signing key and the cleartext data key. "+
			"Move it to %s or destroy it.", cfg.archiveDir, stray, cfg.keyDir)
	}
	secretSealer, err := wsKey.SecretSealer()
	if err != nil {
		log.Fatalf("waiveo-feeder: derive the workspace secret sealer: %v\n"+
			"    a recoverable credential secret (SEC-004's TOTP shared secret) cannot be stored without it", err)
	}
	// ONE Secrets over that sealer, shared by the two halves of the outbound
	// webhook surface: the api operation that installs an endpoint's signing
	// secret seals through it, and the delivery loop below opens through it.
	// Two instances would be two objects over the same key and would work, but
	// sharing one is what makes "the thing that writes the secret and the thing
	// that reads it are the same construction" a fact of the wiring rather than
	// a coincidence two call sites have to keep.
	webhookSecrets := webhookdeliver.NewSecrets(secretSealer)

	// The security-model/1 auth plane. It is built AFTER the event hub because
	// its auditor publishes through it: every flow security-model/1 defines
	// emits an ordinary events/1 audit.event (SEC-150), never a second audit
	// schema of its own, so a login lands in the same stream an operator is
	// already watching.
	//
	// It is opened on nowMs — the clock floor's floor-aware reading — and not on
	// the host wall clock, which is what makes the floor above load-bearing
	// rather than decorative. Every time-windowed check this store owns reads
	// that clock: a grant's `ttl` expiry (SEC-032/035), every TOTP step
	// (SEC-004), the pending-enrollment window, the lockout backoff (SEC-090),
	// and the created/revoked stamps on principals, credentials, sessions, API
	// keys and role bindings. On a host clock rolled back below the floor, all
	// of them stay where the floor says they are instead of walking backwards
	// with the host.
	//
	// WithClockFloor is the floor's LIFECYCLE wiring, distinct from the clock
	// above: a factory reset (SEC-121) destroys the persisted floor along with
	// the credentials it rides beside, rather than handing the next owner a
	// ratchet they have no reachable way to lower.
	//
	// clockAssessment is reported on every login-failure audit record (SEC-091).
	// It is READ from the app's own clock floor (SEC-066-068) rather than
	// hardcoded: SEC-068 defines the assessment as `untrusted` "while it holds no
	// independently verified time above the floor, `trusted` once it does", and
	// that is precisely what ClockFloor.Assessment answers. It reports
	// `untrusted` today for the reason the floor's own wiring note above gives —
	// no authenticated time source is configured — but it reports it because the
	// app asked, not because a constant said so, and it will start telling the
	// truth about `trusted` the moment a source is wired.
	//
	// eventIDs, not a generator of the auditor's own: an audit record and a
	// telemetry record land in the SAME log, and the ordering guarantee EVT-011
	// states is over that log. Two independent id sources would invert against
	// each other inside a shared millisecond, and an audit record that sorted
	// under an already-delivered telemetry record would be appended behind every
	// connected subscriber's cursor — recorded, and never seen.
	//
	// The auditor is built BEFORE the store and handed to it, rather than after:
	// SEC-034 requires an audit record for EVERY grant creation and redemption,
	// the store is what creates and redeems them, and a store that could be
	// opened and used before its sink arrived would have a window — the first
	// boot's setup grant, precisely — in which a grant is minted unrecorded.
	auditor := auth.NewAuditor(eventHub, firstPhotonSite.ScopeNode, nowMs, eventIDs, clockFloor.Assessment)
	authStore, err := auth.Open(filepath.Join(cfg.authDir, authStoreFile), nowMs, ulid.New,
		auth.WithSecretSealer(secretSealer), auth.WithClockFloor(clockFloor),
		auth.WithAuditor(auditor))
	if err != nil {
		log.Fatalf("waiveo-feeder: open auth store: %v", err)
	}
	defer authStore.Close()

	// A role binding and a grant name a scope node that lives in the authoring
	// store, so a node the canonicalization above renamed has to be followed
	// here too. A binding left pointing at the old id would authorize a node
	// that no longer exists, and the principal would quietly lose the subtree
	// they were granted — a silent, security-shaped failure rather than a loud
	// one. Nothing happens unless a scope node actually moved.
	if n, err := authStore.RemapScopeNodes(ctx, renamedScopeNodes); err != nil {
		log.Fatalf("waiveo-feeder: follow canonicalized scope nodes into the auth store: %v", err)
	} else if n > 0 {
		log.Printf("waiveo-feeder: repointed %d authorization record(s) at canonicalized scope nodes", n)
	}

	// Retire second-factor enrollments that were started, never confirmed, and
	// have since aged out (SEC-004). At boot because a box that was off for a
	// month comes back holding every enrollment that was in flight when it
	// stopped; those secrets are already refused on every path, and this is what
	// stops them sitting in the database as well.
	if n, err := authStore.PruneExpiredTOTPEnrollments(ctx); err != nil {
		log.Printf("waiveo-feeder: WARNING — the pending-enrollment sweep failed at boot: %v", err)
	} else if n > 0 {
		log.Printf("waiveo-feeder: retired %d abandoned second-factor enrollment(s) past their window", n)
	}

	// The revocation registry EVT-114 hangs off: revoking a session closes every
	// events/1 stream that session authenticated, rather than merely refusing
	// its next connect.
	revocations := auth.NewRevocations()
	authStore.OnRevoke(revocations.Revoked)

	authn := auth.NewAuthenticator(authStore, auditor, auth.NewDefaultLockout(), revocations)

	// The console binding (SEC-070-078): a host-local Unix domain socket in the
	// auth state directory, admitting only a uid-0 peer (SEC-072) and serving
	// SEC-075's closed verb set attributed to the synthetic `system-console`
	// principal (SEC-073).
	//
	// It is bound HERE, on the same clock floor and the same auditor as every
	// other credential path, and it is bound BEFORE the HTTPS listener because of
	// what SEC-078 says it is for: "the recovery path of last resort precisely
	// because it does not depend on the thing that might be broken." A process
	// that gave up before reaching this line because a certificate would not load
	// would have no recovery path at exactly the moment one is needed.
	//
	// Failure to bind is FATAL rather than a warning. Every way this can fail is
	// a deployment-state problem an operator must see — an unwritable or
	// wrong-owner state directory, a non-socket file sitting on the path, a
	// filesystem that will not honour 0700 — and a box that boots without its
	// recovery path, silently, is the exact class of "shipped non-conformant and
	// nobody noticed" this binding exists to end. It is the same posture the auth
	// store, the workspace key and the clock floor above already take.
	consoleLn, err := startConsoleBinding(cfg.authDir, authStore, clockFloor, auditor)
	if err != nil {
		log.Fatalf("waiveo-feeder: open the console binding: %v", err)
	}
	log.Printf("waiveo-feeder: console binding listening on %s (uid 0 only; verbs %v)", consoleLn.Path(), auth.ConsoleVerbs())

	// The first-boot claim window (SEC-120). On an unclaimed box this mints a
	// one-time setup grant and persists its code 0600; the setup endpoint is
	// claimable ONLY by redeeming it, so an installed-but-unclaimed box on a
	// shared network is never first-come-first-served.
	claim, err := auth.EnsureClaimWindow(ctx, authStore, cfg.authDir, auth.RootScopeNode)
	if err != nil {
		log.Fatalf("waiveo-feeder: evaluate first-boot claim window: %v", err)
	}
	if claim.Claimed {
		log.Printf("waiveo-feeder: workspace is claimed; sign in on the console at /login")
	} else {
		// Printed, not merely persisted: SEC-120 requires the installer present
		// the code "printed, as a QR code, or on-screen", and a headless box's
		// screen is its startup log.
		//
		// The presentation names WHERE to spend the code, because it is the only
		// thing that can. The console deliberately cannot advertise that this box
		// is unclaimed — an unauthenticated claim-state answer would tell anyone
		// who can reach the box that it is sitting inside its claim window right
		// now, which on a shared network is exactly the situation the printed code
		// exists to defend. So its sign-in page offers the setup path to everybody
		// and says nothing about THIS box; this line, read by whoever installed
		// it, is what turns that generic offer into a destination.
		//
		// A path, not a URL: the only host this process could name is the one it
		// was configured to listen on, which is not necessarily the address the
		// operator's browser reaches it by (a NAT, a reverse proxy, a hostname).
		// A confidently wrong URL beside a live one-time code is worse than none.
		log.Printf("waiveo-feeder: WORKSPACE UNCLAIMED — open the console at /setup and claim it with this one-time setup code (also at %s):\n\n    %s\n", claim.CodePath, claim.Code)
	}

	// The runner that executes the work an async api/1 operation accepts with
	// 202 (a fleet enable/disable's Job, api/1 API-111). It is wired
	// explicitly — rather than left to api.New's own default — so this process
	// owns its lifecycle: it starts once the handler is built, and the
	// shutdown path below drains it, so a SIGTERM does not abandon a job
	// halfway through its target list without waiting to see if it can finish.
	jobRunner := api.NewJobRunner()

	// The pack install pipeline's trust anchors (marketplace/1 MKT-009b): the
	// file-backed, host-provisioned local anchor set standing in for the
	// root-signed publisher-namespace delegation until the external trust root's
	// owner ceremony happens — same seam, swappable without touching the
	// pipeline. Absent/empty ⇒ every pack install refuses (fail closed).
	log.Printf("waiveo-feeder: pack install trust anchors: %s", cfg.packTrustPath)

	apiHandler := api.New(st, idem, nowMs, ulid.New, contentStore, contentBaseURL, authn,
		api.WithDevicePlane(deviceRegistry, relayConnSrv), api.WithJobRunner(jobRunner),
		// GET /screen-status joins the authored screen rows to what the relays
		// have observed of them (parity row 5.8, api/screenstatus.go).
		api.WithScreenStatus(screenRegistry),
		api.WithPackTrust(packsig.FileAnchors{Path: cfg.packTrustPath}),
		// The required-pack floor's ONE wiring seam (MKT-093a/MKT-093b). Without
		// this option the store's roster stays nil, the in-transaction floor check
		// returns nil on every call, and the whole floor — implemented, tested,
		// and reachable from a test — enforces nothing on a real box. That was the
		// binary's actual state before this line.
		api.WithRequiredPacks(requiredPacks),
		api.WithWebhookSecrets(webhookSecrets, webhookRotationOverlapMs),
		api.WithWorkspaceArchive(&api.WorkspaceArchive{Dir: cfg.archiveDir, Key: wsKey}),
		// The live store a restore stages beside (archive/1, the offline swap).
		api.WithStorePath(cfg.storePath),
		// The enrollment authority that holds relay certificates (api/1 API-140).
		// Without this the revocation operation records the decision and reaches
		// nothing — which is the state this whole operation was built to end.
		api.WithRelayCertRevoker(enrollSrv.RevokeRelay),
		// The emergency kit a first-boot claim issues (ARC-110). Its verifier
		// lives beside the workspace key, never in the archive directory an
		// operator copies away — a kit verifier travelling off the box with the
		// exports would defeat the physical-access assumption ARC-113 rests on.
		// The workspace signing key's own id names what the kit recovers
		// (ARC-111): it is minted once and is stable for the life of the box.
		api.WithEmergencyKit(&api.EmergencyKitConfig{Dir: cfg.keyDir, WorkspaceID: wsKey.KeyID(), NewID: ulid.New}),
		// The pairing-code operation's relay directory: live connections (and
		// the canonical advertised address each declared at hello, REL-037)
		// from the relay-connection server, and each relay's trust-anchor
		// SPKI from the SAME enrollment registry that issued its certificate
		// — so the commitment an app-formed pairing code carries is computed
		// over the very key the relay's player/1 listener presents (PLY-052).
		api.WithPairing(api.PairingRelayDirectory{
			ConnectedRelays: func() []api.PairingRelay {
				conns := relayConnSrv.ConnectedRelays()
				out := make([]api.PairingRelay, 0, len(conns))
				for _, c := range conns {
					out = append(out, api.PairingRelay{RelayID: c.RelayID, AdvertisedAddress: c.AdvertisedAddress})
				}
				return out
			},
			RelaySPKI: func(relayID string) ([]byte, bool) {
				pub, ok := enrollSrv.RelayEnrollmentKey(relayID)
				if !ok {
					return nil, false
				}
				spki, err := x509.MarshalPKIXPublicKey(pub)
				if err != nil {
					return nil, false
				}
				return spki, true
			},
		}))
	jobRunner.Start()

	// The outbound-webhook delivery loop (events/1 EVT-150-158). It is started
	// here, beside the job runner, because it is the same kind of thing: work an
	// api/1 surface has already accepted and which no request goroutine is
	// waiting on. Without it the registration surface above would accept an
	// endpoint, seal its signing secret, report its delivery state — and never
	// POST anything.
	webhookLoop, err := startWebhookDelivery(st, eventLog, eventHub, webhookSecrets, nowMs)
	if err != nil {
		log.Fatalf("waiveo-feeder: start the webhook delivery loop: %v", err)
	}

	// The embedded console SPA, served at "/" for every non-API path. The API,
	// event-stream, content-origin, telemetry and enrollment/handshake routes are
	// registered as more specific patterns, so http.ServeMux keeps them ahead of
	// this catch-all. `go build` embeds the committed placeholder shell; `make
	// web-build` swaps in the real Vite output before a real run.
	webUI, err := webui.Handler()
	if err != nil {
		log.Fatalf("waiveo-feeder: web UI handler: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzFor(cfg.storePath, cfg.contentPath))
	// contenturl.PathPrefix, not a literal: the route this origin is MOUNTED at
	// and the path every producer mints must be one string, or a perfectly
	// signed url 404s. TestEveryContentURLIsMintedByThisPackage enforces the
	// single spelling.
	mux.Handle(contenturl.PathPrefix, contentStore.Handler())
	mux.Handle("/api/v1/", apiHandler)
	mux.Handle("/telemetry/v1/push", telemetryIngest)
	// The subscriber's visible set (events/1 EVT-120) is resolved per connection
	// against the SAME scope-node tree api/1's read scoping uses, through the
	// same auth.CanRead primitive — the store's tree read is the only input the
	// SSE binding needs to answer it.
	mux.Handle("/events/v1", eventsse.New(eventHub, authn, st.ScopeNodes))
	mux.Handle("/", webUI)
	enrollSrv.Register(mux)

	mux.Handle("/relay/v1", relayConnSrv.Handler())

	// Nudge every live relay connection when the desired-state generation
	// advances (relay/1 REL-057): ALL generation advances — /api/v1 resource
	// CRUD and declarative-pack install/uninstall/row-CRUD alike — commit
	// through the store's one write-transaction choke point, so its
	// post-commit hook is the single seam that catches every writer, not just
	// the HTTP ones. NotifyGenerationAdvance is safe to over-call (it re-reads
	// src.current and the nudge carries the generation, so a relay already at
	// that generation skips the pull) and sends each nudge on its own
	// goroutine, so an api write's latency is never coupled to the slowest
	// relay connection.
	// One hook, two jobs: nudge the relays, and re-arm the override-expiry
	// wait. OnCommit holds a SINGLE function, so a second OnCommit call would
	// silently replace the first — which is how one of these two would have
	// stopped happening with nothing failing.
	wake, wakeExpiry := armOverrideExpiry()
	st.OnCommit(func() {
		relayConnSrv.NotifyGenerationAdvance()
		wakeExpiry()
	})
	// Publish a screen override's TTL lapsing (overrideexpiry.go). Without it
	// the expiry is evaluated correctly and asked for never: an alert's
	// self-limiting half does not happen until some unrelated write advances the
	// generation.
	go overrideExpiryLoop(context.Background(), st, nowMs, wake)

	cert, err := tls.X509KeyPair(id.TLSCertPEM(), id.TLSKeyPEM())
	if err != nil {
		log.Fatalf("waiveo-feeder: load TLS cert: %v", err)
	}

	server := &http.Server{
		Addr:    cfg.listen,
		Handler: apihttp.WithTraceID(mux),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			// Accept (and verify) a client certificate when one is offered —
			// an enrolled relay presenting its enrollment-issued leaf on
			// /relay/v1 (REL-003/041) and on /telemetry/v1/push — while
			// browsers on /api/v1 and screens on /content/ remain
			// certificate-free. This verification is what the telemetry
			// ingest's own identity check stands on: it requires a VERIFIED
			// chain, so a listener wired without ClientCAs refuses every push
			// rather than trusting an unverified leaf. ClientCAPool is read
			// AFTER EnablePersistence, so a restarted feeder keeps verifying
			// leaves it issued before the restart.
			ClientAuth: tls.VerifyClientCertIfGiven,
			ClientCAs:  enrollSrv.ClientCAPool(),
			// TLS 1.3 floor: the /relay/v1 challenge nonce derives from the
			// session's TLS exporter keying material (relay/1 REL-040), so
			// every session this listener accepts must be exporter-capable —
			// pinned here beside the relay dialer's own identical floor
			// rather than discovered per-connection.
			MinVersion: tls.VersionTLS13,
		},
	}

	// The content retention sweep (internal/feeder/contentgc): the only thing
	// standing between a box that runs for years and a content origin that grows
	// forever. Every asset ever uploaded is on disk until something reclaims it,
	// and nothing did.
	//
	// It rides the SAME cadence as the event-log and pending-enrollment sweeps
	// below rather than a second timer, because it is the same kind of work — a
	// periodic pass that retires state which has outlived its use — and because
	// its own guarantee is not the cadence. An asset is reclaimed only after the
	// fleet has converged, its bytes have aged, and it has been observed
	// unreferenced across sweeps, so a sweep that runs late reclaims later and a
	// sweep that never runs reclaims nothing. Neither is a correctness event.
	//
	// The fleet oracle is assembled from the two registries that hold the facts:
	// which relays are enrolled and unrevoked (and may therefore still be serving
	// screens even while disconnected), and what generation each connected relay
	// last said it applied.
	contentSweeper, err := newContentSweeper(st, contentStore,
		contentSweepFleetFloor(enrollSrv.ActiveRelayIDs, relayConnSrv.ConnectedRelays, relayConnSrv.LastStateAck),
		nowMs)
	if err != nil {
		log.Fatalf("waiveo-feeder: build the content retention sweep: %v", err)
	}

	log.Printf("waiveo-feeder listening (HTTPS) on %s (content base %s)", cfg.listen, cfg.contentBaseURL)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServeTLS("", "") }()

	// Keep retiring expired rows while the process runs. The boot sweeps above
	// cover everything that expired while the box was off; this covers a box that
	// stays up for months. Stopped on shutdown so the sweep never races the drain
	// below.
	//
	// The three arms are independent: one failing must not skip the others, which
	// is why no arm short-circuits the loop iteration.
	retentionSweep := time.NewTicker(retentionSweepInterval)
	go func() {
		for range retentionSweep.C {
			if pruned, err := eventLog.Prune(); err != nil {
				log.Printf("waiveo-feeder: WARNING — the event-log retention sweep failed: %v", err)
			} else if pruned.Rows > 0 {
				log.Printf("waiveo-feeder: retired %d event(s) past their retention window %v", pruned.Rows, pruned.ByClass)
			}
			if n, err := authStore.PruneExpiredTOTPEnrollments(ctx); err != nil {
				log.Printf("waiveo-feeder: WARNING — the pending-enrollment sweep failed: %v", err)
			} else if n > 0 {
				log.Printf("waiveo-feeder: retired %d abandoned second-factor enrollment(s) past their window", n)
			}
			runContentSweep(ctx, contentSweeper)
		}
	}()

	// Graceful shutdown. http.Server.Shutdown does NOT touch hijacked
	// connections — every live /relay/v1 WebSocket is invisible to it — so
	// the relay-connection server's own registry (CloseAll) is what
	// actually ends those connections; without it the process would stop
	// listening while every relay connection stayed open.
	//
	// The event hub is the same story for the other direction: a /events/v1
	// subscriber is an endless stream by design, so an SSE connection never
	// goes idle for Shutdown to reclaim and a WS one is hijacked and invisible
	// to it. Hub.Close is what ends both — the WS binding closes naming
	// UNAVAILABLE, so a client reconnects with backoff rather than treating a
	// restart as a protocol failure.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-serveErr:
		log.Fatal(err)
	case sig := <-sigCh:
		log.Printf("waiveo-feeder: %s — shutting down", sig)
		retentionSweep.Stop()
		relayConnSrv.CloseAll()
		eventHub.Close()
		// The console binding closes first among the credential surfaces, and
		// unlinks its socket as it goes, so the next boot's stale-socket check has
		// nothing to reason about after a clean stop.
		if err := consoleLn.Close(); err != nil {
			log.Printf("waiveo-feeder: close the console binding: %v", err)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("waiveo-feeder: shutdown: %v", err)
		}
		// Accepted-but-unfinished Job execution drains after the listener stops,
		// so no new job is accepted while it does. A target left `running` by an
		// expired drain window is exactly the state API-116's resume is defined
		// over — the record is committed either way, never lost.
		if err := jobRunner.Shutdown(shutdownCtx); err != nil {
			log.Printf("waiveo-feeder: job runner shutdown: %v", err)
		}
		// Webhook delivery drains last and on the SAME budget: it talks to a
		// third party this process does not control, so it is the one drain
		// whose slowest case is somebody else's server. Missing the budget
		// abandons the attempt in flight rather than holding the process open —
		// at-least-once (EVT-156) means the receiver sees that delivery again
		// after the restart, under the same delivery id, never not at all.
		if err := webhookLoop.Shutdown(shutdownCtx); err != nil {
			log.Printf("waiveo-feeder: webhook delivery shutdown: %v (an in-flight delivery was abandoned; it redelivers on the next boot)", err)
		}
	}
}

// startConsoleBinding builds and starts this deployment's console binding
// (security-model/1 SEC-070-078) and returns the listener the shutdown path
// closes.
//
// It is a function rather than inline in main for the same reason
// startWebhookDelivery is: so a test can drive the IDENTICAL wiring. A test that
// reassembled it — its own auth.NewConsole over its own listener — would pass
// while main's copy of it was missing, and "the component exists but nothing a
// deployment runs reaches it" is the exact state this binding was in before it
// had a socket at all. The one thing worth guarding here is that a shipped
// binary reaches it, so that is what the wiring is arranged to let a test say.
//
// clockFloor is passed rather than omitted because SEC-075's clock-floor reset
// verb is otherwise UNIMPLEMENTED — and a box whose floor has been driven above
// its wall clock has no other way down (SEC-066's wiring note above).
func startConsoleBinding(authDir string, authStore *auth.Store, clockFloor *auth.ClockFloor, auditor *auth.Auditor) (*auth.ConsoleListener, error) {
	ln, err := auth.ListenConsole(authDir, auth.NewConsole(authStore, clockFloor, auditor), func(format string, args ...any) {
		log.Printf("waiveo-feeder: "+format, args...)
	})
	if err != nil {
		return nil, err
	}
	go ln.Serve()
	return ln, nil
}

// webhookRotationOverlapMs is how long a rotated-away signing secret stays
// acceptable (EVT-158). ONE constant feeds both halves deliberately: the api
// rotation response publishes this instant to the operator, and the delivery
// loop is what actually stops emitting the prior signature at it. Wiring them
// from separate figures would let the platform publish a window it does not
// honour, which is worse than publishing none.
const webhookRotationOverlapMs = events.DefaultRotationOverlapMs

// startWebhookDelivery builds and starts the outbound-webhook delivery loop for
// this deployment, returning the handle the shutdown path drains.
//
// It exists as a function rather than inline in main so the whole-stack test can
// drive the IDENTICAL wiring — the same HTTP client, the same clock and id
// source, the same hub hook, the same interval. A test that reassembled the
// wiring itself would pass while main's copy of it was missing, which is exactly
// the gap this loop was added to close.
//
// log is read directly rather than through hub, which is safe here and would not
// be for every events.Log: this deployment's log is the SQLite-backed
// *store.EventLog, whose own doc states it is concurrency-safe (reads take the
// store's read lock, writes its write lock). The boot and hourly retention
// sweeps in main already read it the same way. The in-memory events.EventLog
// would NOT be safe to share like this.
func startWebhookDelivery(
	st *store.Store,
	eventLog events.Log,
	hub *eventsse.Hub,
	secrets *webhookdeliver.Secrets,
	nowMs func() int64,
) (*webhookdeliver.Loop, error) {
	loop, err := webhookdeliver.NewLoop(webhookdeliver.Config{
		Store: st,
		Log:   eventLog,
		// A client of this process's own, not http.DefaultClient: an endpoint
		// URL is operator-supplied, so its connection pool and its timeouts
		// belong to this loop and not to every other HTTP caller in the binary.
		// The per-attempt deadline is the Deliverer's (EVT-153), applied through
		// the request context, so none is set here.
		HTTP:     &http.Client{},
		NowMs:    nowMs,
		NewID:    ulid.New,
		Secrets:  secrets,
		Endpoint: events.EndpointConfig{RotationOverlapMs: webhookRotationOverlapMs},
		OnDisabled: func(endpointID, url string) {
			// EVT-154's operator-facing signal. A log line is what this
			// deployment has; the disabled status is durable regardless, and an
			// operator re-enables through /api/v1/webhook-endpoints/{id}/enable.
			log.Printf("waiveo-feeder: WEBHOOK ENDPOINT DISABLED — %s (%s) failed too many consecutive deliveries and will receive no more until it is re-enabled", endpointID, url)
		},
		OnError: func(endpointID string, err error) {
			if endpointID == "" {
				log.Printf("waiveo-feeder: webhook delivery: %v", err)
				return
			}
			log.Printf("waiveo-feeder: webhook delivery for endpoint %s: %v", endpointID, err)
		},
	}, webhookdeliver.DefaultInterval)
	if err != nil {
		return nil, err
	}
	// The wake seam. Every event this process records — a relay telemetry push
	// and a security-model audit record alike — lands through the hub's Append,
	// so hooking it here is what makes a fresh event reach a receiver promptly
	// instead of at the next tick. Registered BEFORE Start so no append between
	// the two is missed.
	hub.OnAppend(loop.Notify)
	loop.Start()
	return loop, nil
}

// desiredStateSource rebuilds the feeder's signed desired-state snapshot from the
// app store on demand, caching it by the store's generation AND by the earliest
// instant the derivation could change on its own: a pull is answered from cache
// while both hold, and it is rebuilt (via snapshot.BuildFromStore) when an api
// write has advanced the generation or that instant has passed. This is the seam
// that makes an authored edit change what the relay pulls, while keeping
// desired-state derivation entirely store-driven (site_effective comes from the
// site node, never box-local state).
//
// # Why the generation alone is not a sufficient cache key
//
// It was, and the omission shipped: a screen program override carries a TTL
// (data-model/1 DAT-004c's `expires_at`) whose expiry is evaluated inside the
// derivation (ScreenOverride.Applies), and the generation only advances on a
// WRITE. So a sixty-second alert stayed `priority=preempt pinned=true` for as
// long as nothing else in the store was edited — an hour, a day — while
// DAT-004d, the openapi description and the operator's own mental model all said
// it self-limits. The derived snapshot is a function of (rows, generation,
// INSTANT), and a cache keyed on two of those three serves a stale answer for
// the third exactly as long as nobody touches the first two.
//
// The instant is bounded, not sampled: cachedUntil is the earliest instant this
// build stops being the right answer for a reason no write will announce, so the
// cached snapshot is served for precisely as long as it is still correct and
// rebuilt the first time it is asked for after it stops being.
//
// # Two such reasons, and the second is why a content URL does not rot
//
// The first is the pending override expiry above (nextOverrideExpiry). The
// second is that this build MINTED signed, expiring content URLs
// (contenturl.SnapshotTTL), and a generation cached past their deadline is a
// generation whose every image and video 403s at the screen — HV-1 again, on a
// timer, with nothing anywhere reporting it. Nothing an operator does causes
// that: it is reached by a feeder simply staying up without an authoring write,
// and it lands on whichever screen next reboots or evicts its content cache.
//
// So the cache window is also capped at contenturl.SnapshotRemintInterval, half
// the life of the URLs the build stamped. That is what let SnapshotTTL come down
// from thirty days to one: the deadline no longer has to outlive an authoring
// lull, because a lull no longer means an un-re-minted snapshot. The relay-side
// residual (an outage longer than the TTL, which only REL-066d closes) is
// documented on SnapshotTTL itself.
type desiredStateSource struct {
	store          *store.Store
	contentBaseURL string
	// content is the content origin this feeder serves, and the source of the
	// key every derived snapshot mints its own `url`s under AND publishes as
	// revocation_and_site.content_url_key (REL-066a) for every relay at the site
	// to re-mint with — including the schedule-resolved URLs no app peer can
	// pre-sign, and including re-minting through an outage (REL-050/066d).
	//
	// The key is READ FROM THE ORIGIN (origin.Store.Signer) rather than carried
	// alongside it, so the half that mints and the half that verifies are one
	// value. Carried separately, they were free to differ — and the shape that
	// differed silently was an origin holding a key while every app-authored URL
	// was minted without one, which 403'd every image and video on every screen.
	content *origin.Store
	id      *signing.Identity

	// nowMs is the instant each rebuild resolves every screen's program at
	// (snapshot.BuildFromStore). It is injected rather than read inline because
	// scheduling resolution is per-instant (DAT-111): the instant is an input to
	// the built generation, so a test pins it exactly as it pins any other input.
	nowMs func() int64

	mu        sync.Mutex
	cached    wire.StateSnapshotBody
	cachedGen int64
	haveCache bool
	// cachedUntil is the instant the cached snapshot stops being the right
	// answer for reasons no write will announce: the earlier of the earliest
	// pending override `expires_at` and this build's content-URL re-mint
	// deadline (cacheWindowEnd). It is never zero on a real build — the re-mint
	// term always applies — so the generation is never the only thing that can
	// invalidate the cache.
	cachedUntil int64
}

// cacheWindowEnd is the instant a snapshot built at nowMs over these screen rows
// stops being servable: the EARLIER of the next pending screen-override expiry
// (nextOverrideExpiry — after which the derivation itself is wrong) and the
// content-URL re-mint deadline (after which the derivation is still right but the
// urls in it are on their way to expiring).
//
// Both are "the cache is stale for a reason no write announces", which is why
// they are one bound rather than two mechanisms. The re-mint term is
// unconditional, so the result is always positive: a deployment cannot end up
// caching a generation forever by simply having no TTL'd overrides, which is the
// state every ordinary site is in.
//
// A pure function of (rows, instant), so both halves are testable without running
// a feeder.
func cacheWindowEnd(screens []datamodel.Screen, nowMs int64) int64 {
	until := nowMs + contenturl.SnapshotRemintInterval.Milliseconds()
	if next := nextOverrideExpiry(screens, nowMs); next != 0 && next < until {
		return next
	}
	return until
}

// current returns the snapshot for the store's current generation, rebuilding it
// when the generation has advanced since the last build OR when the last build's
// cache window (cacheWindowEnd) has closed. Safe for concurrent pulls.
func (d *desiredStateSource) current() (wire.StateSnapshotBody, error) {
	ctx := context.Background()
	gen, err := d.store.Generation(ctx)
	if err != nil {
		return wire.StateSnapshotBody{}, err
	}
	now := d.nowMs()

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.haveCache && d.cachedGen == gen && (d.cachedUntil == 0 || now < d.cachedUntil) {
		return d.cached, nil
	}

	ds, err := d.store.DesiredState(ctx)
	if err != nil {
		return wire.StateSnapshotBody{}, err
	}
	snap, degrades, err := snapshot.BuildFromStore(ds, d.content.Signer(d.contentBaseURL, contenturl.SnapshotTTL), d.id, now)
	if err != nil {
		return wire.StateSnapshotBody{}, err
	}
	// A degraded screen is OMITTED from screen_programs rather than resolved
	// against a substituted timezone (DAT-034), so without this line an operator
	// would see a screen silently stop being delivered and have nothing to read.
	for _, e := range degrades {
		log.Printf("waiveo-feeder: desired state generation %d: screen program omitted: %s: %s: %s",
			ds.Generation, e.Field, e.Code, e.Message)
	}
	d.cached = snap
	d.cachedGen = ds.Generation
	d.cachedUntil = cacheWindowEnd(ds.Screens, now)
	d.haveCache = true
	return snap, nil
}

// nextOverrideExpiry is the earliest screen-override `expires_at` STRICTLY AFTER
// nowMs across these screen rows, or zero when none is pending — the one instant
// at which a desired state nobody has written can stop being the right answer
// (data-model/1 DAT-004c/DAT-004d).
//
// Strictly after, not at-or-after: an override whose expiry equals nowMs has
// already stopped applying (Applies is `ExpiresAt > tMs`), so it is accounted
// for by the derivation this instant produced and re-deriving at it would be a
// deadline already met.
//
// It is a pure function of (rows, instant) so both users can be tested without
// running either of them: the snapshot cache's validity window
// (desiredStateSource.current) and the timer that publishes the lapse
// (overrideExpiryLoop).
func nextOverrideExpiry(screens []datamodel.Screen, nowMs int64) int64 {
	next := int64(0)
	for _, s := range screens {
		o := s.Override
		if o == nil || o.ExpiresAt <= nowMs {
			continue
		}
		if next == 0 || o.ExpiresAt < next {
			next = o.ExpiresAt
		}
	}
	return next
}

// healthzFor answers /healthz with this component's real operational state
// rather than a constant.
//
// The previous body was `{"component":"waiveo-feeder","status":"ok"}` — a
// literal, returned whether or not the box had run out of room to write. That is
// not a hypothetical failure here: this deployment has already lost a box to a
// full disk, and a probe that answers "ok" through it tells an operator the one
// thing that is not true.
//
// The relay's own /healthz reports events/1's box.vitals (EVT-070), which is a
// RELAY's physical health — CPU temperature, throttling, undervoltage. None of
// that describes an app process, so this reports what is actually true of one
// rather than borrowing a schema that does not fit. Disk headroom is the member
// they share, and it is the one that has actually bitten.
//
// Deliberately NO store round-trip. A liveness probe that queries the database
// can hang exactly when the database is the problem, turning "degraded" into "no
// answer at all" — and the deploy tooling reads a timeout as a dead process. What
// this reports is cheap and cannot block.
//
// `status` stays "ok" while the process answers, for the reason the relay's does:
// deciding which readings constitute unhealthy is a threshold question that
// belongs to a detector with a stated policy, not to a liveness probe inventing
// one.
func healthzFor(storePath, contentPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"component": "waiveo-feeder",
			"status":    "ok",
		}
		// Both paths, because they are usually different filesystems on an
		// appliance and either filling up stops writes: the store holds every
		// authored row, the content origin holds asset bytes and grows fastest.
		disk := map[string]any{}
		unavailable := []string{}
		for name, path := range map[string]string{
			"store":   filepath.Dir(storePath),
			"content": contentPath,
		} {
			if free, err := freeBytes(path); err == nil {
				disk[name] = free
			} else {
				unavailable = append(unavailable, name)
			}
		}
		if len(disk) > 0 {
			body["disk_headroom_bytes"] = disk
		}
		if len(unavailable) > 0 {
			// Named rather than left absent: a consumer seeing no reading should be
			// able to tell "this path could not be stat'd" from "the field was
			// dropped".
			sort.Strings(unavailable)
			body["disk_headroom_unavailable"] = unavailable
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

// freeBytes reports the bytes available to an unprivileged writer on the
// filesystem holding path. Bavail rather than Bfree: Bfree counts the kernel's
// root reserve, which this process is not writing as, so reporting it would
// promise space the feeder cannot actually use.
func freeBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// placeholderImage builds a tiny in-memory 2x2 PNG — Wave-1 first-photon's
// stand-in for a real content source, ahead of any real ingestion task.
// Generated at process start rather than loaded from a file, so the
// feeder binary has no runtime dependency on a fixture path.
func placeholderImage() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 0xff, A: 0xff})
	img.Set(1, 0, color.RGBA{G: 0xff, A: 0xff})
	img.Set(0, 1, color.RGBA{B: 0xff, A: 0xff})
	img.Set(1, 1, color.RGBA{R: 0xff, G: 0xff, A: 0xff})

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		log.Fatalf("waiveo-feeder: encode placeholder image: %v", err)
	}
	return buf.Bytes()
}
