' PhotonScene.brs — renders the first photon. Creates the PlayerTask, starts it
' once a pairing code is available (or the device is already paired), and swaps
' in a Poster (image), a Video node (video), or a stacked group of full-screen
' Poster/Video children (composed, PLY-015) when the task reports content that
' has already been fetched AND asset_ref-verified (Program.brs never hands back
' a contentUri, or a composed layer, that has not passed that check).

sub init()
    m.bg = m.top.findNode("bg")
    m.poster = m.top.findNode("contentPoster")
    m.video = m.top.findNode("contentVideo")
    m.video.observeField("state", "onVideoStateChange")
    m.composedLayers = m.top.findNode("composedLayers")
    m.status = m.top.findNode("statusLabel")
    m.status.text = "Waiveo Player v3 — starting…"

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
        if res.contentType = "composed"
            renderComposed(res.layers)
        else if res.contentType = "video"
            clearComposed()
            format = res.streamFormat
            if format = "" then format = "mp4"
            content = CreateObject("roSGNode", "ContentNode")
            content.url = res.contentUri
            content.streamFormat = format
            content.live = false

            m.poster.visible = false
            m.video.content = content
            m.video.visible = true
            m.video.control = "play"
        else
            ' image (or any future non-video type this player does not yet
            ' distinguish): stop and hide the Video node first so a prior
            ' playback does not keep running behind a poster.
            clearComposed()
            m.video.control = "stop"
            m.video.visible = false
            m.poster.uri = res.contentUri
            m.poster.visible = true
        end if
        m.status.visible = false
    else
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

' renderComposed presents a "composed" content item's layers (PLY-015):
' every layer is full screen and stacked in `layers` array order (index 0
' furthest back, later indices progressively on top) — this contract defines
' no per-layer geometry/timing schema (Scope, out of scope), so full-screen
' stacking in wire order is the only ordering it gives a player to honor.
' Layers were already fetched + asset_ref-verified by Program.brs before this
' ever runs (identical integrity guarantee as the plain image/video path).
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

' onVideoStateChange handles the Video node's end-of-stream and error states.
' There is no further-lease-pull loop yet (PlayerTask fetches its one photon
' item once) — a "finished" video is deliberately restarted so a screen never
' goes dark simply because its single verified item finished playing, mirroring
' PLY-087's "keep showing what you have" posture for the no-further-content
' case. "error" is surfaced to the status label rather than left as a frozen or
' blank video frame — a silent stuck screen is worse than a visible error.
sub onVideoStateChange()
    state = m.video.state
    if state = "finished"
        print "[player-v3] video finished — restarting playback (end-of-stream)"
        m.video.control = "play"
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
