// Package packhost supervises running pack processes: starting them, giving
// them an identity, swapping one version for another while the host keeps
// running, and — the part that decides whether any of it works — tearing the old
// one down completely.
//
// # Why out of process
//
// Not for security. Isolation was settled by the owner as no-sandbox: an
// operator who installs a hostile extension owns that outcome. The process
// boundary buys two other things, and they are the reason it is here:
//
//   - HOT-SWAP. Code cannot be reliably unloaded in-process. Legacy learned this
//     twice: `import(…?t=)` cache-busts only the entry point, so static relative
//     imports pin the OLD sub-modules forever and automation kept running stale
//     code. A process boundary makes "the old code is gone" a fact of the
//     operating system rather than of a module cache nobody can inspect.
//   - CRASH CONTAINMENT. An extension that wedges or dies takes itself down and
//     not the box.
//
// It is also not optional: the host is Go and extensions are JS.
//
// # How a pack proves it started
//
// There is no wire protocol here and deliberately so. The host mints a
// tier-grant (security-model SEC-037), writes the code to the child's standard
// input, and waits for the grant to be REDEEMED. That is a stronger readiness
// signal than any hello frame: a pack that redeemed has started, read its stdin,
// reached the API and authenticated. A process that merely exists has done none
// of those.
//
// SEC-037 forbids passing the code in an environment variable — readable from
// the process table for the life of the process, which would turn a one-time
// sixty-second code back into a long-lived credential.
//
// # What hot-swap has to guarantee
//
//   - A FAILED UPDATE MUST NOT TAKE THE EXTENSION DOWN. The replacement is
//     started and proven BEFORE the incumbent is stopped; if it cannot start, the
//     incumbent keeps serving and the swap reports failure. An update that breaks
//     an extension is annoying; one that also leaves nothing running is an outage
//     the operator did not ask for.
//   - TEARDOWN MUST BE REAL. Anything an extension registers needs a symmetric
//     unregister or hot-swap leaks. Here the registration is the process, so the
//     symmetric operation is: signal it, wait, kill it if it will not go — then
//     wait again, so the entry cannot be replaced while a child still holds the
//     old code.
package packhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
)

// Identity is the auth surface the supervisor needs: mint a pack's start-time
// grant, and ask whether it has been redeemed.
//
// An interface rather than *auth.Store so the supervisor can be driven against
// a fake in tests that are about process lifecycle — and so this package does
// not acquire the whole auth store as a dependency for two calls.
type Identity interface {
	MintTierGrant(ctx context.Context, packID, scopeNode string, role auth.Role, opts ...auth.MintGrantOption) (auth.MintedGrant, error)
	GrantRedeemed(ctx context.Context, grantID string) (bool, error)
}

// Spec is what the supervisor needs to run one pack.
type Spec struct {
	// ID is the manifest id (`publisher/name`) — the supervisor's key, stable
	// across versions, which is what makes a swap a replacement rather than a
	// second pack with a similar name.
	ID        string
	Version   string
	Argv      []string
	ScopeNode string
	Role      auth.Role
	// APIBaseURL is where the pack reaches this host's api/1 surface, handed
	// over as WAIVEO_API_BASE_URL.
	//
	// A pack cannot be written without it: the grant it reads on stdin is
	// redeemed at `POST /auth/tier-grant/redeem`, and until that address is
	// known there is nowhere to send it. Nothing conveyed it before this field —
	// the child merely inherited the host's environment, which names the host's
	// own listen address nowhere.
	//
	// IN THE ENVIRONMENT, and that is not a contradiction of SEC-037. What
	// SEC-037 keeps out of the environment is the one-time CODE, because an env
	// var is readable from the process table for the life of the process and
	// that would turn a sixty-second credential into a long-lived one. A base
	// URL is not a credential: it is public, it is the same for every pack, and
	// it is exactly the kind of thing an environment variable is for. Putting it
	// on stdin beside the code would also make the handoff a two-field format
	// with an ordering rule, when it is currently one line.
	APIBaseURL string
	// APICAFile is a file holding the PEM the pack must trust to reach
	// APIBaseURL, handed over as WAIVEO_API_CA_FILE. Empty when the deployment
	// serves a publicly-trusted certificate and the system pool suffices.
	//
	// This exists so the reference extension does not have to disable
	// verification. The feeder serves HTTPS with a SELF-SIGNED certificate, so a
	// pack using the system pool fails the handshake — and the shortcut every
	// author would then copy is InsecureSkipVerify, which turns the loopback
	// assumption into a habit that ships. Handing over the anchor keeps
	// verification ON and costs a pack four lines: read the file, AppendCertsFromPEM,
	// use the pool.
	//
	// A PATH rather than the PEM inline, because that is the shape TLS tooling
	// already has (SSL_CERT_FILE, and Go's own CertPool loading), and because an
	// env var holding a certificate is awkward to read in a process listing that
	// is already showing the argv.
	APICAFile string
}

// Running describes a live pack for an operator reading the console.
type Running struct {
	ID        string
	Version   string
	PID       int
	StartedAt time.Time
}

// Options tune the supervisor. The zero value is usable: every field falls back
// to a documented default rather than to zero, because a zero readiness budget
// would fail every start and a zero grace period would kill every pack instantly.
type Options struct {
	// ReadyTimeout bounds how long a pack has to redeem its grant. It must sit
	// under the grant's own ttl (SEC-037's sixty seconds): waiting past that
	// point is waiting for a code that has already expired.
	ReadyTimeout time.Duration
	// ReadyPoll is how often the redemption is checked.
	ReadyPoll time.Duration
	// StopGrace is how long a pack has to exit after its stdin closes before it
	// is killed.
	StopGrace time.Duration

	// RestartBackoff is the delay before the FIRST restart of a pack that
	// crashed; each further crash doubles it, capped at RestartBackoffMax. Zero
	// disables restarting entirely, which is the old behaviour and is what tests
	// that assert on a single exit want.
	//
	// A crashed pack used to stay dead until the host restarted, silently: no log
	// line, and an operator's queued action sat pending forever because nothing
	// was left to lease it. That is the failure this exists to end.
	RestartBackoff time.Duration
	// RestartBackoffMax caps the growth. Zero takes defaultRestartBackoffMax.
	RestartBackoffMax time.Duration
	// RestartHealthy is how long a replacement must survive before its crash
	// history is forgotten. Without it a pack that crashes once a day would
	// eventually inherit a minute-long delay it never earned.
	RestartHealthy time.Duration
	// OnRestart, when set, is told about every restart — the log line whose
	// absence made the original failure silent. It runs on the supervisor's
	// waiter goroutine, so it must not block.
	OnRestart func(id, version string, attempt int, delay time.Duration, err error)
}

// Restart policy defaults. Deliberately unbounded in ATTEMPTS but bounded in
// RATE: a pack whose cause is transient (a peer that was briefly down) must
// recover on its own, and one that is permanently broken must not be given up on
// silently either — giving up would restore the exact silence this replaces. The
// cap is what keeps a crash-loop from becoming a busy-loop.
const (
	defaultRestartBackoffMax = 60 * time.Second
	defaultRestartHealthy    = 60 * time.Second
)

const (
	defaultReadyTimeout = 20 * time.Second
	defaultReadyPoll    = 25 * time.Millisecond
	defaultStopGrace    = 3 * time.Second
)

func (o Options) withDefaults() Options {
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = defaultReadyTimeout
	}
	if o.ReadyPoll <= 0 {
		o.ReadyPoll = defaultReadyPoll
	}
	if o.StopGrace <= 0 {
		o.StopGrace = defaultStopGrace
	}
	return o
}

// Supervisor owns every running pack process.
type Supervisor struct {
	ident Identity
	opts  Options

	mu    sync.Mutex
	packs map[string]*process
	exits []Exit
}

// New builds a supervisor over an identity source.
func New(ident Identity, opts Options) *Supervisor {
	return &Supervisor{ident: ident, opts: opts.withDefaults(), packs: map[string]*process{}}
}

type process struct {
	spec      Spec
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	startedAt time.Time

	// ctx is the context this pack was Started under. A restart is a
	// continuation of that Start, not a new one, so it must die with it —
	// otherwise a supervisor whose owner has gone away keeps relaunching.
	ctx context.Context
	// crashes counts consecutive crashes, reset once a replacement survives
	// RestartHealthy. It is what makes the backoff grow for a pack that is
	// genuinely broken and stay at zero for one that hiccupped.
	crashes int

	// exited is closed by the ONE waiter goroutine launch starts. There can only
	// be one: exec.Cmd.Wait may be called once, so teardown waits on this
	// channel rather than calling Wait itself.
	exited chan struct{}
	// exitErr is the process's exit status, valid once exited is closed.
	exitErr error
	// stopping marks a teardown WE initiated, so the waiter can tell a deliberate
	// stop from a crash. Without it every Stop and every Swap would be reported
	// to the operator as an extension crashing.
	stopping bool
}

// ErrNotRunning is returned for an operation naming a pack the supervisor does
// not have.
var ErrNotRunning = errors.New("packhost: no such pack is running")

// Exit is one pack process ending WITHOUT being asked to.
//
// Recorded because a crash is the operator-facing half of crash containment: an
// extension that dies and is silently forgotten looks identical, from the
// console, to one that was never installed. It is also how an operator learns a
// hot-swap went wrong minutes after it reported success.
type Exit struct {
	ID      string
	Version string
	PID     int
	At      time.Time
	// Err is the process's exit status. Nil means it exited zero of its own
	// accord, which for a supervised pack is still unexpected — a pack's job is
	// to keep running until told to stop.
	Err error
}

// maxRecordedExits bounds the crash log. A pack in a crash loop must not be able
// to exhaust memory through the very mechanism that reports it is crash-looping.
const maxRecordedExits = 64

// Start launches a pack, hands it an identity, and waits for it to prove it came
// up.
//
// A pack already running under this ID is an error rather than a silent second
// process: two copies of one extension is the leak this package exists to
// prevent, and a caller that meant to replace it wants Swap.
func (s *Supervisor) Start(ctx context.Context, spec Spec) (Running, error) {
	s.mu.Lock()
	_, exists := s.packs[spec.ID]
	s.mu.Unlock()
	if exists {
		return Running{}, fmt.Errorf("packhost: %s is already running (use Swap to replace it)", spec.ID)
	}

	p, err := s.launch(ctx, spec)
	if err != nil {
		return Running{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-checked under the lock: two concurrent Starts for one ID must not both
	// land. The loser's process is torn down rather than left parentless.
	if _, exists := s.packs[spec.ID]; exists {
		go teardown(p, s.opts.StopGrace)
		return Running{}, fmt.Errorf("packhost: %s is already running (use Swap to replace it)", spec.ID)
	}
	s.packs[spec.ID] = p
	return p.running(), nil
}

// Swap replaces a running pack with a new version WITHOUT stopping the host.
//
// The order is the contract: the replacement is started and proven first, and
// the incumbent torn down only once the replacement is known to be up. A swap
// that fails leaves the incumbent serving and says so.
//
// Swapping a pack that is not running is a Start: an operator updating an
// extension that happens to be stopped means "run this version", and refusing
// would make the outcome depend on invisible prior state.
func (s *Supervisor) Swap(ctx context.Context, spec Spec) (Running, error) {
	s.mu.Lock()
	old, had := s.packs[spec.ID]
	s.mu.Unlock()

	fresh, err := s.launch(ctx, spec)
	if err != nil {
		if had {
			return Running{}, fmt.Errorf("packhost: swap of %s failed, the running version %s is untouched: %w",
				spec.ID, old.spec.Version, err)
		}
		return Running{}, err
	}

	s.mu.Lock()
	// A same-owner re-register is a REPLACE, not a collision.
	s.packs[spec.ID] = fresh
	s.mu.Unlock()

	if had {
		// Torn down AFTER the map stops pointing at it, so nothing routes a new
		// call to a process on its way out. Synchronous, because "the old code is
		// gone" has to be true when Swap returns — a backgrounded teardown would
		// let a caller observe a version that is supposedly retired.
		teardown(old, s.opts.StopGrace)
	}
	return fresh.running(), nil
}

// Stop tears a pack down and forgets it.
func (s *Supervisor) Stop(id string) error {
	s.mu.Lock()
	p, ok := s.packs[id]
	if ok {
		delete(s.packs, id)
	}
	s.mu.Unlock()
	if !ok {
		return ErrNotRunning
	}
	teardown(p, s.opts.StopGrace)
	return nil
}

// StopAll tears down every pack — the host shutting down.
func (s *Supervisor) StopAll() {
	s.mu.Lock()
	all := make([]*process, 0, len(s.packs))
	for id, p := range s.packs {
		all = append(all, p)
		delete(s.packs, id)
	}
	s.mu.Unlock()
	for _, p := range all {
		teardown(p, s.opts.StopGrace)
	}
}

// Running lists live packs, ordered by id so a console rendering it does not
// reshuffle between reads.
func (s *Supervisor) Running() []Running {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Running, 0, len(s.packs))
	for _, p := range s.packs {
		out = append(out, p.running())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// launch starts the process, hands it its code, and waits for readiness. It
// either returns a pack that is up, or an error having left no process behind.
func (s *Supervisor) launch(ctx context.Context, spec Spec) (*process, error) {
	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("packhost: %s has no argv to start", spec.ID)
	}

	// Minted BEFORE the process exists. A pack that started and then found no
	// identity waiting would have to poll or retry, and the failure would look
	// like a slow pack rather than a host that could not issue a credential.
	grant, err := s.ident.MintTierGrant(ctx, spec.ID, spec.ScopeNode, spec.Role)
	if err != nil {
		return nil, fmt.Errorf("packhost: mint identity for %s: %w", spec.ID, err)
	}

	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	// The host's own environment plus the address. Inheriting is deliberate — a
	// pack needs a PATH, a HOME and a TMPDIR like any other process — and the
	// address is appended rather than replacing the set, so a deployment that
	// exports something for its packs keeps it.
	env := os.Environ()
	if spec.APIBaseURL != "" {
		env = append(env, "WAIVEO_API_BASE_URL="+spec.APIBaseURL)
	}
	if spec.APICAFile != "" {
		env = append(env, "WAIVEO_API_CA_FILE="+spec.APICAFile)
	}
	if len(env) != len(os.Environ()) {
		cmd.Env = env
	}
	// Inherited, so a pack that dies on boot says why in the host's log. Its own
	// diagnostics are usually the only explanation, and swallowing them would
	// leave "it never became ready" as the whole story.
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("packhost: %s stdin: %w", spec.ID, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("packhost: start %s: %w", spec.ID, err)
	}
	p := &process{spec: spec, cmd: cmd, stdin: stdin, startedAt: time.Now(), ctx: ctx, exited: make(chan struct{})}
	// ONE waiter, started here and never anywhere else. It reaps the child the
	// moment it ends — which is what stops a crashed pack lingering as a zombie
	// holding its pid — and tells the supervisor, which is what stops Running()
	// reporting a dead extension as alive.
	go s.watch(p)

	// Over stdin, never an environment variable (SEC-037). An env var is
	// readable from the process table for the life of the process, which turns a
	// one-time sixty-second code into a long-lived credential.
	if _, err := io.WriteString(stdin, grant.Code+"\n"); err != nil {
		teardown(p, s.opts.StopGrace)
		return nil, fmt.Errorf("packhost: hand %s its identity: %w", spec.ID, err)
	}

	if err := s.awaitReady(ctx, grant.Grant.GrantID, spec.ID); err != nil {
		// Every failure path tears the child down. A pack that started and never
		// became ready is still a live process holding its resources, and leaking
		// one per failed start is how a box ends up with fifty.
		teardown(p, s.opts.StopGrace)
		return nil, err
	}
	return p, nil
}

// awaitReady blocks until the pack redeems its grant, the budget elapses, or the
// caller gives up.
func (s *Supervisor) awaitReady(ctx context.Context, grantID, packID string) error {
	deadline := time.Now().Add(s.opts.ReadyTimeout)
	ticker := time.NewTicker(s.opts.ReadyPoll)
	defer ticker.Stop()

	for {
		redeemed, err := s.ident.GrantRedeemed(ctx, grantID)
		if err != nil {
			return fmt.Errorf("packhost: readiness check for %s: %w", packID, err)
		}
		if redeemed {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("packhost: %s did not redeem its identity within %s", packID, s.opts.ReadyTimeout)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("packhost: start of %s cancelled: %w", packID, ctx.Err())
		}
	}
}

func (p *process) running() Running {
	return Running{ID: p.spec.ID, Version: p.spec.Version, PID: p.cmd.Process.Pid, StartedAt: p.startedAt}
}

// teardown ends a pack process for good.
//
// Closing stdin is the shutdown signal: the pack's input goes to EOF, which is
// the one signal available without inventing a protocol. A pack that exits on it
// gets a clean stop.
//
// A pack that does NOT exit is killed. That is the guarantee, not a fallback to
// be embarrassed about: without it, one extension that ignores shutdown keeps
// its old code running forever and every later update stacks another process
// behind it. The wait after the kill matters as much as the kill — returning
// before the child is reaped would let a caller observe "swapped" while the old
// process is still alive.
func teardown(p *process, grace time.Duration) {
	// Marked BEFORE the signal, so the waiter that observes the exit knows it was
	// asked for. Marking it afterwards would leave a window in which a fast exit
	// is reported to the operator as a crash.
	p.stopping = true
	_ = p.stdin.Close()

	select {
	case <-p.exited:
	case <-time.After(grace):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
}

// watch reaps one pack process and, if nobody asked it to stop, records the
// exit and forgets the pack.
func (s *Supervisor) watch(p *process) {
	p.exitErr = p.cmd.Wait()
	close(p.exited)

	s.mu.Lock()
	defer s.mu.Unlock()
	if p.stopping {
		return // a Stop or a Swap; the caller owns the bookkeeping
	}
	// Only forget it if the map still points at THIS process. A swap replaces the
	// entry before tearing the old one down, so a late exit from a retired
	// process must not delete its replacement.
	if cur, ok := s.packs[p.spec.ID]; ok && cur == p {
		delete(s.packs, p.spec.ID)
	}
	s.exits = append(s.exits, Exit{
		ID: p.spec.ID, Version: p.spec.Version, PID: p.cmd.Process.Pid,
		At: time.Now(), Err: p.exitErr,
	})
	if len(s.exits) > maxRecordedExits {
		s.exits = s.exits[len(s.exits)-maxRecordedExits:]
	}

	// A crash is not the end of the pack. Restarting is scheduled OUTSIDE the
	// lock (the delay is long and launch does real work), and only when a policy
	// is configured — zero backoff keeps the old leave-it-dead behaviour for
	// callers that want it.
	if s.opts.RestartBackoff > 0 {
		go s.restart(p)
	}
}

// restart brings a crashed pack back after a growing delay.
//
// Unbounded in attempts and bounded in RATE, on purpose. A pack whose cause was
// transient must recover with no operator action; one that is permanently broken
// must not be silently given up on either, because giving up recreates the exact
// silence this exists to end — an extension that is simply gone, with nothing
// saying so. The cap is what stops a crash-loop becoming a busy-loop.
func (s *Supervisor) restart(dead *process) {
	healthy := s.opts.RestartHealthy
	if healthy <= 0 {
		healthy = defaultRestartHealthy
	}
	maxDelay := s.opts.RestartBackoffMax
	if maxDelay <= 0 {
		maxDelay = defaultRestartBackoffMax
	}

	// A replacement that survived the healthy window earns a clean slate; one
	// that died inside it is still in the same crash-loop and keeps the streak.
	attempt := dead.crashes + 1
	if time.Since(dead.startedAt) >= healthy {
		attempt = 1
	}

	delay := s.opts.RestartBackoff << uint(min(attempt-1, 16))
	if delay > maxDelay || delay <= 0 {
		delay = maxDelay
	}

	select {
	case <-dead.ctx.Done():
		return // the owner is gone; a restart now would outlive its Start
	case <-time.After(delay):
	}

	p, err := s.launch(dead.ctx, dead.spec)
	if s.opts.OnRestart != nil {
		s.opts.OnRestart(dead.spec.ID, dead.spec.Version, attempt, delay, err)
	}
	if err != nil {
		// The relaunch itself failed. Treat it as another crash so the backoff
		// keeps growing rather than spinning at this delay forever.
		dead.crashes = attempt
		dead.startedAt = time.Now()
		if dead.ctx.Err() == nil {
			go s.restart(dead)
		}
		return
	}
	p.crashes = attempt

	s.mu.Lock()
	defer s.mu.Unlock()
	// Only claim the slot if nothing took it while we waited: an operator who
	// installed or swapped this pack during the backoff owns it now, and the
	// replacement we just built must not displace theirs.
	if _, taken := s.packs[dead.spec.ID]; taken {
		go teardown(p, s.opts.StopGrace)
		return
	}
	s.packs[dead.spec.ID] = p
}

// Exits reports the packs that ended without being asked to, oldest first.
func (s *Supervisor) Exits() []Exit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Exit(nil), s.exits...)
}
