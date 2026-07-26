' Program.brs — the on-device player/1 program thread (PLY-080/084/090/091).
' Pulls a signed Lease over the pinned relay connection, fetches EVERY
' content item the Lease's `content` array carries (PLY-014's content-type
' floor), in order, DIRECT from its feeder content-origin URL, verifies each
' item's (and each composed item's own layer's) fetched bytes against its own
' asset_ref (content-addressed integrity) BEFORE returning anything to
' render, and returns a single ordered `items` array (PLY-083a) for
' PhotonScene to present — one entry per `content` array element, IN THAT
' ARRAY'S OWN ORDER, with NO type-based carve-out: a plain `image`/`video`
' item becomes a plain cast entry, and a `composed` item (PLY-015) becomes a
' cast entry of its own carrying its fetched+verified `layers`, cycled
' exactly like any other entry. PLY-083a governs sequencing for "every
' content item a Lease carries" (PLY-083) — it draws no distinction between a
' composed item and a plain one, so this player draws none either: a Lease
' mixing composed and plain items presents ALL of them, in order, never
' silently dropping whichever ones don't match a special-cased first-item
' type. A one-item Lease (of either kind) is simply the degenerate
' one-entry case of this same path.
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
' item (and, per the no-silent-drop rule above, the whole Lease — a
' half-presented cast is exactly the silent content loss PLY-083a exists to
' prevent) rather than guess how to render an unsupported layer.
'
' Scope note (first photon): the Lease's ed25519 signature (PLY-090) is NOT
' verified on-device — Roku exposes no ed25519 primitive, and first-photon's
' trust basis is the pinned TLS channel to the relay plus the asset_ref
' content-address check below. A later increment adds signature verification
' (or a Roku-supported signature scheme).
'
' wvDoProgram(state) where state = { channelToken, relayHost, relayPort, trustPem }
' returns { ok, contentType ("cast"), items (ordered array of cast entries —
' {contentType: "image"|"video", contentUri, streamFormat, durationMs} or
' {contentType: "composed", layers: [{contentUri, contentType, streamFormat}],
' durationMs}), leaseId, error, needsRepair }.

function wvDoProgram(state as Object) as Object
    r = { ok: false, contentType: "", items: invalid, leaseId: "", error: "", needsRepair: false }

    pinFile = wvRehydrateTrustFile(state.trustPem)
    if pinFile = ""
        r.error = "could not rehydrate pinned trust anchor"
        return r
    end if

    base = "https://" + state.relayHost + ":" + state.relayPort

    ' --- pinned program poll (PLY-090): peer verification on against the pinned
    '     relay cert, host verification off (cert reached by IP, no SAN). ---
    ' content_types (PLY-012) declares exactly what this player can actually
    ' render: "image" and "video" are PLY-014's content-type floor — every
    ' conformant player MUST declare both, regardless of whether this
    ' deployment's Go stack currently has a producer that emits `video`
    ' content (it does not yet; PLY-014 declares a player's capability, not a
    ' claim about any particular deployment's authoring surface). "composed"
    ' is deliberately NOT declared: it is PLY-014's optional ("MAY declare
    ' additionally") member, and unlike video there is today no way for ANY
    ' Go code in this repo to even construct one — wire.LeaseContent (the
    ' shape every Lease content item, schedule-projected or app-authored,
    ' ultimately marshals through) carries no `layers` field at all, so a
    ' composed content item cannot reach the wire regardless of what a player
    ' declares. Declaring "composed" here would be a pure over-claim with no
    ' compensating implementation gap to close later — PLY-013 would let a
    ' relay assign one only once a producer for it actually exists, which
    ' is the point in time to re-add this member, not before. Keep this
    ' declaration in lockstep with what a relay could ever actually assign —
    ' PLY-013 filters program assignment against it.
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

    ' --- ordered cast: fetch + verify EVERY content item, IN ARRAY ORDER
    ' (PLY-083a) --- PLY-083a governs "every content item a Lease carries"
    ' with no type-based exception, so this loop makes exactly one pass over
    ' `content` and produces exactly one cast entry per item, in order —
    ' never a special first-item branch that can silently discard the rest
    ' of the array. A plain `image`/`video` item pays the same
    ' fetch-whole-file-then-verify pipeline as before (see the integrity note
    ' at the top of this file for why video pays a full pre-fetch instead of
    ' streaming); a `composed` item (PLY-015) fetches+verifies every one of
    ' its own layers with the IDENTICAL per-item integrity guarantee and
    ' becomes a cast entry of its own, cycled by PhotonScene exactly like any
    ' plain entry (see PhotonScene.brs's renderCastItem). Any item whose type
    ' is none of the three is skipped — forward-compatible with a future
    ' content-type vocabulary this player has not adopted (mirroring
    ' PLY-016's server-side rule, applied defensively here too) — but a
    ' malformed `composed` item (no layers, or a layer outside image/video)
    ' fails the WHOLE Lease rather than silently dropping just that one
    ' entry: a half-presented cast is exactly the silent content loss
    ' PLY-083a exists to prevent.
    castOut = []
    for i = 0 to content.Count() - 1
        item = content[i]
        itemType = wvStr(item.type)

        if itemType = "composed"
            layers = item.layers
            if layers = invalid or layers.Count() = 0
                r.error = "cast item " + i.toStr() + " (composed) carried no layers"
                return r
            end if

            outLayers = []
            for j = 0 to layers.Count() - 1
                layer = layers[j]
                layerType = wvStr(layer.type)
                if layerType <> "image" and layerType <> "video"
                    ' Defensive only: PLY-015 requires a relay to reject any
                    ' non-image/video (or nested composed) layer at compile
                    ' time and never deliver it. If one reaches this player
                    ' anyway (a non-conformant relay, or a future contract
                    ' revision this player has not adopted), the whole
                    ' composed item — and thus the whole Lease — is rejected
                    ' rather than silently dropping a layer or guessing how
                    ' to render an unsupported type.
                    r.error = "cast item " + i.toStr() + " composed layer " + j.toStr() + " has unsupported type '" + layerType + "' (PLY-015 restricts layers to image/video)"
                    return r
                end if

                localPath = wvLocalPathForCastItem(layerType, i.toStr() + "_" + j.toStr())
                fv = wvFetchAndVerifyItem(layer, localPath)
                if not fv.ok
                    r.error = "cast item " + i.toStr() + " composed layer " + j.toStr() + ": " + fv.error
                    return r
                end if

                lr = { contentUri: localPath, contentType: layerType, streamFormat: "" }
                if layerType = "video" then lr.streamFormat = wvVideoStreamFormat()
                outLayers.Push(lr)
            end for

            ' A composed item carries no `duration_ms` of its own in this
            ' contract's wire shape (PLY-083's `{type: "composed", layers}}`
            ' — no duration field) and PLY-083a's "own natural end" is
            ' undefined for composed (per-layer timing is reserved to a
            ' future contract, Scope). Absent any signal, this player uses
            ' the same own-default dwell time an image item with no
            ' duration_ms falls back to (PhotonScene's
            ' wvDefaultImageDurationMs) as its advance signal — a documented
            ' implementation choice, not a contract requirement.
            castOut.Push({ contentType: "composed", layers: outLayers, durationMs: 0 })

        else if itemType = "image" or itemType = "video"
            localPath = wvLocalPathForCastItem(itemType, i.toStr())
            fv = wvFetchAndVerifyItem(item, localPath)
            if not fv.ok
                r.error = "cast item " + i.toStr() + ": " + fv.error
                return r
            end if

            ci = { contentType: itemType, contentUri: localPath, streamFormat: "", durationMs: wvItemDurationMs(item) }
            if itemType = "video" then ci.streamFormat = wvVideoStreamFormat()
            castOut.Push(ci)
        end if
    end for

    if castOut.Count() = 0
        r.error = "lease carried no image/video/composed content item (content-type gate or empty program)"
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
' for a zero value (this contract fixes none, PLY-083b). A non-zero value is
' clamped to wvMinCastDurationMs(): duration_ms rides the wire as a raw,
' unvalidated integer (the relay clamps its own emission floor too — see
' playerserver.leaseContentMinDurationMS — but this is the last line of
' defense against a non-conformant or buggy relay), and this value is fed
' straight into a SceneGraph Timer (PhotonScene.brs renderCastItem) whose
' `duration` field re-arms at whatever rate a near-zero value implies — a
' render-thread freeze hazard on real hardware. A negative value (also
' nonsensical — Timer.duration has no meaningful negative) clamps to the
' same floor rather than reaching a SceneGraph node as undefined behavior.
function wvItemDurationMs(item as Object) as Integer
    d = item.duration_ms
    if d = invalid then return 0
    n = Int(d)
    if n <= 0 then return 0
    if n < wvMinCastDurationMs() then return wvMinCastDurationMs()
    return n
end function

' wvMinCastDurationMs is the floor this player enforces on any non-zero,
' non-default cast-item duration_ms before it is ever handed to a SceneGraph
' Timer — see wvItemDurationMs's own doc for the render-thread-freeze hazard
' an unclamped near-zero value produces.
function wvMinCastDurationMs() as Integer
    return 500
end function

' wvLocalPathForCastItem returns a stable, per-tag local cache path so a
' cast's items (or a composed item's layers, which call this same helper
' with a compound "<itemIndex>_<layerIndex>" tag) never collide with each
' other on disk.
function wvLocalPathForCastItem(itemType as String, tag as String) as String
    ext = ".img"
    if itemType = "video" then ext = ".mp4"
    return "cachefs:/waiveo_player_v3_item" + tag + ext
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
