// Package relaystatus reads a relay's operational state WITHOUT talking to the
// relay.
//
// # Why not an API
//
// The app has an OpenAPI document and `waiveo call` drives every operation in
// it. The relay has nothing of the kind, and inventing one would be the wrong
// answer twice over. It is a device-plane peer, not a management surface: its
// only listener is the player/1 HTTPS server, and the contracts reserve —
// deliberately without defining — a relay-local root socket for privileged local
// operations (`relay/1` REL-019, picked up by security-model SEC-124 for factory
// reset). Bolting a diagnostic REST API onto the player listener would occupy
// that reservation with something nobody specified, and would put an
// unauthenticated read of the whole deployment's topology on a port screens dial.
//
// So this reads the two things that already exist:
//
//  1. The relay's durable operational store — the SQLite file REL-142 scopes a
//     relay's on-disk state to. Opened READ-ONLY, in a separate process, while
//     the relay keeps running: the store is WAL, so a concurrent reader is safe
//     and takes no lock the relay waits on.
//
//  2. The relay's existing unauthenticated `GET /healthz`, which already reports
//     `events/1`'s box.vitals.
//
// Nothing here adds a table, a route, a socket, or a frame type. What that costs
// is honesty about the gap: the relay's CONNECTION state, its clock-trust state,
// its discovered-device set and its per-screen liveness are process memory and
// are invisible from out here. Report.Blind names them rather than letting an
// operator read a clean report as a healthy relay.
//
// # What it never reads
//
// `relay_identity.private_key_pem` is not selected by any query in this file, and
// `player_channel_tokens.token_hash` is counted but never rendered. A diagnostic
// that printed either would be a diagnostic that turns filesystem access into
// protocol access.
package relaystatus

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	_ "modernc.org/sqlite" // the driver internal/relay/identity opens the same file with

	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// DefaultStorePath is where a relay keeps its operational store. It mirrors
// internal/relay/identity.DefaultPath — read from there rather than re-spelled,
// so the two cannot drift — and is relative to the relay's working directory,
// which under the shipped unit is /opt/waiveo-next.
const DefaultStorePath = identity.DefaultPath

// Report is everything this package could learn.
type Report struct {
	StorePath string `json:"store_path"`

	Identity  *IdentityReport `json:"identity,omitempty"`
	Trust     TrustReport     `json:"trust"`
	Applied   *AppliedReport  `json:"applied,omitempty"`
	Clock     *ClockReport    `json:"clock,omitempty"`
	Telemetry TelemetryReport `json:"telemetry"`
	Sessions  SessionsReport  `json:"sessions"`
	Healthz   *HealthzReport  `json:"healthz,omitempty"`
	Problems  []string        `json:"problems,omitempty"`
	Blind     []string        `json:"blind_spots"`
}

// IdentityReport is the relay's enrolled identity, as its certificate states it.
type IdentityReport struct {
	RelayID string `json:"relay_id"`
	// Subject/NotBefore/NotAfter/SPKI are PARSED from the stored leaf rather than
	// stored alongside it. Expiry is the operational fact that matters here — a
	// relay whose leaf lapses stops being able to connect at all — and the store
	// holds no expiry column, so anything that reported one from a field would be
	// reporting a field that does not exist.
	Subject       string `json:"subject"`
	Serial        string `json:"serial"`
	NotBefore     string `json:"not_before"`
	NotAfter      string `json:"not_after"`
	DaysRemaining int    `json:"days_remaining"`
	Expired       bool   `json:"expired"`
	SPKISHA256    string `json:"spki_sha256"`
}

// TrustReport is which trust material the relay captured at enrollment. Both are
// reported as PRESENCE plus a digest, never as bytes: the digest is enough to
// compare two relays or to confirm a rotation landed, and the material itself is
// not a thing a diagnostic needs to print.
type TrustReport struct {
	DesiredStateKey       bool   `json:"desired_state_verification_key"`
	DesiredStateKeySHA256 string `json:"desired_state_verification_key_sha256,omitempty"`
	AppPeerPin            bool   `json:"app_peer_trust_pin"`
	AppPeerPinSHA256      string `json:"app_peer_trust_pin_sha256,omitempty"`
}

// AppliedReport is the last desired-state generation this relay committed —
// REL-055's durable progress marker. It is the single most useful number here:
// it answers "is this relay running what the app published" without the relay
// being reachable.
type AppliedReport struct {
	Generation     int64    `json:"generation"`
	Hash           string   `json:"hash"`
	ScreenIDs      []string `json:"screen_ids"`
	ScreenPrograms int      `json:"screen_programs"`
	RevokedScreens int      `json:"revoked_screens"`
	AdoptedDevices int      `json:"adopted_devices"`
	PackPatterns   int      `json:"pack_match_patterns"`
	ContentRefs    int      `json:"content_refs"`
	DecodeProblems []string `json:"decode_problems,omitempty"`
}

// ClockReport is the persisted advance-only floor (REL-130).
type ClockReport struct {
	FloorMs   int64  `json:"floor_ms"`
	FloorTime string `json:"floor_time"`
	// AheadOfHost is set when the floor is in the future relative to this
	// machine's clock — which means the READER's clock is behind, and is worth
	// saying because it changes how every age in this report should be read.
	AheadOfHost bool `json:"ahead_of_host"`
}

// TelemetryReport is the durable backlog depth. A backlog that only grows is a
// relay that cannot reach its app peer, which is exactly the condition the
// connection state this package cannot see would have told you about.
type TelemetryReport struct {
	Queued       int   `json:"queued"`
	OldestSeq    int64 `json:"oldest_seq,omitempty"`
	NewestSeq    int64 `json:"newest_seq,omitempty"`
	HighWaterSeq int64 `json:"high_water_seq"`
	LossMarkers  int   `json:"loss_markers"`
}

// SessionsReport counts player credentials, never rendering one.
type SessionsReport struct {
	Screens        int `json:"screens"`
	Live           int `json:"live"`
	Expired        int `json:"expired"`
	Terminated     int `json:"terminated"`
	RedeemedGrants int `json:"redeemed_grants"`
	ReportsOwed    int `json:"redemption_reports_owed"`
}

// HealthzReport is the relay's own liveness route, verbatim.
type HealthzReport struct {
	URL               string         `json:"url"`
	Status            int            `json:"http_status"`
	Component         string         `json:"component,omitempty"`
	Reported          string         `json:"status,omitempty"`
	Vitals            map[string]any `json:"vitals,omitempty"`
	VitalsUnavailable []string       `json:"vitals_unavailable,omitempty"`
	Error             string         `json:"error,omitempty"`
	LatencyMs         int64          `json:"latency_ms"`
}

// blindSpots is what a reader of this report must not conclude is fine merely
// because it is absent. Every entry is process memory the inventory of the relay
// confirmed has no durable trace and no accessor outside the process.
var blindSpots = []string{
	"app-peer connection state (connected/backoff/last error) — held only in the running process",
	"clock TRUST state — every boot starts untrusted (REL-131) and the state is never persisted",
	"discovered device candidates (SSDP/mDNS) — deliberately in-memory, not persisted evidence",
	"per-screen liveness, lease acks and render reports — process-lifetime only",
	"loaded edge-rule count and resolved schedule state — process-lifetime only",
}

// Read collects everything readable from the store at path.
//
// A store that does not exist is an ANSWER, not an error: a relay that has never
// enrolled has no file, and that is the most important thing a first-boot
// diagnostic can say. Every other failure is recorded in Problems and the rest of
// the report is still produced, because a corrupt telemetry table should not hide
// a perfectly readable certificate expiry.
func Read(path string) (*Report, error) {
	rep := &Report{StorePath: path, Blind: blindSpots}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		rep.Problems = append(rep.Problems, fmt.Sprintf("no relay store at %s — this relay has never enrolled, or is running from a different working directory", path))
		return rep, nil
	} else if err != nil {
		return nil, fmt.Errorf("relaystatus: stat %s: %w", path, err)
	}

	// mode=ro, and no PRAGMA that writes. identity.Open is deliberately NOT used:
	// it creates tables and runs column migrations, so pointing it at a live
	// relay's store would have this diagnostic MUTATE the thing it is inspecting —
	// and would fail outright against a store owned by another user.
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("relaystatus: open %s read-only: %w", path, err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		// SQLite still needs to create or attach the `-shm` index to read a WAL
		// database, even read-only, so a store whose DIRECTORY this process cannot
		// write is unreadable here — and the driver's message ("attempt to write a
		// readonly database") describes the mechanism rather than the situation.
		// Said plainly, because the fix is a permission and not a flag.
		return nil, fmt.Errorf("relaystatus: open %s read-only: %w — a WAL store needs its `-shm` index, so the directory holding it must be writable by this user even for a read; run as the user the relay runs as", path, err)
	}

	rep.Identity, rep.Trust = readIdentity(db, rep)
	rep.Applied = readApplied(db, rep)
	rep.Clock = readClock(db, rep)
	rep.Telemetry = readTelemetry(db, rep)
	rep.Sessions = readSessions(db, rep)
	return rep, nil
}

func readIdentity(db *sql.DB, rep *Report) (*IdentityReport, TrustReport) {
	var trust TrustReport
	var relayID string
	var certPEM []byte
	// Two columns, named explicitly. `SELECT *` here would pull private_key_pem
	// into this process's memory for no reason at all.
	err := db.QueryRow(`SELECT relay_id, cert_pem FROM relay_identity WHERE id = 1`).Scan(&relayID, &certPEM)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		rep.Problems = append(rep.Problems, "no relay identity row — enrollment has not completed")
		return nil, trust
	case err != nil:
		rep.Problems = append(rep.Problems, "read relay identity: "+err.Error())
		return nil, trust
	}

	id := &IdentityReport{RelayID: relayID}
	if leaf, err := parseLeaf(certPEM); err != nil {
		rep.Problems = append(rep.Problems, "parse enrollment certificate: "+err.Error())
	} else {
		now := time.Now()
		id.Subject = leaf.Subject.String()
		id.Serial = leaf.SerialNumber.String()
		id.NotBefore = leaf.NotBefore.UTC().Format(time.RFC3339)
		id.NotAfter = leaf.NotAfter.UTC().Format(time.RFC3339)
		id.DaysRemaining = int(leaf.NotAfter.Sub(now).Hours() / 24)
		id.Expired = now.After(leaf.NotAfter)
		sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
		id.SPKISHA256 = base64.StdEncoding.EncodeToString(sum[:])
	}

	if raw, ok := readBlob(db, `SELECT public_key FROM desired_state_verification_key WHERE id = 1`, rep); ok {
		trust.DesiredStateKey = true
		trust.DesiredStateKeySHA256 = digest(raw)
	}
	if raw, ok := readBlob(db, `SELECT spki FROM app_peer_trust_pin WHERE id = 1`, rep); ok {
		trust.AppPeerPin = true
		trust.AppPeerPinSHA256 = digest(raw)
	}
	return id, trust
}

func readApplied(db *sql.DB, rep *Report) *AppliedReport {
	var (
		gen                     int64
		hash                    string
		programsRaw, revokedRaw []byte
		inventoryRaw            []byte
	)
	err := db.QueryRow(`SELECT generation, hash, screen_programs, revoked, device_inventory FROM last_applied_generation WHERE id = 1`).
		Scan(&gen, &hash, &programsRaw, &revokedRaw, &inventoryRaw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		rep.Problems = append(rep.Problems, "no last-applied generation — this relay has never applied desired state")
		return nil
	case err != nil:
		rep.Problems = append(rep.Problems, "read last-applied generation: "+err.Error())
		return nil
	}

	out := &AppliedReport{Generation: gen, Hash: hash, ScreenIDs: []string{}}
	var programs []wire.ScreenProgram
	if err := json.Unmarshal(programsRaw, &programs); err != nil {
		out.DecodeProblems = append(out.DecodeProblems, "screen_programs: "+err.Error())
	} else {
		out.ScreenPrograms = len(programs)
		for _, p := range programs {
			out.ScreenIDs = append(out.ScreenIDs, p.ScreenID)
			out.ContentRefs += len(p.Content)
		}
	}
	var revoked []string
	if err := json.Unmarshal(revokedRaw, &revoked); err != nil {
		out.DecodeProblems = append(out.DecodeProblems, "revoked: "+err.Error())
	} else {
		out.RevokedScreens = len(revoked)
	}
	var inv wire.DeviceInventory
	if err := json.Unmarshal(inventoryRaw, &inv); err != nil {
		out.DecodeProblems = append(out.DecodeProblems, "device_inventory: "+err.Error())
	} else {
		out.AdoptedDevices = len(inv.Devices)
		out.PackPatterns = len(inv.PackMatchPatterns)
	}
	return out
}

func readClock(db *sql.DB, rep *Report) *ClockReport {
	var floor int64
	err := db.QueryRow(`SELECT floor_ms FROM clock_floor WHERE id = 1`).Scan(&floor)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		rep.Problems = append(rep.Problems, "read clock floor: "+err.Error())
		return nil
	}
	return &ClockReport{
		FloorMs:     floor,
		FloorTime:   time.UnixMilli(floor).UTC().Format(time.RFC3339),
		AheadOfHost: time.UnixMilli(floor).After(time.Now()),
	}
}

func readTelemetry(db *sql.DB, rep *Report) TelemetryReport {
	var out TelemetryReport
	var oldest, newest sql.NullInt64
	if err := db.QueryRow(`SELECT COUNT(*), MIN(seq), MAX(seq) FROM telemetry_queue`).Scan(&out.Queued, &oldest, &newest); err != nil {
		rep.Problems = append(rep.Problems, "read telemetry queue: "+err.Error())
		return out
	}
	out.OldestSeq, out.NewestSeq = oldest.Int64, newest.Int64
	if err := db.QueryRow(`SELECT seq FROM telemetry_seq_high_water WHERE id = 1`).Scan(&out.HighWaterSeq); err != nil && !errors.Is(err, sql.ErrNoRows) {
		rep.Problems = append(rep.Problems, "read telemetry high water: "+err.Error())
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM telemetry_loss_marker`).Scan(&out.LossMarkers); err != nil {
		rep.Problems = append(rep.Problems, "read telemetry loss markers: "+err.Error())
	}
	return out
}

func readSessions(db *sql.DB, rep *Report) SessionsReport {
	var out SessionsReport
	now := time.Now().UnixMilli()
	// COUNT(DISTINCT screen_id) and three conditional counts. token_hash is never
	// selected: counting credentials is the diagnostic, listing them is not.
	err := db.QueryRow(`
		SELECT COUNT(DISTINCT screen_id),
		       SUM(CASE WHEN terminated_at = 0 AND expires_at >  ? THEN 1 ELSE 0 END),
		       SUM(CASE WHEN terminated_at = 0 AND expires_at <= ? THEN 1 ELSE 0 END),
		       SUM(CASE WHEN terminated_at <> 0 THEN 1 ELSE 0 END)
		FROM player_channel_tokens`, now, now).
		Scan(&out.Screens, nullable(&out.Live), nullable(&out.Expired), nullable(&out.Terminated))
	if err != nil {
		rep.Problems = append(rep.Problems, "read player sessions: "+err.Error())
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM player_redeemed_grants`).Scan(&out.RedeemedGrants); err != nil {
		rep.Problems = append(rep.Problems, "read redeemed grants: "+err.Error())
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM player_redemption_reports`).Scan(&out.ReportsOwed); err != nil {
		rep.Problems = append(rep.Problems, "read owed redemption reports: "+err.Error())
	}
	return out
}

// Healthz probes the relay's own liveness route.
//
// It is the ONLY part of this package that touches the running relay, and it
// uses the route that already exists rather than one added for this. A relay
// serves it under its enrollment leaf — a certificate no public root signed — so
// a caller either supplies the trust to verify it or accepts an unverified read
// of a liveness endpoint, which is what insecure means here and nothing more.
func Healthz(ctx context.Context, url string, tlsCfg *tls.Config) *HealthzReport {
	out := &HealthzReport{URL: url}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	started := time.Now()
	resp, err := client.Do(req)
	out.LatencyMs = time.Since(started).Milliseconds()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer resp.Body.Close()
	out.Status = resp.StatusCode
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		out.Error = err.Error()
		return out
	}
	var payload struct {
		Component         string         `json:"component"`
		Status            string         `json:"status"`
		Vitals            map[string]any `json:"vitals"`
		VitalsUnavailable []string       `json:"vitals_unavailable"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		out.Error = "healthz body is not the expected JSON: " + err.Error()
		return out
	}
	out.Component, out.Reported = payload.Component, payload.Status
	out.Vitals, out.VitalsUnavailable = payload.Vitals, payload.VitalsUnavailable
	return out
}

func parseLeaf(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

func readBlob(db *sql.DB, query string, rep *Report) ([]byte, bool) {
	var raw []byte
	err := db.QueryRow(query).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false
	case err != nil:
		rep.Problems = append(rep.Problems, "read trust material: "+err.Error())
		return nil, false
	}
	return raw, true
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// nullable adapts an int destination to a SUM() that returns NULL over an empty
// table. Without it, a relay with no player sessions at all fails the whole
// query — the one case a fresh box is guaranteed to be in.
func nullable(dst *int) any { return (*nullInt)(dst) }

type nullInt int

func (n *nullInt) Scan(v any) error {
	if v == nil {
		*n = 0
		return nil
	}
	switch t := v.(type) {
	case int64:
		*n = nullInt(t)
	case float64:
		*n = nullInt(t)
	default:
		return fmt.Errorf("relaystatus: unexpected count type %T", v)
	}
	return nil
}
