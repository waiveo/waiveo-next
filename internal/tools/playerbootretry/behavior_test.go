package playerbootretry

import (
	"strings"
	"testing"
)

// These are the guard proper: they state a SCENARIO a screen is really in and
// assert where control goes in the shipped PlayerTask.brs under it. Nothing
// here looks for a keyword. "The failure branch does not contain the word
// return" is a proxy that a one-line edit slips past; "a screen whose first
// pull failed reaches the bottom of the loop and goes round again" is the
// property itself, and it is false for every spelling of giving up.
//
// The scenarios are written the way an incident is described, because that is
// what each one is: the power came back before the relay did; the token was
// revoked; the owner asked the task to stop.

// runPhotonLoops returns the two while loops of runPhoton — the pairing retry
// loop and the program poll loop — parsed as executable statement trees.
func runPhotonLoops(t *testing.T) (pairing, program []stmt) {
	t.Helper()
	body := subBody(t, readPlayerTask(t), "runPhoton")
	stmts, end := parseStatements(t, body, 0)
	if end != len(body) {
		t.Fatalf("runPhoton did not parse to its end: stopped at line %d (%q)", body[end].n, body[end].text)
	}
	loops := collectWhileLoops(stmts)
	if len(loops) != 2 {
		t.Fatalf("runPhoton has %d while loop(s), want 2 (pairing retry, program poll) — the player's structure changed; re-read this guard before adjusting it", len(loops))
	}
	return loops[0].body, loops[1].body
}

// collectWhileLoops returns every while loop in the tree, in source order.
func collectWhileLoops(stmts []stmt) []stmt {
	var out []stmt
	for _, s := range stmts {
		switch s.kind {
		case stmtWhile:
			out = append(out, s)
			out = append(out, collectWhileLoops(s.body)...)
		case stmtFor:
			out = append(out, collectWhileLoops(s.body)...)
		case stmtIf:
			for _, a := range s.arms {
				out = append(out, collectWhileLoops(a.body)...)
			}
		}
	}
	return out
}

// assertAllPathsRetry fails unless EVERY explored path through body reaches the
// bottom of the loop — i.e. the loop goes round again.
func assertAllPathsRetry(t *testing.T, what string, body []stmt, e env) {
	t.Helper()
	paths := walk(body, e)
	if len(exits(paths)) == 0 {
		return
	}
	t.Fatalf("%s\n\nControl leaves the loop here instead of retrying:\n  %s\n\n"+
		"An unattended screen that gives up is a dead screen until a human relaunches the channel, and a power outage "+
		"reproduces this on every TV that beats its relay to boot. See this package's doc.", what, describeExits(paths))
}

// assertEveryPathExits fails unless every path leaves the loop the stated way.
func assertEveryPathExits(t *testing.T, what string, body []stmt, e env, want walkKind) {
	t.Helper()
	paths := walk(body, e)
	for _, p := range paths {
		if p.kind != want {
			where := "the bottom of the loop"
			if p.at.n != 0 {
				where = "line " + itoa(p.at.n) + " (" + p.at.text + ")"
			}
			t.Fatalf("%s\n\nA path reaches %s and %s, want every path to %s.", what, where, p.kind.String(), want.String())
		}
	}
	if len(paths) == 0 {
		t.Fatalf("%s: the loop body walked to no paths at all", what)
	}
}

// THE regression guard, stated as the incident it prevents.
func TestAScreenWhoseFirstPullFailsGoesRoundAgain(t *testing.T) {
	_, program := runPhotonLoops(t)
	assertAllPathsRetry(t,
		"A screen powered on before its relay did: the pull failed, the credential is fine, and nothing has ever rendered.",
		program,
		env{
			"prog.ok":                   triFalse,
			"prog.needsrepair":          triFalse,
			"eversucceeded":             triFalse,
			"wvtaskshouldkeeprunning()": triTrue,
		})
}

func TestAFailedPullOverRenderedContentAlsoGoesRoundAgain(t *testing.T) {
	// The mid-loop half of never-wipe: content is up, one poll failed. The loop
	// must keep polling — and it must not blank the screen, which the sibling
	// publish guard covers.
	_, program := runPhotonLoops(t)
	assertAllPathsRetry(t,
		"A working screen hit one transient poll failure.",
		program,
		env{
			"prog.ok":                   triFalse,
			"prog.needsrepair":          triFalse,
			"eversucceeded":             triTrue,
			"wvtaskshouldkeeprunning()": triTrue,
		})
}

func TestASuccessfulPullKeepsPolling(t *testing.T) {
	// PLY-082/094/101: the loop does not stop once it has something to draw,
	// or a superseded Lease would never be adopted.
	_, program := runPhotonLoops(t)
	assertAllPathsRetry(t,
		"A pull succeeded and the cast was published.",
		program,
		env{
			"prog.ok":                   triTrue,
			"eversucceeded":             triTrue,
			"wvtaskshouldkeeprunning()": triTrue,
		})
}

func TestADeadCredentialIsStillTheOneWayOut(t *testing.T) {
	// The guard cuts both ways. "Never give up" must not have been achieved by
	// deleting the one exit that is correct: a revoked or invalid channel token
	// (PLY-072/073/136) can never succeed by being retried, and a screen that
	// polls it forever never tells anyone it needs re-pairing.
	_, program := runPhotonLoops(t)
	assertEveryPathExits(t,
		"The relay rejected the channel token as unusable (needsRepair).",
		program,
		env{
			"prog.ok":                   triFalse,
			"prog.needsrepair":          triTrue,
			"eversucceeded":             triFalse,
			"wvtaskshouldkeeprunning()": triTrue,
		},
		walkReturn)
}

func TestTheOwnersStopRequestIsHonored(t *testing.T) {
	// The other legal exit, and the reason the retry cannot simply be `while
	// true` with no checks: a Task blocked in its own loop survives its node's
	// removal (this fleet's own Task-thread leak), so the owner's stop has to
	// end the thread.
	_, program := runPhotonLoops(t)
	assertEveryPathExits(t,
		"PhotonScene stopped the task (re-provisioning with a new pairing code).",
		program,
		env{
			"prog.ok":                   triFalse,
			"prog.needsrepair":          triFalse,
			"eversucceeded":             triFalse,
			"wvtaskshouldkeeprunning()": triFalse,
		},
		walkReturn)
}

// The pairing loop had the identical one-shot shape one step earlier: a
// freshly-provisioned screen pairs against a relay that may not be listening
// yet, and there is no persisted state to fall back on.
func TestAFailedPairingWithNothingToFallBackOnRetries(t *testing.T) {
	pairing, _ := runPhotonLoops(t)
	assertAllPathsRetry(t,
		"A freshly provisioned screen's pairing failed and it holds no persisted pairing.",
		pairing,
		env{
			"pair.ok":                   triFalse,
			"state.paired":              triFalse,
			"wvtaskshouldkeeprunning()": triTrue,
		})
}

func TestAFailedPairingWithAPersistedPairingStopsRetryingTheCode(t *testing.T) {
	// Never-wipe from the other side: the persisted pairing is a strictly
	// better thing to try than a code that just failed, so this one must LEAVE
	// the loop — and leave it by `exit while`, into the program poll, not by
	// returning out of the task.
	pairing, _ := runPhotonLoops(t)
	assertEveryPathExits(t,
		"A supplied pairing code failed, but a persisted pairing is already held.",
		pairing,
		env{
			"pair.ok":                   triFalse,
			"state.paired":              triTrue,
			"wvtaskshouldkeeprunning()": triTrue,
		},
		walkExitWhile)
}

func TestASuccessfulPairingReachesTheProgramLoop(t *testing.T) {
	pairing, _ := runPhotonLoops(t)
	assertEveryPathExits(t,
		"The pairing code was accepted.",
		pairing,
		env{
			"pair.ok":                   triTrue,
			"wvtaskshouldkeeprunning()": triTrue,
		},
		walkExitWhile)
}

// A retry that does not wait is the other way to get this wrong, and it is not
// visible to a control-flow walk: reaching the bottom of the loop with no
// backoff computed is a hot loop against a relay that is already struggling.
func TestTheRetryPathActuallyWaits(t *testing.T) {
	_, program := runPhotonLoops(t)
	if !mentions(program, "wvProgramRetryBackoffMs(") {
		t.Error("the program loop never computes a retry backoff; retrying with no wait hammers a relay that is already down")
	}
	if !mentions(program, "wvSleepInterruptible(") {
		t.Error("the program loop never sleeps through wvSleepInterruptible, so a stop requested during the wait cannot be honored (the Task-thread leak)")
	}
	pairing, _ := runPhotonLoops(t)
	if !mentions(pairing, "wvProgramRetryBackoffMs(") {
		t.Error("the pairing loop never backs off between attempts")
	}
}

// mentions reports whether any statement anywhere in the tree contains needle.
func mentions(stmts []stmt, needle string) bool {
	for _, s := range stmts {
		if strings.Contains(s.at.text, needle) {
			return true
		}
		if mentions(s.body, needle) {
			return true
		}
		for _, a := range s.arms {
			if mentions(a.body, needle) {
				return true
			}
		}
	}
	return false
}
