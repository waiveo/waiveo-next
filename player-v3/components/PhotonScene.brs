' PhotonScene.brs — renders the first photon. Creates the PlayerTask, starts it
' once a pairing code is available (or the device is already paired), and
' renders the "cast" the task reports (PLY-083a: an ordered, cycling sequence
' of one-or-more verified content items, in Lease `content` array order, with
' NO type-based exception) once that content has already been fetched AND
' asset_ref-verified (Program.brs never hands back a contentUri, or a composed
' layer, that has not passed that check). A cast entry is either a plain
' image (Poster) or video (Video node) item, or a `composed` item (PLY-015)
' rendered as a stacked group of full-screen Poster/Video children — all
' three entry kinds cycle through the exact same renderCastItem/advanceCast
' path, so a Lease mixing them presents every one of them in order.

sub init()
    m.bg = m.top.findNode("bg")
    m.poster = m.top.findNode("contentPoster")
    m.video = m.top.findNode("contentVideo")
    m.video.observeField("state", "onVideoStateChange")
    ' Secondary, free coverage for PLY-158 while a video item is actually
    ' playing — the platform suppresses its idle surface for a playing video
    ' node on its own. It covers only video items, so it is not the mechanism
    ' (an image cast plays nothing); see IdleDefeat.brs.
    m.video.disableScreenSaver = true
    m.composedLayers = m.top.findNode("composedLayers")

    ' slideLayers holds a "slide" item's positioned native children
    ' (renderSlide); slideClockTimer refreshes every SELF-TICKING layer's Label
    ' once a second. slideTicking pairs each such live Label with what it needs
    ' to recompute itself — its kind, its format string, and (for a countdown)
    ' its target instant — so the ONE shared timer refreshes them all.
    '
    ' Self-ticking means the clock/date/countdown kinds, whose value is a
    ' function of the current time and which the player therefore computes
    ' itself. The weather/entity kinds are NOT here: their value is resolved on
    ' the box and arrives on the layer, so they are drawn once and change only
    ' when a new Lease brings a new value.
    '
    ' The timer is a fixed node (like castTimer) precisely so this scene always
    ' holds a handle to STOP it on item change (clearSlide) and on shutdown — a
    ' per-slide timer that outlives its slide is the leak class this player
    ' already fixed for Task threads. One timer for every ticking layer, rather
    ' than one per layer, is the same rule seen from the other side: N labels on
    ' a slide must not mean N things to remember to stop.
    m.slideLayers = m.top.findNode("slideLayers")
    m.slideClockTimer = m.top.findNode("slideClockTimer")
    m.slideClockTimer.duration = 1
    m.slideClockTimer.observeField("fire", "onSlideClockTick")
    m.slideTicking = []

    ' Interactive slide layers (parity milestones 1.5/3.7). focusTargets is the
    ' list of D-pad-focusable regions the SHOWING slide carries — one per layer
    ' that names a ping (wire.LayerIsInteractive) and one per item of every nav
    ' layer — rebuilt by renderSlide and emptied by clearSlide, exactly like
    ' slideTicking. focusIndex is which of them the ring is on, -1 for none.
    '
    ' The ring itself is four fixed Rectangles (see PhotonScene.xml): allocated
    ' once for the life of the app and repositioned as focus moves, never created
    ' per slide.
    m.focusRing = m.top.findNode("focusRing")
    m.focusRingTop = m.top.findNode("focusRingTop")
    m.focusRingBottom = m.top.findNode("focusRingBottom")
    m.focusRingLeft = m.top.findNode("focusRingLeft")
    m.focusRingRight = m.top.findNode("focusRingRight")
    m.focusTargets = []
    m.focusIndex = -1

    ' The lease the currently-showing content came from, refreshed on every
    ' successful poll (onPhotonResult). A press reports it so the platform can
    ' attribute the interaction to exactly what was on screen — the relay refuses
    ' an interaction naming a lease it did not issue to this screen.
    m.leaseId = ""

    ' The showing slide's own dwell, remembered so an interaction can RE-ARM it
    ' (wvRestartDwell): somebody working a menu must not have the slide pulled
    ' out from under them mid-press.
    m.currentDwellMs = 0

    ' The one thread a press is posted on. Created here, once, and never again —
    ' the same rule IdleDefeatTask above states, and the rule the legacy player
    ' broke by creating a fresh Task per press (Task threads outlive their owning
    ' node, so a wall panel pressed a few hundred times a day walks into the
    ' firmware thread cap). The scene steers it by writing its `press` field.
    m.interactionTask = CreateObject("roSGNode", "InteractionTask")
    m.interactionTask.control = "RUN"

    ' A Scene receives key events only while it holds focus. Without this the
    ' D-pad reaches nothing at all and every interactive layer is inert — the
    ' exact "surface that accepts work it never performs" this player must not
    ' ship. Asserted by TestPhotonSceneTakesFocusForKeyEvents.
    m.top.setFocus(true)

    m.status = m.top.findNode("statusLabel")
    m.status.text = "Waiveo Player v3 — starting…"

    ' castTimer drives an image cast item's own dwell time (PLY-083b): started
    ' fresh for every image item (renderCastItem), firing once (repeat=false)
    ' to advance to the next item (PLY-083a). A video item's own advance
    ' signal is its Video node's "finished" state (onVideoStateChange), not
    ' this timer — the timer is simply left stopped while a video item shows.
    m.castTimer = m.top.findNode("castTimer")
    m.castTimer.observeField("fire", "onCastTimerFire")

    ' PLY-158: hold this platform's own idle/inactivity surface off assigned
    ' content. The refresh that does it CANNOT run here — roAppManager is a
    ' MAIN|TASK-only component and hardware refuses to create it on the render
    ' thread — so it runs on IdleDefeatTask, created exactly once here and
    ' never again (not per cast, per item, per render, or when the PlayerTask
    ' below is re-created), because Task threads outlive the nodes that own
    ' them. This scene only ever steers it by writing its fields: `engaged`
    ' says whether content is assigned (setIdleDefeat), and idleDefeatHeartbeat
    ' beats `ownerTick` for as long as this scene lives so the Task can
    ' self-exit if this scene ever stops. See IdleDefeat.brs for why the
    ' manifest's screensaver_private flag does not cover this and why a
    ' permanently-playing hidden video was rejected.
    m.idleDefeatTask = CreateObject("roSGNode", "IdleDefeatTask")
    m.idleDefeatEngaged = false
    m.idleDefeatHeartbeat = m.top.findNode("idleDefeatHeartbeat")
    m.idleDefeatHeartbeat.duration = wvIdleDefeatOwnerHeartbeatSeconds()
    m.idleDefeatHeartbeat.observeField("fire", "onIdleDefeatHeartbeat")
    m.idleDefeatHeartbeat.control = "start"
    m.idleDefeatTask.control = "RUN"

    m.castItems = []
    m.castIndex = 0
    m.castSignature = ""

    m.task = CreateObject("roSGNode", "PlayerTask")
    m.task.observeField("photonResult", "onPhotonResult")

    m.started = false
    m.startedCode = ""
    m.top.observeField("pairingCode", "maybeStart")
    maybeStart()
end sub

' maybeStart launches the PlayerTask as soon as we either already hold a
' persisted pairing or have been handed a pairing code. Main sets pairingCode
' AFTER the scene shows, so when persisted state exists the task fast-starts
' with no code first — a code arriving later is deliberate operator
' re-provisioning and re-runs the (one-shot) task with it, otherwise the
' launch-arg code would be silently dropped whenever already paired.
sub maybeStart()
    code = m.top.pairingCode
    if code = invalid then code = ""

    if m.started
        if code = "" or code = m.startedCode then return
        m.task.control = "STOP"
        m.task = CreateObject("roSGNode", "PlayerTask")
        m.task.observeField("photonResult", "onPhotonResult")
    end if

    state = wvStorageLoad()
    if state.paired or code <> ""
        m.started = true
        m.startedCode = code
        m.status.text = "Waiveo Player v3 — connecting…"
        m.task.pairingCode = code
        m.task.control = "RUN"
    else
        m.status.text = "Waiveo Player v3 — enter pairing code to begin"
    end if
end sub

sub onPhotonResult()
    res = m.task.photonResult
    if res = invalid then return

    if res.ok
        ' The lease the content below came from, refreshed on EVERY successful
        ' poll rather than only on a changed program: a relay mints a fresh
        ' lease_id per pull (PLY-097) and only accepts an interaction naming one
        ' it recently issued, so a screen showing an unchanged cast for an hour
        ' would otherwise report a lease_id long since evicted and have every
        ' press refused LEASE_UNKNOWN.
        m.leaseId = castStr(res.leaseId)

        ' PLY-093: `display: "blank"` — a successful pull whose instruction is
        ' "show nothing". Program.brs reports it as contentType "blank" with no
        ' items, and it is reported as a SUCCESS precisely so this branch can
        ' exist: the not-ok branch below means "I could not get a program" and
        ' correctly leaves a status line on screen, which is not what an
        ' intentionally blanked wall should show.
        '
        ' This is a full teardown, not merely an empty cast: startCast on an
        ' empty items array returns early without hiding the Poster or stopping
        ' the Video, so a blank arriving over an image would have left that
        ' image on the wall — the same defect one layer down.
        '
        ' Idle defeat goes off with it (PLY-158's obligation is tied to being
        ' actively assigned NON-blank content, and PLY-155 says a relay treats
        ' an intentionally blanked screen the same way), and the status label
        ' stays hidden: a blanked screen is black, not a screen with an
        ' explanation written across it.
        '
        ' Blanking runs ONCE per transition, gated on the same castSignature the
        ' content path dedupes with rather than on a second flag: the poll loop
        ' delivers a fresh blank Lease every ten seconds for as long as the
        ' screen is dark, and tearing down (and printing) on every one of them
        ' would be a line every ten seconds all night. wvBlankSignature() cannot
        ' collide with a real cast's signature — castSignature always begins
        ' with an item count — so "already blank" and "already showing this
        ' program" are one question with one answer.
        if castStr(res.contentType) = "blank"
            if m.castSignature <> wvBlankSignature()
                tearDownContent()
                m.castSignature = wvBlankSignature()
                print "[player-v3] display:blank (PLY-093) — screen blanked; the Lease is still accepted and active (PLY-091/104)"
            end if
            setIdleDefeat(false)
            m.status.visible = false
            return
        end if

        ' One or more already-fetched, already-verified cast items (PLY-083a),
        ' each either plain image/video or composed (PLY-015); a single-item
        ' cast is the degenerate case of the exact same cycling logic
        ' (renderCastItem loops back to itself).
        '
        ' Only (re)start the cast when the program actually CHANGED. The task
        ' re-polls on its own cadence (PLY-083a's lifecycle wording), and a
        ' poll that returns the same program must not restart the sequence:
        ' with a poll interval shorter than the cast's total run time, an
        ' unconditional restart pins the screen near item 0 forever and the
        ' tail of the cast is never displayed at all — the cycle PLY-083a
        ' requires would silently never complete.
        sig = castSignature(res.items)
        if sig <> m.castSignature
            m.castSignature = sig
            startCast(res.items)
        end if

        ' PLY-158: content is assigned right now, so the platform's idle
        ' surface must not be allowed to cover it. Re-evaluated on EVERY
        ' successful poll rather than only on a changed cast — the engaged
        ' state has to track what is assigned now, not what last changed —
        ' and setIdleDefeat is a no-op when that state already matches.
        setIdleDefeat(wvIdleDefeatShouldEngage(res.contentType, res.items))

        m.status.visible = false
    else
        ' Could not get a program at all. Tear the screen down through the SAME
        ' routine the blank path uses, so the two can never disagree about which
        ' surface one of them forgot — then say so on screen, which is the one
        ' thing that distinguishes this from an intentional blank.
        tearDownContent()
        ' PLY-158's obligation is tied to being actively assigned non-blank
        ' content; this branch has torn the content down and is showing a
        ' status line, so stop holding the platform awake.
        setIdleDefeat(false)
        detail = res.status
        if res.error <> "" then detail = detail + " — " + res.error
        m.status.text = "Waiveo Player v3 — " + detail
        m.status.visible = true
    end if
end sub

' tearDownContent removes EVERYTHING this scene can have on screen and forgets
' what was showing. It is the one teardown both no-content paths share — an
' intentional blank (PLY-093) and a failed program — so the two can never
' diverge on which of the four render surfaces they remembered to clear.
'
' Forgetting m.castSignature is part of the teardown, not an extra: the
' signature exists to answer "is the program already on screen?", and once the
' screen has been cleared the honest answer for EVERY program is no. Leaving a
' stale signature behind means the next Lease carrying that same program is
' recognised as already-showing, startCast is skipped, and the screen stays
' black under a program it was told to display.
sub tearDownContent()
    stopCastTimer()
    clearComposed()
    clearSlide()
    m.video.control = "stop"
    m.video.visible = false
    m.poster.visible = false
    m.castItems = []
    m.castIndex = 0
    m.castSignature = ""
    m.currentDwellMs = 0
end sub

' castSignature identifies a cast by its ordered content, so an unchanged
' re-poll is distinguishable from a genuinely new program. Built from each
' item's own resolved URI and dwell rather than a lease id, because a relay
' re-issues a lease (new lease_id, new issued_at) on every poll while the
' content it carries is unchanged — keying on the lease would make every
' poll look like a change, which is the bug this exists to prevent.
' castStr coerces a possibly-invalid field to a string. Defined locally
' because a SceneGraph component's scope does not inherit pkg:/source
' globals — only the scripts its own XML includes.
function castStr(v as Dynamic) as String
    if v = invalid then return ""
    return v.toStr()
end function

' wvBlankSignature is the castSignature value that means "this screen is
' intentionally blank" (PLY-093). It is deliberately a word: castSignature builds
' every real value starting with the item COUNT, so no cast can ever produce it,
' and an equality test against it is therefore an exact "am I already blank?".
function wvBlankSignature() as String
    return "blank"
end function

function castSignature(items as Object) as String
    if items = invalid then return ""
    sig = items.Count().toStr()
    for i = 0 to items.Count() - 1
        it = items[i]
        sig = sig + "|" + castStr(it.contentType) + ";" + castStr(it.contentUri) + ";"
        if it.durationMs <> invalid then sig = sig + it.durationMs.toStr()
        ' The item's own cast-local slide id: it is what a nav item's target
        ' resolves against, so two programs whose slides are identically drawn but
        ' differently identified genuinely differ — a menu would jump somewhere
        ' else. Cheap to include and wrong to leave out.
        sig = sig + ";S" + castStr(it.slideId)
        ' A composed layer contributes only its resolved contentUri; a slide
        ' layer additionally contributes its kind, text, color, and geometry —
        ' two slides that share the same image bytes but differ in a text/clock
        ' string, a color, or a position are genuinely different programs and
        ' MUST re-render, so keying a slide's signature on contentUri alone
        ' (invalid for every non-image layer) would collapse them into one and
        ' pin the screen on the first. castStr coerces the fields absent on a
        ' composed layer (kind/text/color/x…) to "" so this stays correct for
        ' both item kinds.
        '
        ' A slide layer also contributes its SERVER-RESOLVED `value` and its
        ' countdown target/entity subject, and the value one is load-bearing:
        ' the weather and entity widgets change ONLY by a new Lease carrying a
        ' new value, on an otherwise byte-identical program. Leaving `value` out
        ' of the signature would make every such Lease look like the program
        ' already showing, so the screen would keep drawing the reading it first
        ' booted with and the widget would be exactly the frozen picture this
        ' whole native path exists to avoid. The cost is that a changed reading
        ' restarts the cast at item 0 — acceptable, since a resolved value only
        ' changes on the order of the box's own refresh cadence, not per poll.
        if it.layers <> invalid
            for j = 0 to it.layers.Count() - 1
                ly = it.layers[j]
                sig = sig + ";L" + castStr(ly.contentUri) + "," + castStr(ly.kind) + "," + castStr(ly.text) + "," + castStr(ly.color) + "," + castStr(ly.x) + "," + castStr(ly.y) + "," + castStr(ly.w) + "," + castStr(ly.h) + "," + castStr(ly.value) + "," + castStr(ly.target_ms) + "," + castStr(ly.entity_id)
                ' The INTERACTIVE members. Both are load-bearing in the signature
                ' for the same reason `value` is: they change what the slide DOES
                ' without changing a single visible pixel of the other layers, so
                ' a program that only re-pointed a menu item — or renamed a
                ' button's ping — would otherwise look identical to the one
                ' already showing and never re-render. The screen would keep an
                ' outdated menu wired to the old targets indefinitely.
                sig = sig + "," + castStr(ly.ping_name)
                if ly.items <> invalid
                    for k = 0 to ly.items.Count() - 1
                        nit = ly.items[k]
                        sig = sig + ",N" + castStr(nit.label) + ">" + castStr(nit.target_slide_id)
                    end for
                end if
            end for
        end if
    end for
    return sig
end function

' startCast begins (or replaces) an ordered cast (PLY-083a): stops any
' composed-layer rendering, resets to item 0, and renders it.
sub startCast(items as Object)
    clearComposed()
    clearSlide()
    m.castItems = items
    m.castIndex = 0
    if m.castItems = invalid or m.castItems.Count() = 0
        ' Program.brs never returns contentType "cast" with an empty items
        ' array (it errors instead), but guard defensively rather than index
        ' into an empty array below.
        return
    end if
    renderCastItem()
end sub

' renderCastItem presents m.castItems[m.castIndex]: a Poster for an image
' item (arming castTimer for its own dwell time, PLY-083b, falling back to
' this player's own default when the item carries none), the Video node for
' a video item (its own "finished" state is the advance signal, PLY-083a —
' castTimer is left stopped for a video item), or a stacked composedLayers
' group for a composed item (PLY-015, arming castTimer with this player's own
' default dwell time as its advance signal — PLY-083a defines no per-layer
' "natural end" for composed, Scope). Every node/URI here was already fetched
' + asset_ref-verified by Program.brs before this ever runs. Whichever of the
' three is NOT being shown is explicitly torn down every call (clearComposed/
' stop-and-hide-video/hide-poster) so a Lease cycling between item kinds never
' leaves a stale node from a prior item visible or (for a composed video
' layer) still decoding behind the new one.
sub renderCastItem()
    if m.castItems = invalid or m.castItems.Count() = 0 then return
    item = m.castItems[m.castIndex]

    stopCastTimer()

    if item.contentType = "composed"
        clearSlide()
        m.poster.visible = false
        m.video.control = "stop"
        m.video.visible = false
        renderComposed(item.layers) ' renderComposed clears any prior composed children itself.

        durationMs = wvClampCastDurationMs(item.durationMs)
        m.castTimer.duration = durationMs / 1000.0
        m.castTimer.control = "start"
        layerCount = 0
        if item.layers <> invalid then layerCount = item.layers.Count()
        print "[player-v3] cast item " + m.castIndex.toStr() + "/" + m.castItems.Count().toStr() + " (composed, " + layerCount.toStr() + " layers, " + durationMs.toStr() + "ms dwell — this player's own default advance signal, PLY-083a defines none for composed): advancing on timer"

    else if item.contentType = "slide"
        ' A "slide" item (native slide rendering): tear down the other three
        ' render surfaces and draw each positioned layer. renderSlide clears any
        ' prior slide children AND stops the clock timer itself before rebuilding,
        ' then arms castTimer for this slide's own dwell (a slide advances like an
        ' image — it has no natural end).
        clearComposed()
        m.poster.visible = false
        m.video.control = "stop"
        m.video.visible = false
        renderSlide(item.layers)

        durationMs = wvClampCastDurationMs(item.durationMs)
        ' Remembered so an interaction can RE-ARM the dwell (wvRestartDwell): a
        ' viewer working a menu must not have the slide advance out from under
        ' them between two presses.
        m.currentDwellMs = durationMs
        m.castTimer.duration = durationMs / 1000.0
        m.castTimer.control = "start"
        layerCount = 0
        if item.layers <> invalid then layerCount = item.layers.Count()
        print "[player-v3] cast item " + m.castIndex.toStr() + "/" + m.castItems.Count().toStr() + " (slide, " + layerCount.toStr() + " layers, " + durationMs.toStr() + "ms dwell): rendered natively"

    else if item.contentType = "video"
        clearComposed()
        clearSlide()
        m.poster.visible = false
        format = item.streamFormat
        if format = "" then format = "mp4"
        content = CreateObject("roSGNode", "ContentNode")
        content.url = item.contentUri
        content.streamFormat = format
        content.live = false
        ' A Video node arriving here in "finished" state — a single-video
        ' cast whose prior play-through already ended (advanceCast wraps
        ' back to index 0, which is itself for a 1-item video cast), or any
        ' video-to-video transition — will NOT reliably restart on a bare
        ' content reassignment; SceneGraph requires an explicit stop first.
        ' Always stop before assigning new content rather than assume the
        ' node is already in a state that accepts one.
        m.video.control = "stop"
        m.video.content = content
        m.video.visible = true
        m.video.control = "play"
        print "[player-v3] cast item " + m.castIndex.toStr() + "/" + m.castItems.Count().toStr() + " (video, advances on end-of-stream): " + item.contentUri

    else
        clearComposed()
        clearSlide()
        m.video.control = "stop"
        m.video.visible = false
        m.poster.uri = item.contentUri
        m.poster.visible = true

        durationMs = wvClampCastDurationMs(item.durationMs)
        m.castTimer.duration = durationMs / 1000.0
        m.castTimer.control = "start"
        print "[player-v3] cast item " + m.castIndex.toStr() + "/" + m.castItems.Count().toStr() + " (image, " + durationMs.toStr() + "ms dwell): " + item.contentUri
    end if
end sub

' wvClampCastDurationMs resolves a cast item's own duration_ms (already
' clamped once in Program.brs's wvItemDurationMs, and once more at the
' relay's own emission point, playerserver.leaseContentMinDurationMS) to the
' value actually armed on castTimer: absent/zero falls back to this player's
' own default dwell time (PLY-083b fixes no default), and any surviving
' non-zero value below this player's own floor is raised to it. This is
' belt-and-suspenders: castTimer.duration is a SceneGraph Timer field this
' player's render thread re-arms on every fire, so the value reaching it here
' is checked one last time regardless of what already clamped it upstream —
' an unclamped near-zero value would re-arm the timer at a CPU-saturating
' rate and starve the render thread, the classic Roku freeze signature.
function wvClampCastDurationMs(durationMs as Integer) as Integer
    if durationMs <= 0 then return wvDefaultImageDurationMs()
    if durationMs < wvMinCastTimerDurationMs() then return wvMinCastTimerDurationMs()
    return durationMs
end function

' wvMinCastTimerDurationMs is the floor wvClampCastDurationMs enforces — see
' its own doc.
function wvMinCastTimerDurationMs() as Integer
    return 500
end function

' advanceCast moves to the next item, wrapping back to index 0 after the
' last (PLY-083a's "continuously repeating cycle").
sub advanceCast()
    if m.castItems = invalid or m.castItems.Count() = 0 then return
    m.castIndex = (m.castIndex + 1) mod m.castItems.Count()
    renderCastItem()
end sub

' onCastTimerFire is castTimer's own "fire" field-change handler — an image
' cast item's dwell time has elapsed (PLY-083b); advance to the next item.
sub onCastTimerFire()
    print "[player-v3] cast image dwell time elapsed — advancing to next item"
    advanceCast()
end sub

' setIdleDefeat engages or disengages PLY-158's defeat of this platform's own
' idle/inactivity surface by telling the always-running IdleDefeatTask whether
' content is assigned. Called on every program result, so it acts only on an
' actual transition — which is also exactly what makes the two console lines
' below readable: one when the mechanism starts holding the platform's idle
' clock off assigned content, one when it stops, and nothing in between (the
' Task's own refresh lines aside). Nothing is created or destroyed here: the
' Task is engaged, not spawned, and the first refresh follows within one of
' its wake-ups.
sub setIdleDefeat(engage as Boolean)
    if engage = m.idleDefeatEngaged then return
    m.idleDefeatEngaged = engage
    m.idleDefeatTask.engaged = engage

    if engage
        print "[player-v3] idle-defeat ENGAGED (PLY-158) — content assigned; the idle-defeat task refreshes this platform's idle clock every " + wvIdleDefeatIntervalSeconds().toStr() + "s"
    else
        print "[player-v3] idle-defeat DISENGAGED (PLY-158) — no content assigned; this platform's idle surface is free to engage"
    end if
end sub

' onIdleDefeatHeartbeat is idleDefeatHeartbeat's own "fire" handler, and does
' one thing: prove to IdleDefeatTask that this scene still exists. It beats
' whether or not the mechanism is engaged, which is what lets the Task treat
' silence as "my owner is gone" and self-exit instead of lingering as a
' stranded thread. Deliberately silent on the console — it says nothing about
' whether the platform's idle clock is actually being refreshed, and the Task
' already reports that.
sub onIdleDefeatHeartbeat()
    m.idleDefeatTask.ownerTick = m.idleDefeatTask.ownerTick + 1
end sub

' shutdown is called (callFunc) by Main when the screen closes. Removing or
' dropping a node does NOT stop the Task thread it owns — that is precisely
' how this fleet once leaked threads until a device hit its firmware thread cap
' — so both of this scene's Tasks are told to stop cooperatively (stopFlag,
' honored at the Task's next wake-up) and then stopped at the firmware level.
function shutdown() as Boolean
    if m.idleDefeatTask <> invalid
        m.idleDefeatTask.stopFlag = true
        m.idleDefeatTask.control = "STOP"
    end if
    if m.idleDefeatHeartbeat <> invalid then m.idleDefeatHeartbeat.control = "stop"
    ' The interaction thread is stopped on exactly the same terms as the
    ' idle-defeat one, and for exactly the same reason: it is a Task, so it
    ' outlives the node that owns it unless it is told to stop. Cooperative flag
    ' first (honored at its next wake-up, which is at most one wait timeout
    ' away), then the firmware-level stop.
    if m.interactionTask <> invalid
        m.interactionTask.stopFlag = true
        m.interactionTask.control = "STOP"
    end if
    if m.task <> invalid then m.task.control = "STOP"
    if m.castTimer <> invalid then m.castTimer.control = "stop"
    ' A slide's clock timer is a repeating Timer; like castTimer it must be
    ' stopped on teardown so it never fires against a torn-down slide.
    if m.slideClockTimer <> invalid then m.slideClockTimer.control = "stop"
    print "[player-v3] scene shutdown — idle-defeat, interaction and player tasks stopped"
    return true
end function

' stopCastTimer halts castTimer so a superseded cast (a fresh photonResult
' arriving, or a switch to a composed item) never fires a stale advance
' against content that is no longer showing.
sub stopCastTimer()
    m.castTimer.control = "stop"
end sub

' wvDefaultImageDurationMs is this player's own default dwell time for an
' image cast item whose `duration_ms` is absent or zero — PLY-083b fixes no
' contract-level default, leaving it to a player. 8000ms matches this
' ecosystem's own demo-cast per-item dwell time (snapshot.demoCastItemDurationMS
' — internal/feeder/snapshot/democast.go), not a coincidence: it is the same
' reasonable signage dwell time, just this player's own copy of that choice
' since a player has no way to read a feeder-side Go constant.
function wvDefaultImageDurationMs() as Integer
    return 8000
end function

' renderComposed presents a "composed" content item's layers (PLY-015):
' every layer is full screen and stacked in `layers` array order (index 0
' furthest back, later indices progressively on top) — this contract defines
' no per-layer geometry/timing schema (Scope, out of scope), so full-screen
' stacking in wire order is the only ordering it gives a player to honor.
' Layers were already fetched + asset_ref-verified by Program.brs before this
' ever runs (identical integrity guarantee as a cast item).
sub renderComposed(layers as Object)
    m.poster.visible = false
    m.video.control = "stop"
    m.video.visible = false
    clearComposed()

    if layers = invalid then return

    for each layer in layers
        if layer.contentType = "video"
            v = CreateObject("roSGNode", "Video")
            v.width = 1920
            v.height = 1080
            v.disableScreenSaver = true ' Same free, video-only PLY-158 coverage as the plain content Video node (init).
            v.observeField("state", "onComposedVideoStateChange")
            format = layer.streamFormat
            if format = "" then format = "mp4"
            content = CreateObject("roSGNode", "ContentNode")
            content.url = layer.contentUri
            content.streamFormat = format
            content.live = false
            v.content = content
            m.composedLayers.appendChild(v)
            v.control = "play"
        else
            p = CreateObject("roSGNode", "Poster")
            p.width = 1920
            p.height = 1080
            p.loadDisplayMode = "scaleToFill"
            p.uri = layer.contentUri
            m.composedLayers.appendChild(p)
        end if
    end for

    m.composedLayers.visible = true
end sub

' clearComposed stops any composed-layer Video children (so a superseded
' composed item never leaves a video still decoding behind whatever replaces
' it — no leaked playback, mirroring this player's no-leaked-Task-thread
' discipline) and removes every dynamically created layer child.
sub clearComposed()
    for each child in m.composedLayers.getChildren(-1, 0)
        if child.subtype() = "Video" then child.control = "stop"
    end for
    m.composedLayers.removeChildrenIndex(m.composedLayers.getChildCount(), 0)
    m.composedLayers.visible = false
end sub

' renderSlide presents a "slide" content item's layers (native slide rendering,
' PLY-012's "slide" type): each layer becomes ONE positioned SceneGraph node,
' created in `layers` array order so index 0 is furthest back and later indices
' paint on top (the same z-order rule composed uses, but here each node is
' individually translated to its own [x,y] and sized to its own w×h in the fixed
' 1920x1080 canvas rather than full-screen-stacked). The eight kinds fall into
' three groups, and the grouping is the design — see the wire Layer doc.
'
' STATIC (drawn once from what the layer carries):
'   text  -> Label (text; font size from font_px; color from color; horizAlign
'            from align — each applied only when the layer carries it)
'   rect  -> Rectangle filled with the layer's color
'   image -> Poster of the already-fetched + asset_ref-verified local contentUri
'            (Program.brs attached it; a slide image layer pays the identical
'            integrity guarantee as a plain image item)
'   video -> Video node of the already-fetched + asset_ref-verified local
'            contentUri, positioned and sized like every other layer and LOOPED
'            (see renderSlideVideo). It is grouped with the static kinds because
'            nothing on this thread has to keep it up to date: the platform
'            decodes it, so unlike a clock it needs no tick record and no timer
'
' SELF-TICKING (a function of the current time, so this player computes them and
' recomputes them once a second on the shared slideClockTimer — the visible proof
' this is native and not a frozen PNG). Each is recorded in m.slideTicking with
' what it needs to recompute itself, and each is SEEDED with its correct value at
' creation so it is right immediately rather than blank until the first tick:
'   clock     -> current LOCAL time through the layer's Go-reference-layout `text`
'   date      -> current LOCAL date through the same formatter and the same
'                layout convention — a date IS a time format, so there is one
'                formatter here, not two that could disagree
'   countdown -> the remaining time to the layer's absolute `target_ms`, rendered
'                through the countdown grammar (D/H/M/S tokens, not a
'                reference-time layout — a duration has no hour-of-day)
'
' SERVER-RESOLVED (drawn once, verbatim, from the `value` the box already
' computed — the player does NO lookup, NO formatting and NO unit conversion for
' these; the relay filled `value` as it issued this Lease):
'   weather -> Label of layer.value
'   entity  -> Label of layer.value
'
' An unrecognized kind is skipped (forward-compatible with a future kind this
' player has not adopted) rather than aborting the whole slide — the relay
' already validated the stack against the closed kind set, so this only guards a
' non-conformant relay or a newer contract, and skipping one unknown decorative
' layer degrades better than a blank screen. renderSlide clears any prior slide
' (children + tick timer) before building.
sub renderSlide(layers as Object)
    clearSlide()

    if layers = invalid then return

    for each layer in layers
        kind = wvSlideStr(layer.kind)
        ' Whether this layer produced anything on screen. Only a DRAWN layer may
        ' become a focus region: an unrecognized kind is skipped (see this sub's
        ' doc), and giving one a focus target anyway would put the ring around
        ' empty space the viewer cannot see, press, or move past without
        ' understanding why — a focus trap on exactly the forward-compatibility
        ' path that is supposed to degrade gracefully.
        drawn = true
        x = wvSlideInt(layer.x)
        y = wvSlideInt(layer.y)
        w = wvSlideInt(layer.w)
        h = wvSlideInt(layer.h)

        if kind = "text"
            lbl = createSlideLabel(wvSlideStr(layer.text), layer, x, y, w, h)
            m.slideLayers.appendChild(lbl)

        else if kind = "clock" or kind = "date"
            ' One branch for both: a date is a time format, so it goes through
            ' the same Go-reference-layout formatter. Splitting them would mean
            ' two formatters that could disagree about what "Jan" means.
            fmt = wvSlideStr(layer.text)
            lbl = createSlideLabel(wvFormatClockTime(fmt), layer, x, y, w, h)
            m.slideLayers.appendChild(lbl)
            ' Record the live Label + its format so slideClockTimer can refresh
            ' it; its text was just seeded so it is correct even before the first
            ' tick a second from now.
            m.slideTicking.Push({ label: lbl, kind: kind, format: fmt, target: 0 })

        else if kind = "countdown"
            fmt = wvSlideStr(layer.text)
            ' target_ms is an ABSOLUTE UTC epoch in MILLISECONDS, which does not
            ' fit a 32-bit Integer — it is carried as a Double and reduced to
            ' epoch SECONDS once, here, where it still fits comfortably. Every
            ' subsequent tick then subtracts roDateTime's own epoch seconds, so
            ' the countdown needs no timezone knowledge at all.
            target = wvSlideEpochSeconds(layer.target_ms)
            lbl = createSlideLabel(wvFormatCountdown(target - wvNowEpochSeconds(), fmt), layer, x, y, w, h)
            m.slideLayers.appendChild(lbl)
            m.slideTicking.Push({ label: lbl, kind: kind, format: fmt, target: target })

        else if kind = "weather" or kind = "entity"
            ' Server-resolved: the box already turned a forecast reading or an
            ' observed entity state into display text and put it in `value`
            ' (internal/slidelive, at Lease issuance). Draw it verbatim — this
            ' player deliberately performs no lookup, no formatting and no unit
            ' conversion for these, so there is exactly one place those decisions
            ' are made. No tick record either: the value changes only when a new
            ' Lease brings a new one, which re-renders the slide outright.
            lbl = createSlideLabel(wvSlideStr(layer.value), layer, x, y, w, h)
            m.slideLayers.appendChild(lbl)

        else if kind = "rect"
            rect = CreateObject("roSGNode", "Rectangle")
            rect.translation = [x, y]
            rect.width = w
            rect.height = h
            col = wvSlideColor(wvSlideStr(layer.color))
            if col <> "" then rect.color = col
            m.slideLayers.appendChild(rect)

        else if kind = "image"
            ' A content layer with no contentUri is a layer whose bytes this
            ' device does not hold — Program.brs could not fetch or verify them
            ' and DEGRADED the layer rather than discard the slide (see its
            ' degrading note, and PLY-087). It gets a visible placeholder, not a
            ' hole: an absent Poster is indistinguishable on a wall from a slide
            ' authored without one, and "the picture is missing" is exactly what
            ' the operator needs to see. This is the same choice slidelive makes
            ' one level up, where an unreachable weather or entity source draws
            ' its Unavailable "—" instead of blanking the whole slide.
            if wvSlideStr(layer.contentUri) = ""
                m.slideLayers.appendChild(createDegradedLayer(x, y, w, h))
            else
                p = CreateObject("roSGNode", "Poster")
                p.translation = [x, y]
                p.width = w
                p.height = h
                p.loadDisplayMode = "scaleToFill"
                p.uri = wvSlideStr(layer.contentUri)
                m.slideLayers.appendChild(p)
            end if

        else if kind = "video"
            ' Same rule as the image layer above, for the same reason.
            if wvSlideStr(layer.contentUri) = ""
                m.slideLayers.appendChild(createDegradedLayer(x, y, w, h))
            else
                m.slideLayers.appendChild(renderSlideVideo(layer, x, y, w, h))
            end if

        else if kind = "ping"
            ' A ping is a BUTTON: its label is drawn exactly as a `text` layer's
            ' is (same styling path, so font_px/color/align behave identically and
            ' an author does not learn two rules), and everything that makes it a
            ' button rather than a caption is the focus region registered below.
            ' It deliberately draws no background of its own: this model has one
            ' way to put a colored box behind something — a `rect` layer under it
            ' — and inventing a second, button-only fill would be a second color
            ' field with its own defaults to disagree with.
            lbl = createSlideLabel(wvSlideStr(layer.text), layer, x, y, w, h)
            m.slideLayers.appendChild(lbl)

        else if kind = "nav"
            ' Each nav ITEM becomes its own Label and its own focus region, laid
            ' out inside the layer's box along its longer axis (wvNavItemRects).
            ' Items are individual focus targets rather than one target with
            ' internal state, which is what lets the SAME spatial traversal move
            ' between two menu items, from a menu item to a ping button, and back
            ' — no per-kind key handling, and no "which control owns the D-pad
            ' right now?" question to get wrong (the legacy player carried exactly
            ' that question and answered it with a nav-vs-widget precedence rule).
            rects = wvNavItemRects(x, y, w, h, layer.items)
            if layer.items <> invalid
                for i = 0 to layer.items.Count() - 1
                    it = layer.items[i]
                    r = rects[i]
                    itemLbl = createSlideLabel(wvSlideStr(it.label), layer, r[0], r[1], r[2], r[3])
                    ' A menu item is centered in its own cell unless the author
                    ' said otherwise: an item's cell is computed, not authored, so
                    ' left-aligning by default would pin every label to a boundary
                    ' the author never drew.
                    if wvSlideStr(layer.align) = "" then itemLbl.horizAlign = "center"
                    itemLbl.vertAlign = "center"
                    m.slideLayers.appendChild(itemLbl)
                    m.focusTargets.Push({
                        x: r[0], y: r[1], w: r[2], h: r[3],
                        action: "nav",
                        pingName: "",
                        targetSlideId: wvSlideStr(it.target_slide_id)
                    })
                end for
            end if

        else
            ' Unknown kind — skip (see this sub's doc). Nothing is drawn for it.
            drawn = false
            print "[player-v3] slide layer with unsupported kind '" + kind + "' skipped"
        end if

        ' A ping name makes ANY layer focusable — that is the whole of the
        ' interactive-widget mechanism (wire.LayerIsInteractive): a `ping` layer
        ' requires one, and an `entity` reading, an `image`, or a `rect` used as
        ' an invisible hotspot may carry one too. Registered OUTSIDE the kind
        ' chain above, deliberately: putting it in the `ping` branch is the
        ' half-implementation this codebase keeps shipping — the required
        ' direction covered and the optional-on-every-other-kind direction
        ' silently inert, so a widget an author made interactive would draw
        ' perfectly and never take focus.
        '
        ' A nav layer is skipped here even if it somehow carried a ping name: its
        ' own items are already focus targets, and a whole-layer target on top of
        ' them would sit over every item and steal their focus.
        pingName = wvSlideStr(layer.ping_name)
        if pingName <> "" and kind <> "nav" and drawn
            m.focusTargets.Push({
                x: x, y: y, w: w, h: h,
                action: "ping",
                pingName: pingName,
                targetSlideId: ""
            })
        end if
    end for

    m.slideLayers.visible = true

    ' Put focus on the first interactive region this slide carries, if any. First
    ' in Z-ORDER rather than nearest a corner: the layer stack's order is the one
    ' thing the author controls directly, so "the first thing I placed" is a
    ' predictable landing spot, and a slide with exactly one button — by far the
    ' commonest case — lands on it with certainty.
    if m.focusTargets.Count() > 0
        setFocusIndex(0)
        print "[player-v3] slide has " + m.focusTargets.Count().toStr() + " interactive region(s) — D-pad focus active"
    else
        hideFocusRing()
    end if

    ' Start the once-a-second refresh only if the slide actually carries a
    ' self-ticking layer — a slide of static text, images and server-resolved
    ' widgets needs no timer running behind it at all.
    if m.slideTicking.Count() > 0
        m.slideClockTimer.duration = 1
        m.slideClockTimer.control = "start"
        print "[player-v3] slide tick started (" + m.slideTicking.Count().toStr() + " live layer(s), 1s tick)"
    end if
end sub

' renderSlideVideo builds the positioned, playing Video node for a slide's
' `video` layer — the only moving element a slide can carry. The bytes were
' already fetched and asset_ref-verified by Program.brs, which attached the
' local path as `contentUri` and the container format as `streamFormat`, so this
' function makes no decision about how the content arrived.
'
' Three choices are worth stating because they are not the obvious defaults:
'
'   - It LOOPS. A slide advances on its own dwell time (castTimer), not on the
'     video's end of stream — unlike a plain `video` cast item, where the end of
'     stream IS the advance signal. A slide's video is one element among several
'     and cannot be allowed to decide when the whole slide ends; without loop it
'     would simply freeze on its last frame for the remainder of the dwell,
'     which reads on a wall as a crashed screen. Looping makes a 5s clip fill a
'     30s slide, which is what a signage operator means by putting it there.
'   - It OBSERVES `state`. This node drives no sequencing, so the observer exists
'     only to report — which is the entire point, and the reason it was added
'     after the fact. Without it a slide video that fails on codec, network stall
'     or malformed container reports NOTHING: the region is black, no log line
'     exists anywhere, and there is no diagnosis. That is not a hypothetical
'     gap. Roku's own /plugin_inspect screenshot captures the GRAPHICS plane
'     only, so a black video region is what a capture shows even when playback is
'     perfect — with no state log and no visual channel there is no instrument at
'     all, and slide-video playback could not honestly be called verified. The
'     ITEM video path (onVideoStateChange) and the COMPOSED layer path
'     (onComposedVideoStateChange) both already observe `state`; this was the one
'     of the three that did not, which is exactly the shape this player keeps
'     shipping — a mechanism that works beside a reporting half that does not
'     exist. Teardown needs nothing extra for it: the observer lives on the node,
'     clearSlide stops and removes that node, and the composed path has been
'     doing precisely this since it was written.
'   - disableScreenSaver, matching the plain content Video node and composed
'     video layers: free, video-only PLY-158 coverage while it plays. It is not
'     the mechanism (IdleDefeatTask is) — it just costs nothing here.
'
' The node is returned rather than appended, so renderSlide's own loop stays the
' single place layers are added to the group in z-order.
function renderSlideVideo(layer as Object, x as Integer, y as Integer, w as Integer, h as Integer) as Object
    v = CreateObject("roSGNode", "Video")
    v.translation = [x, y]
    v.width = w
    v.height = h
    v.disableScreenSaver = true
    v.observeField("state", "onSlideVideoStateChange")

    format = wvSlideStr(layer.streamFormat)
    if format = "" then format = "mp4"
    content = CreateObject("roSGNode", "ContentNode")
    content.url = wvSlideStr(layer.contentUri)
    content.streamFormat = format
    content.live = false
    v.content = content
    v.loop = true
    v.control = "play"
    return v
end function

' createSlideLabel builds a positioned Label for a text/clock layer, applying
' font_px (via a sizable system-font file), color, and align only when the layer
' carries them — an absent style falls back to the Label's own default (PLY-012
' fixes none, mirroring the wire validator treating font_px/color/align as
' optional). width/height are set so horizAlign has a box to align within.
function createSlideLabel(text as String, layer as Object, x as Integer, y as Integer, w as Integer, h as Integer) as Object
    lbl = CreateObject("roSGNode", "Label")
    lbl.translation = [x, y]
    lbl.width = w
    lbl.height = h
    lbl.text = text

    fontPx = wvSlideInt(layer.font_px)
    if fontPx > 0
        font = CreateObject("roSGNode", "Font")
        ' "font:SystemFontFile" is the built-in system font-FILE URI whose size
        ' is settable (the fixed-size "font:MediumSystemFont" registry nodes are
        ' not resizable), so an authored font_px maps to a real pixel size.
        font.uri = "font:SystemFontFile"
        font.size = fontPx
        lbl.font = font
    end if

    col = wvSlideColor(wvSlideStr(layer.color))
    if col <> "" then lbl.color = col

    align = wvSlideStr(layer.align)
    if align = "left" or align = "center" or align = "right"
        lbl.horizAlign = align
    end if

    return lbl
end function

' createDegradedLayer builds the placeholder drawn in place of a slide `image` or
' `video` layer whose bytes this device does not hold (Program.brs degraded it —
' see its degrading note and PLY-087).
'
' It is a MUTED BOX with an em dash centred in it, positioned and sized exactly
' as the missing content would have been. Every part of that is deliberate:
'
'   - drawing something rather than nothing. An absent Poster leaves a hole a
'     viewer reads as "this slide was authored that way" and an operator cannot
'     see at all; the whole reason the slide is still on screen is that losing
'     its title, its clock and its other layers to one 403 fixes nothing
'     (wire.Layer.Value makes the identical argument for a live widget), and that
'     bargain is only honest if the loss is visible.
'   - the em dash, and not an error string. It is exactly what slidelive.
'     Unavailable puts in a weather or entity widget whose source could not
'     answer, so one convention covers every degraded element of a slide rather
'     than two that a viewer has to learn separately. A stack trace on a wall
'     helps nobody; the console log carries the reason (Program.brs prints the
'     item index, the layer index and the underlying error).
'   - a muted fill rather than the alert red: this is a missing picture, not an
'     emergency, and it must not out-shout the content that DID load.
'
' The two nodes are wrapped in one translated Group so renderSlide's loop still
' appends exactly one child per layer, keeping z-order = layer index.
function createDegradedLayer(x as Integer, y as Integer, w as Integer, h as Integer) as Object
    g = CreateObject("roSGNode", "Group")
    g.translation = [x, y]

    ' `fill` rather than `box`: `box` is a BrightScript reserved builtin and
    ' using it as an identifier is a compile error, not a style preference (the
    ' same rule `nextIdx` and `cell` already follow elsewhere in this file).
    fill = CreateObject("roSGNode", "Rectangle")
    fill.width = w
    fill.height = h
    fill.color = wvDegradedLayerColor()
    g.appendChild(fill)

    mark = CreateObject("roSGNode", "Label")
    mark.width = w
    mark.height = h
    mark.text = wvDegradedLayerText()
    mark.color = wvDegradedLayerTextColor()
    mark.horizAlign = "center"
    mark.vertAlign = "center"
    font = CreateObject("roSGNode", "Font")
    font.uri = "font:SystemFontFile"
    font.size = wvDegradedLayerFontPx(h)
    mark.font = font
    g.appendChild(mark)

    return g
end function

' wvDegradedLayerText is the placeholder glyph — the SAME em dash
' slidelive.Unavailable uses for a live widget whose source could not answer, so
' a slide degrades one way rather than two. Keep them equal.
function wvDegradedLayerText() as String
    return "—"
end function

' wvDegradedLayerColor / wvDegradedLayerTextColor are the placeholder's fill and
' glyph colors: a dark neutral gray under a lighter gray dash. Neutral on
' purpose — a degraded picture must be legible as missing without competing with
' the layers that loaded, and must never be mistaken for the alert red an
' override slide is drawn on (wire.alertBackgroundColor).
function wvDegradedLayerColor() as String
    return "0x2B2B2BFF"
end function

function wvDegradedLayerTextColor() as String
    return "0x8A8A8AFF"
end function

' wvDegradedLayerFontPx sizes the dash to the box it sits in — a third of the
' height — so a small logo slot and a full-bleed background both read correctly,
' clamped at both ends so a 40px strip still shows something and a full-canvas
' layer does not render a 360px dash.
function wvDegradedLayerFontPx(h as Integer) as Integer
    px = Int(h / 3)
    if px < 24 then px = 24
    if px > 96 then px = 96
    return px
end function

' clearSlide tears a slide down completely, in an order that is the whole point:
'
'  1. STOP the tick timer, so it can never fire against a Label about to be
'     removed. Stopping after the children were removed would leave one
'     already-queued fire able to touch a torn-down node.
'  2. Drop the tick records, so nothing holds a reference to those Labels.
'  3. STOP every Video child. Removing a Video node does NOT stop its playback —
'     the same "a thing outlives the node that owned it" hazard this player
'     already learned from Task threads and from composed video layers
'     (clearComposed does exactly this, for exactly this reason). A slide's
'     video left running would keep decoding, and keep its audio playing,
'     underneath whatever replaced it.
'  4. Remove every dynamically created slide child.
'
' Called before rendering a new slide, on switching to any other item kind, on a
' program-result error, and at scene shutdown — the same total-teardown
' discipline clearComposed applies, now covering the same two resource classes.
sub clearSlide()
    stopSlideClock()
    m.slideTicking = []
    ' Drop the focus regions and hide the ring BEFORE the children go: a ring
    ' left visible over a torn-down slide is an outline around nothing, and a
    ' stale focus target would let the next OK press dispatch the PREVIOUS
    ' slide's action — a button the viewer can no longer see firing an automation
    ' they did not choose. Same ordering rule, and the same reason, as stopping
    ' the tick timer first.
    m.focusTargets = []
    m.focusIndex = -1
    hideFocusRing()
    for each child in m.slideLayers.getChildren(-1, 0)
        if child.subtype() = "Video" then child.control = "stop"
    end for
    m.slideLayers.removeChildrenIndex(m.slideLayers.getChildCount(), 0)
    m.slideLayers.visible = false
end sub

' stopSlideClock halts the repeating slide tick timer. Split out so shutdown and
' clearSlide share one guarded stop.
sub stopSlideClock()
    if m.slideClockTimer <> invalid then m.slideClockTimer.control = "stop"
end sub

' onSlideClockTick is slideClockTimer's "fire" handler — once a second it
' recomputes every SELF-TICKING Label on the current slide: a clock or date from
' the current local time through its own layout, a countdown from the remaining
' seconds to its own target. This is what makes those widgets visibly live
' on-device rather than a picture of when the Lease arrived.
'
' The weather and entity widgets are deliberately absent: their value came from
' the box on the Lease, so there is nothing for this player to recompute — they
' change when a new Lease arrives, which re-renders the slide.
'
' It reads m.slideTicking, which clearSlide empties (and stops the timer) on
' teardown, so it can never touch a removed node.
sub onSlideClockTick()
    nowSec = wvNowEpochSeconds()
    for each c in m.slideTicking
        if c.kind = "countdown"
            c.label.text = wvFormatCountdown(c.target - nowSec, c.format)
        else
            c.label.text = wvFormatClockTime(c.format)
        end if
    end for
end sub

' ==========================================================================
' Interactive slide layers: D-pad focus, OK dispatch, nav jumps
' (parity milestones 1.5/3.7)
' ==========================================================================

' onKeyEvent is this scene's remote handler, and the ONLY one — the scene takes
' focus in init so key events reach it at all.
'
' It handles keys only while the SHOWING slide carries interactive regions
' (m.focusTargets), and returns false for everything else. Returning false
' matters: an unhandled key must stay unhandled so the platform keeps its own
' meaning for it (Home exits, and a future admin surface can claim a key without
' first prising it out of this function). A scene that swallowed every key would
' make a screen unexitable by remote, which on a wall-mounted panel with no
' keyboard is a genuine field problem.
'
' Only key DOWN is acted on. A remote delivers press and release for every key,
' and acting on both dispatches every interaction twice — two automations run for
' one human press.
'
' Every key this function consumes also RE-ARMS the slide's dwell timer. A slide
' carrying a menu is still a slide with a dwell time, and without this a viewer
' three items into a menu has it replaced mid-thought. Re-arming rather than
' pausing keeps the ordinary case honest: a slide nobody touches still advances
' exactly on its authored dwell, and one somebody is actively working stays put
' for as long as they keep working it.
function onKeyEvent(key as String, press as Boolean) as Boolean
    if not press then return false
    if m.focusTargets = invalid or m.focusTargets.Count() = 0 then return false

    if key = "OK"
        wvRestartDwell()
        activateFocusedTarget()
        return true
    end if

    if key = "up" or key = "down" or key = "left" or key = "right"
        ' `nextIdx` rather than `next`: `next` is a BrightScript keyword.
        nextIdx = wvSpatialNeighbor(m.focusIndex, key)
        if nextIdx < 0
            ' No target lies that way. Do NOT consume the key: a slide with one
            ' button must leave left/right free for whatever else the platform or
            ' a later surface does with them, and swallowing a press that moved
            ' nothing is indistinguishable to the viewer from the player having
            ' hung.
            return false
        end if
        wvRestartDwell()
        setFocusIndex(nextIdx)
        return true
    end if

    return false
end function

' wvRestartDwell re-arms the showing slide's advance timer at its full authored
' dwell. Guarded on a remembered non-zero dwell so it can never arm the timer
' with a zero duration (the render-thread-saturating value wvClampCastDurationMs
' exists to prevent) on an item kind that does not advance on a timer at all.
sub wvRestartDwell()
    if m.currentDwellMs = invalid or m.currentDwellMs <= 0 then return
    m.castTimer.control = "stop"
    m.castTimer.duration = m.currentDwellMs / 1000.0
    m.castTimer.control = "start"
end sub

' activateFocusedTarget performs the focused region's action — the OK press.
'
'   ping -> hand the press to InteractionTask, which POSTs it to the relay off
'           this thread. Fire and forget by design: the render thread must not
'           block on a network round trip (see InteractionTask.brs), so the
'           viewer's feedback is immediate and local.
'   nav  -> jump to the cast item whose slide id the item targets.
sub activateFocusedTarget()
    if m.focusIndex < 0 or m.focusIndex >= m.focusTargets.Count() then return
    t = m.focusTargets[m.focusIndex]

    if t.action = "nav"
        jumpToSlideId(t.targetSlideId)
        return
    end if

    if m.leaseId = ""
        ' Nothing has been served yet, so there is no lease to attribute the
        ' press to and the relay would refuse it. Say so rather than post a
        ' request built to fail.
        print "[player-v3] press '" + t.pingName + "' NOT sent — no lease is currently held"
        return
    end if

    ' A FRESH assoc array per press, never a mutated-and-re-set one: a
    ' SceneGraph field observer fires on assignment, and re-assigning the same
    ' reference makes what the Task reads depend on when it happens to run rather
    ' than on what was published (the same rule PlayerTask states for
    ' photonResult).
    press = {
        lease_id: m.leaseId,
        interaction: t.pingName,
        slide_id: currentSlideId()
    }
    m.interactionTask.press = press
    print "[player-v3] press '" + t.pingName + "' -> relay (lease " + m.leaseId + ")"
end sub

' currentSlideId is the cast-local slide id of the item on screen, or "" for an
' item that has none (a plain image/video, an inline slide, a generated alert).
function currentSlideId() as String
    if m.castItems = invalid or m.castIndex < 0 or m.castIndex >= m.castItems.Count() then return ""
    return wvSlideStr(m.castItems[m.castIndex].slideId)
end function

' jumpToSlideId presents the cast item whose cast-local slide id is slideId — a
' nav item's OK action.
'
' The lookup is by ID over the cast's own items, never by an index carried on the
' wire, because the two are not the same thing: a cast's slides are projected
' into the SAME content array as whatever else the playlist carries, so an index
' authored against the cast addresses the wrong item the moment a playlist gains
' an entry before it. Matching on the id the projection stamped onto the item
' (LeaseContent.slide_id) is invariant under every such change.
'
' A target that resolves to nothing is LOGGED and does nothing else. It should be
' unreachable — the authoring gate refuses a cast whose nav item targets a slide
' the cast does not declare (datamodel.checkNavTargets) — so reaching it means
' either an older relay or a program assembled some other way, and guessing
' (jumping to slide 0, say) would send a viewer somewhere nobody chose.
sub jumpToSlideId(slideId as String)
    if slideId = "" then return
    if m.castItems = invalid then return
    for i = 0 to m.castItems.Count() - 1
        if wvSlideStr(m.castItems[i].slideId) = slideId
            print "[player-v3] nav -> slide '" + slideId + "' (cast item " + i.toStr() + ")"
            m.castIndex = i
            renderCastItem()
            return
        end if
    end for
    print "[player-v3] nav target '" + slideId + "' matches no item in the current cast — ignored"
end sub

' setFocusIndex moves focus to target i and repositions the ring around it.
sub setFocusIndex(i as Integer)
    if i < 0 or i >= m.focusTargets.Count()
        hideFocusRing()
        return
    end if
    m.focusIndex = i
    t = m.focusTargets[i]
    showFocusRing(t.x, t.y, t.w, t.h)
end sub

' showFocusRing draws the four-Rectangle outline around a region, inset outward
' by wvFocusRingPad so the ring frames the target rather than covering its edges.
'
' The ring is CLAMPED to the canvas: a target flush against an edge would
' otherwise place a bar at a negative coordinate, where SceneGraph draws it
' off-screen and the outline silently becomes three-sided — focus that looks like
' focus on every layer except the ones at the edges, which is where an author is
' most likely to put a menu.
sub showFocusRing(x as Integer, y as Integer, w as Integer, h as Integer)
    pad = wvFocusRingPad()
    th = wvFocusRingThickness()

    rx = x - pad
    ry = y - pad
    rw = w + pad * 2
    rh = h + pad * 2
    if rx < 0
        rw = rw + rx
        rx = 0
    end if
    if ry < 0
        rh = rh + ry
        ry = 0
    end if
    if rx + rw > 1920 then rw = 1920 - rx
    if ry + rh > 1080 then rh = 1080 - ry
    if rw <= 0 or rh <= 0
        hideFocusRing()
        return
    end if

    m.focusRingTop.translation = [rx, ry]
    m.focusRingTop.width = rw
    m.focusRingTop.height = th

    m.focusRingBottom.translation = [rx, ry + rh - th]
    m.focusRingBottom.width = rw
    m.focusRingBottom.height = th

    m.focusRingLeft.translation = [rx, ry]
    m.focusRingLeft.width = th
    m.focusRingLeft.height = rh

    m.focusRingRight.translation = [rx + rw - th, ry]
    m.focusRingRight.width = th
    m.focusRingRight.height = rh

    m.focusRing.visible = true
end sub

' hideFocusRing hides the outline and clears the focus index, so a later OK can
' never dispatch against a region nothing is pointing at.
sub hideFocusRing()
    if m.focusRing <> invalid then m.focusRing.visible = false
    m.focusIndex = -1
end sub

' wvFocusRingPad / wvFocusRingThickness size the outline. Sized for a television
' seen across a room, not a desk monitor: a 2px hairline is invisible at viewing
' distance, which makes focus untrackable and the whole D-pad model unusable.
function wvFocusRingPad() as Integer
    return 8
end function

function wvFocusRingThickness() as Integer
    return 6
end function

' wvSpatialNeighbor picks the focus target to move to from `from` in direction
' `key`, or -1 when nothing lies that way.
'
' The rule is: consider only targets whose CENTRE is strictly beyond the current
' target's centre in the pressed direction, and choose the one minimising
' (distance along the pressed axis) + (perpendicular offset x 2). The
' perpendicular weight is what makes a grid behave: without it, pressing `right`
' from a menu item can land on a button two rows down merely because it is a few
' pixels further right, and the viewer's mental model breaks immediately. It is a
' weight rather than a hard "must overlap on the other axis" rule because real
' slides are not grids — a ping button beside a menu is rarely exactly aligned
' with it, and a strict rule would make it unreachable.
'
' Spatial rather than list-order traversal is the point. Order-based movement
' (next/previous in the layer stack) is trivial to write and wrong on any slide
' where the author's layout does not match their insertion order — pressing
' `right` and having focus jump upward is the kind of thing that reads as broken
' hardware.
function wvSpatialNeighbor(from as Integer, key as String) as Integer
    if from < 0 or from >= m.focusTargets.Count() then return -1
    cur = m.focusTargets[from]
    cx = cur.x + cur.w / 2
    cy = cur.y + cur.h / 2

    best = -1
    bestScore = 0
    for i = 0 to m.focusTargets.Count() - 1
        if i <> from
            t = m.focusTargets[i]
            tx = t.x + t.w / 2
            ty = t.y + t.h / 2

            along = 0
            across = 0
            ok = false
            if key = "right"
                ok = tx > cx
                along = tx - cx
                across = Abs(ty - cy)
            else if key = "left"
                ok = tx < cx
                along = cx - tx
                across = Abs(ty - cy)
            else if key = "down"
                ok = ty > cy
                along = ty - cy
                across = Abs(tx - cx)
            else if key = "up"
                ok = ty < cy
                along = cy - ty
                across = Abs(tx - cx)
            end if

            if ok
                score = along + across * 2
                if best < 0 or score < bestScore
                    best = i
                    bestScore = score
                end if
            end if
        end if
    end for
    return best
end function

' wvNavItemRects lays a nav layer's items out inside its own box and returns one
' [x, y, w, h] per item, in item order.
'
' It is the on-device TRANSCRIPTION of wire.NavItemRects (internal/shared/wire/
' lease.go), and it must stay one: the Studio canvas draws the menu from the Go
' function's rects and this player focuses and hits it with these, so any
' disagreement puts the focus ring somewhere other than the label it belongs to.
' BrightScript cannot call Go, so the two are pinned by a test that reads THIS
' source (TestNavItemRectsMatchesPlayerTranscription) rather than left to drift.
'
' The axis follows the box's own aspect — wider than tall is a row, otherwise a
' column — so the drawn geometry alone decides the orientation and there is no
' second authored field that could contradict it. The LAST item absorbs the
' integer-division remainder so the items exactly fill the box.
function wvNavItemRects(x as Integer, y as Integer, w as Integer, h as Integer, items as Object) as Object
    out = []
    if items = invalid then return out
    n = items.Count()
    if n <= 0 then return out

    ' `cell` rather than `step`: `step` is a BrightScript keyword (for…step) and
    ' using it as an identifier is a parse error, not a style preference.
    if w >= h
        cell = Int(w / n)
        for i = 0 to n - 1
            iw = cell
            if i = n - 1 then iw = w - cell * (n - 1)
            out.Push([x + cell * i, y, iw, h])
        end for
        return out
    end if

    cell = Int(h / n)
    for i = 0 to n - 1
        ih = cell
        if i = n - 1 then ih = h - cell * (n - 1)
        out.Push([x, y + cell * i, w, ih])
    end for
    return out
end function

' wvNowEpochSeconds is the current UTC instant in Unix epoch SECONDS — the unit a
' countdown's target is reduced to. roDateTime is UTC unless told otherwise, and
' a countdown deliberately stays in UTC on both sides: the target is an absolute
' instant, so converting either end to local time could only introduce an error.
function wvNowEpochSeconds() as Integer
    dt = CreateObject("roDateTime")
    return dt.AsSeconds()
end function

' wvSlideEpochSeconds reduces a layer's `target_ms` (absolute UTC epoch
' MILLISECONDS) to epoch seconds. The millisecond value exceeds the 32-bit
' Integer range, so it is read as a Double and divided BEFORE being narrowed —
' narrowing first would overflow and produce a nonsense target.
function wvSlideEpochSeconds(v as Dynamic) as Integer
    if v = invalid then return 0
    ms# = 0.0
    ms# = ms# + v
    return Int(ms# / 1000.0)
end function

' wvFormatCountdown renders a remaining-time span through the countdown layout
' grammar the wire Layer doc fixes: `DD`/`D` days, `HH`/`H` hours, `MM`/`M`
' minutes, `SS`/`S` seconds, longest match first, everything else literal. An
' empty layout means "HH:MM:SS".
'
' A larger unit's remainder is only taken out when that larger unit APPEARS in
' the layout: "HH:MM:SS" on a two-day span shows 48 hours rather than silently
' dropping the days, which is the failure a fixed hours-mod-24 would produce on
' exactly the slides (a multi-day countdown) most likely to be authored.
'
' A span that has passed clamps to zero rather than counting up: a finished
' countdown reads "00:00:00", and negative numbers on a wall read as a bug.
function wvFormatCountdown(remainSec as Integer, format as String) as String
    fmt = format
    if fmt = "" then fmt = "HH:MM:SS"

    secs = remainSec
    if secs < 0 then secs = 0

    days = 0
    hours = 0
    mins = 0
    if Instr(1, fmt, "D") > 0
        days = Int(secs / 86400)
        secs = secs - days * 86400
    end if
    if Instr(1, fmt, "H") > 0
        hours = Int(secs / 3600)
        secs = secs - hours * 3600
    end if
    if Instr(1, fmt, "M") > 0
        mins = Int(secs / 60)
        secs = secs - mins * 60
    end if

    ' Doubled tokens before single ones so the walk takes the longest match at
    ' each position — "DD" must never be read as two "D"s.
    tokens = [
        { t: "DD", v: wvPad2(days) },
        { t: "HH", v: wvPad2(hours) },
        { t: "MM", v: wvPad2(mins) },
        { t: "SS", v: wvPad2(secs) },
        { t: "D", v: days.toStr() },
        { t: "H", v: hours.toStr() },
        { t: "M", v: mins.toStr() },
        { t: "S", v: secs.toStr() }
    ]
    return wvApplyFormatTokens(fmt, tokens)
end function

' wvApplyFormatTokens walks a format string left to right, replacing the FIRST
' matching token from `tokens` at each position and copying any unmatched
' character through literally. The caller is responsible for ordering `tokens`
' longest-first, which is what makes "first match" mean "longest match".
'
' It is shared by the two format grammars (reference-time and countdown) rather
' than written twice: the walk is identical and only the token table differs, and
' two copies of a string walk is two places for an off-by-one to hide.
function wvApplyFormatTokens(format as String, tokens as Object) as String
    result = ""
    n = Len(format)
    i = 0
    while i < n
        matched = false
        for each tok in tokens
            tl = Len(tok.t)
            if i + tl <= n
                if Mid(format, i + 1, tl) = tok.t   ' Mid is 1-indexed
                    result = result + tok.v
                    i = i + tl
                    matched = true
                    exit for
                end if
            end if
        end for
        if not matched
            result = result + Mid(format, i + 1, 1)
            i = i + 1
        end if
    end while
    return result
end function

' wvFormatClockTime renders the current LOCAL time through a Go-reference-time
' layout string (the convention the producer's clock AND date formats both use —
' the wire tests seed "15:04", "15:04:05", "3:04 PM", "Monday, January 2"). It
' walks the format left to right (wvApplyFormatTokens), at each position
' replacing the longest recognized reference token with the corresponding
' current-time value and copying any unrecognized character (":", " ", etc.)
' through literally. Supported tokens are the common date/time ones; a token this
' does not recognize (e.g. a timezone form) simply passes through as literal text
' rather than erroring — a slide with an exotic format still draws, just with
' that piece uninterpreted.
'
' It serves the `date` kind as well as the `clock` kind, unchanged: a date is a
' time format, and the whole point of fixing ONE layout convention on the wire is
' that one formatter can answer both.
function wvFormatClockTime(format as String) as String
    if format = "" then return ""

    dt = CreateObject("roDateTime")
    dt.ToLocalTime()   ' device-timezone local time (roDateTime is UTC otherwise)
    year = dt.GetYear()
    month = dt.GetMonth()          ' 1-12
    day = dt.GetDayOfMonth()
    hour24 = dt.GetHours()         ' 0-23
    minute = dt.GetMinutes()
    second = dt.GetSeconds()
    weekday = dt.GetDayOfWeek()    ' 0=Sunday .. 6=Saturday

    hour12 = hour24 mod 12
    if hour12 = 0 then hour12 = 12
    ampmUpper = "AM"
    ampmLower = "am"
    if hour24 >= 12
        ampmUpper = "PM"
        ampmLower = "pm"
    end if

    monthsShort = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]
    monthsLong = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"]
    daysShort = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"]
    daysLong = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"]

    ' Month/weekday are 1-based / 0-based; guard the index defensively in case a
    ' platform ever returns an out-of-range value.
    monShort = "" : monLong = "" : monNum = month.toStr() : monPad = wvPad2(month)
    if month >= 1 and month <= 12
        monShort = monthsShort[month - 1]
        monLong = monthsLong[month - 1]
    end if
    dayShort = "" : dayLong = ""
    if weekday >= 0 and weekday <= 6
        dayShort = daysShort[weekday]
        dayLong = daysLong[weekday]
    end if

    ' Reference tokens ordered LONGEST-first so the walk below takes the longest
    ' match at each position ("2006" before "06"/"2", "15" before "1"/"5").
    tokens = [
        { t: "January", v: monLong },
        { t: "Monday", v: dayLong },
        { t: "2006", v: year.toStr() },
        { t: "Jan", v: monShort },
        { t: "Mon", v: dayShort },
        { t: "15", v: wvPad2(hour24) },
        { t: "03", v: wvPad2(hour12) },
        { t: "04", v: wvPad2(minute) },
        { t: "05", v: wvPad2(second) },
        { t: "01", v: monPad },
        { t: "02", v: wvPad2(day) },
        { t: "06", v: wvPad2(year mod 100) },
        { t: "PM", v: ampmUpper },
        { t: "pm", v: ampmLower },
        { t: "3", v: hour12.toStr() },
        { t: "4", v: minute.toStr() },
        { t: "5", v: second.toStr() },
        { t: "1", v: monNum },
        { t: "2", v: day.toStr() }
    ]

    return wvApplyFormatTokens(format, tokens)
end function

' wvPad2 zero-pads a non-negative integer to at least two digits.
function wvPad2(n as Integer) as String
    s = n.toStr()
    if Len(s) < 2 then s = "0" + s
    return s
end function

' wvSlideColor converts a wire "#RRGGBB" color to the "0xRRGGBBAA" string a
' SceneGraph color field accepts (opaque alpha appended). Returns "" for an
' absent or malformed value so the caller can fall back to the node's own
' default color rather than forcing black. The relay already validated every
' color as #RRGGBB (wire.isHexColor), so this is belt-and-suspenders.
function wvSlideColor(hex as String) as String
    if hex = "" then return ""
    h = hex
    if Left(h, 1) = "#" then h = Mid(h, 2)   ' strip leading '#'
    if Len(h) <> 6 then return ""
    return "0x" + h + "FF"
end function

' wvSlideStr coerces a possibly-invalid layer field to a string. Local to this
' component because a SceneGraph component's scope does not inherit pkg:/source
' globals (only the scripts its own XML includes), so Pairing.brs's wvStr is not
' in scope here.
function wvSlideStr(v as Dynamic) as String
    if v = invalid then return ""
    if type(v) = "roString" or type(v) = "String" then return v
    return v.toStr()
end function

' wvSlideInt coerces a possibly-invalid numeric layer field to an integer,
' defaulting to 0 (a real canvas coordinate — a layer always states geometry, so
' this only guards a malformed wire value).
function wvSlideInt(v as Dynamic) as Integer
    if v = invalid then return 0
    return Int(v)
end function

' onVideoStateChange handles the plain content Video node's end-of-stream and
' error states. A "finished" video cast item advances to the next cast item
' (PLY-083a) — a one-item cast's "next" item is itself, so this is exactly
' this player's prior "restart on finish" behavior in the degenerate case,
' not a behavior change for an existing single-video Lease. "error" is
' surfaced to the status label rather than left as a frozen or blank video
' frame — a silent stuck screen is worse than a visible error.
sub onVideoStateChange()
    state = m.video.state
    if state = "finished"
        print "[player-v3] cast item video finished — advancing (end-of-stream, PLY-083a)"
        advanceCast()
    else if state = "error"
        errCode = m.video.errorCode
        errMsg = m.video.errorMsg
        if errMsg = invalid then errMsg = ""
        print "[player-v3] video ERROR code=" + errCode.toStr() + " msg=" + errMsg
        m.video.visible = false
        m.status.text = "Waiveo Player v3 — video playback error (" + errCode.toStr() + ") " + errMsg
        m.status.visible = true
    end if
end sub

' onComposedVideoStateChange mirrors onVideoStateChange's end-of-stream and
' error handling for a composed item's own video layer(s) (renderComposed) —
' there can be more than one such node, so the changed node itself is read
' off the field-change event rather than a single fixed m. field.
sub onComposedVideoStateChange(event as Object)
    node = event.GetRoSGNode()
    state = event.GetData()
    if state = "finished"
        print "[player-v3] composed layer video finished — restarting playback (end-of-stream)"
        node.control = "play"
    else if state = "error"
        errCode = node.errorCode
        errMsg = node.errorMsg
        if errMsg = invalid then errMsg = ""
        print "[player-v3] composed layer video ERROR code=" + errCode.toStr() + " msg=" + errMsg
    end if
end sub

' onSlideVideoStateChange is the slide `video` layer's observer
' (renderSlideVideo). It exists ONLY to report, and that is the point — see
' renderSlideVideo's own doc for why a fire-and-forget slide video is
' undiagnosable from outside the device.
'
' It drives nothing. A slide advances on its own dwell timer, and this node
' loops, so neither "finished" nor anything else here changes what is on screen —
' unlike onVideoStateChange, whose "finished" IS the cast's advance signal. Every
' transition is printed rather than just the failures, because the question row
' 2.5 has to answer is "did it PLAY", and only a positive signal answers it: a
' `playing` line is the evidence a screenshot cannot supply (Roku captures the
' graphics plane only, so the video region reads black either way). The volume is
' bounded by the slide's own dwell — a handful of lines per render, not a stream.
'
' Both values are coerced through wvSlideStr/wvSlideInt before they touch a
' concatenation. A diagnostic must never be the thing that takes the player
' down: `"string" + invalid` is a type mismatch that terminates the thread it
' runs on, which is how a single slide once killed this whole player from inside
' a log line.
sub onSlideVideoStateChange(event as Object)
    node = event.GetRoSGNode()
    state = wvSlideStr(event.GetData())
    if state = "error"
        errMsg = ""
        if node <> invalid then errMsg = wvSlideStr(node.errorMsg)
        errCode = 0
        if node <> invalid then errCode = wvSlideInt(node.errorCode)
        print "[player-v3] slide layer video ERROR code=" + errCode.toStr() + " msg=" + errMsg + " — this layer's region is black; the rest of the slide is unaffected"
    else
        print "[player-v3] slide layer video state -> " + state
    end if
end sub
