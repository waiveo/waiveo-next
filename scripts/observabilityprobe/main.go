// Command observabilityprobe is the `make dev` live observability check: it
// reads the app peer's /events/v1 SSE stream and confirms a relay-fired
// automation.run reached it — the last mile of the observability loop (a fired
// edge rule -> the relay telemetry channel -> the app event log -> a live SSE
// subscriber). The relay's dev-stack demo (WAIVEO_RELAY_DEMO_OBSERVE) fires the
// seeded edge rule once at boot; its automation.run rides the telemetry push to
// the feeder's ingest and into the event log. This probe reads it back.
//
// It is written in Go (like the other make-dev probes) because the feeder serves
// /events/v1 over an ed25519-leaf TLS cert some system curl builds cannot
// handshake; Go's TLS stack handles ed25519, matching the all-Go stack.
//
// AUTHENTICATION: /events/v1 is authenticated on both bindings (events/1
// EVT-110/111), and a subscriber's visible set is resolved per connection from
// its role bindings (EVT-120), so an unauthenticated read is a 401 and a read
// with no binding sees nothing. The probe presents the local dev API key through
// scripts/devcred — that package's doc carries the provisioning path (`make
// dev-key`) and the authority the key holds.
//
// The probe requests the WHOLE retained backlog via a genesis resume_from — a
// valid ULID strictly below every real event id, which events/1 resolves to a
// retention gap that streams the retained log from its oldest id (EVT-140/143),
// the boot-fired automation.run included — rather than a fresh (from-now) stream
// that would miss an event recorded before the probe connected. An empty log
// answers that resume_from with a 400 RESUME_FROM_INVALID (nothing recorded to
// resume against yet); the probe retries until the automation.run lands or the
// budget elapses. A 401/403 is NOT retried: it is a definitive refusal of the
// credential, and retrying it for the whole budget only buys a timeout message
// that blames the observability loop for an auth fault. Prints OBSERVABILITY OK
// on success, exits non-zero otherwise.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/scripts/devcred"
)

// feederSSEURL is the app peer's live events stream (events/1 EVT-100); the
// feeder serves it on its own HTTPS listener. FEEDER_SSE_URL overrides it.
const feederSSEURL = "https://127.0.0.1:7420/events/v1"

// resumeFromGenesis is a valid-but-minimal ULID (26 zeros) strictly below every
// real event id, so the resume resolves to a retention gap that streams the WHOLE
// retained backlog (EVT-140/143) — the boot-fired automation.run included — never
// a fresh (from-now) stream that would miss it.
const resumeFromGenesis = "00000000000000000000000000"

// automationRunSchema is the events/1 schema the fired edge rule publishes.
const automationRunSchema = "automation.run"

func main() {
	url := feederSSEURL
	if v := os.Getenv("FEEDER_SSE_URL"); v != "" {
		url = v
	}

	// No client timeout: each request is bounded by its own context below, since
	// a successful subscribe holds the stream open by design.
	client, err := devcred.Client(0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "OBSERVABILITY FAIL: %v\n", err)
		os.Exit(1)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		detail, ok, refusal := probeOnce(client, url)
		if ok {
			fmt.Printf("OBSERVABILITY OK (%s)\n", detail)
			return
		}
		if refusal != "" {
			fmt.Fprintf(os.Stderr, "OBSERVABILITY FAIL: the events stream refused the presented credential: %s\n", refusal)
			os.Exit(1)
		}
		time.Sleep(500 * time.Millisecond)
	}

	fmt.Fprintln(os.Stderr, "OBSERVABILITY FAIL: no relay automation.run reached the app /events/v1 stream within the budget")
	os.Exit(1)
}

// probeOnce opens the SSE stream requesting the full backlog and scans it for an
// automation.run event (origin relay, mode_disposition ran). It returns a detail
// string + true on the first match.
//
// A non-match is reported two ways, because the two conditions want opposite
// treatment. A 400 RESUME_FROM_INVALID means the event log is still empty
// (nothing ingested yet) and the caller should retry. A 401/403 means the
// credential itself was refused; no amount of retrying changes that, so it comes
// back as a non-empty `refusal` carrying the server's Problem body for the
// caller to print and stop on.
func probeOnce(client *http.Client, url string) (detail string, ok bool, refusal string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"?resume_from="+resumeFromGenesis, nil)
	if err != nil {
		return "", false, ""
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		return "", false, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return "", false, fmt.Sprintf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, "" // 400 = log still empty (RESUME_FROM_INVALID); retry
	}
	d, matched := scanForAutomationRun(resp.Body)
	return d, matched, ""
}

// scanForAutomationRun reads SSE frames from r (the resolved backlog is written
// and flushed immediately on connect, then the stream stays open for live) and
// returns on the first automation.run event that carries origin relay and
// mode_disposition ran. A non-event frame (e.g. the leading retention gap) is
// skipped. It returns false when the read deadline (the request context) closes
// the body before a match.
func scanForAutomationRun(r io.Reader) (string, bool) {
	br := bufio.NewReader(r)
	var event, data string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return "", false
		}
		if line == "\n" { // blank line terminates a frame (EVT-103)
			if event == "event" {
				if detail, ok := matchAutomationRun(data); ok {
					return detail, true
				}
			}
			event, data = "", ""
			continue
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// matchAutomationRun reports whether data is an events/1 automation.run envelope
// with origin relay and a "ran" mode_disposition — the fired-rule signature the
// observability loop must deliver end to end (EVT-040/041/042/100).
func matchAutomationRun(data string) (string, bool) {
	var env struct {
		Schema  string          `json:"schema"`
		Origin  string          `json:"origin"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return "", false
	}
	if env.Schema != automationRunSchema || env.Origin != "relay" {
		return "", false
	}
	var p struct {
		RuleID          string `json:"rule_id"`
		ModeDisposition string `json:"mode_disposition"`
	}
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return "", false
	}
	if p.ModeDisposition != "ran" {
		return "", false
	}
	return fmt.Sprintf("SSE subscriber read a relay automation.run: rule_id=%s mode_disposition=%s origin=%s", p.RuleID, p.ModeDisposition, env.Origin), true
}
