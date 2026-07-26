' Program.brs — the on-device player/1 program thread (PLY-080/084/090/091).
' Pulls a signed Lease over the pinned relay connection, fetches the one
' content item (image, video, or a "composed" item's image/video layers —
' PLY-014's content-type floor plus PLY-015's composed restriction) DIRECT
' from its feeder content-origin URL, verifies the fetched bytes against each
' item's own asset_ref (content-addressed integrity) BEFORE returning
' anything to render, and returns either a single local file path + content
' type (image/video) or a layers array (composed) for PhotonScene to present.
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
' A "composed" item's layers pay the IDENTICAL per-layer guarantee (see
' wvFetchAndVerifyItem below) — composing layers never relaxes integrity.
'
' Composed note (PLY-015/PLY-083): this contract restricts a composed item's
' `layers` to `image`/`video` members and reserves per-layer geometry/timing
' to a future contract (Scope, out of scope) — it defines no positioning
' schema at all. Absent one, this player renders every layer full-screen,
' stacked in `layers` array order (index 0 furthest back, later indices
' progressively on top) — the only ordering the wire shape itself gives a
' player to honor. A layer whose `type` is anything other than image/video
' MUST NOT reach a player at all (a relay rejects it at compile time,
' PLY-015) — if one arrives anyway this player rejects the whole composed
' item rather than guess how to render an unsupported layer.
'
' Scope note (first photon): the Lease's ed25519 signature (PLY-090) is NOT
' verified on-device — Roku exposes no ed25519 primitive, and first-photon's
' trust basis is the pinned TLS channel to the relay plus the asset_ref
' content-address check below. A later increment adds signature verification
' (or a Roku-supported signature scheme).
'
' wvDoProgram(state) where state = { channelToken, relayHost, relayPort, trustPem }
' returns { ok, contentUri, contentType ("image"|"video"|"composed"),
' streamFormat (video only), layers (composed only — array of {contentUri,
' contentType, streamFormat}), leaseId, error, needsRepair }.

function wvDoProgram(state as Object) as Object
    r = { ok: false, contentUri: "", contentType: "", streamFormat: "", layers: invalid, leaseId: "", error: "", needsRepair: false }

    pinFile = wvRehydrateTrustFile(state.trustPem)
    if pinFile = ""
        r.error = "could not rehydrate pinned trust anchor"
        return r
    end if

    base = "https://" + state.relayHost + ":" + state.relayPort

    ' --- pinned program poll (PLY-090): peer verification on against the pinned
    '     relay cert, host verification off (cert reached by IP, no SAN). ---
    ' content_types (PLY-012) declares exactly what this player can actually
    ' render: "image" and "video" (wvFetchAndVerifyItem/the fetch path below),
    ' plus "composed" now that wvDoProgram assembles its image/video layers
    ' (PLY-015) into a stacked full-screen render. Keep this declaration in
    ' lockstep with what is really implemented — a relay filters program
    ' assignment against it (PLY-013), so an over-claim here would get a
    ' player content it cannot show, and an under-claim would starve it of
    ' content it could.
    reqBody = FormatJson({ capabilities: { content_types: ["image", "video", "composed"], player_version: "3.0.0" } })
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
        r.error = "lease carried no image, video, or composed content item (content-type gate or empty program)"
        return r
    end if
    itemType = wvStr(item.type)

    if itemType = "composed"
        ' --- composed item (PLY-015/PLY-083): fetch+verify every layer with
        ' the identical per-item integrity pipeline plain image/video items
        ' use (wvFetchAndVerifyItem), one local file per layer so a video
        ' layer and an image layer never collide on disk. ---
        layers = item.layers
        if layers = invalid or layers.Count() = 0
            r.error = "composed content item carried no layers"
            return r
        end if

        outLayers = []
        for i = 0 to layers.Count() - 1
            layer = layers[i]
            layerType = wvStr(layer.type)
            if layerType <> "image" and layerType <> "video"
                ' Defensive only: PLY-015 requires a relay to reject any
                ' non-image/video (or nested composed) layer at compile time
                ' and never deliver it. If one reaches this player anyway
                ' (a non-conformant relay, or a future contract revision this
                ' player has not adopted), the whole composed item is
                ' rejected rather than silently dropping a layer or guessing
                ' how to render an unsupported type.
                r.error = "composed layer " + i.toStr() + " has unsupported type '" + layerType + "' (PLY-015 restricts layers to image/video)"
                return r
            end if

            localPath = wvLocalPathForLayer(layerType, i)
            fv = wvFetchAndVerifyItem(layer, localPath)
            if not fv.ok
                r.error = "composed layer " + i.toStr() + ": " + fv.error
                return r
            end if

            lr = { contentUri: localPath, contentType: layerType, streamFormat: "" }
            if layerType = "video" then lr.streamFormat = wvVideoStreamFormat()
            outLayers.Push(lr)
        end for

        wvAckLease(base, state.channelToken, pinFile, r.leaseId)

        r.layers = outLayers
        r.contentType = "composed"
        r.ok = true
        return r
    end if

    ' --- direct content fetch (PLY-084) + asset_ref integrity ---
    ' Same fetch-whole-file-then-verify pipeline for both types (see the
    ' integrity note at the top of this file for why video pays a full
    ' pre-fetch instead of streaming). The local path's extension only aids
    ' debugging — the Video node below is told its format explicitly via
    ' streamFormat, not by sniffing this filename.
    localPath = "cachefs:/waiveo_player_v3_content.img"
    if itemType = "video" then localPath = "cachefs:/waiveo_player_v3_content.mp4"

    fv = wvFetchAndVerifyItem(item, localPath)
    if not fv.ok
        r.error = fv.error
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

' wvFirstPlayableItem returns the first content item of type "image",
' "video", or "composed" in the lease (PLY-014's content-type floor plus
' PLY-015's composed form), or invalid.
function wvFirstPlayableItem(lease as Object) as Dynamic
    content = lease.content
    if content = invalid then return invalid
    for each item in content
        if item.type = "image" or item.type = "video" or item.type = "composed" then return item
    end for
    return invalid
end function

' wvLocalPathForLayer returns a stable, per-layer local cache path so a
' composed item's layers never collide with each other or with a plain
' single-item fetch's own path.
function wvLocalPathForLayer(layerType as String, index as Integer) as String
    ext = ".img"
    if layerType = "video" then ext = ".mp4"
    return "cachefs:/waiveo_player_v3_layer" + index.toStr() + ext
end function

' wvFetchAndVerifyItem fetches a single Content reference's `url` to
' localPath and verifies the fetched bytes against its `asset_ref`
' (PLY-084/PLY-014 integrity). Shared by the plain image/video path and each
' layer of a "composed" item (PLY-015) so every layer pays the identical
' asset_ref-verified-before-presented guarantee as a plain item.
function wvFetchAndVerifyItem(item as Object, localPath as String) as Object
    r = { ok: false, error: "" }

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

    r.ok = true
    return r
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
