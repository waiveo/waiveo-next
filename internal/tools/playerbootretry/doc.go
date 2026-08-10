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
// # Why the guard is a source-structure test
//
// This repo has no BrightScript unit-test runner, and BrightScript cannot be
// executed off-device: the player's behaviour is otherwise verified by the
// brighterscript compile gate (which cannot see control flow) and by
// on-hardware conformance runs (which need a Roku in the room). Neither would
// fail on a re-introduced early return, which is exactly the regression that
// must not slip through again.
//
// So this package parses the real PlayerTask.brs — the shipped source, not a
// copy — and asserts the STRUCTURE the fix depends on: that the program loop's
// only terminal exit is the dead-credential one, that a first-pull failure
// falls through to a backoff rather than out of the loop, and that the pairing
// loop retries on the same terms. The parser is small and deliberately strict:
// it understands `if`/`else if`/`else`/`end if`, `while`/`end while`, and
// single-line `if … then …`, which is all PlayerTask.brs's control flow uses.
// A structural change it cannot read fails loudly rather than passing vacuously
// — a guard that silently stops guarding is worse than no guard.
//
// What it deliberately does NOT assert is wording, log text, or formatting.
// Those change; the "never give up" property is the thing that must not.
package playerbootretry
