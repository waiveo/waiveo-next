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
    ' (renderSlide); slideClockTimer refreshes every clock layer's Label once a
    ' second. slideClocks pairs each live clock Label with its own Go-reference
    ' time-format string so the one shared timer can refresh them all. The timer
    ' is a fixed node (like castTimer) precisely so this scene always holds a
    ' handle to STOP it on item change (clearSlide) and on shutdown — a per-slide
    ' timer that outlives its slide is the leak class this player already fixed
    ' for Task threads.
    m.slideLayers = m.top.findNode("slideLayers")
    m.slideClockTimer = m.top.findNode("slideClockTimer")
    m.slideClockTimer.duration = 1
    m.slideClockTimer.observeField("fire", "onSlideClockTick")
    m.slideClocks = []

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
        stopCastTimer()
        ' PLY-158's obligation is tied to being actively assigned non-blank
        ' content; this branch has torn the content down and is showing a
        ' status line, so stop holding the platform awake.
        setIdleDefeat(false)
        clearComposed()
        clearSlide()
        m.video.control = "stop"
        m.video.visible = false
        m.poster.visible = false
        detail = res.status
        if res.error <> "" then detail = detail + " — " + res.error
        m.status.text = "Waiveo Player v3 — " + detail
        m.status.visible = true
    end if
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

function castSignature(items as Object) as String
    if items = invalid then return ""
    sig = items.Count().toStr()
    for i = 0 to items.Count() - 1
        it = items[i]
        sig = sig + "|" + castStr(it.contentType) + ";" + castStr(it.contentUri) + ";"
        if it.durationMs <> invalid then sig = sig + it.durationMs.toStr()
        ' A composed layer contributes only its resolved contentUri; a slide
        ' layer additionally contributes its kind, text, color, and geometry —
        ' two slides that share the same image bytes but differ in a text/clock
        ' string, a color, or a position are genuinely different programs and
        ' MUST re-render, so keying a slide's signature on contentUri alone
        ' (invalid for every non-image layer) would collapse them into one and
        ' pin the screen on the first. castStr coerces the fields absent on a
        ' composed layer (kind/text/color/x…) to "" so this stays correct for
        ' both item kinds.
        if it.layers <> invalid
            for j = 0 to it.layers.Count() - 1
                ly = it.layers[j]
                sig = sig + ";L" + castStr(ly.contentUri) + "," + castStr(ly.kind) + "," + castStr(ly.text) + "," + castStr(ly.color) + "," + castStr(ly.x) + "," + castStr(ly.y) + "," + castStr(ly.w) + "," + castStr(ly.h)
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
    if m.task <> invalid then m.task.control = "STOP"
    if m.castTimer <> invalid then m.castTimer.control = "stop"
    ' A slide's clock timer is a repeating Timer; like castTimer it must be
    ' stopped on teardown so it never fires against a torn-down slide.
    if m.slideClockTimer <> invalid then m.slideClockTimer.control = "stop"
    print "[player-v3] scene shutdown — idle-defeat and player tasks stopped"
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
' 1920x1080 canvas rather than full-screen-stacked). The four v1 kinds:
'   text  -> Label (text; font size from font_px; color from color; horizAlign
'            from align — each applied only when the layer carries it)
'   rect  -> Rectangle filled with the layer's color
'   image -> Poster of the already-fetched + asset_ref-verified local contentUri
'            (Program.brs attached it; a slide image layer pays the identical
'            integrity guarantee as a plain image item)
'   clock -> a Label showing the current LOCAL time formatted through the layer's
'            `text` Go-reference-layout format string, refreshed once a second by
'            the shared slideClockTimer (the visible proof this is native, not a
'            frozen PNG). Every clock Label is recorded in m.slideClocks with its
'            format so the one timer refreshes them all.
' An unrecognized kind is skipped (forward-compatible with a future kind this
' player has not adopted) rather than aborting the whole slide — the relay
' already validated the stack against the closed kind set, so this only guards a
' non-conformant relay or a newer contract, and skipping one unknown decorative
' layer degrades better than a blank screen. renderSlide clears any prior slide
' (children + clock timer) before building.
sub renderSlide(layers as Object)
    clearSlide()

    if layers = invalid then return

    for each layer in layers
        kind = wvSlideStr(layer.kind)
        x = wvSlideInt(layer.x)
        y = wvSlideInt(layer.y)
        w = wvSlideInt(layer.w)
        h = wvSlideInt(layer.h)

        if kind = "text"
            lbl = createSlideLabel(wvSlideStr(layer.text), layer, x, y, w, h)
            m.slideLayers.appendChild(lbl)

        else if kind = "clock"
            fmt = wvSlideStr(layer.text)
            lbl = createSlideLabel(wvFormatClockTime(fmt), layer, x, y, w, h)
            m.slideLayers.appendChild(lbl)
            ' Record the live Label + its format so slideClockTimer can refresh
            ' it; its text was just seeded so it is correct even before the first
            ' tick a second from now.
            m.slideClocks.Push({ label: lbl, format: fmt })

        else if kind = "rect"
            rect = CreateObject("roSGNode", "Rectangle")
            rect.translation = [x, y]
            rect.width = w
            rect.height = h
            col = wvSlideColor(wvSlideStr(layer.color))
            if col <> "" then rect.color = col
            m.slideLayers.appendChild(rect)

        else if kind = "image"
            p = CreateObject("roSGNode", "Poster")
            p.translation = [x, y]
            p.width = w
            p.height = h
            p.loadDisplayMode = "scaleToFill"
            p.uri = wvSlideStr(layer.contentUri)
            m.slideLayers.appendChild(p)

        else
            ' Unknown kind — skip (see this sub's doc). Nothing is drawn for it.
            print "[player-v3] slide layer with unsupported kind '" + kind + "' skipped"
        end if
    end for

    m.slideLayers.visible = true

    ' Start the once-a-second refresh only if the slide actually carries a clock
    ' layer — an all-static slide needs no ticking timer running behind it.
    if m.slideClocks.Count() > 0
        m.slideClockTimer.duration = 1
        m.slideClockTimer.control = "start"
        print "[player-v3] slide clock started (" + m.slideClocks.Count().toStr() + " clock layer(s), 1s tick)"
    end if
end sub

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

' clearSlide tears a slide down completely: it STOPS the clock timer first (so it
' can never fire against a Label about to be removed), drops the clock records,
' then removes every dynamically created slide child. Called before rendering a
' new slide, on switching to any other item kind, on a program-result error, and
' at scene shutdown — the same total-teardown discipline clearComposed applies.
sub clearSlide()
    stopSlideClock()
    m.slideClocks = []
    m.slideLayers.removeChildrenIndex(m.slideLayers.getChildCount(), 0)
    m.slideLayers.visible = false
end sub

' stopSlideClock halts the repeating clock timer. Split out so shutdown and
' clearSlide share one guarded stop.
sub stopSlideClock()
    if m.slideClockTimer <> invalid then m.slideClockTimer.control = "stop"
end sub

' onSlideClockTick is slideClockTimer's "fire" handler — once a second it
' refreshes every live clock Label with the current local time reformatted
' through that layer's own format string. This is what makes the clock visibly
' tick on-device. It reads m.slideClocks, which clearSlide empties (and stops
' the timer) on teardown, so it can never touch a removed node.
sub onSlideClockTick()
    for each c in m.slideClocks
        c.label.text = wvFormatClockTime(c.format)
    end for
end sub

' wvFormatClockTime renders the current LOCAL time through a Go-reference-time
' layout string (the convention the producer's clock format uses — the wire
' tests seed "15:04", "15:04:05", "3:04 PM"). It walks the format left to right,
' at each position replacing the longest recognized reference token with the
' corresponding current-time value and copying any unrecognized character
' (":", " ", etc.) through literally. Supported tokens are the common date/time
' ones; a token this does not recognize (e.g. a timezone form) simply passes
' through as literal text rather than erroring — a slide with an exotic format
' still draws, just with that piece uninterpreted.
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
