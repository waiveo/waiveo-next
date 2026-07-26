' Program.brs — the on-device player/1 program thread (PLY-080/084/090/091).
' Pulls a signed Lease over the pinned relay connection, fetches the one
' content item (image OR video — PLY-014's content-type floor) DIRECT from its
' feeder content-origin URL, verifies the fetched bytes against the lease's
' asset_ref (content-addressed integrity) BEFORE returning anything to render,
' and returns a local file path + content type for PhotonScene to present via
' a Poster (image) or Video node (video).
'
' Integrity note (video, PLY-084/PLY-014): this player fetches the WHOLE asset
' to a local file and hashes it before ever handing a URI back for playback —
' exactly the same asset_ref-verified-before-presented guarantee as the image
' path, just applied to a larger file. There is no streaming/progressive-start
' path here: a large video's playback start is gated on its full download, not
' merely its first bytes. That is a real latency cost for a large asset, but
' it means the integrity property is IDENTICAL between image and video — a
' player never presents a byte this check has not covered. A future streaming
' player would need a different integrity primitive (e.g. a per-chunk hash
' tree) to preserve any equivalent guarantee without paying for a full
' pre-fetch; that does not exist yet, so streaming is out of scope here.
'
' Scope note (first photon): the Lease's ed25519 signature (PLY-090) is NOT
' verified on-device — Roku exposes no ed25519 primitive, and first-photon's
' trust basis is the pinned TLS channel to the relay plus the asset_ref
' content-address check below. A later increment adds signature verification
' (or a Roku-supported signature scheme).
'
' wvDoProgram(state) where state = { channelToken, relayHost, relayPort, trustPem }
' returns { ok, contentUri, contentType ("image"|"video"), streamFormat
' (video only), leaseId, error, needsRepair }.

function wvDoProgram(state as Object) as Object
    r = { ok: false, contentUri: "", contentType: "", streamFormat: "", leaseId: "", error: "", needsRepair: false }

    pinFile = wvRehydrateTrustFile(state.trustPem)
    if pinFile = ""
        r.error = "could not rehydrate pinned trust anchor"
        return r
    end if

    base = "https://" + state.relayHost + ":" + state.relayPort

    ' --- pinned program poll (PLY-090): peer verification on against the pinned
    '     relay cert, host verification off (cert reached by IP, no SAN). ---
    ' content_types (PLY-012) declares exactly what this player can actually
    ' render: both "image" and "video" now that wvFirstPlayableItem/the fetch
    ' path below handle both. Keep this declaration in lockstep with what is
    ' really implemented — a relay filters program assignment against it
    ' (PLY-013), so an over-claim here would get a player content it cannot
    ' show, and an under-claim would starve it of content it could.
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

    item = wvFirstPlayableItem(lease)
    if item = invalid
        r.error = "lease carried no image or video content item (content-type gate or empty program)"
        return r
    end if
    itemType = wvStr(item.type)

    ' --- direct content fetch (PLY-084) + asset_ref integrity ---
    ' Same fetch-whole-file-then-verify pipeline for both types (see the
    ' integrity note at the top of this file for why video pays a full
    ' pre-fetch instead of streaming). The local path's extension only aids
    ' debugging — the Video node below is told its format explicitly via
    ' streamFormat, not by sniffing this filename.
    localPath = "cachefs:/waiveo_player_v3_content.img"
    if itemType = "video" then localPath = "cachefs:/waiveo_player_v3_content.mp4"

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

    r.contentUri = localPath
    r.contentType = itemType
    if itemType = "video" then r.streamFormat = wvVideoStreamFormat()
    r.ok = true
    return r
end function

' wvFirstPlayableItem returns the first content item of type "image" or
' "video" in the lease (PLY-014's content-type floor), or invalid. A
' "composed" item is not matched here — this player does not declare
' "composed" in content_types (PLY-012/013), so a conformant relay never
' assigns one, but this loop would skip it even if one arrived.
function wvFirstPlayableItem(lease as Object) as Dynamic
    content = lease.content
    if content = invalid then return invalid
    for each item in content
        if item.type = "image" or item.type = "video" then return item
    end for
    return invalid
end function

' wvVideoStreamFormat is the Video node ContentNode.streamFormat this player
' assumes for every video content item. A Content reference (PLY-083) carries
' no format field of its own, and this player's fetch pipeline only ever
' downloads a single whole file to local storage (the integrity note above) —
' which can only ever be played back as a plain progressive file, never as an
' adaptive manifest (HLS/DASH) referencing further segment URLs. "mp4" matches
' this ecosystem's own video content-type convention (contracts/archive-1.md's
' `"content_type": "video/mp4"` worked example). Supporting another progressive
' container, or an adaptive format, would need a real format field on the wire
' shape and a different (non-full-file) fetch strategy — not implemented here.
function wvVideoStreamFormat() as String
    return "mp4"
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
