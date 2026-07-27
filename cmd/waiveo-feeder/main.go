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
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/devices"
	"github.com/maaxton/waiveo-next/internal/app/eventingest"
	"github.com/maaxton/waiveo-next/internal/app/eventsse"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/app/webui"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/feeder/enroll"
	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/feeder/relayconn"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// feederEventLogRetention bounds the app-side live-event log (events/1): the most
// recent telemetry-derived events retained for a resuming/backlog-reading
// /events/v1 subscriber. It is generous for the low automation.run rate a
// first-photon dev stack produces; the specific horizon is a platform-config
// concern events/1 leaves open (EVT-153/154), so it is a named constant here, not
// a contract value.
const feederEventLogRetention = 4096

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
	demoCast       string // "" (default, single first-photon image) or "multi" (the real 3-item demo cast)
}

// demoCastModeMulti is loadConfig's WAIVEO_FEEDER_DEMO_CAST value that swaps
// the single first-photon image for snapshot.DemoCastItems' real 3-item
// ordered demo cast on the DIRECT screen_programs[0].content path (REL-061).
// Any other value (including unset/"") keeps today's exact single-image
// behavior — unset is the make-dev/CI default, so neither is affected by
// this flag's existence.
//
// CAVEAT, stated here rather than left implicit: this flag changes ONLY the
// app-authored screen_programs baseline SetServedProgram configures at boot
// (cmd/waiveo-relay/main.go). A screen GOVERNED by a schedule (this codebase's
// separate, already multi-item-capable playlist/daypart resolution engine,
// internal/relay/schedulehost) has its Lease re-derived from that schedule's
// own playlist on every resolve, which supersedes this baseline the moment the
// schedule resolver first ticks (cmd/waiveo-relay/main.go's own doc: "replacing
// the app-authored screen_programs baseline"). The make-dev/box seed
// (store.SeedDemo) DOES seed a governing schedule, so this flag's effect is
// visible at boot and on any screen the seeded schedule does not govern; making
// it durably win over an active governing schedule is a separate, larger change
// to the schedule/playlist engine, out of this flag's scope.
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
const defaultAuthDir = ".dev/feeder-auth"

// authStoreFile is the auth database's filename inside defaultAuthDir.
const authStoreFile = "auth.db"

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
		demoCast:       envOr(env, "WAIVEO_FEEDER_DEMO_CAST", ""),
	}
}

func envOr(env func(string) string, key, def string) string {
	if v := env(key); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := loadConfig(os.Getenv)

	id, err := signing.LoadOrCreate(signing.DefaultDir)
	if err != nil {
		log.Fatalf("waiveo-feeder: load identity: %v", err)
	}
	log.Printf("waiveo-feeder identity loaded (signing pub %s)", hex.EncodeToString(id.SigningPub()))

	// The content origin is dir-backed so operator-uploaded assets survive a
	// restart in lock-step with the persisted playlists that reference them
	// (cfg.contentPath, a sibling of the store DB). The placeholder is re-added
	// every boot — content-addressed, so re-adding it is a no-op on disk.
	contentStore, err := origin.Open(cfg.contentPath)
	if err != nil {
		log.Fatalf("waiveo-feeder: open content store: %v", err)
	}
	img := placeholderImage()
	if _, err := contentStore.Add(img); err != nil {
		log.Fatalf("waiveo-feeder: persist placeholder image: %v", err)
	}

	// The direct screen_programs.content cast this feeder builds
	// (snapshot.BuildFromStoreCast): the single placeholder image by default —
	// {Bytes: img} with NO ContentType/DurationMS set, so it marshals
	// byte-identically to every prior release (snapshot.Build's own doc on
	// ContentRef's omitempty tags) — or the real 3-item demo cast
	// (snapshot.DemoCastItems) when WAIVEO_FEEDER_DEMO_CAST=multi. Every
	// item's bytes are added to the SAME contentStore the placeholder above
	// uses, so each is immediately servable at its own content-addressed URL —
	// each item keeps its own digest, independently verifiable, not just the
	// first (CastItem's doc).
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
		log.Printf("waiveo-feeder: multi-item demo cast enabled (%d items) — see demoCastModeMulti's doc for its interaction with a governing schedule", len(items))
	}

	contentBaseURL := cfg.contentBaseURL
	g := grant.Mint()

	// The app-side store (scope-nodes + scheduling-core rows) the api authors into
	// and the desired-state is derived from. Seeded with the make-dev demo only
	// when empty, so a first run resolves a program while a persisted store keeps
	// whatever was authored.
	ctx := context.Background()
	st, err := store.Open(cfg.storePath)
	if err != nil {
		log.Fatalf("waiveo-feeder: open store: %v", err)
	}
	defer st.Close()

	assetRef := signhash.ContentID(img)
	if gen, err := st.Generation(ctx); err != nil {
		log.Fatalf("waiveo-feeder: read store generation: %v", err)
	} else if gen == 0 {
		if err := st.SeedDemo(ctx, assetRef); err != nil {
			log.Fatalf("waiveo-feeder: seed demo: %v", err)
		}
		log.Printf("waiveo-feeder seeded make-dev demo into %s", cfg.storePath)
	}

	// The desired-state source: rebuilds the signed snapshot from the store,
	// cached by generation and invalidated when an api write advances it — so each
	// pull serves the current generation (the authoring loop's serving half).
	src := &desiredStateSource{
		store: st, items: castItems, contentBaseURL: contentBaseURL, id: id,
		grants: []wire.PairingGrant{g},
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
	relayConnSrv := relayconn.New(
		src.current,
		enrollSrv.RelayEnrollmentKey,
		enrollSrv.IsRevoked,
		firstPhotonSite,
		hello.AppPeerImplementedMinors(1, 1),
		firstPhotonRecognizedFeatures,
	)

	// The device plane's read model: the adopted devices and entities the
	// devices/entities list operations serve, and the resolver a command's
	// target entity is looked up in to find the relay that owns it (REL-112).
	// It starts empty — a relay populates it from its own discovery and
	// adoption reports (REL-110/111), which is a separate pipeline; until then
	// the routes are live and answer truthfully rather than 404-ing as
	// unmounted paths.
	deviceRegistry := devices.New()

	nowMs := func() int64 { return time.Now().UnixMilli() }
	idem := apihttp.NewIdempotencyStore(nowMs, 0)

	// The live observability plane (events/1 EVT-010/013/100/130-144): ONE shared
	// event log the relay-telemetry ingest writes into and the /events/v1 SSE
	// server streams from, bridged by an eventsse.Hub — the concurrent-safe
	// live-transport boundary the EventLog delegates its synchronization to. The
	// relay pushes each fired rule's automation.run to /telemetry/v1/push;
	// eventingest reconstructs a full events/1 envelope (origin: relay, the site
	// scope_node, a recording-order id, the schema's cost/retention class),
	// events.Validates it (EVT-013), and appends it THROUGH the Hub, whose Append
	// wakes every connected /events/v1 subscriber so a telemetry-derived event
	// pushes live (REL-090/092 -> EVT-010/100). firstPhotonSite.ScopeNode is the
	// authoritative site node stamped onto every ingested event (the REL-090 wire
	// record carries no per-record scope); a ulid.Monotonic() factory mints each
	// event's recording-order id (EVT-011). It is monotonic — NOT plain ulid.New —
	// because ingest reconstructs a whole PushBatch in a tight loop and two rule
	// firings inside one relay flush tick easily share a wall-clock millisecond;
	// plain New's independent random tail could then sort the second-recorded event
	// below the first, delivering them out of recording order and, in a narrow
	// interleaving with a live drain, permanently dropping one from a lagging
	// subscriber's stream (EVT-011, REL-094/097, EVT-135/143). The monotonic factory
	// makes same-millisecond ids sort in mint (recording) order.
	//
	// /events/v1 is authenticated (EVT-110-114, wired below). /telemetry/v1/push
	// is NOT: it is a relay pushing over the mTLS-capable feeder listener with an
	// enrollment-issued client certificate, which is its own authentication path
	// (relay/1), not a platform session — an unauthenticated- or scoped-intake
	// operation in API-092's sense rather than an api/1 route that forgot to
	// authenticate.
	eventLog := events.NewEventLog(feederEventLogRetention)
	eventHub := eventsse.NewHub(eventLog)
	telemetryIngest := eventingest.New(eventHub, firstPhotonSite.ScopeNode, ulid.Monotonic())

	// The security-model/1 auth plane. It is built AFTER the event hub because
	// its auditor publishes through it: every flow security-model/1 defines
	// emits an ordinary events/1 audit.event (SEC-150), never a second audit
	// schema of its own, so a login lands in the same stream an operator is
	// already watching.
	authStore, err := auth.OpenDefault(filepath.Join(cfg.authDir, authStoreFile))
	if err != nil {
		log.Fatalf("waiveo-feeder: open auth store: %v", err)
	}
	defer authStore.Close()

	// The revocation registry EVT-114 hangs off: revoking a session closes every
	// events/1 stream that session authenticated, rather than merely refusing
	// its next connect.
	revocations := auth.NewRevocations()
	authStore.OnRevoke(revocations.Revoked)

	// clockAssessment is reported on every login-failure audit record (SEC-091).
	// It is hardcoded to `untrusted` because that is the honest reading: SEC-068
	// defines `untrusted` as "while it holds no independently verified time above
	// the floor", and this app maintains no persisted monotonic clock floor yet
	// (SEC-066-068). It becomes a real reading when that floor lands; reporting
	// `trusted` in the meantime would put a claim in the audit trail that nothing
	// backs.
	clockAssessment := func() string { return auth.ClockUntrusted }
	auditor := auth.NewAuditor(eventHub, firstPhotonSite.ScopeNode, nowMs, ulid.New, clockAssessment)
	authn := auth.NewAuthenticator(authStore, auditor, auth.NewDefaultLockout(), revocations)

	// The first-boot claim window (SEC-120). On an unclaimed box this mints a
	// one-time setup grant and persists its code 0600; the setup endpoint is
	// claimable ONLY by redeeming it, so an installed-but-unclaimed box on a
	// shared network is never first-come-first-served.
	claim, err := auth.EnsureClaimWindow(ctx, authStore, cfg.authDir, auth.RootScopeNode, auditor)
	if err != nil {
		log.Fatalf("waiveo-feeder: evaluate first-boot claim window: %v", err)
	}
	if claim.Claimed {
		log.Printf("waiveo-feeder: workspace is claimed; sign in at /api/v1/auth/login")
	} else {
		// Printed, not merely persisted: SEC-120 requires the installer present
		// the code "printed, as a QR code, or on-screen", and a headless box's
		// screen is its startup log.
		log.Printf("waiveo-feeder: WORKSPACE UNCLAIMED — claim it with this one-time setup code (also at %s):\n\n    %s\n", claim.CodePath, claim.Code)
	}

	apiHandler := api.New(st, idem, nowMs, ulid.New, contentStore, contentBaseURL, authn,
		api.WithDevicePlane(deviceRegistry, relayConnSrv))

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
	mux.HandleFunc("/healthz", healthz)
	mux.Handle("/content/", contentStore.Handler())
	mux.Handle("/api/v1/", apiHandler)
	mux.Handle("/telemetry/v1/push", telemetryIngest)
	mux.Handle("/events/v1", eventsse.New(eventHub, authn))
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
	st.OnCommit(relayConnSrv.NotifyGenerationAdvance)

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
			// /relay/v1 (REL-003/041) — while browsers on /api/v1 and screens
			// on /content/ remain certificate-free. ClientCAPool is read
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

	log.Printf("waiveo-feeder listening (HTTPS) on %s (content base %s)", cfg.listen, cfg.contentBaseURL)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServeTLS("", "") }()

	// Graceful shutdown. http.Server.Shutdown does NOT touch hijacked
	// connections — every live /relay/v1 WebSocket is invisible to it — so
	// the relay-connection server's own registry (CloseAll) is what
	// actually ends those connections; without it the process would stop
	// listening while every relay connection stayed open.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-serveErr:
		log.Fatal(err)
	case sig := <-sigCh:
		log.Printf("waiveo-feeder: %s — shutting down", sig)
		relayConnSrv.CloseAll()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("waiveo-feeder: shutdown: %v", err)
		}
	}
}

// desiredStateSource rebuilds the feeder's signed desired-state snapshot from the
// app store on demand, caching it by the store's generation: a pull for an
// unchanged generation returns the cached snapshot, and it is rebuilt (via
// snapshot.BuildFromStoreCast) only when an api write has advanced the generation.
// This is the seam that makes an authored edit change what the relay pulls, while
// keeping desired-state derivation entirely store-driven (site_effective comes
// from the site node, never box-local state).
type desiredStateSource struct {
	store *store.Store
	// items is the direct screen_programs[0].content ordered cast
	// (BuildFromStoreCast) — main's default single-image boot passes a single
	// CastItem with no ContentType/DurationMS set (byte-identical to the
	// pre-cast BuildFromStore path, ContentRef's own doc comment); a
	// WAIVEO_FEEDER_DEMO_CAST=multi boot passes snapshot.DemoCastItems'
	// 3-item cast instead. MUST be non-empty.
	items          []snapshot.CastItem
	contentBaseURL string
	id             *signing.Identity
	grants         []wire.PairingGrant

	mu        sync.Mutex
	cached    wire.StateSnapshotBody
	cachedGen int64
	haveCache bool
}

// current returns the snapshot for the store's current generation, rebuilding it
// only when the generation has advanced since the last build. Safe for
// concurrent pulls.
func (d *desiredStateSource) current() (wire.StateSnapshotBody, error) {
	ctx := context.Background()
	gen, err := d.store.Generation(ctx)
	if err != nil {
		return wire.StateSnapshotBody{}, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.haveCache && d.cachedGen == gen {
		return d.cached, nil
	}

	ds, err := d.store.DesiredState(ctx)
	if err != nil {
		return wire.StateSnapshotBody{}, err
	}
	snap, err := snapshot.BuildFromStoreCast(ds, d.items, d.contentBaseURL, d.id, d.grants)
	if err != nil {
		return wire.StateSnapshotBody{}, err
	}
	d.cached = snap
	d.cachedGen = ds.Generation
	d.haveCache = true
	return snap, nil
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"component": "waiveo-feeder",
		"status":    "ok",
	})
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
