' IdleDefeatTask.brs — the run loop that keeps this platform's idle/inactivity
' surface off assigned content (PLY-158). It exists as a Task purely because
' roAppManager is a MAIN|TASK-only component and hardware refuses to create it
' on the render thread; source/IdleDefeat.brs records that finding in full.

sub init()
    m.top.functionName = "runIdleDefeat"
end sub

' runIdleDefeat is a single loop for the whole life of the app. It never spawns
' anything, and it has three independent ways to end, because a Task that
' cannot end is the shape that previously leaked threads in this fleet until a
' device hit its firmware thread cap:
'
'   1. control  — the owner set control="stop"/"STOP". A Task blocked in
'                 sleep() is NOT preemptively killed by the runtime, so this is
'                 checked between sleeps rather than assumed (same reason, and
'                 the same verified-on-device behavior, as PlayerTask's own
'                 wvSleepInterruptible).
'   2. stopFlag — PhotonScene.shutdown() asked cooperatively.
'   3. orphan   — the owner stopped beating ownerTick, so there is no longer
'                 anyone this thread can serve.
'
' Everything else it needs is read off m.top each pass, so the scene steers it
' by writing fields and never by creating or destroying it.
sub runIdleDefeat()
    chunkMs = wvIdleDefeatPollChunkMs()
    intervalMs = wvIdleDefeatIntervalSeconds() * 1000
    orphanMs = wvIdleDefeatOrphanMs()

    lastOwnerTick = m.top.ownerTick
    ownerSilentMs = 0
    ' Start "already due" so the first refresh lands within one wake-up of
    ' content being assigned rather than a full interval later: the platform's
    ' idle clock has been running untouched for as long as this player had
    ' nothing assigned, and it could be moments from expiring when the first
    ' item appears.
    sinceRefreshMs = intervalMs
    refreshes = 0
    failures = 0

    print "[player-v3] idle-defeat task started (PLY-158) — one Task for the life of this app; refresh every " + wvIdleDefeatIntervalSeconds().toStr() + "s while content is assigned, owner heartbeat every " + wvIdleDefeatOwnerHeartbeatSeconds().toStr() + "s"

    while true
        if m.top.engaged = true
            if sinceRefreshMs >= intervalMs
                sinceRefreshMs = 0
                reason = wvIdleDefeatRefresh()
                if reason = ""
                    failures = 0
                    refreshes = refreshes + 1
                    ' First refresh after engaging, then one line every Nth:
                    ' enough for on-device verification to READ that the
                    ' mechanism is still running deep into a device's idle
                    ' delay instead of inferring it from an absent screensaver.
                    if refreshes = 1 or refreshes mod wvIdleDefeatHeartbeatTicks() = 0
                        print "[player-v3] idle-defeat refresh #" + refreshes.toStr() + " OK (PLY-158) — platform idle clock reset; engaged for " + ((refreshes - 1) * wvIdleDefeatIntervalSeconds()).toStr() + "s"
                    end if
                else
                    ' A refresh that did not happen is the failure mode that
                    ' previously went completely unreported on hardware, so it
                    ' gets a line of its own — immediately, then rate-limited
                    ' so a permanently unusable platform call cannot flood the
                    ' console for the life of the app.
                    refreshes = 0
                    failures = failures + 1
                    if failures = 1 or failures mod wvIdleDefeatHeartbeatTicks() = 0
                        print "[player-v3] idle-defeat REFRESH FAILED #" + failures.toStr() + " (PLY-158) — " + reason + "; this platform's idle surface may cover assigned content"
                    end if
                end if
            end if
        else
            ' Disengaged: hold the refresh "due" so re-engaging acts at once,
            ' and reset the counters so the lines above always read as
            ' "since this player was last assigned content".
            sinceRefreshMs = intervalMs
            refreshes = 0
            failures = 0
        end if

        sleep(chunkMs)
        ownerSilentMs = ownerSilentMs + chunkMs
        sinceRefreshMs = sinceRefreshMs + chunkMs

        ' The three exits, all evaluated after a wake-up so each is honored
        ' within one chunk and none of them can be misread during the moment
        ' the thread is starting up.
        if LCase(m.top.control) <> "run"
            print "[player-v3] idle-defeat task STOPPED (PLY-158) — control field is no longer RUN"
            return
        end if

        if m.top.stopFlag = true
            print "[player-v3] idle-defeat task STOPPED (PLY-158) — shutdown requested by the scene"
            return
        end if

        ' Orphan self-check. The owner beats ownerTick whether or not it is
        ' engaged, so silence here means the scene that owns this Task is gone
        ' — not merely that there is nothing to refresh right now. Exit rather
        ' than occupy a thread nobody can reach.
        tick = m.top.ownerTick
        if tick <> lastOwnerTick
            lastOwnerTick = tick
            ownerSilentMs = 0
        else if ownerSilentMs >= orphanMs
            print "[player-v3] idle-defeat task STOPPED (PLY-158) — no owner heartbeat for " + (ownerSilentMs \ 1000).toStr() + "s; self-exiting rather than stranding a thread"
            return
        end if
    end while
end sub
