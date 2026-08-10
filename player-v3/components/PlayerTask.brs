' PlayerTask.brs — the player/1 client thread. Loads persisted state, pairs if
' needed (using the input pairingCode), then repeatedly pulls its program
' (PLY-080/081), publishing progress and each fetched + verified cast on
' photonResult.
'
' Re-poll loop (PLY-082/083a/094/101): after the first successful pull, this
' task does NOT return — it keeps polling at wvProgramPollIntervalMs()'s own
' cadence (this contract's own draft-note proposes roughly 10s, Program
' delivery) for as long as it keeps succeeding, publishing a fresh
' photonResult on every successful pull. PhotonScene's onPhotonResult always
' fully replaces whatever it is currently rendering with a new photonResult
' (startCast), so a Lease that supersedes the one currently active (PLY-094)
' — including a `preempt`-priority one PLY-101 requires be adopted
' immediately — is presented as soon as the NEXT poll observes it, rather
' than never, which is what a one-shot pull-and-exit left the screen with.
' A transient poll failure mid-loop does not blank an already-rendering
' screen (never-wipe): it is logged and the loop simply retries at the next
' interval — this includes a 401 CHANNEL_TOKEN_EXPIRED (PLY-073), which
' wvDoProgram does NOT surface as needsRepair (see Program.brs's own 401
' classification comment): it is retryable via renewal, never a trigger to
' clear anything or re-pair, so it rides this same transient path. A 401
' needsRepair failure (CHANNEL_TOKEN_REVOKED or CHANNEL_TOKEN_INVALID,
' PLY-072/073/136, at any point — the first pull or a later one) is
' different: that token is no longer usable at all, so ONLY the channel
' token is cleared (the trust anchor is a separate credential PLY-136
' forbids clearing here) and the loop stops, publishing a failure so the
' operator sees a "needs re-pairing" status rather than a screen that
' silently keeps showing content under a token the relay has rejected.
'
' Boot-retry forever (the power-outage case): a screen that comes back before
' its relay does gets a FAILED first pull, and there is nothing on screen yet
' to protect. That is not a reason to stop. This loop therefore has exactly
' ONE terminal exit — a needsRepair 401, where the credential itself is dead
' and no amount of retrying can revive it — and every other failure, first
' pull or thousandth, retries forever on a capped exponential backoff
' (wvProgramRetryBackoffMs: 2s doubling to a 60s ceiling). A one-shot exit on
' the first failure is the defect this shape exists to prevent: the whole
' fleet powers on together after an outage, every screen races the box's own
' boot, and each one that lost that race would sit dead — ECP-alive, channel
' foregrounded, nothing rendering — until a human relaunched it. Retrying is
' the same never-wipe discipline the mid-loop failure path already applies,
' extended to the one moment it was missing from.
'
' What DOES change while no Lease has ever rendered is how loudly the retry
' says so. A failure BEFORE the first success publishes a photonResult
' (ok:false) once the streak reaches wvRetryStatusAfterFailures(), and again
' on every failure after that, so the wall shows "reconnecting… (attempt N)"
' instead of an unexplained black rectangle — and, critically, only in that
' state: PhotonScene's onPhotonResult tears the current cast DOWN on any
' not-ok result, so publishing one AFTER content is up would blank a working
' screen over a single transient poll error. That is exactly what never-wipe
' forbids, which is why everSucceeded still gates the publish (and only the
' publish — never the retry).
'
' Pairing is retried on the same terms, for the same reason. A launch-arg
' pairing code with NO persisted state to fall back on used to publish
' "pairing failed" and return, which is the identical dead-screen outcome one
' step earlier: the first thing a freshly-provisioned screen does after an
' outage is pair, against a relay that may not be listening yet. It now
' retries the code forever on the same backoff. A pairing failure that DOES
' have persisted state to fall back on is not retried at all — never-wipe
' already has a better answer there (keep the existing pairing and go poll).
'
' Interruptible sleep (wvSleepInterruptible): a Task node whose functionName
' is still running when its owner sets control="stop" is NOT preemptively
' killed by the runtime — a function blocked in a long, uninterrupted
' sleep() survives its own node's removal (this fleet's own prior Task
' thread leak). PhotonScene's maybeStart re-provisioning path does exactly
' that (stops this task and creates a fresh one) whenever an operator
' supplies a new pairing code while already paired, so this loop's own
' between-polls wait is chopped into small chunks, each checking
' m.top.control, so a stop request is honored within one chunk rather than
' leaving this thread polling forever in the background.

sub init()
    m.top.functionName = "runPhoton"
end sub

sub runPhoton()
    result = { phase: "start", ok: false, contentType: "", items: invalid, status: "starting", error: "" }

    state = wvStorageLoad()

    ' An explicitly supplied launch-arg pairing code is deliberate operator
    ' re-provisioning: try it FIRST even when already paired (a re-keyed relay
    ' leaves persisted state stale forever otherwise). Never-wipe still holds:
    ' persisted state is only replaced AFTER the new pairing succeeds; on
    ' failure we fall back to the persisted pairing untouched.
    code = m.top.pairingCode
    if code = invalid then code = ""

    if code <> ""
        pairFailures = 0
        while true
            if not wvTaskShouldKeepRunning() then return

            print "[player-v3] pairing…"
            pair = wvDoPairing(code)
            if pair.ok
                wvStoragePersistPairing(pair.channelToken, pair.screenId, pair.relayHost, pair.relayPort, pair.trustPem)
                print "[player-v3] paired (screen " + pair.screenId + "), token persisted"
                state = wvStorageLoad()
                exit while
            end if

            print "[player-v3] pairing FAILED: " + pair.error
            if state.paired
                ' Never-wipe: a persisted pairing is a strictly better thing to
                ' try than this code, so stop retrying the code and go poll with
                ' what we already hold, untouched.
                print "[player-v3] keeping existing persisted pairing (never-wipe)"
                exit while
            end if

            ' No persisted pairing to fall back on. Retrying is the ONLY path to
            ' a rendering screen — the relay is very likely just not up yet —
            ' so back off and try the same code again, forever, surfacing a
            ' status once the streak is long enough to be worth reporting.
            pairFailures = pairFailures + 1
            if pairFailures >= wvRetryStatusAfterFailures()
                ' A FRESH assoc array per publish, never a mutated-and-re-set
                ' one: a SceneGraph field observer fires on assignment, and
                ' re-assigning the same reference makes what the observer reads
                ' depend on when it happens to run rather than on what was
                ' published.
                m.top.photonResult = { phase: "pair_failed", ok: false, contentType: "", items: invalid, status: "pairing failed, retrying (attempt " + pairFailures.toStr() + ")", error: pair.error }
            end if
            wvSleepInterruptible(wvProgramRetryBackoffMs(pairFailures))
        end while
    else if not state.paired
        result.phase = "need_code"
        result.status = "waiting for pairing code"
        m.top.photonResult = result
        return
    end if

    everSucceeded = false
    pullFailures = 0
    while true
        if not wvTaskShouldKeepRunning() then return

        print "[player-v3] pulling program…"
        prog = wvDoProgram({ channelToken: state.channelToken, relayHost: state.relayHost, relayPort: state.relayPort, trustPem: state.trustPem })

        waitMs = wvProgramPollIntervalMs()
        if prog.ok
            everSucceeded = true
            pullFailures = 0
            itemCount = 0
            if prog.items <> invalid then itemCount = prog.items.Count()
            print "[player-v3] PHOTON — cast (" + itemCount.toStr() + " item(s)) verified + ready to render"
            if prog.items <> invalid
                for i = 0 to prog.items.Count() - 1
                    it = prog.items[i]
                    if it.contentType = "composed"
                        layerCount = 0
                        if it.layers <> invalid then layerCount = it.layers.Count()
                        print "[player-v3]   item " + i.toStr() + ": composed (" + layerCount.toStr() + " layers)"
                    else
                        print "[player-v3]   item " + i.toStr() + ": " + it.contentType + " durationMs=" + it.durationMs.toStr() + " " + it.contentUri
                    end if
                end for
            end if

            result = { phase: "done", ok: true, contentType: prog.contentType, items: prog.items, status: "photon", error: "" }
            m.top.photonResult = result

        else if prog.needsRepair
            ' PLY-072/073/136: the channel token itself is no longer usable
            ' at all (revoked, or never valid — CHANNEL_TOKEN_EXPIRED does
            ' NOT reach this branch, see Program.brs) — clear ONLY that
            ' credential (never the trust anchor, PLY-136), keep the relay
            ' address, and stop this loop rather than keep polling a token
            ' that will never succeed again without a fresh pairing code
            ' (delivered by a future maybeStart re-provisioning, not by
            ' retrying in place).
            wvStorageClearChannelToken()
            result = { phase: "program_failed", ok: false, contentType: "", items: invalid, status: "program failed", error: prog.error }
            print "[player-v3] program FAILED (needs repair): " + prog.error
            m.top.photonResult = result
            return

        else
            ' EVERY other failure — the first pull included — retries. This
            ' branch must never return: see this file's own boot-retry-forever
            ' doc for why a one-shot exit here is the dead-screen defect that
            ' every power outage reproduces.
            pullFailures = pullFailures + 1
            if everSucceeded
                ' Never-wipe: content is already on screen. Log it and keep
                ' rendering — publishing a not-ok photonResult here would make
                ' PhotonScene tear the cast down over one transient error.
                print "[player-v3] program poll failed (keeping current content, never-wipe): " + prog.error
            else
                print "[player-v3] program FAILED (attempt " + pullFailures.toStr() + ", retrying): " + prog.error
                if pullFailures >= wvRetryStatusAfterFailures()
                    ' Nothing has ever rendered, so there is nothing to protect
                    ' and a silent black screen helps nobody: say what is going
                    ' on. A fresh assoc array per publish (see the pairing
                    ' loop's own note).
                    m.top.photonResult = { phase: "program_failed", ok: false, contentType: "", items: invalid, status: "no program yet, retrying (attempt " + pullFailures.toStr() + ")", error: prog.error }
                end if
            end if

            waitMs = wvProgramRetryBackoffMs(pullFailures)
        end if

        ' One wait per iteration, at whichever cadence this iteration earned:
        ' the ordinary poll interval after a success, the backoff after a
        ' failure. Both go through the same interruptible sleep, so a stop
        ' requested during a 60s backoff is still honored within one chunk.
        wvSleepInterruptible(waitMs)
    end while
end sub

' wvProgramPollIntervalMs is this player's ordinary (non-long-poll) Program
' delivery pull cadence — PLY-082's own draft-note proposes roughly 10
' seconds; this player does not implement the `long_poll` request parameter
' PLY-082 separately allows for, so this is simply how often it re-pulls.
function wvProgramPollIntervalMs() as Integer
    return 10000
end function

' wvProgramRetryBaseMs is the FIRST retry wait after a failed pull or a failed
' pairing — deliberately shorter than the ordinary poll interval, because the
' case this exists for (a screen that beat its relay to power-on) usually
' clears within a few seconds and the screen is showing nothing until it does.
function wvProgramRetryBaseMs() as Integer
    return 2000
end function

' wvProgramRetryCapMs is the ceiling the backoff doubles up to and then stays
' at forever. Capped rather than unbounded for the reason the retry is
' unbounded: a screen that has been dark for an hour must still recover within
' a minute of its relay coming back, not wait out an exponent.
function wvProgramRetryCapMs() as Integer
    return 60000
end function

' wvRetryStatusAfterFailures is how many CONSECUTIVE failures a screen with
' nothing rendered yet accumulates before it starts saying so on-screen. Sized
' so an ordinary relay restart — a handful of seconds, well inside the
' pre-cap backoff — passes without ever putting a scary message on a wall,
' while a genuinely stuck screen stops being a silent black rectangle.
function wvRetryStatusAfterFailures() as Integer
    return 10
end function

' wvProgramRetryBackoffMs is the wait before retry number consecutiveFailures
' (1-based): wvProgramRetryBaseMs doubled once per prior failure, clamped at
' wvProgramRetryCapMs — 2s, 4s, 8s, 16s, 32s, then 60s forever.
'
' The doubling is written as a loop that returns AT the cap rather than as a
' shift-then-clamp, because a 32-bit BrightScript Integer overflows into a
' negative number about thirty failures in — and a negative wait is not a
' short wait, it is a busy loop hammering an already-struggling relay several
' hundred times a second. Returning at the cap means the multiply can never
' run on a value that is already at or past it.
function wvProgramRetryBackoffMs(consecutiveFailures as Integer) as Integer
    ms = wvProgramRetryBaseMs()
    if consecutiveFailures <= 1 then return ms
    n = 1
    while n < consecutiveFailures
        ms = ms * 2
        if ms >= wvProgramRetryCapMs() then return wvProgramRetryCapMs()
        n = n + 1
    end while
    return ms
end function

' wvTaskShouldKeepRunning reports whether this Task's owner still wants it
' running — read directly rather than cached, so a STOP requested mid-loop
' is observed the next time this is checked. Compared case-insensitively:
' a Task's `control` field is set with a mixed/upper-case literal ("RUN",
' matching maybeStart's own `m.task.control = "RUN"`) but reads back
' lower-cased ("run") from inside the task's own running thread — a real,
' verified-on-device Roku control-field behavior, not a hypothetical.
function wvTaskShouldKeepRunning() as Boolean
    return LCase(m.top.control) = "run"
end function

' wvSleepInterruptible sleeps up to totalMs, but in small chunks, checking
' wvTaskShouldKeepRunning() between each — see this file's own top-of-file
' doc for why an uninterrupted sleep() risks leaking this thread past its
' owner's stop request.
sub wvSleepInterruptible(totalMs as Integer)
    chunkMs = 250
    elapsed = 0
    while elapsed < totalMs
        if not wvTaskShouldKeepRunning() then return
        stepMs = chunkMs
        if totalMs - elapsed < stepMs then stepMs = totalMs - elapsed
        sleep(stepMs)
        elapsed = elapsed + stepMs
    end while
end sub
