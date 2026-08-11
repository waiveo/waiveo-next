// Package playerinteract holds the regression guard for player-v3's
// INTERACTIVE-LAYER behaviour: D-pad focus, the OK dispatch, and the nav
// geometry (player-v3/components/PhotonScene.brs, InteractionTask.brs).
//
// # Why a source-reading guard
//
// This repo has no BrightScript unit-test runner and BrightScript cannot be
// executed off-device. The player's behaviour is otherwise verified by the
// brighterscript compile gate — which sees syntax and scope, and nothing about
// control flow or geometry — and by on-hardware runs, which need a Roku in the
// room. Neither would fail on any of the regressions below, and each of them
// produces a screen that looks completely correct and does nothing:
//
//   - the Scene never takes focus, so no key event reaches it and every
//     interactive layer is inert. This is the single most likely way the whole
//     feature silently stops working, because nothing else about the player
//     changes: the button draws, the ring draws, the remote does nothing.
//   - focus targets are collected only inside the `ping` KIND branch, so an
//     ordinary widget an author made interactive by giving it a ping_name gets
//     no focus region — tracker row 3.7, present in the model and absent on the
//     device. It is the mirror-direction half-fix this codebase keeps shipping.
//   - the nav item rects drift from wire.NavItemRects, so the focus ring lands
//     somewhere other than the label it belongs to.
//   - clearSlide stops clearing the focus state, so an OK press dispatches the
//     PREVIOUS slide's action — a button the viewer can no longer see firing an
//     automation nobody chose.
//   - the interaction Task is created per press (the legacy player's shape) or
//     never stopped at shutdown, which is this fleet's own thread-leak class:
//     Task threads outlive the node that owns them, and a wall panel pressed a
//     few hundred times a day reaches the firmware thread cap.
//
// It reads the SHIPPED source, not a fixture copy, for the reason
// internal/tools/playerbootretry states: a copy drifts, and a guard reading a
// drifted copy passes while the shipped player regresses.
//
// # What it deliberately does not assert
//
// Wording, log text, colours, and the exact pixel sizes of the ring. Those are
// meant to change. What must not change is that focus exists, that it covers
// both arms of "interactive", that the geometry matches the Go definition, and
// that nothing is leaked.
package playerinteract
