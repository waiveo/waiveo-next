// Package telemetryhttp is the concrete relay→app-peer transport for the
// telemetry upstream channel (contracts/relay-1.md REL-090/092/097): an
// telemetry.Upstream that POSTs a telemetry.push batch to the app peer's
// /telemetry/v1/push route over the relay's feeder-trusting TLS client and
// reads back the telemetry.ack cursor.
//
// internal/relay/telemetry holds the in-memory buffer + retention-cursor logic
// and defines the Upstream seam so its tests stay transport-free; this package
// is the wire that seam abstracts. It carries no buffer state of its own: the
// telemetry.Channel owns the buffer and only advances retention on an ack this
// transport actually returned, so a push that fails here leaves the batch
// buffered for the next flush (REL-097 — an unacknowledged batch is retried, its
// entries never discarded).
package telemetryhttp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/maaxton/waiveo-next/internal/relay/telemetry"
)

// pushPath is the app peer's telemetry ingest route (REL-090); New joins it to
// the configured app base URL.
const pushPath = "/telemetry/v1/push"

// Upstream is the concrete telemetry.Upstream: a stateless POST-and-decode that
// delivers one telemetry.push to the app peer and returns its telemetry.ack.
type Upstream struct {
	url    string       // fully-formed <appBaseURL>/telemetry/v1/push endpoint
	client *http.Client // the relay's feeder-trusting TLS client
}

// New returns a telemetry.Upstream that POSTs telemetry.push batches to
// appBaseURL's /telemetry/v1/push route (REL-090) using client — the relay's
// feeder-trusting TLS client (in the Wave-1 co-located feeder+relay deployment
// the app peer's self-signed listener certificate has no separate trust anchor,
// so client is the same server-authenticated bootstrap client the relay already
// reaches the feeder with). A trailing slash on appBaseURL is tolerated.
func New(appBaseURL string, client *http.Client) telemetry.Upstream {
	return &Upstream{
		url:    strings.TrimRight(appBaseURL, "/") + pushPath,
		client: client,
	}
}

// Push delivers one telemetry.push batch to the app peer and returns the parsed
// telemetry.ack (REL-090/092). It marshals batch to the REL-090
// {entries, loss_markers} wire shape the Channel built, POSTs it, and decodes
// the {ack_through_seq, loss_markers_acked} response. A transport error or any
// non-2xx status returns an error with a zero Ack — the Channel then leaves the
// batch buffered and retries it (REL-097), so retention is advanced ONLY on an
// ack this method actually returned.
func (u *Upstream) Push(batch telemetry.PushBatch) (telemetry.Ack, error) {
	body, err := json.Marshal(batch)
	if err != nil {
		return telemetry.Ack{}, fmt.Errorf("telemetryhttp: marshal telemetry.push: %w", err)
	}

	resp, err := u.client.Post(u.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return telemetry.Ack{}, fmt.Errorf("telemetryhttp: POST %s: %w", u.url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Not acknowledged: surface a bounded prefix of the body for context and
		// return a zero Ack so the Channel retains and retries the batch (REL-097).
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return telemetry.Ack{}, fmt.Errorf("telemetryhttp: POST %s: status %d: %s",
			u.url, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var ack telemetry.Ack
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return telemetry.Ack{}, fmt.Errorf("telemetryhttp: decode telemetry.ack: %w", err)
	}
	return ack, nil
}
