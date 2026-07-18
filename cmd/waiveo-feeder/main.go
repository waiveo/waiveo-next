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
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"os"

	"github.com/maaxton/waiveo-next/internal/feeder/enroll"
	"github.com/maaxton/waiveo-next/internal/feeder/grant"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// config is the feeder's deployment-time addressing. Defaults keep the Wave-1
// loopback dev/CI behavior byte-identical; the on-box deployment overrides them
// so the content URL a screen fetches direct (contentBaseURL, baked into the
// signed snapshot -> lease) resolves to a LAN-reachable host rather than the
// box's own loopback.
type config struct {
	listen         string // TCP bind address for the HTTPS listener
	contentBaseURL string // scheme+host the direct-fetch content URL is built from
}

// loadConfig reads the feeder config from env (via `env`, os.Getenv in main),
// falling back to the loopback defaults. contentBaseURL defaults to the listen
// address so an unconfigured feeder behaves exactly as before.
func loadConfig(env func(string) string) config {
	listen := envOr(env, "WAIVEO_FEEDER_LISTEN", "127.0.0.1:7420")
	return config{
		listen:         listen,
		contentBaseURL: envOr(env, "WAIVEO_FEEDER_CONTENT_URL", "https://"+listen),
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

	contentStore := origin.New()
	img := placeholderImage()
	contentStore.Add(img)

	contentBaseURL := cfg.contentBaseURL
	g := grant.Mint()

	snap, err := snapshot.Build(img, contentBaseURL, id, []wire.PairingGrant{g})
	if err != nil {
		log.Fatalf("waiveo-feeder: build snapshot: %v", err)
	}

	enrollSrv, err := enroll.NewServer(id, snap)
	if err != nil {
		log.Fatalf("waiveo-feeder: enrollment server: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.Handle("/content/", contentStore.Handler())
	enrollSrv.Register(mux)

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
