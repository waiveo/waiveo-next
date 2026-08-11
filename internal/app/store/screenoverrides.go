package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// screenoverrides.go is the store's PUSH-NOW subsystem: the per-screen "show
// this here, now" override an operator sets from the console, which outranks
// whatever the schedule would otherwise resolve for that screen until it is
// explicitly cleared.
//
// # Why this exists at all
//
// waiveo-next's content delivery is schedule-driven and pull-only, which is the
// right default and is strictly more capable than the legacy stack for planned
// content. What it had no expression of is the single most common unplanned
// signage action: an operator standing in front of a wall who needs THIS cast on
// THAT screen right now — legacy's PreviewManager/notifyScreenTune. Authoring a
// one-off daypart to say it is not the same act, is not reversible in one click,
// and permanently pollutes the schedule with an emergency.
//
// # Why it is desired state, and not a live command
//
// A push-now could have been a fire-and-forget frame down the relay connection,
// and that would have been both faster to write and wrong. What a screen is
// showing has to survive: a relay restart, a power cut, and an app-peer outage
// all leave the screen playing, and a live-frame override would silently
// evaporate at each of them and drop the screen back onto its schedule with
// nobody told. Persisting it as desired state means the override rides the
// signed snapshot like everything else (relay/1 REL-061), the relay keeps
// serving it while disconnected (REL-055), and it is still in force after a
// reboot — which is what an operator who pushed an emergency notice to a lobby
// expects.
//
// It is also what makes it PROMPT with no new push channel. Every write here
// bumps the generation, so the post-commit hook nudges every live relay
// (REL-057), the relay re-pulls and applies within its own apply path, and the
// screen picks the new Lease up on its next ordinary program poll (~10s,
// PLY-082). "Now" in signage terms is the next poll, and the poll already exists.
//
// # Why a dedicated table and not a resource Kind
//
// An override is set and cleared, never PATCHed; it carries no revision to
// condition a write on, no external_id, and no placement of its own (it
// borrows the screen row's). It is keyed by screen_id rather than by an id of
// its own precisely because a screen has AT MOST ONE — "show this here now"
// twice in a row means the second one wins, not that the screen now has two
// overrides to reconcile. The generic CRUD machinery models none of that.
// Same reasoning pairinggrants.go and jobs.go each record for themselves.
const screenOverridesSchema = `
CREATE TABLE IF NOT EXISTS screen_overrides (
	screen_id   TEXT PRIMARY KEY,
	cast_id     TEXT NOT NULL DEFAULT '',
	playlist_id TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL
);
`

// ScreenOverride is one screen's active push-now assignment: exactly one of
// CastID/PlaylistID is set (SetScreenOverride refuses anything else), and
// CreatedAt is the epoch-ms instant it was pushed — which is what lets a console
// show an operator "pushed 4 minutes ago" beside the screen it is overriding,
// so an override left on by mistake is visible rather than merely in force.
type ScreenOverride struct {
	ScreenID   string
	CastID     string
	PlaylistID string
	CreatedAt  int64
}

// ErrScreenOverrideScreenUnknown reports a SetScreenOverride naming a screen
// identity row this store does not hold — the caller's 404, decided inside the
// write transaction so an override can never be persisted against a screen that
// was deleted between the caller's read and this write. An orphan override would
// be invisible in every console (nothing lists it) and would keep no screen from
// resolving, but it would also never be clearable through the screen it names.
var ErrScreenOverrideScreenUnknown = errors.New("store: screen override names an unknown screen row")

// ErrScreenOverrideTargetUnknown reports a SetScreenOverride naming a cast or
// playlist row this store does not hold. Refused rather than persisted: an
// override pointing at nothing projects to an EMPTY content list, which reaches
// the screen as a program with no content at all — a black screen an operator
// asked to show something on, with no error anywhere to explain it. The refusal
// is the same in-transaction shape the screen check is, and for the same reason:
// the row could be deleted between a handler's validation read and this write.
var ErrScreenOverrideTargetUnknown = errors.New("store: screen override names an unknown cast or playlist row")

// SetScreenOverride installs (or replaces) screenID's push-now override and
// bumps the desired-state generation, so the very next snapshot carries the
// screen a `preempt`-priority program (REL-061/PLY-108) and the post-commit hook
// nudges every live relay connection (REL-057).
//
// Exactly one of castID/playlistID must be non-empty. Both, or neither, is a
// programming error rather than an operator one — a caller that cannot say what
// to show has not made a request — and is refused here rather than resolved by
// precedence, because a silent "cast wins over playlist" rule would make a
// console bug present as the wrong content on a wall.
//
// Replacing is a plain upsert with no revision check, and that is deliberate for
// a resource whose whole semantics are "the latest instruction wins": two
// operators pushing different casts to one screen within a second of each other
// should leave the screen showing the later one, not leave the second operator
// with a 412 about a row they never read.
func (s *Store) SetScreenOverride(ctx context.Context, screenID, castID, playlistID string) (ScreenOverride, error) {
	if screenID == "" {
		return ScreenOverride{}, fmt.Errorf("store: SetScreenOverride: screen_id must not be empty")
	}
	if (castID == "") == (playlistID == "") {
		return ScreenOverride{}, fmt.Errorf("store: SetScreenOverride: exactly one of cast_id/playlist_id must be set")
	}
	written := ScreenOverride{ScreenID: screenID, CastID: castID, PlaylistID: playlistID}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		if err := requireRowExists(ctx, tx, string(KindScreen), screenID, ErrScreenOverrideScreenUnknown); err != nil {
			return err
		}
		target, kind := castID, KindCast
		if target == "" {
			target, kind = playlistID, KindPlaylist
		}
		if err := requireRowExists(ctx, tx, string(kind), target, ErrScreenOverrideTargetUnknown); err != nil {
			return err
		}
		// INSERT OR REPLACE, keyed on the screen: one override per screen, the
		// latest instruction winning (see this function's own doc).
		written.CreatedAt = s.nowMs()
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO screen_overrides (screen_id, cast_id, playlist_id, created_at) VALUES (?, ?, ?, ?)`,
			written.ScreenID, written.CastID, written.PlaylistID, written.CreatedAt,
		); err != nil {
			return fmt.Errorf("insert screen override: %w", err)
		}
		return bumpGeneration(ctx, tx)
	})
	if err != nil {
		return ScreenOverride{}, err
	}
	return written, nil
}

// ClearScreenOverride removes screenID's push-now override, returning whether
// one was actually removed, and bumps the desired-state generation so the screen
// falls back to its schedule on the next generation the relays apply.
//
// The generation bump is skipped when nothing was removed. Clearing an override
// that is not there is a no-op an operator can perform freely (a double-click on
// "return to schedule", a stale console view), and turning each one into a fresh
// generation would make every relay on the site re-pull, re-install its programs
// and re-resolve its schedule for a change that did not happen — REL-070 exists
// to suppress exactly that kind of empty apply, and not generating it in the
// first place is better than suppressing it downstream.
func (s *Store) ClearScreenOverride(ctx context.Context, screenID string) (cleared bool, err error) {
	if screenID == "" {
		return false, fmt.Errorf("store: ClearScreenOverride: screen_id must not be empty")
	}
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM screen_overrides WHERE screen_id = ?`, screenID)
		if err != nil {
			return fmt.Errorf("delete screen override: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete screen override: rows affected: %w", err)
		}
		if n == 0 {
			return nil
		}
		cleared = true
		return bumpGeneration(ctx, tx)
	})
	if err != nil {
		return false, err
	}
	return cleared, nil
}

// ScreenOverrides returns every active override keyed by screen_id, under the
// store's read lock — the console-facing read, so a screens page can show which
// screens are currently overridden and offer to clear them.
//
// The DesiredState projection does NOT go through this method: it reads inside
// its own single lock section (readScreenOverrides), for the reason that
// function's own doc gives.
func (s *Store) ScreenOverrides(ctx context.Context) (map[string]ScreenOverride, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readScreenOverrides(ctx, s.db)
}

// readScreenOverrides reads the whole override table into a screen_id-keyed map.
// It takes a queryer rather than the Store so DesiredState can call it INSIDE
// its own single s.mu.RLock() section: composing DesiredState out of separately
// locked public methods would let a write commit (and bump the generation) in
// the gap between two of them, binding a stale generation to fresher content and
// breaking the (generation, hash) signing invariant REL-053/075 rests on.
//
// A map rather than a slice because every consumer asks the same question — "is
// THIS screen overridden?" — once per screen while walking the screen rows, and
// a slice would make that a linear scan per screen.
func readScreenOverrides(ctx context.Context, q queryer) (map[string]ScreenOverride, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT screen_id, cast_id, playlist_id, created_at FROM screen_overrides ORDER BY screen_id`)
	if err != nil {
		return nil, fmt.Errorf("store: read screen overrides: %w", err)
	}
	defer rows.Close()

	out := map[string]ScreenOverride{}
	for rows.Next() {
		var o ScreenOverride
		if err := rows.Scan(&o.ScreenID, &o.CastID, &o.PlaylistID, &o.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan screen override: %w", err)
		}
		out[o.ScreenID] = o
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate screen overrides: %w", err)
	}
	return out, nil
}

// requireRowExists returns notFound when table holds no row with this id, and a
// wrapped storage error for anything else. It exists so the two referential
// checks above read identically and so both run INSIDE the write transaction:
// checking outside it would leave the window this guard is for — the referenced
// row deleted between the check and the insert — wide open.
func requireRowExists(ctx context.Context, tx *sql.Tx, table, id string, notFound error) error {
	var one int
	// The table name is a Kind constant from this package, never caller input,
	// so it is interpolated exactly as every other statement in this store
	// interpolates one (tableFor's own contract); the id is bound.
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT 1 FROM %s WHERE id = ?`, table), id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound
	}
	if err != nil {
		return fmt.Errorf("check %s row %s: %w", table, id, err)
	}
	return nil
}
