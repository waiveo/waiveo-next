' Http.brs — roUrlTransfer helpers for the player/1 flow, factored so the TLS
' posture of each call site is explicit and matches the contract:
'
'   - Bootstrap pair POST (PLY-040/041): peer+host verification DISABLED for the
'     one pairing attempt. The response's trust_anchors are authenticated
'     out-of-band by the fingerprint commitment, not by this TLS channel.
'   - Steady-state program poll (PLY-090): peer verification ENABLED against the
'     pinned relay trust-anchor cert file; host verification DISABLED (the relay
'     bootstrap cert is reached by IP and carries no matching SAN — the pin is
'     the cert itself, not its hostname).
'   - Direct content fetch (PLY-084): peer+host verification DISABLED; integrity
'     is the asset_ref content-address check the caller runs on the bytes, not
'     this channel (mirrors the Go virtual player's fetchContent).
'
' All patterns (async + wait, GetFailureReason, negative libcurl codes) are the
' hardware-verified ones from the Wave-0 TLS spike.

' wvHttpJson performs an HTTP request expecting a JSON (text) response.
' opts: {
'   method: "GET"|"POST", url, body (String or invalid), bearer (String or invalid),
'   certFile (path String or invalid), peerVerify (Boolean or invalid),
'   hostVerify (Boolean or invalid), timeoutMs (Integer, default 8000)
' }
' Returns { ok: Boolean(2xx), code: Integer, body: String, headers: AA,
'           failureReason: String, startFailed: Boolean }.
'
' `headers` carries the response headers verbatim (roUrlEvent.GetResponseHeaders),
' empty when the transfer never produced a response. It is NOT a convenience: on
' observed Roku firmware GetString() returns an EMPTY body for ANY 401 no matter
' what the server sent, while the headers for that same response arrive intact
' (measured on device against this relay, which demonstrably sends a correct
' application/problem+json body a non-Roku client reads fine). The response's
' `Problem-Code` header (player/1 PLY-008) is therefore the ONLY channel through
' which the Error-taxonomy code of a 401 reaches this player at all — see
' wv401Code in Program.brs.
function wvHttpJson(opts as Object) as Object
    result = { ok: false, code: -9999, body: "", headers: {}, failureReason: "", startFailed: false }

    tr = CreateObject("roUrlTransfer")
    tr.SetUrl(opts.url)

    if opts.DoesExist("certFile")
        if opts.certFile <> invalid
            tr.SetCertificatesFile(opts.certFile)
        end if
    end if
    if opts.DoesExist("peerVerify")
        if opts.peerVerify <> invalid then tr.EnablePeerVerification(opts.peerVerify)
    end if
    if opts.DoesExist("hostVerify")
        if opts.hostVerify <> invalid then tr.EnableHostVerification(opts.hostVerify)
    end if

    tr.AddHeader("Content-Type", "application/json")
    if opts.DoesExist("bearer")
        if opts.bearer <> invalid
            tr.AddHeader("Authorization", "Bearer " + opts.bearer)
        end if
    end if

    timeoutMs = 8000
    if opts.DoesExist("timeoutMs")
        if opts.timeoutMs <> invalid then timeoutMs = opts.timeoutMs
    end if

    msgPort = CreateObject("roMessagePort")
    tr.SetMessagePort(msgPort)

    body = ""
    if opts.DoesExist("body")
        if opts.body <> invalid then body = opts.body
    end if

    startOk = false
    try
        if opts.method = "POST"
            startOk = tr.AsyncPostFromString(body)
        else if body <> ""
            ' GET carrying a JSON body (PLY-080's worked example): set the method
            ' explicitly, then post the body. Confirmed pattern on-device TODO.
            tr.SetRequest("GET")
            startOk = tr.AsyncPostFromString(body)
        else
            startOk = tr.AsyncGetToString()
        end if
    catch e
        result.startFailed = true
        result.failureReason = "exception starting transfer: " + e.message
        return result
    end try

    if not startOk
        result.startFailed = true
        result.failureReason = "transfer failed to start"
        return result
    end if

    msg = wait(timeoutMs, msgPort)
    if msg = invalid
        result.code = -28
        result.failureReason = "client-side timeout"
        return result
    end if

    result.code = msg.GetResponseCode()
    result.body = msg.GetString()
    hdrs = invalid
    try
        hdrs = msg.GetResponseHeaders()
    catch e
        hdrs = invalid
    end try
    if hdrs <> invalid then result.headers = hdrs
    reason = msg.GetFailureReason()
    if reason = invalid then reason = ""
    result.failureReason = reason
    if result.code >= 200
        if result.code < 300 then result.ok = true
    end if
    return result
end function

' wvHeaderValue reads one header out of a response header map, case-insensitively
' and without assuming the map's own lookup mode. roUrlEvent.GetResponseHeaders
' returns whatever key casing the firmware chose ("Problem-Code", "problem-code",
' …) in an associative array whose case-sensitivity mode this code does not
' control, so neither a direct `headers[name]` index nor a fixed-case one is
' safe. Iterating and comparing LCase(key) is correct under every combination.
' Returns "" when headers is invalid/empty or carries no such header — an absent
' header is never itself a value (PLY-008: a client falls back to the body).
function wvHeaderValue(headers as Object, name as String) as String
    if headers = invalid then return ""
    want = LCase(name)
    for each k in headers
        if LCase(k) = want
            v = headers[k]
            if v = invalid then return ""
            return wvStr(v)
        end if
    end for
    return ""
end function

' wvHttpGetToFile fetches url directly to a local file path (binary-safe, for
' image content). Returns { ok, code, failureReason, startFailed }.
function wvHttpGetToFile(url as String, path as String, timeoutMs as Integer) as Object
    result = { ok: false, code: -9999, failureReason: "", startFailed: false }

    tr = CreateObject("roUrlTransfer")
    tr.SetUrl(url)
    ' PLY-084: verification disabled; integrity is the caller's asset_ref check.
    tr.EnablePeerVerification(false)
    tr.EnableHostVerification(false)

    msgPort = CreateObject("roMessagePort")
    tr.SetMessagePort(msgPort)

    startOk = false
    try
        startOk = tr.AsyncGetToFile(path)
    catch e
        result.startFailed = true
        result.failureReason = "exception starting file transfer: " + e.message
        return result
    end try
    if not startOk
        result.startFailed = true
        result.failureReason = "file transfer failed to start"
        return result
    end if

    msg = wait(timeoutMs, msgPort)
    if msg = invalid
        result.code = -28
        result.failureReason = "client-side timeout"
        return result
    end if

    result.code = msg.GetResponseCode()
    reason = msg.GetFailureReason()
    if reason = invalid then reason = ""
    result.failureReason = reason
    if result.code >= 200
        if result.code < 300 then result.ok = true
    end if
    return result
end function
