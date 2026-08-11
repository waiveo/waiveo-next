// Package playerbootretry holds the regression guard for player-v3's
// boot-retry discipline (player-v3/components/PlayerTask.brs).
//
// # The defect this guards
//
// PlayerTask's program loop used to EXIT on the first failed pull: if no Lease
// had ever rendered (`everSucceeded` still false), it published a failure and
// returned, ending the thread. A TV that powered on before its relay did
// therefore sat dead — ECP-alive, channel foregrounded, nothing on screen —
// until a human relaunched it. Every power outage reproduces it, on every
// screen that wins the race to boot, which on this fleet is most of them.
//
// It is a defect CLASS, not an incident: the same one-shot shape was present
// in the pairing path, and the reason it survived review twice is that the
// code reads as careful — it publishes a status, it does not spin, and the
// return looks like the tidy thing to do. The only thing wrong with it is that
// giving up is never the right answer for an unattended screen.
//
// # Why the guard reads the source, and why it EXECUTES it
//
// This repo has no BrightScript unit-test runner, and BrightScript cannot be
// executed off-device: the player's behaviour is otherwise verified by the
// brighterscript compile gate (which cannot see control flow) and by
// on-hardware conformance runs (which need a Roku in the room). Neither would
// fail on a re-introduced early return, which is exactly the regression that
// must not slip through again. So this package parses the real PlayerTask.brs
// — the shipped source, not a copy, which would drift.
//
// It then does two different things with it, and the split is the lesson this
// package learned the hard way:
//
//   - behavior_test.go WALKS the parsed loops under a stated scenario ("a
//     screen powered on before its relay did") and asserts where control goes.
//     That is the property itself: under these conditions the loop reaches its
//     bottom and goes round again. Conditions the tiny evaluator cannot model
//     are UNKNOWN and both arms are walked, so the approximation errs toward
//     reporting exits that might be unreachable, never toward missing one.
//   - brsparse_test.go asserts the STRUCTURE around it — which arm the one
//     legal exit belongs to, that the retry path computes a backoff, that a
//     failure is not published over content already on screen, and what the
//     tunables are. These say things a walk cannot.
//
// The first half exists because the second half, alone, was not enough. The
// original guard asked whether a branch contained a statement BEGINNING with
// `return`, which catches
//
//	if not everSucceeded
//	    return
//	end if
//
// and misses
//
//	if not everSucceeded then return
//
// — the same defect, in BrightScript's ordinary spelling, already used twice in
// PlayerTask.brs for its stop check. An adversarial review re-introduced the
// dead-screen defect that way and every gate stayed green. Asking "does this
// text look like giving up" has as many holes as there are ways to write it;
// asking "does control reach the bottom" has none, so that is what is asked
// now. (The structural half's exit detection was fixed too, and both loop
// guards share one definition of it — see loopExits.)
//
// What the guard deliberately does NOT assert is wording, log text, or
// formatting. Those change; the "never give up" property is the thing that must
// not.
package playerbootretry
