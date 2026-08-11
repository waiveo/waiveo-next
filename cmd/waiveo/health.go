package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/apiop"
)

// healthOperation is the one operation this command is about. It is named — not
// hand-built — so the request itself still goes through the derived engine and
// its response is still checked against the document's own SystemHealth schema.
const healthOperation = "getSystemHealth"

// cmdHealth answers "is this deployment working", to the standard the workspace
// asks for: port-bound, HTTP 200, AND the correct payload shape. Each of the
// three is a separate check with its own verdict, because each fails
// differently:
//
//   - port-bound alone is what a `curl -sf` gets you, and a wedged process that
//     still accepts connections passes it;
//   - 200 alone passes for a box whose store is unreadable, because the health
//     route's job is to REPORT that, not to refuse;
//   - the payload shape is what catches a deployment serving a stale or wrong
//     build — the answer arrives, and it is not the answer the document
//     declares.
//
// Only the third can be checked without a hand-written expectation, and it is
// checked against api/openapi.yaml's own SystemHealth schema rather than against
// a struct written here.
func cmdHealth(ctx context.Context, args []string, e env) (int, error) {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var conn connFlags
	conn.bind(fs)
	asJSON := fs.Bool("json", false, "emit the whole verdict as JSON")
	if err := fs.Parse(args); err != nil {
		return exitFailure, err
	}

	s, err := apiop.Load()
	if err != nil {
		return exitFailure, err
	}
	op, ok := s.Lookup(healthOperation)
	if !ok {
		return exitFailure, fmt.Errorf("the embedded api document declares no %s operation", healthOperation)
	}
	client, cfg, addrSource, err := conn.client(s)
	if err != nil {
		return exitFailure, err
	}

	verdict := healthVerdict{BaseURL: cfg.BaseURL, AddressFrom: addrSource, Checks: []healthCheck{}}

	// 1. Port-bound and answering at all. /healthz is unauthenticated and outside
	// /api/v1, so it is the one probe that separates "nothing is listening" from
	// "something is listening and refusing me".
	verdict.add(probeHealthz(ctx, cfg))

	// 2 and 3. The authenticated summary, and its shape.
	res, err := client.Do(ctx, op, apiop.Args{})
	if err != nil {
		verdict.add(healthCheck{Name: "system-health", Status: "down", Detail: err.Error()})
		return finishHealth(e, verdict, *asJSON)
	}
	verdict.TraceID = res.TraceID
	switch {
	case res.Status == http.StatusUnauthorized, res.Status == http.StatusForbidden:
		detail := fmt.Sprintf("HTTP %d — the deployment is up and this CLI is not authorized for it", res.Status)
		if cfg.Token == "" {
			detail += fmt.Sprintf("; no credential was sent (install one at %s, mode 0600, or pass --token-file)", defaultTokenPath())
		}
		verdict.add(healthCheck{Name: "system-health", Status: "unknown", Detail: detail})
		return finishHealth(e, verdict, *asJSON)
	case !res.OK():
		verdict.add(healthCheck{Name: "system-health", Status: "down", Detail: fmt.Sprintf("HTTP %d: %s", res.Status, firstLineOf(string(res.Body)))})
		return finishHealth(e, verdict, *asJSON)
	}
	verdict.add(healthCheck{Name: "system-health", Status: "ok", Detail: fmt.Sprintf("HTTP %d in %dms", res.Status, res.Duration.Milliseconds())})

	if err := s.ValidateResponse(op, res); err != nil {
		// This is the check the workspace's "correct payload shape" clause is
		// about, and it is a FAILURE rather than a warning here: `waiveo health` is
		// the command whose whole job is to be trusted, and a box answering a shape
		// the document does not declare is a box nothing downstream can parse.
		verdict.add(healthCheck{Name: "payload-shape", Status: "down", Detail: err.Error()})
		return finishHealth(e, verdict, *asJSON)
	}
	verdict.add(healthCheck{Name: "payload-shape", Status: "ok", Detail: "body matches the declared SystemHealth schema"})

	var summary systemHealth
	if err := json.Unmarshal(res.Body, &summary); err != nil {
		verdict.add(healthCheck{Name: "payload-shape", Status: "down", Detail: err.Error()})
		return finishHealth(e, verdict, *asJSON)
	}
	verdict.Summary = &summary
	verdict.add(healthCheck{Name: "deployment", Status: summary.Status, Detail: summary.detail()})
	return finishHealth(e, verdict, *asJSON)
}

// probeHealthz hits the unauthenticated liveness route.
//
// It is spelled here rather than derived because it is not an api/1 operation:
// it lives outside the /api/v1 prefix precisely so it can be reached with no
// credential and no version negotiation. Deriving it would mean putting it in
// the document, which would put an unauthenticated route inside a versioned,
// authenticated surface.
func probeHealthz(ctx context.Context, cfg apiop.Config) healthCheck {
	url := strings.TrimRight(cfg.BaseURL, "/") + "/healthz"
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureTLS} //nolint:gosec // operator-selected, and only for a self-signed dev leaf
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return healthCheck{Name: "reachable", Status: "unknown", Detail: "read --ca-file: " + err.Error()}
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return healthCheck{Name: "reachable", Status: "unknown", Detail: "--ca-file holds no PEM certificate"}
		}
		tlsCfg.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return healthCheck{Name: "reachable", Status: "down", Detail: err.Error()}
	}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return healthCheck{Name: "reachable", Status: "down", Detail: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return healthCheck{Name: "reachable", Status: "degraded", Detail: fmt.Sprintf("/healthz answered HTTP %d", resp.StatusCode)}
	}
	return healthCheck{Name: "reachable", Status: "ok", Detail: fmt.Sprintf("/healthz 200 in %dms", time.Since(started).Milliseconds())}
}

// systemHealth mirrors only the members this command renders. It is a READING
// convenience: the authoritative shape check is ValidateResponse against the
// document, which happens before this is decoded, so this struct being partial
// cannot weaken the verdict.
type systemHealth struct {
	Status      string `json:"status"`
	UptimeMs    int64  `json:"uptime_ms"`
	Version     string `json:"version"`
	CheckedAtMs int64  `json:"checked_at_ms"`
	Services    []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"services"`
	Storage struct {
		Path      string `json:"path"`
		Status    string `json:"status"`
		FreeBytes *int64 `json:"free_bytes"`
		Detail    string `json:"detail"`
	} `json:"storage"`
	Relays []struct {
		RelayID     string `json:"relay_id"`
		Address     string `json:"address"`
		ScreenCount int    `json:"screen_count"`
	} `json:"relays"`
	Screens struct {
		Total     int `json:"total"`
		Live      int `json:"live"`
		Fetching  int `json:"fetching"`
		Rejected  int `json:"rejected"`
		Stale     int `json:"stale"`
		NeverSeen int `json:"never_seen"`
		Paired    int `json:"paired"`
	} `json:"screens"`
}

func (h systemHealth) detail() string {
	uptime := "unknown"
	if h.UptimeMs >= 0 {
		uptime = (time.Duration(h.UptimeMs) * time.Millisecond).Round(time.Second).String()
	}
	return fmt.Sprintf("version %s, up %s", h.Version, uptime)
}

type healthCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type healthVerdict struct {
	BaseURL     string        `json:"base_url"`
	AddressFrom string        `json:"address_from,omitempty"`
	TraceID     string        `json:"trace_id,omitempty"`
	Checks      []healthCheck `json:"checks"`
	Summary     *systemHealth `json:"system_health,omitempty"`
	Verdict     string        `json:"verdict"`
}

func (v *healthVerdict) add(c healthCheck) { v.Checks = append(v.Checks, c) }

// worst is the same derivation the document specifies for SystemHealth.status —
// the worst grade any component carries, with `unknown` above `ok` and below
// `degraded`, because a check that could not run is not a passing check and is
// not an outage.
func (v *healthVerdict) worst() string {
	rank := map[string]int{"ok": 0, "unknown": 1, "low": 2, "degraded": 2, "critical": 3, "down": 3}
	worst := "ok"
	for _, c := range v.Checks {
		if rank[c.Status] > rank[worst] {
			worst = c.Status
		}
	}
	return worst
}

func finishHealth(e env, v healthVerdict, asJSON bool) (int, error) {
	v.Verdict = v.worst()
	code := exitOK
	switch v.Verdict {
	case "ok":
		code = exitOK
	case "unknown", "degraded", "low":
		code = exitDegraded
	default:
		code = exitFailure
	}

	if asJSON {
		enc := json.NewEncoder(e.out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return exitFailure, err
		}
		return code, nil
	}

	fmt.Fprintf(e.out, "waiveo health %s\n", v.BaseURL)
	for _, c := range v.Checks {
		fmt.Fprintf(e.out, "  %-14s %-9s %s\n", c.Name, c.Status, c.Detail)
	}
	if s := v.Summary; s != nil {
		for _, svc := range s.Services {
			fmt.Fprintf(e.out, "    service %-12s %-9s %s\n", svc.Name, svc.Status, svc.Detail)
		}
		fmt.Fprintf(e.out, "    storage %-12s %-9s %s\n", s.Storage.Path, s.Storage.Status, s.Storage.Detail)
		if len(s.Relays) == 0 {
			fmt.Fprintln(e.out, "    relays       (none connected — every screen and device is unreachable)")
		}
		for _, r := range s.Relays {
			fmt.Fprintf(e.out, "    relay   %-12s %s  %d screen(s)\n", r.RelayID, r.Address, r.ScreenCount)
		}
		fmt.Fprintf(e.out, "    screens      %d total / %d live / %d fetching / %d rejected / %d stale / %d never seen / %d paired\n",
			s.Screens.Total, s.Screens.Live, s.Screens.Fetching, s.Screens.Rejected, s.Screens.Stale, s.Screens.NeverSeen, s.Screens.Paired)
	}
	fmt.Fprintf(e.out, "HEALTH %s (exit %d)\n", strings.ToUpper(v.Verdict), code)
	return code, nil
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
