// Package playercontentcache holds the regression guards for two player-v3
// behaviours that no other gate in this repo can see: the content cache
// (player-v3/source/Program.brs) and the video slide layer's render + teardown
// (player-v3/components/PhotonScene.brs).
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
//
// So these tests parse the REAL shipped files — not a copy, which would drift —
// and assert the structure those behaviours are made of.
//
// # What this can and cannot see
//
// It is a STRUCTURAL guard, in the sense brsparse_test.go's half of
// internal/tools/playerbootretry is: it answers "does this function call that
// one, and in what order relative to this other call", not "what does control
// do under these conditions". That is the right instrument for these particular
// properties — each of them is a call that must exist, in a function, on one
// side of another call — but it is worth being plain that a sufficiently
// creative rewrite could satisfy the structure and lose the behaviour.
//
// What it deliberately does NOT assert is wording, log text, comments, or
// formatting. Those change constantly and none of them is the property.
package playercontentcache
