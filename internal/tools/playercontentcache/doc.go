// Package playercontentcache holds the regression guards for the player-v3
// behaviours no other gate in this repo can see. It began as the content cache's
// guard (player-v3/source/Program.brs) and now covers everything on that same
// unwatched surface: the cache, the video slide layer's render + teardown, how a
// Lease degrades when an asset will not fetch, and what `display: "blank"`
// (player/1 PLY-093) actually does to the screen — the last two across both
// Program.brs and player-v3/components/PhotonScene.brs. The name is historical;
// the subject is "the shipped BrightScript player, where the compile gate is the
// only other instrument".
//
// # Why a guard, and why it reads the shipped source
//
// BrightScript cannot be executed off-device and this repo has no BrightScript
// unit-test runner. The player's behaviour is otherwise verified by the
// brighterscript compile gate, which sees syntax and nothing else, and by
// on-hardware runs, which need a Roku in the room. Neither would fail on any of
// the regressions below — every one of them COMPILES, and every one of them is
// silent on the device too:
//
//   - the cache's hit path starts fetching again. Nothing breaks; the screen
//     still shows the right thing. It just re-downloads and re-hashes every
//     asset every ten seconds, which on a wall of screens sharing one LAN is
//     the difference between an idle link and a saturated one, and on a
//     Pi-class CPU is a full-file sha256 per asset per poll.
//   - the trim stops being called. Nothing breaks until a device that has been
//     running for months has no space left, at which point everything breaks at
//     once, on the whole fleet, for a reason nobody will connect to this.
//   - a video slide layer's bytes are never fetched, because the layer loop
//     tests for `image` alone. The slide draws with a hole where the video is.
//   - clearSlide stops removing Video children, or stops STOPPING them first.
//     Removing a Video node does not stop playback, so the outgoing slide's
//     video keeps decoding — and keeps playing audio — underneath whatever
//     replaced it. This player has already been bitten twice by the "a thing
//     outlives the node that owned it" shape (Task threads, then composed video
//     layers); this is the same shape a third time.
//   - one unfetchable asset takes the WHOLE program down again. Every gate in
//     this repo passed while it did, for a whole session, on real hardware: the
//     player logged "keeping current content, never-wipe" and kept an hour-old
//     slide up, and a second slide referencing no assets at all was discarded
//     with the one that failed (HV-2, 2026-08-11). Nothing crashes, nothing
//     errors upstream, and the console line reads like correct behaviour.
//   - a `display: "blank"` Lease is treated as a failed pull. The screen simply
//     does not change, which is indistinguishable from a healthy screen showing
//     the same thing — an expired alert override stays on the wall and a screen
//     scheduled dark overnight never goes dark (HV-4, 2026-08-11). This one was
//     invisible to the conformance corpus too: internal/virtualplayer honours
//     `display` correctly, so every case drove the double and passed.
//   - a slide video is fire-and-forget. It reports no state at all, and because
//     a Roku screenshot captures only the graphics plane, a black video region
//     is what a capture shows whether playback is perfect or dead — so there is
//     no instrument, and playback cannot be verified from outside the device at
//     all (HV-5).
//
// So these tests parse the REAL shipped files — not a copy, which would drift —
// and assert the structure those behaviours are made of.
//
// # Two instruments, and why both
//
// Most of these guards are STRUCTURAL, in the sense brsparse_test.go's half of
// internal/tools/playerbootretry is: they answer "does this function call that
// one, and in what order relative to this other call", not "what does control
// do under these conditions". That is the right instrument for a property that
// IS a call — the trim happens once, after the last fetch, before the Lease is
// published — but it is worth being plain that a sufficiently creative rewrite
// could satisfy the structure and lose the behaviour.
//
// The cache's BOUND is not a property of that shape, and asserting it
// structurally is how it shipped broken. The trim mentioned both caps, called
// wvDeleteCachedFile, deleted from its map and named keepPrev — every structural
// fact held — while a single mis-assignment (`cache.keepPrev = protectedKeys`)
// made the protected set the running union of every key ever kept, so nothing
// was ever evictable and both caps were inert from the second poll onward. A
// reviewer confirmed the blindness by making the trim provably incapable of
// eviction and watching this package stay green.
//
// So the bound is now EXECUTED. brsrun_test.go is a small interpreter for the
// BrightScript subset these routines are written in; it runs the real shipped
// wvTrimContentCache over a real cache, poll after poll, and the tests assert
// what the cache CONTAINS and which files were unlinked. It fails loudly on any
// construct it cannot model, so a player edit it cannot read is a red test
// naming the line rather than a silent pass.
//
// The same reasoning put wvDoProgram on the executed side (programdegrade_test.go).
// Which items survive a partial failure, and what a blank instruction resolves
// to, are decisions over a whole Lease — "wvDoProgram contains a `return r`" is
// equally true of the player that shipped HV-2 and the one that fixed it. So
// those tests build a Lease in Go, run the shipped routine over it with only the
// I/O faked, and assert the cast that comes back. PhotonScene's half of the same
// two defects stays structural (scenedisplay_test.go), because there the
// property IS a set of calls against SceneGraph nodes and modelling
// roSGNode well enough to run it would be a far larger fake than the code under
// test.
//
// What it deliberately does NOT assert is wording, log text, comments, or
// formatting. Those change constantly and none of them is the property.
package playercontentcache
