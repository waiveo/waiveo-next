' Program.brs — the on-device player/1 program thread (PLY-080/084/090/091).
' Pulls a signed Lease over the pinned relay connection, fetches the one image
' content item DIRECT from its feeder content-origin URL, verifies the fetched
' bytes against the lease's asset_ref (content-addressed integrity), and returns
' a local file path for the SceneGraph Poster to render.
'
' Scope note (first photon): the Lease's ed25519 signature (PLY-090) is NOT
' verified on-device — Roku exposes no ed25519 primitive, and first-photon's
' trust basis is the pinned TLS channel to the relay plus the asset_ref
' content-address check below. A later increment adds signature verification
' (or a Roku-supported signature scheme).
'
' wvDoProgram(state) where state = { channelToken, relayHost, relayPort, trustPem }
' returns { ok, imageUri, leaseId, error, needsRepair }.

function wvDoProgram(state as Object) as Object
    r = { ok: false, imageUri: "", leaseId: "", error: "", needsRepair: false }

    pinFile = wvRehydrateTrustFile(state.trustPem)
    if pinFile = ""
        r.error = "could not rehydrate pinned trust anchor"
        return r
    end if

    base = "https://" + state.relayHost + ":" + state.relayPort

    ' --- pinned program poll (PLY-090): peer verification on against the pinned
    '     relay cert, host verification off (cert reached by IP, no SAN). ---
    reqBody = FormatJson({ capabilities: { content_types: ["image", "video"], player_version: "3.0.0" } })
    resp = wvHttpJson({
        method: "GET",
        url: base + "/player/v1/program",
        body: reqBody,
        bearer: state.channelToken,
        certFile: pinFile,
        peerVerify: true,
        hostVerify: false,
        timeoutMs: 8000
    })

    if not resp.ok
        ' 401 CHANNEL_TOKEN_INVALID/EXPIRED (PLY-072/073): the token must be
        ' re-paired; signal the caller rather than looping.
        if resp.code = 401 then r.needsRepair = true
        r.error = wvProgramErrorText(resp)
        return r
    end if

    lease = ParseJson(resp.body)
    if lease = invalid
        r.error = "program response was not valid JSON"
        return r
    end if
    r.leaseId = wvStr(lease.lease_id)

    item = wvFirstImageItem(lease)
    if item = invalid
        r.error = "lease carried no image content item (content-type gate or empty program)"
        return r
    end if

    ' --- direct content fetch (PLY-084) + asset_ref integrity ---
    localPath = "cachefs:/waiveo_player_v3_content.img"
    fetch = wvHttpGetToFile(wvStr(item.url), localPath, 15000)
    if not fetch.ok
        r.error = "content fetch failed: HTTP " + fetch.code.toStr() + " " + fetch.failureReason
        return r
    end if

    ok = wvVerifyAssetRef(localPath, wvStr(item.asset_ref))
    if not ok
        r.error = "content integrity check FAILED (fetched bytes do not match asset_ref " + wvStr(item.asset_ref) + ")"
        return r
    end if

    ' --- best-effort lease ack (PLY-091), over the pinned connection ---
    wvAckLease(base, state.channelToken, pinFile, r.leaseId)

    r.imageUri = localPath
    r.ok = true
    return r
end function

' wvFirstImageItem returns the first content item of type "image" in the lease,
' or invalid.
function wvFirstImageItem(lease as Object) as Dynamic
    content = lease.content
    if content = invalid then return invalid
    for each item in content
        if item.type = "image" then return item
    end for
    return invalid
end function

' wvVerifyAssetRef reports whether the bytes at path content-address to assetRef
' ("sha256:<hex>"). This is first-photon's integrity guarantee for the direct
' content fetch (PLY-084), standing in for TLS trust on the content channel.
function wvVerifyAssetRef(path as String, assetRef as String) as Boolean
    if assetRef = "" then return false
    prefix = "sha256:"
    if assetRef.Instr(prefix) <> 0 then return false   ' must start with sha256:
    wantHex = LCase(assetRef.Mid(prefix.Len()))

    ba = CreateObject("roByteArray")
    ok = false
    try
        ok = ba.ReadFile(path)
    catch e
        return false
    end try
    if not ok then return false
    if ba.Count() = 0 then return false

    gotHex = wvSha256Hex(ba)
    return wvHexEqualConstant(gotHex, wantHex)
end function

' wvAckLease POSTs a lease acknowledgement (PLY-091), best-effort — a failure
' here does not block rendering an already-verified image.
sub wvAckLease(base as String, channelToken as String, pinFile as String, leaseId as String)
    if leaseId = "" then return
    ackBody = FormatJson({ lease_id: leaseId, accepted: true })
    wvHttpJson({
        method: "POST",
        url: base + "/player/v1/lease/ack",
        body: ackBody,
        bearer: channelToken,
        certFile: pinFile,
        peerVerify: true,
        hostVerify: false,
        timeoutMs: 8000
    })
end sub

function wvProgramErrorText(resp as Object) as String
    if resp.body <> ""
        pb = ParseJson(resp.body)
        if pb <> invalid
            if pb.code <> invalid
                return "program rejected: " + wvStr(pb.code) + " (" + wvStr(pb.title) + ")"
            end if
        end if
    end if
    if resp.startFailed then return "could not reach relay: " + resp.failureReason
    return "program HTTP " + resp.code.toStr() + " " + resp.failureReason
end function
