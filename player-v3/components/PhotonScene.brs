' PhotonScene.brs — renders the first photon. Creates the PlayerTask, starts it
' once a pairing code is available (or the device is already paired), and swaps
' in a Poster (image) or a Video node (video) when the task reports content
' that has already been fetched AND asset_ref-verified (Program.brs never hands
' back a contentUri that has not passed that check).

sub init()
    m.bg = m.top.findNode("bg")
    m.poster = m.top.findNode("contentPoster")
    m.video = m.top.findNode("contentVideo")
    m.video.observeField("state", "onVideoStateChange")
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
        if res.contentType = "video"
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
            m.video.control = "stop"
            m.video.visible = false
            m.poster.uri = res.contentUri
            m.poster.visible = true
        end if
        m.status.visible = false
    else
        m.video.control = "stop"
        m.video.visible = false
        m.poster.visible = false
        detail = res.status
        if res.error <> "" then detail = detail + " — " + res.error
        m.status.text = "Waiveo Player v3 — " + detail
        m.status.visible = true
    end if
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
