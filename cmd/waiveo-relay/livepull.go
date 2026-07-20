package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/automation"
	"github.com/maaxton/waiveo-next/internal/relay/automationhost"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/hello"
	"github.com/maaxton/waiveo-next/internal/relay/playerserver"
	"github.com/maaxton/waiveo-next/internal/relay/schedulehost"
)

// rePullInterval is the POC cadence the running relay re-pulls the feeder's
// desired-state at (relay/1 REL-051): a few seconds, bounded so a newly-authored
// generation is picked up live within one interval without a restart. relay/1
// defines no server push, so polling is the deliberate POC mechanism (documented
// in the Wave-2 authoring-loop plan). The live binary drives the loop from a
// time.Ticker at this cadence; tests inject a manual channel, so nothing here
// sleeps on the wall clock.
const rePullInterval = 3 * time.Second

// pullFunc pulls one verified desired-state generation. The running binary wraps
// desiredstate.Pull against the co-located feeder (which itself verifies the
// snapshot's hash + signature against the enrollment-anchored trust anchor and
// enforces generation monotonicity, REL-052/071); a test injects a fake returning
// a scripted generation sequence. A pull failure is returned as an error the
// re-pull loop treats as non-fatal (REL-055).
type pullFunc func() (desiredstate.Applied, error)

// scheduleDriver owns the schedule-resolver lifecycle across desired-state
// generations for one relay. apply installs a generation's resolvers + background
// re-resolve loops over the player server; a subsequent apply cancels the prior
// generation's loops before installing the new ones (the REL-056 atomic-swap
// discipline for the serving side of a generation apply). Cancellation stops the
// loops but cannot interrupt a resolver goroutine already mid-resolve, so a
// superseded generation's late write could otherwise land after the new serve;
// that hazard is fenced at the write point instead — every resolver stamps its
// generation onto playerserver.Server.SetProgram, which drops a strictly-older
// generation's write (REL-052/056), so a stale in-flight resolver can never
// revert the new serve. apply is driven only from the boot path (once) and the
// single-threaded re-pull loop, so the cancel handoff needs no lock.
type scheduleDriver struct {
	srv        *playerserver.Server
	sink       *automation.CommandSink
	site       hello.SiteBinding
	signingKey ed25519.PrivateKey
	tickEvery  time.Duration

	// cancel tears down the currently-installed generation's resolve loops; nil
	// before the first apply.
	cancel context.CancelFunc
}

// apply resolves + serves applied.Schedule at nowMs and installs its background
// re-resolve loops, after cancelling any prior generation's loops. The new loops
// run under a child of ctx, so cancelling ctx (process shutdown) stops them too.
// It returns the resolvers built (empty when the schedule governs no carried
// screen — the additive serving policy leaves the app-authored program in place).
func (d *scheduleDriver) apply(ctx context.Context, applied desiredstate.Applied, nowMs int64) []*schedulehost.Resolver {
	if d.cancel != nil {
		d.cancel() // tear down the prior generation's resolve loops before the swap
	}
	loopCtx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	return resolveAndServe(loopCtx, applied, d.srv, d.sink, d.site, d.signingKey, d.tickEvery, nowMs)
}

// rePuller applies a re-pulled desired-state generation to the relay's live
// serving state. It tracks the last-applied generation so a same/lower generation
// is a no-op (REL-052/070) and only a strictly higher one is applied (REL-056):
// the schedule resolvers are re-driven (scheduleDriver.apply) and the automation
// engine's edge rules reloaded (automationhost.Host.ApplyEdgeRules). A pull error
// is non-fatal — the last-applied program keeps being served (REL-055).
type rePuller struct {
	pull    pullFunc
	driver  *scheduleDriver
	host    *automationhost.Host
	nowFn   func() int64
	lastGen int64
}

// tick performs one re-pull and returns whether it applied a new generation.
//
//   - A pull error (transport failure, or the pull rejecting a regressed
//     generation as desiredstate.ErrGenerationRegressed) is logged and ignored:
//     the last-applied generation stays served (REL-055 / REL-052).
//   - A pulled generation not strictly greater than the last-applied one is a
//     no-op — no re-resolve churn (REL-070; and the loop's own monotonic guard
//     mirroring REL-052 for a lower one).
//   - A strictly higher generation is applied: desiredstate.Pull has already
//     persisted it atomically (REL-056); tick re-drives the live serving state to
//     match — re-resolving the schedule and reloading the edge rules.
func (p *rePuller) tick(ctx context.Context) bool {
	applied, err := p.pull()
	if err != nil {
		if errors.Is(err, desiredstate.ErrGenerationRegressed) {
			log.Printf("waiveo-relay re-pull: rejected regressed generation (REL-052); keeping last-applied generation %d", p.lastGen)
		} else {
			log.Printf("waiveo-relay re-pull: pull failed (%v); keeping last-applied generation %d (REL-055)", err, p.lastGen)
		}
		return false
	}
	if applied.Generation <= p.lastGen {
		return false // same/lower generation — no re-resolve churn (REL-052/070)
	}

	// Strictly higher generation — apply it live. Pull already persisted the new
	// generation atomically (REL-056); re-drive the serving side to match.
	p.driver.apply(ctx, applied, p.nowFn())
	if err := p.host.ApplyEdgeRules(applied.EdgeRules, int(applied.Generation)); err != nil {
		// ApplyEdgeRules is fail-closed per rule (a bad rule is skipped, not fatal);
		// a returned error is unexpected, so log and keep serving rather than crash.
		log.Printf("waiveo-relay re-pull: reload edge rules for generation %d: %v", applied.Generation, err)
	}
	log.Printf("waiveo-relay re-pull: applied generation %d live (schedule re-resolved, %d edge rule(s) loaded)", applied.Generation, p.host.EdgeRuleCount())
	p.lastGen = applied.Generation
	return true
}

// rePullLoop drives rePuller.tick once per tick delivered on ticks — a
// time.Ticker's channel in the binary, a manual channel in tests, so nothing here
// sleeps on the wall clock. It returns when ctx is cancelled or ticks is closed.
// This is the relay's live loop: a generation authored between ticks is picked up
// and applied within one interval, without a restart.
func rePullLoop(ctx context.Context, ticks <-chan time.Time, p *rePuller) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			p.tick(ctx)
		}
	}
}
