package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
)

// maintenanceOnStoreOpenError enters maintenance mode iff err is the
// newer-epoch refusal (store.EpochTooNewError), and reports whether it handled
// it. It keeps the errors.As check beside the maintenance implementation so the
// boot site reads as one line and main.go's import set is untouched. A failure
// to even stand up the maintenance listener is fatal — there is nothing left to
// degrade to.
func maintenanceOnStoreOpenError(cfg config, id *signing.Identity, err error) bool {
	var ete *store.EpochTooNewError
	if !errors.As(err, &ete) {
		return false
	}
	if mErr := runMaintenanceMode(cfg, id, ete); mErr != nil {
		log.Fatalf("waiveo-feeder: maintenance mode: %v", mErr)
	}
	return true
}

// Maintenance mode is what a boot does when it CANNOT open its own workspace for
// a reason a restart will not fix on its own — today, a workspace file written
// at a platform schema epoch newer than this build understands (archive/1
// ARC-041/104, store.EpochTooNewError). The alternative the box used to take was
// log.Fatal, which under a process supervisor is a crash loop: the box
// disappears from the network every few seconds and an operator sees a flapping
// unit rather than a diagnosable state.
//
// Instead the process stays up on the SAME listener it would normally serve,
// answering /healthz with maintenance_mode true and the reason, and refusing
// every other route with 503. An operator (or the fleet dashboard) can see
// exactly why the box is not serving and what has to change — roll the binary
// forward to a build that understands the epoch, or restore a compatible
// workspace — without the box having ever silently downgraded the file by
// running this build's older migrations over it.
//
// It deliberately does NOT touch the store: the whole point is that the store
// could not be opened. It holds no state and serves one truth.

// maintenanceBody is the /healthz payload in maintenance mode. It is its own
// function so the exact shape is pinned by a test rather than assembled inline
// in the handler.
func maintenanceBody(cause *store.EpochTooNewError) map[string]any {
	return map[string]any{
		"component":        "waiveo-feeder",
		"status":           "maintenance",
		"maintenance_mode": true,
		"reason":           "workspace_schema_epoch_too_new",
		"detail":           cause.Error(),
		"workspace_schema_epoch": map[string]any{
			"on_disk":    cause.OnDisk,
			"understood": cause.Understood,
		},
	}
}

// maintenanceMux is the whole HTTP surface a maintenance-mode boot serves:
// /healthz reports the degraded state, and every other path is 503 with the same
// body, so a probe against any route learns the box is in maintenance and why.
// Pure and self-contained — no store, no identity — so a test drives it directly.
func maintenanceMux(cause *store.EpochTooNewError) http.Handler {
	body := maintenanceBody(cause)
	write := func(w http.ResponseWriter, status int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// 200: the process IS answering; maintenance is reported in the body, not
		// as a transport failure, so a liveness probe reading only the status code
		// does not mistake a diagnosable maintenance boot for a dead process.
		write(w, http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Everything else is unavailable: the box is not serving its normal
		// surfaces while it cannot open its workspace.
		write(w, http.StatusServiceUnavailable)
	})
	return mux
}

// runMaintenanceMode binds the feeder's normal listener with its normal TLS
// identity and serves maintenanceMux until the process is signalled to stop. It
// returns nil on a clean shutdown so main exits 0 — a maintenance boot is a
// deliberate, operator-visible state, not a crash.
func runMaintenanceMode(cfg config, id *signing.Identity, cause *store.EpochTooNewError) error {
	cert, err := tls.X509KeyPair(id.TLSCertPEM(), id.TLSKeyPEM())
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:      cfg.listen,
		Handler:   maintenanceMux(cause),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
	}
	log.Printf("waiveo-feeder: MAINTENANCE MODE on %s — %v", cfg.listen, cause)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServeTLS("", "") }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-serveErr:
		return err
	case sig := <-sigCh:
		log.Printf("waiveo-feeder: %s — leaving maintenance mode", sig)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}
