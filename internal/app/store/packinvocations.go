package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// The pack-invocation queue: how the host calls INTO an extension.
//
// # Why a queue and not a call
//
// This is the one direction plain HTTP cannot serve. An extension is a separate
// OS process with no inbound address the host can dial: it may be starting,
// swapping, or not running at all. So the host does not call the pack — it
// enqueues work, and the pack leases it when it is ready. The invocation is a
// row, which means it survives a restart of either side and an operator can see
// what is outstanding.
//
// # The lease
//
// A leased invocation is claimed by exactly one worker for a bounded time. The
// claim is a compare-and-swap inside the write transaction (`WHERE state =
// 'pending'`), the same discipline grant redemption uses, so two packs polling
// at once cannot both take the same row regardless of timing.
//
// A lease EXPIRES rather than being held forever, because the holder is a
// process that can die between leasing and answering. Expiry returns the row to
// pending — except where that would be wrong, which is the whole of the next
// paragraph.
//
// # Why at-most-once is a property of the ACTION, not of the queue
//
// manifest/1 MAN-103 makes every declared action state an idempotency class,
// and says a `not-idempotent` action MUST NOT be automatically replayed by the
// host's retry or job-recovery machinery. A queue that re-offered every expired
// lease would be exactly that machinery, and it would silently turn "send the
// invoice" into "send the invoice twice" the first time a pack was killed
// mid-handler.
//
// So an expired lease on a `safe-to-retry` invocation returns to pending, and an
// expired lease on a `not-idempotent` one FAILS with a stated reason. The second
// is not a lesser outcome: an operator being told the action's fate is unknown
// is strictly better than the host guessing, and guessing "retry" is the guess
// that causes damage.
const packInvocationsSchema = `
CREATE TABLE IF NOT EXISTS pack_invocations (
	invocation_id  TEXT PRIMARY KEY,
	pack_id        TEXT NOT NULL,
	action         TEXT NOT NULL,
	params         TEXT NOT NULL DEFAULT '{}',
	-- MAN-103's class, copied onto the row at enqueue. Read from the row rather
	-- than re-derived from the manifest at expiry time: the manifest can change
	-- under an update, and the promise made to THIS invocation was the one in
	-- force when it was accepted.
	idempotency    TEXT NOT NULL,
	state          TEXT NOT NULL,
	lease_expires  INTEGER NOT NULL DEFAULT 0,
	result         TEXT NOT NULL DEFAULT '',
	error_code     TEXT NOT NULL DEFAULT '',
	error_message  TEXT NOT NULL DEFAULT '',
	trace_id       TEXT NOT NULL DEFAULT '',
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pack_invocations_claim
	ON pack_invocations (pack_id, state, created_at);
`

// Invocation states.
const (
	// InvocationPending is queued and unclaimed.
	InvocationPending = "pending"
	// InvocationLeased is claimed by a worker until lease_expires.
	InvocationLeased = "leased"
	// InvocationSucceeded and InvocationFailed are terminal.
	InvocationSucceeded = "succeeded"
	InvocationFailed    = "failed"
)

// Idempotency classes (MAN-103).
const (
	IdempotencySafeToRetry   = "safe-to-retry"
	IdempotencyNotIdempotent = "not-idempotent"
)

// PackInvocation is one queued call into a pack.
type PackInvocation struct {
	InvocationID string
	PackID       string
	Action       string
	Params       json.RawMessage
	Idempotency  string
	State        string
	LeaseExpires int64
	Result       json.RawMessage
	ErrorCode    string
	ErrorMessage string
	TraceID      string
	CreatedAt    int64
	UpdatedAt    int64
}

// Terminal reports whether the invocation has an answer.
func (i PackInvocation) Terminal() bool {
	return i.State == InvocationSucceeded || i.State == InvocationFailed
}

// ErrInvocationNotLeased is a result posted against an invocation the caller
// does not hold — already answered, expired, or never leased.
var ErrInvocationNotLeased = errors.New("store: invocation is not leased by this caller")

// EnqueuePackInvocation queues a call into a pack.
func (s *Store) EnqueuePackInvocation(ctx context.Context, in PackInvocation) (PackInvocation, error) {
	if in.Idempotency != IdempotencySafeToRetry && in.Idempotency != IdempotencyNotIdempotent {
		return PackInvocation{}, fmt.Errorf(
			"store: invocation of %s/%s declares idempotency %q, want %s or %s (MAN-103)",
			in.PackID, in.Action, in.Idempotency, IdempotencySafeToRetry, IdempotencyNotIdempotent)
	}
	out := in
	out.State = InvocationPending
	if len(out.Params) == 0 {
		out.Params = json.RawMessage("{}")
	}

	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		// The owning pack must exist. Queuing work for a pack that is not
		// installed would produce a row nothing can ever lease, which reads to an
		// operator as an extension that is ignoring its work.
		if _, found, err := getPack(ctx, tx, in.PackID); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
		now := s.nowMs()
		out.InvocationID = ulid.New()
		out.CreatedAt, out.UpdatedAt = now, now
		_, err := tx.ExecContext(ctx,
			`INSERT INTO pack_invocations
			   (invocation_id, pack_id, action, params, idempotency, state, created_at, updated_at, trace_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			out.InvocationID, out.PackID, out.Action, string(out.Params), out.Idempotency,
			out.State, out.CreatedAt, out.UpdatedAt, out.TraceID)
		return err
	})
	if err != nil {
		return PackInvocation{}, err
	}
	return out, nil
}

// LeasePackInvocation claims the oldest pending invocation for packID, or
// returns ok=false when there is nothing to do.
//
// Expiry is swept FIRST, in the same transaction, so a claim never has to look
// past a row whose holder is gone — and so the sweep happens on the path that is
// already running rather than needing a timer nobody watches.
func (s *Store) LeasePackInvocation(ctx context.Context, packID string, leaseMs int64) (PackInvocation, bool, error) {
	var out PackInvocation
	var found bool

	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMs()
		if err := expireLeases(ctx, tx, packID, now); err != nil {
			return err
		}

		row := tx.QueryRowContext(ctx,
			`SELECT invocation_id, pack_id, action, params, idempotency, state,
			        lease_expires, trace_id, created_at, updated_at
			   FROM pack_invocations
			  WHERE pack_id = ? AND state = ?
			  ORDER BY created_at ASC, invocation_id ASC
			  LIMIT 1`, packID, InvocationPending)
		var inv PackInvocation
		var params string
		if err := row.Scan(&inv.InvocationID, &inv.PackID, &inv.Action, &params, &inv.Idempotency,
			&inv.State, &inv.LeaseExpires, &inv.TraceID, &inv.CreatedAt, &inv.UpdatedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil // nothing pending; found stays false
			}
			return fmt.Errorf("store: read pending invocation: %w", err)
		}
		inv.Params = json.RawMessage(params)

		// Compare-and-swap on the state we read. Two workers polling at once
		// cannot both take the row: the second's UPDATE matches nothing.
		res, err := tx.ExecContext(ctx,
			`UPDATE pack_invocations
			    SET state = ?, lease_expires = ?, updated_at = ?
			  WHERE invocation_id = ? AND state = ?`,
			InvocationLeased, now+leaseMs, now, inv.InvocationID, InvocationPending)
		if err != nil {
			return fmt.Errorf("store: lease invocation: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // another worker took it between the read and the write
		}

		inv.State, inv.LeaseExpires, inv.UpdatedAt = InvocationLeased, now+leaseMs, now
		out, found = inv, true
		return nil
	})
	if err != nil {
		return PackInvocation{}, false, err
	}
	return out, found, nil
}

// expireLeases resolves leases whose holder never came back.
//
// The two idempotency classes go different ways, and that split is MAN-103
// rather than a tuning choice: a `safe-to-retry` invocation returns to pending
// so another worker can take it, and a `not-idempotent` one FAILS, because the
// host cannot know whether the dead holder had already performed it and
// re-offering it would be exactly the automatic replay MAN-103 forbids.
func expireLeases(ctx context.Context, tx *sql.Tx, packID string, now int64) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE pack_invocations
		    SET state = ?, lease_expires = 0, updated_at = ?
		  WHERE pack_id = ? AND state = ? AND lease_expires <= ? AND idempotency = ?`,
		InvocationPending, now, packID, InvocationLeased, now, IdempotencySafeToRetry); err != nil {
		return fmt.Errorf("store: return expired retryable leases: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE pack_invocations
		    SET state = ?, lease_expires = 0, updated_at = ?,
		        error_code = 'LEASE_EXPIRED',
		        error_message = 'the extension took this action and did not report back before its lease expired; whether it ran is unknown, and a not-idempotent action is never replayed automatically (manifest/1 MAN-103)'
		  WHERE pack_id = ? AND state = ? AND lease_expires <= ? AND idempotency = ?`,
		InvocationFailed, now, packID, InvocationLeased, now, IdempotencyNotIdempotent); err != nil {
		return fmt.Errorf("store: fail expired non-idempotent leases: %w", err)
	}
	return nil
}

// CompletePackInvocation records a leased invocation's outcome.
//
// Refused unless the row is still LEASED. A result arriving after the lease
// expired is a worker reporting on work the queue has already resolved — and
// accepting it would let a `not-idempotent` invocation that was failed at expiry
// flip to succeeded, erasing the very uncertainty the failure was recording.
func (s *Store) CompletePackInvocation(ctx context.Context, invocationID string, result json.RawMessage, errCode, errMessage string) (PackInvocation, error) {
	var out PackInvocation
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := s.nowMs()
		state := InvocationSucceeded
		if errCode != "" {
			state = InvocationFailed
		}
		if len(result) == 0 {
			result = json.RawMessage("{}")
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE pack_invocations
			    SET state = ?, result = ?, error_code = ?, error_message = ?, lease_expires = 0, updated_at = ?
			  WHERE invocation_id = ? AND state = ?`,
			state, string(result), errCode, errMessage, now, invocationID, InvocationLeased)
		if err != nil {
			return fmt.Errorf("store: complete invocation: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrInvocationNotLeased
		}
		inv, err := readInvocation(ctx, tx, invocationID)
		if err != nil {
			return err
		}
		out = inv
		return nil
	})
	if err != nil {
		return PackInvocation{}, err
	}
	return out, nil
}

// GetPackInvocation reads one invocation, resolving any expired lease first so a
// caller polling for an outcome sees the same state a lease attempt would.
func (s *Store) GetPackInvocation(ctx context.Context, invocationID string) (PackInvocation, error) {
	var out PackInvocation
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		inv, err := readInvocation(ctx, tx, invocationID)
		if err != nil {
			return err
		}
		if inv.State == InvocationLeased && inv.LeaseExpires <= s.nowMs() {
			if err := expireLeases(ctx, tx, inv.PackID, s.nowMs()); err != nil {
				return err
			}
			if inv, err = readInvocation(ctx, tx, invocationID); err != nil {
				return err
			}
		}
		out = inv
		return nil
	})
	if err != nil {
		return PackInvocation{}, err
	}
	return out, nil
}

func readInvocation(ctx context.Context, tx *sql.Tx, invocationID string) (PackInvocation, error) {
	var inv PackInvocation
	var params, result string
	err := tx.QueryRowContext(ctx,
		`SELECT invocation_id, pack_id, action, params, idempotency, state, lease_expires,
		        result, error_code, error_message, trace_id, created_at, updated_at
		   FROM pack_invocations WHERE invocation_id = ?`, invocationID,
	).Scan(&inv.InvocationID, &inv.PackID, &inv.Action, &params, &inv.Idempotency, &inv.State,
		&inv.LeaseExpires, &result, &inv.ErrorCode, &inv.ErrorMessage, &inv.TraceID,
		&inv.CreatedAt, &inv.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PackInvocation{}, ErrNotFound
	}
	if err != nil {
		return PackInvocation{}, fmt.Errorf("store: read invocation %s: %w", invocationID, err)
	}
	inv.Params, inv.Result = json.RawMessage(params), json.RawMessage(result)
	return inv, nil
}
