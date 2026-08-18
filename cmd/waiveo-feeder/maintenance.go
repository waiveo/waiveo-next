package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
)

// maintenanceOnStoreOpenError enters maintenance mode iff err is one of the
// "cannot open the workspace, and a restart will not fix it" refusals, and
// reports whether it handled it. It keeps the errors.As checks beside the
// maintenance implementation so the boot site reads as one line and main.go's
// import set is untouched. A failure to even stand up the maintenance listener
// is fatal — there is nothing left to degrade to.
func maintenanceOnStoreOpenError(cfg config, id *signing.Identity, err error) bool {
	cause, ok := maintenanceCauseFor(err)
	if !ok {
		return false
	}
	if mErr := runMaintenanceMode(cfg, id, cause); mErr != nil {
		log.Fatalf("waiveo-feeder: maintenance mode: %v", mErr)
	}
	return true
}

// Maintenance mode is what a boot does when it CANNOT open its own workspace for
// a reason a restart will not fix on its own. The alternative the box used to
// take was log.Fatal, which under a process supervisor is a crash loop: the box
// disappears from the network every few seconds and an operator sees a flapping
// unit rather than a diagnosable state. Worse, the one sentence naming the cause
// is then only in journald, on a box that is down and taking its relay with it —
// /healthz is connection-refused and the console log page cannot show lines from
// before the process started, which is exactly the crash an operator is looking
// for.
//
// Instead the process stays up on the SAME listener it would normally serve,
// answering /healthz with maintenance_mode true and the reason, and refusing
// every other route with 503. An operator (or the fleet dashboard) can see
// exactly why the box is not serving and what has to change, without the box
// having ever silently altered the file to get itself running.
//
// It deliberately does NOT touch the store: the whole point is that the store
// could not be opened. It holds no state and serves one truth.
//
// Two refusals qualify today, and they are the same class from the operator's
// side — the workspace is unopenable and only a human decision changes that:
//
//   - store.EpochTooNewError: the file was written at a platform schema epoch
//     newer than this build understands (archive/1 ARC-041/104). Roll the binary
//     forward, or restore a compatible workspace.
//   - store.SchemaMigrationBlockedError: this build declares a column the file
//     lacks and SQLite cannot retrofit it (or the retrofit failed part-way and
//     committed nothing). The store is intact and unchanged; the column needs a
//     DEFAULT, or a table rebuild under a schema epoch.

// maintenanceCause is the one reading the maintenance surface serves: why the
// workspace could not be opened, in the words an operator needs, plus whatever
// structured detail that particular refusal carries.
type maintenanceCause struct {
	reason string
	detail string
	remedy string
	extra  map[string]any
}

// maintenanceCauseFor classifies a store-open failure. It is the ONE place that
// decides which refusals degrade to maintenance mode and which stay fatal, so
// adding a refusal type and forgetting to route it is a change to this function
// rather than a silent fall-through to log.Fatal at the boot site.
func maintenanceCauseFor(err error) (maintenanceCause, bool) {
	var epoch *store.EpochTooNewError
	if errors.As(err, &epoch) {
		return maintenanceCause{
			reason: "workspace_schema_epoch_too_new",
			detail: epoch.Error(),
			remedy: "roll this box forward to a build that understands the workspace's schema epoch, " +
				"or restore a workspace this build can open; the file has NOT been modified",
			extra: map[string]any{
				"workspace_schema_epoch": map[string]any{
					"on_disk":    epoch.OnDisk,
					"understood": epoch.Understood,
				},
			},
		}, true
	}

	var blocked *store.SchemaMigrationBlockedError
	if errors.As(err, &blocked) {
		columns := make([]string, 0, len(blocked.Blocked))
		for _, b := range blocked.Blocked {
			columns = append(columns, fmt.Sprintf("%s.%s: %s", b.Table, b.Column, b.Reason))
		}
		return maintenanceCause{
			reason: "workspace_schema_migration_blocked",
			detail: blocked.Error(),
			remedy: "this build declares a column the stored schema cannot be given by ALTER TABLE; " +
				"nothing was written and every row is intact. Roll back to the previous build, or ship one " +
				"whose column carries a DEFAULT (or rebuilds the table under a new schema epoch)",
			extra: map[string]any{"workspace_schema_blocked_columns": columns},
		}, true
	}
	return maintenanceCause{}, false
}

// maintenanceBody is the /healthz payload in maintenance mode. It is its own
// function so the exact shape is pinned by a test rather than assembled inline
// in the handler.
func maintenanceBody(cause maintenanceCause) map[string]any {
	body := map[string]any{
		"component":        "waiveo-feeder",
		"status":           "maintenance",
		"maintenance_mode": true,
		"reason":           cause.reason,
		"detail":           cause.detail,
		"remedy":           cause.remedy,
	}
	for k, v := range cause.extra {
		body[k] = v
	}
	return body
}

// maintenanceMux is the whole HTTP surface a maintenance-mode boot serves:
// /healthz reports the degraded state, and every other path is 503 with the same
// body, so a probe against any route learns the box is in maintenance and why.
// Pure and self-contained — no store, no identity — so a test drives it directly.
func maintenanceMux(cause maintenanceCause) http.Handler {
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
func runMaintenanceMode(cfg config, id *signing.Identity, cause maintenanceCause) error {
	cert, err := tls.X509KeyPair(id.TLSCertPEM(), id.TLSKeyPEM())
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:      cfg.listen,
		Handler:   maintenanceMux(cause),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
	}
	log.Printf("waiveo-feeder: MAINTENANCE MODE on %s — %s: %s\n    %s",
		cfg.listen, cause.reason, cause.detail, cause.remedy)

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
