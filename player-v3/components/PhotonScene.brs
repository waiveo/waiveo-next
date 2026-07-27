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
    m.status = m.top.findNode("statusLabel")
    m.status.text = "Waiveo Player v3 — starting…"

    ' castTimer drives an image cast item's own dwell time (PLY-083b): started
    ' fresh for every image item (renderCastItem), firing once (repeat=false)
    ' to advance to the next item (PLY-083a). A video item's own advance
    ' signal is its Video node's "finished" state (onVideoStateChange), not
    ' this timer — the timer is simply left stopped while a video item shows.
    m.castTimer = m.top.findNode("castTimer")
    m.castTimer.observeField("fire", "onCastTimerFire")

    ' idleDefeatTimer holds this platform's own idle/inactivity surface off
    ' assigned content (PLY-158). It is XML-declared and wired exactly once
    ' here — never per cast, per item, or per render — and only RUNS while
    ' content is actually assigned (setIdleDefeat). See IdleDefeat.brs for
    ' why the manifest's screensaver_private flag does not cover this and why
    ' a permanently-playing hidden video was rejected.
    m.idleDefeatTimer = m.top.findNode("idleDefeatTimer")
    m.idleDefeatTimer.duration = wvIdleDefeatIntervalSeconds()
    m.idleDefeatTimer.observeField("fire", "onIdleDefeatTick")
    m.idleDefeatEngaged = false
    m.idleDefeatTicks = 0

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
        if it.layers <> invalid
            for j = 0 to it.layers.Count() - 1
                sig = sig + ";L" + castStr(it.layers[j].contentUri)
            end for
        end if
    end for
    return sig
end function

' startCast begins (or replaces) an ordered cast (PLY-083a): stops any
' composed-layer rendering, resets to item 0, and renders it.
sub startCast(items as Object)
    clearComposed()
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

    else if item.contentType = "video"
        clearComposed()
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
' idle/inactivity surface. Called on every program result, so it acts only on
' an actual transition — which is also exactly what makes the two console
' lines below readable: one when the mechanism starts holding the platform's
' idle clock off assigned content, one when it stops, and nothing in between
' (onIdleDefeatTick's own heartbeat aside). On engage the clock is refreshed
' immediately as well as on the timer: it has been running untouched for as
' long as this player had nothing assigned, so waiting a full interval could
' let it expire between the first item appearing and the first tick.
sub setIdleDefeat(engage as Boolean)
    if engage = m.idleDefeatEngaged then return
    m.idleDefeatEngaged = engage

    if engage
        m.idleDefeatTicks = 0
        wvIdleDefeatPing()
        m.idleDefeatTimer.control = "start"
        print "[player-v3] idle-defeat ENGAGED (PLY-158) — content assigned; refreshing this platform's idle clock every " + wvIdleDefeatIntervalSeconds().toStr() + "s"
    else
        m.idleDefeatTimer.control = "stop"
        print "[player-v3] idle-defeat DISENGAGED (PLY-158) — no content assigned; this platform's idle surface is free to engage"
    end if
end sub

' onIdleDefeatTick is idleDefeatTimer's own "fire" handler: one refresh of the
' platform's last-input time per tick (wvIdleDefeatPing), plus a periodic
' heartbeat line so on-device verification can READ that the mechanism is
' still running deep into a device's idle delay rather than infer it from the
' absence of a screensaver. See wvIdleDefeatHeartbeatTicks for the ratio.
sub onIdleDefeatTick()
    wvIdleDefeatPing()
    m.idleDefeatTicks = m.idleDefeatTicks + 1
    if m.idleDefeatTicks mod wvIdleDefeatHeartbeatTicks() = 0
        print "[player-v3] idle-defeat alive (PLY-158) — " + m.idleDefeatTicks.toStr() + " refreshes, " + (m.idleDefeatTicks * wvIdleDefeatIntervalSeconds()).toStr() + "s engaged"
    end if
end sub

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
