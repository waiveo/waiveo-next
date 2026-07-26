' Program.brs — the on-device player/1 program thread (PLY-080/084/090/091).
' Pulls a signed Lease over the pinned relay connection, fetches EVERY plain
' image/video content item the Lease's `content` array carries (PLY-014's
' content-type floor), in order, DIRECT from its feeder content-origin URL,
' verifies each item's fetched bytes against its own asset_ref
' (content-addressed integrity) BEFORE returning anything to render, and
' returns either an ordered `items` array (PLY-083a: one or more plain
' image/video items, each already fetched + verified, each carrying its own
' `durationMs` per PLY-083b) or a `layers` array (a single "composed" item,
' PLY-015) for PhotonScene to present. A Lease whose first content item is
' "composed" is handled as that single composed item (this codebase does not
' mix a composed item into an ordered cast); otherwise every image/video item
' present becomes one entry of `items`, cycled by PhotonScene per PLY-083a —
' a one-item Lease is simply the degenerate one-entry case of the same path,
' so this is not two code paths glued together, just one.
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
' returns { ok, contentType ("cast"|"composed"), items (cast only — ordered
' array of {contentUri, contentType ("image"|"video"), streamFormat,
' durationMs}), layers (composed only — array of {contentUri, contentType,
' streamFormat}), leaseId, error, needsRepair }.

function wvDoProgram(state as Object) as Object
    r = { ok: false, contentType: "", items: invalid, layers: invalid, leaseId: "", error: "", needsRepair: false }

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

    ' Printed verbatim (evidence trail): this is the exact Lease JSON the relay
    ' signed and served over the pinned connection, before any parsing.
    print "[player-v3] program response body: " + resp.body

    lease = ParseJson(resp.body)
    if lease = invalid
        r.error = "program response was not valid JSON"
        return r
    end if
    r.leaseId = wvStr(lease.lease_id)

    content = lease.content
    if content = invalid or content.Count() = 0
        r.error = "lease carried an empty or missing content array"
        return r
    end if

    ' A Lease whose first item is "composed" is handled as that single
    ' composed item (unchanged from this player's pre-cast behavior) — this
    ' codebase never mixes a composed item into an ordered cast. Every other
    ' Lease is treated as an ordered cast of its image/video items, PLY-083a
    ' (a one-item cast being the degenerate single-item case).
    firstType = wvStr(content[0].type)

    if firstType = "composed"
        item = content[0]
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

            localPath = wvLocalPathForCastItem(layerType, i)
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

    ' --- ordered cast: fetch + verify EVERY image/video item, in order
    ' (PLY-083a) --- Same fetch-whole-file-then-verify pipeline for both
    ' types (see the integrity note at the top of this file for why video
    ' pays a full pre-fetch instead of streaming). Any item whose type is
    ' neither image nor video (nor composed, handled above) is skipped —
    ' forward-compatible with a future content-type vocabulary this player
    ' has not adopted (mirroring PLY-016's server-side rule, applied
    ' defensively here too).
    castOut = []
    for i = 0 to content.Count() - 1
        item = content[i]
        itemType = wvStr(item.type)
        if itemType = "image" or itemType = "video"
            localPath = wvLocalPathForCastItem(itemType, i)
            fv = wvFetchAndVerifyItem(item, localPath)
            if not fv.ok
                r.error = "cast item " + i.toStr() + ": " + fv.error
                return r
            end if

            ci = { contentUri: localPath, contentType: itemType, streamFormat: "", durationMs: wvItemDurationMs(item) }
            if itemType = "video" then ci.streamFormat = wvVideoStreamFormat()
            castOut.Push(ci)
        end if
    end for

    if castOut.Count() = 0
        r.error = "lease carried no image or video content item (content-type gate or empty program)"
        return r
    end if

    ' --- best-effort lease ack (PLY-091), over the pinned connection ---
    wvAckLease(base, state.channelToken, pinFile, r.leaseId)

    r.items = castOut
    r.contentType = "cast"
    r.ok = true
    return r
end function

' wvItemDurationMs reads a Content reference's own `duration_ms` (PLY-083b),
' returning 0 when absent — PhotonScene supplies its own default dwell time
' for a zero value (this contract fixes none, PLY-083b).
function wvItemDurationMs(item as Object) as Integer
    d = item.duration_ms
    if d = invalid then return 0
    return Int(d)
end function

' wvLocalPathForCastItem returns a stable, per-index local cache path so a
' cast's items (or a composed item's layers, which call this same helper)
' never collide with each other on disk.
function wvLocalPathForCastItem(itemType as String, index as Integer) as String
    ext = ".img"
    if itemType = "video" then ext = ".mp4"
    return "cachefs:/waiveo_player_v3_item" + index.toStr() + ext
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
