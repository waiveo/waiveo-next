' PhotonScene.brs — renders the first photon. Creates the PlayerTask, starts it
' once a pairing code is available (or the device is already paired), and swaps
' the Poster in when the task reports a verified image.

sub init()
    m.bg = m.top.findNode("bg")
    m.poster = m.top.findNode("contentPoster")
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
        m.poster.uri = res.imageUri
        m.poster.visible = true
        m.status.visible = false
    else
        detail = res.status
        if res.error <> "" then detail = detail + " — " + res.error
        m.status.text = "Waiveo Player v3 — " + detail
        m.status.visible = true
    end if
end sub
