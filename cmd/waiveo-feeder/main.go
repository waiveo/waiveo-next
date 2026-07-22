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
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/eventingest"
	"github.com/maaxton/waiveo-next/internal/app/eventsse"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/feeder/enroll"
	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
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
	contentPath    string // directory the content origin persists uploaded asset bytes to
}

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

// loadConfig reads the feeder config from env (via `env`, os.Getenv in main),
// falling back to the loopback defaults. contentBaseURL defaults to the listen
// address so an unconfigured feeder behaves exactly as before.
func loadConfig(env func(string) string) config {
	listen := envOr(env, "WAIVEO_FEEDER_LISTEN", "127.0.0.1:7420")
	return config{
		listen:         listen,
		contentBaseURL: envOr(env, "WAIVEO_FEEDER_CONTENT_URL", "https://"+listen),
		storePath:      envOr(env, "WAIVEO_FEEDER_STORE", defaultStorePath),
		contentPath:    envOr(env, "WAIVEO_FEEDER_CONTENT_DIR", defaultContentPath),
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
		store: st, img: img, contentBaseURL: contentBaseURL, id: id,
		grants: []wire.PairingGrant{g},
	}
	initialSnap, err := src.current()
	if err != nil {
		log.Fatalf("waiveo-feeder: build initial desired state: %v", err)
	}

	enrollSrv, err := enroll.NewServer(id, initialSnap)
	if err != nil {
		log.Fatalf("waiveo-feeder: enrollment server: %v", err)
	}
	// Serve the store-derived, generation-tracked desired state from the pull
	// endpoint (superseding the static initialSnap).
	enrollSrv.SetSnapshotProvider(src.current)

	// The connection handshake's app-peer server (relay/1 REL-030–039): it
	// issues the challenge nonce and answers a relay's hello, verifying the
	// channel binding against the enrollment key the enroll server recorded
	// (REL-032, RelayEnrollmentKey), negotiating the version (REL-033/034,
	// N−1 via AppPeerImplementedMinors), and returning this feeder's
	// authoritative site_binding (REL-036).
	helloSrv := hello.NewAppPeerServer(
		enrollSrv.RelayEnrollmentKey,
		firstPhotonSite,
		hello.AppPeerImplementedMinors(1, 1),
		firstPhotonRecognizedFeatures,
		nil,
	)

	// The api/1 authoring surface (scope-nodes + scheduling-core CRUD), mounted
	// under /api/v1 on the same TLS listener. Auth is DEFERRED for this dev-lab
	// POC (unauthenticated — the api/1 conventions treat the principal as a given;
	// the idempotency principal is a fixed POC ULID inside the api package),
	// documented, not silent. The clock is injected so no wall-clock read lives in
	// the api/idempotency layers.
	// The api's content upload writes into the SAME contentStore the /content/
	// handler below serves from (one origin.Store instance), so an uploaded asset
	// is immediately servable; contentBaseURL is this feeder's own content-origin
	// base the upload's returned url is built from (REL-061/140).
	nowMs := func() int64 { return time.Now().UnixMilli() }
	idem := apihttp.NewIdempotencyStore(nowMs, 0)
	apiHandler := api.New(st, idem, nowMs, contentStore, contentBaseURL)

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
	// record carries no per-record scope); ulid.New mints each event's
	// recording-order id (EVT-011). Auth is DEFERRED for this dev-lab POC — the
	// ingest + SSE endpoints are unauthenticated (EVT-110-114 is the documented
	// seam), and the relay pushes over the existing feeder TLS.
	eventLog := events.NewEventLog(feederEventLogRetention)
	eventHub := eventsse.NewHub(eventLog)
	telemetryIngest := eventingest.New(eventHub, firstPhotonSite.ScopeNode, ulid.New)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.Handle("/content/", contentStore.Handler())
	mux.Handle("/api/v1/", apiHandler)
	mux.Handle("/telemetry/v1/push", telemetryIngest)
	mux.Handle("/events/v1", eventsse.New(eventHub))
	enrollSrv.Register(mux)
	helloSrv.Register(mux)

	cert, err := tls.X509KeyPair(id.TLSCertPEM(), id.TLSKeyPEM())
	if err != nil {
		log.Fatalf("waiveo-feeder: load TLS cert: %v", err)
	}

	server := &http.Server{
		Addr:      cfg.listen,
		Handler:   apihttp.WithTraceID(mux),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}

	log.Printf("waiveo-feeder listening (HTTPS) on %s (content base %s)", cfg.listen, cfg.contentBaseURL)
	log.Fatal(server.ListenAndServeTLS("", ""))
}

// desiredStateSource rebuilds the feeder's signed desired-state snapshot from the
// app store on demand, caching it by the store's generation: a pull for an
// unchanged generation returns the cached snapshot, and it is rebuilt (via
// snapshot.BuildFromStore) only when an api write has advanced the generation.
// This is the seam that makes an authored edit change what the relay pulls, while
// keeping desired-state derivation entirely store-driven (site_effective comes
// from the site node, never box-local state).
type desiredStateSource struct {
	store          *store.Store
	img            []byte
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
	snap, err := snapshot.BuildFromStore(ds, d.img, d.contentBaseURL, d.id, d.grants)
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
