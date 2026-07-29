package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/maaxton/waiveo-next/internal/shared/secretfile"
)

// clockfloor.go is the APP-TIER persisted monotonic clock floor security-model/1
// SEC-066 requires, its advance gate (SEC-067), and the `trusted`/`untrusted`
// accuracy assessment derived from the two (SEC-068).
//
// It is deliberately the app's own component and not a reuse of
// internal/relay/identity's relay-side floor. SEC-066 says so in its own text:
// "this section is the app-side owner of the floor and the assessment SEC-062
// surfaces, distinct from relay/1's relay-side floor and never a restatement of
// relay/1's own exchange." The two live in different processes, persist to
// different files, and advance from different evidence — a relay advances from a
// timestamp signed under its enrollment-anchored key, an app from whatever
// authenticated time source its deployment tier trusts.
//
// # Two decisions worth stating rather than leaving to be inferred
//
//  1. ONLY the floor is persisted; the "have I verified time this boot"
//     bit is NOT. That is not an omission — it is what SEC-068's own definition
//     of the assessment forces. `untrusted` is defined as "while it holds no
//     independently verified time above the floor". After a restart the app
//     holds a floor it INHERITED, not one it has verified in this boot, so it
//     holds no independently verified time above that floor and the honest
//     reading is `untrusted`. Persisting `trusted` across a restart would let a
//     box that verified time once in its life report `trusted` forever, which is
//     exactly the claim SEC-062 exists to let an operator disbelieve.
//
//  2. Now() CLAMPS but never PERSISTS. A bare host wall-clock reading is the one
//     value SEC-067 names outright as unable to advance the floor ("never a bare
//     host wall-clock reading"), so a clock that quietly wrote max(wall, floor)
//     back to disk on every read would launder exactly the input the advance gate
//     is written to reject. The floor moves only through Advance, and only from a
//     source the caller has vouched as independently verifiable.

// TimeSource classifies the evidence behind a candidate time value offered to
// the clock floor (SEC-067). It is a closed two-value type rather than a bool so
// a call site names what it is asserting — `TimeSourceVerifiable` is a claim
// about provenance the caller is making, and a bare `true` at a call site makes
// that claim invisible in review.
type TimeSource string

const (
	// TimeSourceVerifiable is a time value the app can verify independent of an
	// unauthenticated claim: authenticated external time (an authenticated
	// network-time protocol) or a platform-signed timestamp (SEC-067). Only this
	// source may advance the floor.
	TimeSourceVerifiable TimeSource = "verifiable"
	// TimeSourceUnauthenticated is any unverifiable candidate — a bare host
	// wall-clock reading, a client-supplied time claim, a peer's unsigned hint.
	// It can never advance the floor and can never flip the assessment.
	TimeSourceUnauthenticated TimeSource = "unauthenticated"
)

// clockFloorFile is the persisted floor's filename inside the app's auth-state
// directory. It rides beside the auth database because they share a lifecycle:
// a factory reset that destroys the credential store has no business leaving a
// clock floor behind, and an operator locating one has located the other.
//
// That shared lifecycle is ENFORCED, not merely asserted: a Store opened with
// WithClockFloor destroys this file in DestroyLocalAuthState (destroy.go), which
// is the auth tier's half of SEC-121's factory reset. It said so here for a
// while before any code did it, which is how a re-provisioned box could inherit
// the previous owner's ratchet.
const clockFloorFile = "clock-floor.json"

// clockFloorState is the on-disk shape. It is JSON with a named field rather
// than a bare integer so a later minor can add the provenance of the last
// verified advance without a format break, and so a human reading the file can
// tell what the number means.
type clockFloorState struct {
	FloorMs int64 `json:"floor_ms"`
}

// ClockFloor is the app's persisted best-known-time lower bound plus the
// accuracy assessment derived from it. It is safe for concurrent use: the
// assessment is read on the audit path of every login failure (SEC-091) while an
// advance may be arriving from a time-sync path on another goroutine.
//
// The zero value is not usable; construct with OpenClockFloor.
type ClockFloor struct {
	mu   sync.Mutex
	path string
	wall func() int64
	// floorMs is the persisted lower bound. Zero means "no floor has ever been
	// established", which is a real and distinct state from "the floor is the
	// epoch": a box that has never learned verified time clamps nothing.
	floorMs int64
	// verified records whether THIS BOOT has accepted a verifiable time value
	// above the floor. See the file doc for why it is not persisted.
	verified bool
}

// OpenClockFloor loads (or initializes) the clock floor persisted in dir and
// returns it reading wall as the host clock. A missing file is not an error: a
// box that has never established a floor has none, and inventing one from the
// host clock would be exactly the unverified advance SEC-067 forbids.
//
// A CORRUPT file is also not fatal, and that is deliberate. The floor is a
// safety lower bound, not a credential: refusing to boot over an unparseable one
// would turn a damaged byte into an unadministrable box, which is strictly worse
// than booting with no floor and reporting `untrusted` — the state the app is
// already required to handle.
func OpenClockFloor(dir string, wall func() int64) (*ClockFloor, error) {
	if wall == nil {
		return nil, errors.New("auth: OpenClockFloor requires a host clock")
	}
	if dir == "" {
		return nil, errors.New("auth: OpenClockFloor requires a directory")
	}
	if err := secretfile.EnsureDir(dir); err != nil {
		return nil, fmt.Errorf("auth: clock floor dir: %w", err)
	}
	c := &ClockFloor{path: filepath.Join(dir, clockFloorFile), wall: wall}
	data, err := os.ReadFile(c.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return c, nil
	case err != nil:
		return nil, fmt.Errorf("auth: read clock floor: %w", err)
	}
	var st clockFloorState
	if err := json.Unmarshal(data, &st); err != nil || st.FloorMs < 0 {
		return c, nil
	}
	c.floorMs = st.FloorMs
	return c, nil
}

// Now is the app's notion of current time: the LATER of the host wall clock and
// the persisted floor (SEC-066).
//
// This is the whole of SEC-066's restart guarantee. A host clock rolled back
// below time the app has already verified cannot walk the app's own clock
// backwards with it, so a time-windowed check — a TOTP step, a grant `ttl`
// (SEC-032/035) — cannot be re-opened by turning the box's clock back.
func (c *ClockFloor) Now() int64 {
	wall := c.wall()
	c.mu.Lock()
	floor := c.floorMs
	c.mu.Unlock()
	if floor > wall {
		return floor
	}
	return wall
}

// FloorMs returns the persisted floor, or 0 when none has ever been
// established.
func (c *ClockFloor) FloorMs() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.floorMs
}

// Advance offers tsMs as a candidate floor from src and reports whether the
// floor actually moved (SEC-067).
//
// It returns false — with no error — in the two ordinary refusals, because
// neither is a failure of this call:
//
//   - src is TimeSourceUnauthenticated. "An unauthenticated time claim MUST NOT
//     by itself advance the floor." The value is discarded whatever its
//     magnitude; a claim of the year 2030 is refused exactly as a claim of
//     yesterday is, since the attack this gate stops is a FORWARD jump that
//     retires a live credential or expires a live grant.
//   - tsMs is at or below the current floor. The floor "never moves backward"
//     (SEC-066), so a lower verified reading is simply not news.
func (c *ClockFloor) Advance(tsMs int64, src TimeSource) (bool, error) {
	if src != TimeSourceVerifiable {
		return false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if tsMs <= c.floorMs {
		return false, nil
	}
	prev := c.floorMs
	c.floorMs = tsMs
	if err := c.persistLocked(); err != nil {
		c.floorMs = prev
		return false, err
	}
	// The assessment flips only on an advance that actually happened: SEC-068
	// defines `trusted` as holding "independently verified time ABOVE the floor",
	// so a verifiable value that did not clear the floor has not established one.
	c.verified = true
	return true, nil
}

// Assessment is SEC-068's `trusted`/`untrusted` clock-accuracy reading:
// `untrusted` while the app holds no independently verified time above its
// floor, `trusted` once it does. It is the value `waiveo auth recover` surfaces
// before proceeding (SEC-062) and the one every login-failure `audit.event`
// carries (SEC-091).
func (c *ClockFloor) Assessment() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.verified {
		return ClockTrusted
	}
	return ClockUntrusted
}

// Reset discards the persisted floor and the current boot's verification — the
// console binding's clock-floor reset verb (SEC-075), and the only sanctioned
// way the floor ever moves DOWN.
//
// It exists because a floor is a one-way ratchet by design, which means a single
// bad verified reading (a time source that was authenticated and wrong) would
// otherwise strand a deployment in the future forever. SEC-075 puts the escape
// hatch behind the console binding precisely because uid-0 can already edit the
// file this method removes, so the verb adds an audit record and a typed
// operation rather than new reach (SEC-076).
func (c *ClockFloor) Reset() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.floorMs = 0
	c.verified = false
	if err := os.Remove(c.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("auth: reset clock floor: %w", err)
	}
	return nil
}

// persistLocked writes the floor through the shared secret-file discipline:
// 0600, installed by rename, so a crash mid-write leaves either the old floor or
// the new one and never a truncated file. The floor is not itself a secret, but
// it is security state in a directory that is entirely 0600 already, and an
// atomic install is the property that actually matters here.
func (c *ClockFloor) persistLocked() error {
	data, err := json.Marshal(clockFloorState{FloorMs: c.floorMs})
	if err != nil {
		return fmt.Errorf("auth: encode clock floor: %w", err)
	}
	if err := secretfile.Write(c.path, data); err != nil {
		return fmt.Errorf("auth: persist clock floor: %w", err)
	}
	return nil
}
