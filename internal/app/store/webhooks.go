package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// webhooks.go is the private half of an events/1 outbound webhook endpoint: its
// signing secrets and its delivery progress (EVT-153-155, EVT-158).
//
// It is a separate table from the endpoint's own resource row on purpose, and
// the split is a security boundary rather than a normalization preference. The
// generic resource handlers serve a row's stored `body` bytes back VERBATIM
// (api.go's get/list/patch all write res.Body straight out), so anything
// carried in that body is disclosed to every principal who may read the row. A
// signing secret in the body would therefore be a signing secret published on
// the read path, and no amount of care in a handler could take it back — the
// only reliable fix is for the secret never to be in the served representation
// at all.
//
// The secrets here are RECOVERABLE, not comparable: the platform is the SIGNER,
// so it must read the exact bytes back to compute an HMAC. That rules out
// hashing and puts them under internal/shared/secretseal, sealed by the caller
// (internal/app/webhookdeliver) under a per-row AAD before they ever reach this
// file. This file stores the sealed base64 opaquely and holds no key — the same
// division the auth package's TOTP secret uses, where the store persists a
// sealed column and the sealing key lives with the workspace key.
//
// Two deliberate departures from the resource-table CRUD, both matching the
// jobs subsystem's own reasoning:
//
//   - Delivery state is NOT a resource Kind. It carries no revision, no
//     external_id and no labels of its own; it is execution state ABOUT another
//     row, and modelling it as a Kind would hand it an ETag/If-Match surface and
//     a place in the desired-state projection that nothing gives it a meaning
//     for.
//   - A delivery-state write does NOT bump the store generation, and so runs
//     through runWriteTx rather than writeTx. The generation is the feeder's
//     desired-state cursor (REL-052): a webhook endpoint's progress through the
//     event log is not desired state, and bumping it would nudge every connected
//     relay to re-fetch a snapshot that did not change every time one delivery
//     landed.

// webhooksSchema is the delivery-state table. One row per registered endpoint,
// keyed by the endpoint's own id — the row is created on demand (the first
// secret write or the first delivery attempt), so an endpoint that has never
// been given a secret simply has no row rather than a row full of zero values
// that reads like a configured endpoint.
const webhooksSchema = `
CREATE TABLE IF NOT EXISTS webhook_delivery_state (
	endpoint_id          TEXT PRIMARY KEY,
	status               TEXT NOT NULL DEFAULT 'active',
	consecutive_failures INTEGER NOT NULL DEFAULT 0,
	last_delivered_id    TEXT NOT NULL DEFAULT '',
	attempt              INTEGER NOT NULL DEFAULT 0,
	delivery_id          TEXT NOT NULL DEFAULT '',
	next_attempt_at_ms   INTEGER NOT NULL DEFAULT 0,
	sealed_secret        TEXT NOT NULL DEFAULT '',
	sealed_prior_secret  TEXT NOT NULL DEFAULT '',
	rotated_at_ms        INTEGER NOT NULL DEFAULT 0,
	updated_at           INTEGER NOT NULL
);
`

// WebhookDeliveryState is one endpoint's persisted delivery state.
//
// The secret columns are the SEALED base64 blobs, never plaintext: this type
// crosses no trust boundary where a plaintext secret would be appropriate, and
// naming the fields for what they hold is what stops a caller from putting a
// raw secret in one by accident.
type WebhookDeliveryState struct {
	EndpointID string
	// Status is events.EndpointActive or events.EndpointDisabled (EVT-154).
	Status string
	// ConsecutiveFailures counts fully-exhausted deliveries in an unbroken run
	// (EVT-154), reset by any success.
	ConsecutiveFailures int
	// LastDeliveredID is the endpoint's resume cursor (EVT-155) — the id of the
	// most recent envelope a delivery attempt for actually succeeded. It
	// advances only after a 2xx, which is what makes delivery at-least-once
	// (EVT-156) rather than at-most-once across a crash.
	LastDeliveredID string
	// Attempt counts the retries of the CURRENT logical delivery (EVT-153),
	// reset when that delivery either succeeds or exhausts its budget.
	Attempt int
	// DeliveryID is the current logical delivery's X-Waiveo-Delivery-Id, held
	// here so it stays stable across the retries of one delivery (EVT-151) even
	// when those retries span a restart.
	DeliveryID string
	// NextAttemptAtMs is the earliest instant the next attempt may be made —
	// the backoff gate. A failing endpoint sits behind this while healthy ones
	// keep being served on the same pass.
	NextAttemptAtMs int64
	// SealedSecret / SealedPriorSecret are secretseal blobs. SealedPriorSecret
	// is empty until the first rotation.
	SealedSecret      string
	SealedPriorSecret string
	// RotatedAtMs is when the current secret took over — the instant EVT-158's
	// overlap window is measured from. Zero before any rotation.
	RotatedAtMs int64
	UpdatedAt   int64
}

// ErrWebhookStateNotFound is returned for an endpoint with no delivery-state
// row yet: one that was registered but never given a signing secret, and that
// no delivery has ever been attempted for.
var ErrWebhookStateNotFound = errors.New("store: this webhook endpoint has no delivery state")

const webhookStateColumns = `endpoint_id, status, consecutive_failures, last_delivered_id,
	attempt, delivery_id, next_attempt_at_ms, sealed_secret, sealed_prior_secret, rotated_at_ms, updated_at`

// WebhookDeliveryStateFor returns one endpoint's delivery state, or
// ErrWebhookStateNotFound.
func (s *Store) WebhookDeliveryStateFor(ctx context.Context, endpointID string) (WebhookDeliveryState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRowContext(ctx,
		`SELECT `+webhookStateColumns+` FROM webhook_delivery_state WHERE endpoint_id = ?`, endpointID)
	st, err := scanWebhookState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return WebhookDeliveryState{}, ErrWebhookStateNotFound
	}
	if err != nil {
		return WebhookDeliveryState{}, fmt.Errorf("store: read webhook delivery state: %w", err)
	}
	return st, nil
}

// WebhookDeliveryStates returns every endpoint's delivery state, in endpoint-id
// order — the inventory the delivery loop walks on each pass.
func (s *Store) WebhookDeliveryStates(ctx context.Context) ([]WebhookDeliveryState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+webhookStateColumns+` FROM webhook_delivery_state ORDER BY endpoint_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list webhook delivery state: %w", err)
	}
	defer rows.Close()

	var out []WebhookDeliveryState
	for rows.Next() {
		st, err := scanWebhookState(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan webhook delivery state: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list webhook delivery state: %w", err)
	}
	return out, nil
}

// PutWebhookDeliveryProgress writes back the delivery-progress half of an
// endpoint's state, leaving the secret columns exactly as they are.
//
// The two halves are written by different actors on different schedules — the
// delivery loop moves progress after every attempt, the api layer rotates a
// secret when an operator says so — and a single whole-row upsert would let
// whichever wrote last silently revert the other. Splitting the writes at the
// column boundary that matches the ownership boundary is what stops a delivery
// attempt in flight across a rotation from restoring the superseded secret.
//
// The row is created if absent, so the loop never has to check first.
func (s *Store) PutWebhookDeliveryProgress(ctx context.Context, st WebhookDeliveryState) error {
	if st.EndpointID == "" {
		return errors.New("store: webhook delivery progress needs an endpoint id")
	}
	now := s.nowMs()
	return s.runWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO webhook_delivery_state
			   (endpoint_id, status, consecutive_failures, last_delivered_id, attempt, delivery_id, next_attempt_at_ms, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(endpoint_id) DO UPDATE SET
			   status               = excluded.status,
			   consecutive_failures = excluded.consecutive_failures,
			   last_delivered_id    = excluded.last_delivered_id,
			   attempt              = excluded.attempt,
			   delivery_id          = excluded.delivery_id,
			   next_attempt_at_ms   = excluded.next_attempt_at_ms,
			   updated_at           = excluded.updated_at`,
			st.EndpointID, st.Status, st.ConsecutiveFailures, st.LastDeliveredID,
			st.Attempt, st.DeliveryID, st.NextAttemptAtMs, now)
		if err != nil {
			return fmt.Errorf("store: write webhook delivery progress: %w", err)
		}
		return nil
	})
}

// RotateWebhookSecret installs sealedSecret as the endpoint's current signing
// secret at atMs, demoting whatever was current to the prior slot (EVT-158).
//
// Only the immediately prior secret is ever kept: a second rotation inside the
// first's overlap window replaces it, which is exactly EVT-158's "the current or
// the immediately prior secret" and never two generations back. The FIRST call
// for an endpoint leaves the prior slot empty — there is no earlier secret a
// receiver could still be holding, so there is no overlap to open.
func (s *Store) RotateWebhookSecret(ctx context.Context, endpointID, sealedSecret string, atMs int64) error {
	if endpointID == "" {
		return errors.New("store: rotating a webhook secret needs an endpoint id")
	}
	if sealedSecret == "" {
		return errors.New("store: refusing to install an empty webhook signing secret")
	}
	now := s.nowMs()
	return s.runWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO webhook_delivery_state (endpoint_id, sealed_secret, sealed_prior_secret, rotated_at_ms, updated_at)
			 VALUES (?, ?, '', ?, ?)
			 ON CONFLICT(endpoint_id) DO UPDATE SET
			   sealed_prior_secret = webhook_delivery_state.sealed_secret,
			   sealed_secret       = excluded.sealed_secret,
			   rotated_at_ms       = excluded.rotated_at_ms,
			   updated_at          = excluded.updated_at`,
			endpointID, sealedSecret, atMs, now)
		if err != nil {
			return fmt.Errorf("store: rotate webhook signing secret: %w", err)
		}
		return nil
	})
}

// DeleteWebhookDeliveryState removes an endpoint's delivery state, secrets and
// all. Deleting a state row that is not there is not an error — the caller is
// reconciling, not asserting.
func (s *Store) DeleteWebhookDeliveryState(ctx context.Context, endpointID string) error {
	return s.runWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM webhook_delivery_state WHERE endpoint_id = ?`, endpointID); err != nil {
			return fmt.Errorf("store: delete webhook delivery state: %w", err)
		}
		return nil
	})
}

// rowScanner is the one method *sql.Row and *sql.Rows share, so one scan
// function serves both the single-row read and the list.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanWebhookState(sc rowScanner) (WebhookDeliveryState, error) {
	var st WebhookDeliveryState
	err := sc.Scan(&st.EndpointID, &st.Status, &st.ConsecutiveFailures, &st.LastDeliveredID,
		&st.Attempt, &st.DeliveryID, &st.NextAttemptAtMs,
		&st.SealedSecret, &st.SealedPriorSecret, &st.RotatedAtMs, &st.UpdatedAt)
	return st, err
}
