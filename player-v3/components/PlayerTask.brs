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
' interval. A 401 needsRepair failure (PLY-072/073, at any point — the first
' pull or a later one) is different: the credential is no longer usable at
' all, so the credential is cleared and the loop stops, publishing a
' failure so the operator sees a "needs re-pairing" status rather than a
' screen that silently keeps showing content under a token the relay has
' already revoked.
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
        print "[player-v3] pairing…"
        pair = wvDoPairing(code)
        if pair.ok
            wvStoragePersistPairing(pair.channelToken, pair.screenId, pair.relayHost, pair.relayPort, pair.trustPem)
            print "[player-v3] paired (screen " + pair.screenId + "), token persisted"
            state = wvStorageLoad()
        else
            print "[player-v3] pairing FAILED: " + pair.error
            if not state.paired
                result.phase = "pair_failed"
                result.status = "pairing failed"
                result.error = pair.error
                m.top.photonResult = result
                return
            end if
            print "[player-v3] keeping existing persisted pairing (never-wipe)"
        end if
    else if not state.paired
        result.phase = "need_code"
        result.status = "waiting for pairing code"
        m.top.photonResult = result
        return
    end if

    everSucceeded = false
    while true
        if not wvTaskShouldKeepRunning() then return

        print "[player-v3] pulling program…"
        prog = wvDoProgram({ channelToken: state.channelToken, relayHost: state.relayHost, relayPort: state.relayPort, trustPem: state.trustPem })

        if prog.ok
            everSucceeded = true
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
            ' PLY-072/073: the channel token itself is no longer usable
            ' (revoked or expired) — clear only the credential, keep the
            ' relay address, and stop this loop rather than keep polling a
            ' token that will never succeed again without a fresh pairing
            ' code (delivered by a future maybeStart re-provisioning, not by
            ' retrying in place).
            wvStorageClearCredentials()
            result = { phase: "program_failed", ok: false, contentType: "", items: invalid, status: "program failed", error: prog.error }
            print "[player-v3] program FAILED (needs repair): " + prog.error
            m.top.photonResult = result
            return

        else if not everSucceeded
            ' The very first pull failed outright (no Lease has ever been
            ' shown yet, so there is nothing on screen to protect) — surface
            ' the failure exactly as before rather than spin silently.
            result = { phase: "program_failed", ok: false, contentType: "", items: invalid, status: "program failed", error: prog.error }
            print "[player-v3] program FAILED: " + prog.error
            m.top.photonResult = result
            return

        else
            ' A transient failure AFTER content was already showing —
            ' never-wipe: log it and keep whatever is currently rendering in
            ' place; the next poll retries.
            print "[player-v3] program poll failed (keeping current content, never-wipe): " + prog.error
        end if

        wvSleepInterruptible(wvProgramPollIntervalMs())
    end while
end sub

' wvProgramPollIntervalMs is this player's ordinary (non-long-poll) Program
' delivery pull cadence — PLY-082's own draft-note proposes roughly 10
' seconds; this player does not implement the `long_poll` request parameter
' PLY-082 separately allows for, so this is simply how often it re-pulls.
function wvProgramPollIntervalMs() as Integer
    return 10000
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
