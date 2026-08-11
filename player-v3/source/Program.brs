' Program.brs — the on-device player/1 program thread (PLY-080/084/090/091).
' Pulls a signed Lease over the pinned relay connection, materializes EVERY
' content item the Lease's `content` array carries (PLY-014's content-type
' floor), in order, DIRECT from its feeder content-origin URL — fetching only
' what this device does not already hold (the content cache below) and
' verifying each newly fetched item's (and each composed/slide layer's) bytes
' against its own asset_ref (content-addressed integrity) BEFORE returning
' anything to render — and returns a single ordered `items` array (PLY-083a) for
' PhotonScene to present — one entry per PRESENTABLE `content` array element,
' IN THAT ARRAY'S OWN ORDER, with NO type-based carve-out: a plain
' `image`/`video` item becomes a plain cast entry, and a `composed` item
' (PLY-015) becomes a cast entry of its own carrying its fetched+verified
' `layers`, cycled exactly like any other entry. PLY-083a governs sequencing
' for "every content item a Lease carries" (PLY-083) — it draws no distinction
' between a composed item and a plain one, so this player draws none either: a
' Lease mixing composed and plain items presents ALL of them, in order, never
' dropping whichever ones don't match a special-cased first-item type. A
' one-item Lease (of either kind) is simply the degenerate one-entry case of
' this same path.
'
' "Presentable" is doing real work in that sentence and the Degrading note
' below is where it is defined: an item this device genuinely cannot render
' (its bytes will not fetch, its type is unknown) is dropped from the cast with
' a console line naming it, rather than taken as grounds to discard the items
' around it. A `display: "blank"` Lease (PLY-093) is a different thing again —
' not an empty cast but an instruction — and is returned as contentType
' "blank"; see wvDoProgram's own display block.
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
' wvEnsureContent below) — composing layers never relaxes integrity.
'
' Content cache (parity milestone 2.6): this poll loop runs every ~10s
' (PlayerTask's wvProgramPollIntervalMs) and USED to re-download and re-hash
' every content item on every single pass, whether or not anything had changed
' — the scene's castSignature gated only the RE-RENDER, never the fetch. For a
' wall of screens on one LAN that is a permanent, pointless load on the box; for
' a video item it is the difference between an idle link and a saturated one,
' plus a full-file sha256 every ten seconds on a Pi-class CPU.
'
' So a fetched asset is remembered, keyed on its own content-addressed
' asset_ref, and a later poll that names the same asset_ref reuses the local
' file with NO request and NO re-hash. Keying on the content address rather than
' on a URL, a lease id or an ETag is what makes that sound rather than merely
' fast: the key IS the sha256 of the bytes, so "the same key" and "the same
' bytes" are the same statement — there is no version of an asset_ref whose
' content differs from what was verified when it was first stored, and therefore
' no staleness question to get wrong. (The origin serves a strong ETag and an
' `immutable` lifetime for the same reason, internal/feeder/origin — this cache
' is simply the client that needs no round trip at all to use them.)
'
' The cache is BOUNDED and cannot fill the device: see wvTrimContentCache.
'
' Composed note (PLY-015/PLY-083): this contract restricts a composed item's
' `layers` to `image`/`video` members and reserves per-layer geometry/timing
' to a future contract (Scope, out of scope) — it defines no positioning
' schema at all. Absent one, this player renders every layer full-screen,
' stacked in `layers` array order (index 0 furthest back, later indices
' progressively on top) — the only ordering the wire shape itself gives a
' player to honor. A layer whose `type` is anything other than image/video
' MUST NOT reach a player at all (a relay rejects it at compile time,
' PLY-015); if one arrives anyway this player drops THAT LAYER rather than
' guess how to render an unsupported type — see the degrading rule below for
' why it does not take the item, or the Lease, down with it.
'
' Degrading (PLY-087, and the hardware defect it was written for): a Lease is
' resolved item by item and, within a slide or composed item, layer by layer.
' A failure NEVER escalates past the smallest thing it actually broke:
'
'   - a slide layer whose bytes will not fetch is DEGRADED — the layer is left
'     with no contentUri and PhotonScene draws a visible "unavailable"
'     placeholder in its geometry (createDegradedLayer), exactly as a weather
'     or entity layer whose source could not answer is drawn as slidelive's
'     Unavailable "—". The rest of the slide — its title, its clock, its other
'     images — draws normally;
'   - an item that resolves to nothing at all (an unfetchable plain
'     image/video, a slide/composed carrying no layers, a type this player
'     does not know) is DROPPED, loudly, and every other item in the same
'     Lease still presents;
'   - only when the whole Lease resolves to NO presentable item does this poll
'     fail, which is the one case never-wipe is actually for.
'
' This reverses an earlier rule that read PLY-083a's "present every item" as
' "present every item or none", and it was wrong in the most expensive
' direction. Observed on The Hanger 2026-08-11: one layer of one slide 403'd,
' the whole program was rejected, a second slide with NO assets at all was
' discarded with it, and the screen sat on an hour-old test slide for a whole
' session — never-wipe faithfully preserving the wrong thing. PLY-087 is
' explicit that a player which cannot reach a content origin "MUST continue
' rendering whatever content it already holds locally that remains valid under
' that Lease, rather than treat the fetch failure as a reason to stop
' rendering entirely", and wire.Layer.Value already argues the identical case
' one level down: requiring a live widget's value "would mean a forecast
' service being unreachable BLANKS THE WHOLE SLIDE — losing the title, the
' image and the clock to fix nothing."
'
' The half that keeps this from becoming silent content loss is the LOG: every
' degrade and every drop prints, with the item/layer index and the underlying
' error, so a shortened rotation is a diagnosable event rather than a screen
' that quietly shows less than it was assigned.
'
' Scope note (first photon): the Lease's ed25519 signature (PLY-090) is NOT
' verified on-device — Roku exposes no ed25519 primitive, and first-photon's
' trust basis is the pinned TLS channel to the relay plus the asset_ref
' content-address check below. A later increment adds signature verification
' (or a Roku-supported signature scheme).
'
' wvDoProgram(state) where state = { channelToken, relayHost, relayPort, trustPem }
' returns { ok, contentType ("cast" or "blank"), items (ordered array of cast
' entries — {contentType: "image"|"video", contentUri, streamFormat,
' durationMs}, {contentType: "composed", layers: [{contentUri, contentType,
' streamFormat}], durationMs}, or {contentType: "slide", layers, durationMs,
' slideId}; EMPTY for contentType "blank"), leaseId, error, needsRepair }.
'
' contentType "blank" is a SUCCESS (ok = true), not a failure: see the
' display-handling block in wvDoProgram for why the difference is the whole
' point of this return shape.

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
    ' conformant player MUST declare both. Both are now genuinely produced by
    ' this deployment's Go stack as well as declared here: a playlist item
    ' states its own `content_type` (datamodel.PlaylistItem.ContentType) and
    ' both content projections carry it onto the Lease item's `type`, so a
    ' `video` declaration is what makes an operator's uploaded clip actually
    ' reach this player instead of being filtered out. "composed"
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
    '
    ' "slide" IS declared: this player carries a native slide renderer
    ' (PhotonScene.renderSlide draws each text/rect/image/clock layer as a
    ' positioned SceneGraph node, with the clock Label live-refreshed once a
    ' second) and the Go stack CAN construct a slide item (wire.LeaseContent
    ' carries `layers`, populated for a `type:"slide"` item). Unlike "composed"
    ' this is a real capability backed by both a producer and a renderer, so it
    ' is a true declaration, not an over-claim. PLY-013 lets the relay serve a
    ' slide only to a player that declares it, so an older player is
    ' transparently never served one. This array MUST stay byte-identical to
    ' Pairing.brs's declaration (PLY-012) — a mismatch between the pair-time and
    ' poll-time capability sets is a real bug.
    reqBody = FormatJson({ capabilities: { content_types: ["image", "video", "slide"], player_version: "3.0.0" } })
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
        ' Auth error taxonomy (PLY-072/073/136; relay-side in
        ' internal/relay/playerserver/program.go's handleProgram): a 401 from
        ' THIS relay carries exactly one of three codes today —
        ' CHANNEL_TOKEN_INVALID (absent, malformed, or unresolvable token),
        ' CHANNEL_TOKEN_REVOKED (token's screen_id is revoked, or no longer
        ' exists at all), or CHANNEL_TOKEN_EXPIRED (token past its own
        ' expires_at). PLY-073 requires these be handled differently:
        '
        ' - CHANNEL_TOKEN_EXPIRED is retryable via renewal (PLY-074), never a
        '   trigger to re-enter Pairing redemption. Neither PLY-074's
        '   dedicated renewal call nor its inline-refresh variant exists yet
        '   on this relay (see handleProgram's own PLY-074 note), so the
        '   least-destructive thing this player can do today is leave the
        '   credential untouched and let the caller's ordinary poll cadence
        '   retry — needsRepair stays false, so PlayerTask.brs treats this
        '   exactly like any other transient poll failure.
        '
        ' - CHANNEL_TOKEN_REVOKED and CHANNEL_TOKEN_INVALID are BOTH terminal
        '   for this token (PLY-073/PLY-136): a revoked/gone-screen token and
        '   a token that never validated at all are the same "this token no
        '   longer validates" signal PLY-136 names, so both clear ONLY the
        '   channel token (never the trust anchor, PLY-136) and re-enter
        '   Pairing redemption — signaled here as needsRepair.
        '
        ' - Any OTHER 401 code — one this relay does not emit today, or a
        '   future one this player doesn't yet recognize — takes the SAME
        '   path as CHANNEL_TOKEN_EXPIRED, not the REVOKED/INVALID path: an
        '   unrecognized code is not evidence the credential is actually
        '   invalid, so treating it as terminal would risk destroying a
        '   still-good session over a response this player doesn't
        '   understand. Retrying is the least destructive action that still
        '   makes progress if the condition clears, and it never wipes a
        '   credential it doesn't have solid grounds to.
        if resp.code = 401
            code401 = wv401Code(resp)
            if code401 = "CHANNEL_TOKEN_REVOKED" or code401 = "CHANNEL_TOKEN_INVALID"
                r.needsRepair = true
            end if
        end if
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

    ' --- display (PLY-093): an INSTRUCTION, not a content count ---
    '
    ' `display` is exactly one of "content" or "blank". A "blank" Lease is a
    ' SUCCESSFUL pull that says show nothing — PLY-093's on-but-showing-nothing
    ' state a compiled program can assign, which is how a daypart's
    ' display_power reaches a screen (data-model/1 DAT-114/115), how a screen
    ' scheduled dark overnight goes dark, and how an expiring alert override
    ' returns a wall to nothing (DAT-004d). PLY-093 is explicit that a blank
    ' Lease's own `content` array MAY be empty, so an empty array under
    ' display:blank is the NORMAL shape, not a malformed one.
    '
    ' Conflating that with a failed pull is the defect this block exists to
    ' close. Never-wipe is a fallback for UNREACHABILITY — "I could not get a
    ' program, so keep showing what I already have" — and applying it to a
    ' valid, signed instruction the player merely dislikes is not conservatism,
    ' it is ignoring the instruction. Observed on The Hanger 2026-08-11: a
    ' 120s-TTL "EVACUATE - THIS IS A DRILL" override lapsed correctly
    ' server-side, the relay served a valid Lease carrying
    ' `"display":"blank"`/`"content":[]`, and this player logged it as
    ' `program poll failed (keeping current content, never-wipe): lease carried
    ' an empty or missing content array` — screenshots before and after the
    ' lapse were byte-identical and the evacuation notice never left the wall.
    '
    ' internal/virtualplayer (preempt.go's AdoptionOf) has always honored
    ' `display`, and the conformance corpus drives THAT double — which is
    ' precisely why every gate was green while the device ignored the contract.
    '
    ' The comparison is deliberately positive ("blank"), never a negation of
    ' "content": an absent, empty or unrecognized `display` from an older or
    ' non-conformant relay must keep the CONTENT path, because blanking a wall
    ' on a value this player does not understand is the mirror defect, and it
    ' is worse than the bug — never-wipe at least preserves what an operator
    ' last chose.
    display = wvStr(lease.display)
    blank = (display = "blank")

    content = lease.content
    if not blank
        if content = invalid or content.Count() = 0
            r.error = "lease carried an empty or missing content array"
            return r
        end if
    end if

    ' --- ordered cast: materialize + verify EVERY content item, IN ARRAY ORDER
    ' (PLY-083a) --- PLY-083a governs "every content item a Lease carries"
    ' with no type-based exception, so this loop makes exactly one pass over
    ' `content` and produces at most one cast entry per item, in order —
    ' never a special first-item branch that can silently discard the rest
    ' of the array. A plain `image`/`video` item goes through the same
    ' cache-or-fetch-then-verify pipeline every other content-bearing thing does
    ' (wvEnsureContent; see the integrity note at the top of this file for why
    ' video pays a full pre-fetch instead of streaming); a `composed` item
    ' (PLY-015) resolves its own layers with the IDENTICAL per-layer integrity
    ' guarantee and becomes a cast entry of its own, cycled by PhotonScene
    ' exactly like any plain entry (see PhotonScene.brs's renderCastItem).
    '
    ' Failure is contained at the SMALLEST scope it broke — see the degrading
    ' note at the top of this file, and PLY-087, for why. An item this player
    ' cannot present at all is dropped (loudly) and the rest of the Lease still
    ' presents; a slide layer whose bytes will not fetch degrades to a visible
    ' placeholder and the rest of its slide still draws; an item whose type is
    ' none of the three is dropped the same way, which keeps this
    ' forward-compatible with a content-type vocabulary this player has not
    ' adopted (mirroring PLY-016's server-side rule, applied defensively here).
    ' NOTHING in this loop returns early: the only Lease-level failure is the
    ' one decided after it, when the Lease resolved to no presentable item at
    ' all.
    '
    ' `keep` accumulates the cache key of every asset this Lease references, so
    ' the trim at the bottom knows what is in use. It is built HERE, from the
    ' same pass that resolves them, rather than by re-walking the Lease
    ' afterwards: two walks that could disagree about what is referenced is how
    ' a cache evicts something a screen is playing.
    '
    ' A display:blank Lease skips the loop entirely: PLY-093 says its `content`
    ' array shows none of itself, so resolving it would fetch bytes nothing can
    ' put on screen.
    castOut = []
    keep = {}
    fetched = 0
    reused = 0
    degraded = 0
    dropped = 0
    if not blank
        for i = 0 to content.Count() - 1
            item = content[i]
            itemType = wvStr(item.type)
            ' `entry` is this item's cast entry, or invalid if it could not be
            ' built; `why` says why not. One drop decision per item, taken at
            ' the bottom of the loop, so there is exactly one place an item can
            ' leave the cast and exactly one place that gets logged.
            entry = invalid
            why = ""

            if itemType = "composed"
                layers = item.layers
                if layers = invalid or layers.Count() = 0
                    why = "(composed) carried no layers"
                else
                    outLayers = []
                    for j = 0 to layers.Count() - 1
                        layer = layers[j]
                        layerType = wvStr(layer.type)
                        if layerType <> "image" and layerType <> "video"
                            ' Defensive only: PLY-015 requires a relay to reject
                            ' any non-image/video (or nested composed) layer at
                            ' compile time and never deliver it. If one reaches
                            ' this player anyway (a non-conformant relay, or a
                            ' future contract revision this player has not
                            ' adopted), the LAYER is dropped — this player will
                            ' not guess how to render an unsupported type — and
                            ' the rest of the stack still composes.
                            degraded = degraded + 1
                            print "[player-v3] cast item " + i.toStr() + " composed layer " + j.toStr() + " DROPPED: unsupported type '" + layerType + "' (PLY-015 restricts layers to image/video)"
                        else
                            fv = wvEnsureContent(layer, layerType)
                            if fv.ok
                                keep[layerType + ":" + wvAssetRefKey(wvStr(layer.asset_ref))] = true
                                if fv.cached then reused = reused + 1 else fetched = fetched + 1
                                lr = { contentUri: fv.path, contentType: layerType, streamFormat: "" }
                                if layerType = "video" then lr.streamFormat = wvVideoStreamFormat()
                                outLayers.Push(lr)
                            else
                                degraded = degraded + 1
                                print "[player-v3] cast item " + i.toStr() + " composed layer " + j.toStr() + " (" + layerType + ") DROPPED: " + fv.error
                            end if
                        end if
                    end for

                    if outLayers.Count() = 0
                        why = "(composed) resolved none of its layers"
                    else
                        ' A composed item carries no `duration_ms` of its own in
                        ' this contract's wire shape (PLY-083's `{type:
                        ' "composed", layers}` — no duration field) and
                        ' PLY-083a's "own natural end" is undefined for composed
                        ' (per-layer timing is reserved to a future contract,
                        ' Scope). Absent any signal, this player uses the same
                        ' own-default dwell time an image item with no
                        ' duration_ms falls back to (PhotonScene's
                        ' wvDefaultImageDurationMs) as its advance signal — a
                        ' documented implementation choice, not a contract
                        ' requirement.
                        entry = { contentType: "composed", layers: outLayers, durationMs: 0 }
                    end if
                end if

            else if itemType = "slide"
                ' A "slide" content item (PLY-012's "slide" type; native slide
                ' rendering, parity milestone 2) carries an ordered `layers`
                ' array of positioned native elements in a fixed 1920x1080
                ' canvas. Exactly two of its kinds reference external bytes —
                ' `image` and `video` (wvLayerFetchesContent, mirroring
                ' wire.LayerFetchesContent, which is the producer side of the
                ' same pair) — and both pay the IDENTICAL
                ' asset_ref-verified-before-presented guarantee
                ' (wvEnsureContent) a plain image/video item or a composed layer
                ' pays: the verified local path is attached to the layer as
                ' `contentUri` before the layer is handed to PhotonScene. Every
                ' other kind is pure declarative draw data (no external bytes)
                ' and is passed through untouched. The relay has already
                ' validated the whole layer stack (wire.ValidateSlideLayers —
                ' closed kind set, geometry within canvas, per-kind required
                ' fields) and drops a slide it cannot validate, so this player
                ' does not re-validate kinds here. Layers ride through in array
                ' order (z-order = index).
                layers = item.layers
                if layers = invalid or layers.Count() = 0
                    why = "(slide) carried no layers"
                else
                    for j = 0 to layers.Count() - 1
                        layer = layers[j]
                        layerKind = wvStr(layer.kind)
                        if wvLayerFetchesContent(layerKind)
                            fv = wvEnsureContent(layer, layerKind)
                            if fv.ok
                                keep[layerKind + ":" + wvAssetRefKey(wvStr(layer.asset_ref))] = true
                                if fv.cached then reused = reused + 1 else fetched = fetched + 1
                                layer.contentUri = fv.path
                                ' A video layer additionally carries the
                                ' ContentNode streamFormat its Video node needs,
                                ' decided HERE for the same reason a composed
                                ' video layer's is (this file owns the fetch
                                ' strategy, and the format follows from it — see
                                ' wvVideoStreamFormat). PhotonScene then never
                                ' has to know anything about how the bytes
                                ' arrived.
                                if layerKind = "video" then layer.streamFormat = wvVideoStreamFormat()
                            else
                                ' DEGRADE this layer, keep the slide. An empty
                                ' contentUri is the single signal PhotonScene's
                                ' renderSlide draws its "unavailable"
                                ' placeholder from (createDegradedLayer), so
                                ' this is set explicitly rather than left absent
                                ' — the same fact, said out loud, and it also
                                ' covers the case wvLayerFetchesContent's own
                                ' doc warns about (a content layer that reached
                                ' the scene with no uri and drew a silent hole).
                                degraded = degraded + 1
                                layer.contentUri = ""
                                print "[player-v3] cast item " + i.toStr() + " slide layer " + j.toStr() + " (" + layerKind + ") DEGRADED — the slide still draws, with an unavailable placeholder in this layer's place: " + fv.error
                            end if
                        end if
                    end for

                    ' A slide has no natural end (a clock ticks forever), so it
                    ' advances on its own dwell time exactly like an image item:
                    ' its own duration_ms when it carries one (PLY-083b), else
                    ' this player's default dwell. A single-slide Lease simply
                    ' re-renders itself each cycle (the clock timer is torn down
                    ' and restarted cleanly). slideId is the item's CAST-LOCAL
                    ' slide id (LeaseContent.slide_id), carried through
                    ' untouched. It is what a `nav` layer's items resolve their
                    ' jump target against, and what a press reports so the
                    ' platform can attribute an interaction to the slide that
                    ' solicited it. An item that carries none (an inline slide
                    ' item, a generated alert slide) yields "" and simply never
                    ' matches a nav target.
                    entry = { contentType: "slide", layers: layers, durationMs: wvItemDurationMs(item), slideId: wvStr(item.slide_id) }
                end if

            else if itemType = "image" or itemType = "video"
                fv = wvEnsureContent(item, itemType)
                if fv.ok
                    keep[itemType + ":" + wvAssetRefKey(wvStr(item.asset_ref))] = true
                    if fv.cached then reused = reused + 1 else fetched = fetched + 1
                    ci = { contentType: itemType, contentUri: fv.path, streamFormat: "", durationMs: wvItemDurationMs(item) }
                    if itemType = "video" then ci.streamFormat = wvVideoStreamFormat()
                    entry = ci
                else
                    ' A plain item IS its asset: there is no partial version of
                    ' it to draw, so it is dropped rather than degraded. The
                    ' items around it are untouched.
                    why = fv.error
                end if

            else
                why = "unsupported content type '" + itemType + "'"
            end if

            if entry <> invalid
                castOut.Push(entry)
            else
                dropped = dropped + 1
                print "[player-v3] cast item " + i.toStr() + " DROPPED — every other item in this Lease still presents: " + why
            end if
        end for
    end if

    if blank
        ' PLY-093: a successful pull whose instruction is "show nothing".
        r.contentType = "blank"
    else
        if castOut.Count() = 0
            ' NOT a display instruction — this Lease asked for content and this
            ' player could not present ONE item of it. That is unreachability
            ' (or an empty program), which is exactly the case never-wipe is
            ' for: report the failure and let PlayerTask keep whatever is on
            ' screen. Reaching this branch when even one item resolved would be
            ' the whole defect back again.
            r.error = "lease carried " + content.Count().toStr() + " content item(s) and this player could not present any of them (see the per-item DROPPED lines above)"
            return r
        end if
        r.contentType = "cast"
    end if

    ' The Lease resolved, so the cache now knows exactly what is in use and
    ' can drop what is not. Trimming HERE — after the last item, before anything
    ' is published — is deliberate: trimming per item could evict an asset a
    ' later item in the SAME Lease is about to reuse, and trimming after
    ' publishing would race the scene's own teardown of the outgoing program.
    ' On a failed Lease this is never reached, so a poll that errors out leaves
    ' the cache exactly as it was and the screen keeps whatever it is showing
    ' (the same never-wipe discipline PlayerTask applies to the program itself).
    ' A blank Lease DOES reach it, with an empty keep-set: nothing is on screen,
    ' so nothing but the one generation of grace protects anything.
    wvTrimContentCache(keep)
    ' `degraded` counts LAYER-level losses of both kinds — a slide layer
    ' replaced by a placeholder and a composed layer removed from its stack —
    ' because from an operator's seat they are one fact: this program is not
    ' being shown in full. The per-layer lines above say which, and why.
    print "[player-v3] content: " + fetched.toStr() + " fetched, " + reused.toStr() + " reused from cache, " + degraded.toStr() + " layer(s) degraded or dropped, " + dropped.toStr() + " item(s) dropped"

    ' --- best-effort lease ack (PLY-091), over the pinned connection ---
    ' Reached on both adopted outcomes — a cast (possibly a degraded one) and a
    ' blank. PLY-104 is explicit that a blank Lease is still accepted and
    ' persisted, so an unacknowledged blank would be a screen that went dark
    ' without ever telling the platform it had.
    wvAckLease(base, state.channelToken, pinFile, r.leaseId)

    r.items = castOut
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

' wvLocalPathForContent returns the local file an asset's bytes live at. The
' path is derived from the asset's own CONTENT ADDRESS, not from its position in
' the Lease, and that is the whole design of the cache: the same bytes always
' land at the same path, so "have I already got this?" is answerable by key
' alone and a playlist reorder does not invalidate a single byte on disk. (The
' previous scheme named files by cast index — "item0", "item1_2" — under which
' swapping two items made both files wrong, so both had to be refetched to be
' safe. Nothing could cache under that naming.)
'
' hexKey is the asset_ref's hex digest (wvAssetRefKey). itemType only picks the
' extension, so an image and a video that somehow shared a digest would still
' land in separate files.
'
' An asset with no usable content address ("" hexKey — an absent or malformed
' asset_ref) gets ONE shared scratch path and is never cached: it cannot pass
' the integrity check that follows, so it is about to be rejected anyway, and
' giving it a stable name would be caching a failure.
function wvLocalPathForContent(itemType as String, hexKey as String) as String
    ext = ".img"
    if itemType = "video" then ext = ".mp4"
    if hexKey = "" then return "cachefs:/" + wvContentCachePrefix() + "unverifiable" + ext
    return "cachefs:/" + wvContentCachePrefix() + itemType + "_" + hexKey + ext
end function

' wvContentCachePrefix is the filename prefix EVERY file this player writes to
' cachefs carries. Sweeping is done by prefix (wvSweepContentCacheDir), so this
' one string is what separates "files this player owns and may delete" from
' anything else sharing that filesystem.
function wvContentCachePrefix() as String
    return "wvc_"
end function

' wvLegacyContentCachePrefix is the prefix an OLDER build of this player wrote
' its per-index item files under. Nothing writes it any more, but a device
' upgraded in place still has those files, and no code would ever have removed
' them — they would sit on cachefs forever. The startup sweep deletes them too,
' which is the only chance this player gets to reclaim that space.
function wvLegacyContentCachePrefix() as String
    return "waiveo_player_v3_item"
end function

' wvAssetRefKey reduces an `asset_ref` ("sha256:<hex>") to its hex digest — the
' cache key. Returns "" for anything that is not in that grammar, which is
' exactly the set of refs the integrity check will reject anyway, so an
' unusable ref can never become a cache key.
function wvAssetRefKey(assetRef as String) as String
    prefix = "sha256:"
    if assetRef = "" then return ""
    if assetRef.Instr(prefix) <> 0 then return ""
    return LCase(assetRef.Mid(prefix.Len()))
end function

' wvContentCache returns this thread's content cache, creating it on first use
' and SWEEPING the cache directory at that moment.
'
' The sweep is the cold-start half of the boundedness guarantee. In-run, the
' cache knows every file it wrote and trims them (wvTrimContentCache); across a
' restart it knows nothing, so anything already on disk under this player's
' prefix is unaccounted-for and is deleted. The cost is that a rebooted screen
' refetches its current program once, which is exactly what it did on EVERY poll
' before this existed; the benefit is that files can never accumulate across
' reboots, which is the only way an unattended fleet fills a device.
'
' Adopting those files instead (re-hashing each to confirm it still matches its
' filename) was considered and rejected: it would re-pay, at every boot, the
' whole-file hash this cache exists to stop paying, and trusting them WITHOUT
' re-hashing would present bytes this run has never verified — the one property
' the fetch path guarantees unconditionally.
'
' It lives on `m`, the Task thread's own scope, so it persists across polls for
' as long as the thread does and dies with it. A re-provisioned player
' (PhotonScene.maybeStart creating a fresh PlayerTask) therefore starts cold,
' which is correct: that path exists for a screen being pointed at a different
' relay entirely.
function wvContentCache() as Object
    if m.wvContentCacheState = invalid
        m.wvContentCacheState = { entries: {}, tick: 0, keepPrev: {} }
        wvSweepContentCacheDir()
    end if
    return m.wvContentCacheState
end function

' wvSweepContentCacheDir deletes every file on cachefs this player owns — its
' current prefix and the legacy one. Best-effort throughout: cachefs is a
' platform-managed filesystem the OS may itself clear, so a listing or a delete
' that fails is not a reason to fail a poll (the worst case is a file that stays
' until the next sweep, and the trim below still bounds what THIS run adds).
sub wvSweepContentCacheDir()
    fs = invalid
    try
        fs = CreateObject("roFileSystem")
    catch e
        return
    end try
    if fs = invalid then return

    names = invalid
    try
        names = fs.GetDirectoryListing("cachefs:/")
    catch e
        return
    end try
    if names = invalid then return

    removed = 0
    for each name in names
        n = wvStr(name)
        if n.Instr(wvContentCachePrefix()) = 0 or n.Instr(wvLegacyContentCachePrefix()) = 0
            try
                if fs.Delete("cachefs:/" + n) then removed = removed + 1
            catch e
                ' Best-effort: see this sub's own doc.
            end try
        end if
    end for
    if removed > 0
        print "[player-v3] content cache: swept " + removed.toStr() + " file(s) left by a previous run"
    end if
end sub

' wvEnsureContent makes a single Content reference's bytes available locally and
' returns the path they are at — fetching and verifying them only if this device
' does not already hold that exact asset.
'
' This is the ONE place content becomes a local file, shared by the plain
' image/video item path, every layer of a "composed" item (PLY-015), and every
' content-bearing layer of a "slide" item. Sharing it is what makes the
' asset_ref-verified-before-presented guarantee (PLY-084/PLY-014) and the cache
' the same mechanism for all of them, rather than three that could diverge.
'
' Returns { ok, path, error, cached }. `cached` is reported so the caller can say
' on the console which items cost a transfer and which did not — the evidence
' that the cache is actually working, which is otherwise invisible.
'
' The cache HIT path deliberately performs no re-hash. That is the entire saving,
' and it is sound for one reason: the key is the content address. These bytes
' were hashed and matched against this exact digest when they were stored, in
' this same process, and nothing but this player writes that path. Re-hashing
' would be re-asking a question whose answer cannot have changed, at the cost of
' a full file read every ten seconds — which is the defect being fixed.
function wvEnsureContent(item as Object, itemType as String) as Object
    r = { ok: false, path: "", error: "", cached: false }

    assetRef = wvStr(item.asset_ref)
    hexKey = wvAssetRefKey(assetRef)
    cache = wvContentCache()
    key = itemType + ":" + hexKey
    localPath = wvLocalPathForContent(itemType, hexKey)

    if hexKey <> ""
        entry = cache.entries[key]
        if entry <> invalid
            cache.tick = cache.tick + 1
            entry.used = cache.tick
            r.ok = true
            r.path = entry.path
            r.cached = true
            return r
        end if
    end if

    fetch = wvHttpGetToFile(wvStr(item.url), localPath, wvContentFetchTimeoutMs())
    if not fetch.ok
        r.error = "content fetch failed: HTTP " + fetch.code.toStr() + " " + fetch.failureReason
        return r
    end if

    verified = wvVerifyStoredBytes(localPath, assetRef)
    if not verified.ok
        r.error = "content integrity check FAILED (fetched bytes do not match asset_ref " + assetRef + ")"
        return r
    end if

    if hexKey <> ""
        cache.tick = cache.tick + 1
        cache.entries[key] = { path: localPath, sizeBytes: verified.sizeBytes, used: cache.tick }
    end if

    r.ok = true
    r.path = localPath
    return r
end function

' wvContentFetchTimeoutMs is the per-asset transfer timeout. It is generous
' because a video is a whole file on a shared LAN and this player has no
' progressive-start path (see the integrity note at the top of this file); a
' timeout tuned for a PNG would make video permanently unfetchable rather than
' merely slow, and a failed fetch costs the whole Lease.
function wvContentFetchTimeoutMs() as Integer
    return 120000
end function

' wvTrimContentCache bounds the cache after a Lease has been materialized.
'
' `keep` is the set of keys THIS Lease referenced. Those are never evicted for
' the obvious reason (a file the screen is about to play must exist), and
' neither is the set the PREVIOUS Lease referenced: the scene may still be
' rendering the outgoing program at the instant this runs — a Video node
' decoding from a file — so a program change grants one generation of grace
' rather than deleting under a node that is still reading. Everything older is
' unreferenced by anything on screen and is evicted, least-recently-used first,
' until the cache is within BOTH caps.
'
' That makes the true bound "whatever the current and immediately preceding
' programs actually reference, plus at most the caps' worth of recent history" —
' which is bounded by the fleet's own scheduled content and cannot grow without
' limit no matter how long a screen runs or how many distinct assets pass
' through it. The caps alone could not promise that (a single program larger
' than the byte cap must still play), and the keep-sets alone could not either
' (a screen cycling through hundreds of assets would keep every one).
'
' The two key-sets below are DIFFERENT sets and must stay different, which is
' the one thing this routine got wrong: `thisKeep` is what the NEXT trim
' inherits as its single generation of grace, and `protectedKeys` is what THIS
' trim may not evict (thisKeep plus the previous generation). Handing
' protectedKeys forward instead makes keepPrev the running UNION of every key
' ever kept — and since a key enters `entries` during the very poll that puts it
' in `keep`, every entry would be protected forever, `oldestKey` would always be
' "", and both caps would be inert for the life of the thread. That is a bound
' that reads as enforced, tests as present, and reclaims nothing.
sub wvTrimContentCache(keep as Object)
    cache = wvContentCache()

    ' `thisKeep` is a COPY rather than `keep` itself: it outlives this call by
    ' one generation, and a set the caller still owns could be mutated (or
    ' reused) after we have stored it.
    thisKeep = {}
    protectedKeys = {}
    for each k in keep
        thisKeep[k] = true
        protectedKeys[k] = true
    end for
    for each k in cache.keepPrev
        if cache.entries[k] <> invalid then protectedKeys[k] = true
    end for

    ' Count what the evictable set holds, then drop the least recently used
    ' until it fits. Recomputed from the entries themselves rather than carried
    ' as a running total, so a miscount can never accumulate.
    while true
        totalEntries = 0
        totalBytes = 0
        oldestKey = ""
        oldestUsed = 0
        for each k in cache.entries
            e = cache.entries[k]
            totalEntries = totalEntries + 1
            totalBytes = totalBytes + e.sizeBytes
            if protectedKeys[k] = invalid
                if oldestKey = "" or e.used < oldestUsed
                    oldestKey = k
                    oldestUsed = e.used
                end if
            end if
        end for

        if totalEntries <= wvContentCacheMaxEntries() and totalBytes <= wvContentCacheMaxBytes() then exit while
        if oldestKey = "" then exit while ' over budget, but everything left is in use.

        victim = cache.entries[oldestKey]
        wvDeleteCachedFile(victim.path)
        cache.entries.Delete(oldestKey)
        print "[player-v3] content cache: evicted " + victim.path + " (" + victim.sizeBytes.toStr() + " bytes)"
    end while

    ' ONE generation of grace, not a running union: see this routine's doc.
    cache.keepPrev = thisKeep
end sub

' wvContentCacheMaxEntries and wvContentCacheMaxBytes are the cache's two caps.
' Both are needed and neither implies the other: a hundred small posters blow
' the entry count while using almost no space, and two long videos blow the byte
' budget as two entries. The byte cap is sized for a signage box's worth of
' clips on the smallest supported hardware rather than for the largest — the
' penalty for being under-sized is a refetch, and the penalty for being
' over-sized is a device with no room left for the platform itself.
function wvContentCacheMaxEntries() as Integer
    return 16
end function

function wvContentCacheMaxBytes() as Integer
    return 96 * 1024 * 1024
end function

' wvDeleteCachedFile removes one cached file, best-effort — a delete that fails
' leaves a file the next startup sweep will collect, which is strictly better
' than failing a poll over housekeeping.
sub wvDeleteCachedFile(path as String)
    try
        fs = CreateObject("roFileSystem")
        fs.Delete(path)
    catch e
        ' Best-effort: see this sub's own doc.
    end try
end sub

' wvVerifyStoredBytes reports whether the bytes at path content-address to
' assetRef ("sha256:<hex>"), and how many bytes there are. This is first-photon's
' integrity guarantee for the direct content fetch (PLY-084), standing in for TLS
' trust on the content channel.
'
' It returns the size alongside the verdict because the caller needs both and the
' file is already fully in memory to be hashed: asking a separate helper for the
' size would read the whole file a second time, which on a video is the most
' expensive thing this player does.
function wvVerifyStoredBytes(path as String, assetRef as String) as Object
    r = { ok: false, sizeBytes: 0 }

    wantHex = wvAssetRefKey(assetRef)
    if wantHex = "" then return r

    ba = CreateObject("roByteArray")
    ok = false
    try
        ok = ba.ReadFile(path)
    catch e
        return r
    end try
    if not ok then return r
    if ba.Count() = 0 then return r

    gotHex = wvSha256Hex(ba)
    if not wvHexEqualConstant(gotHex, wantHex) then return r

    r.ok = true
    r.sizeBytes = ba.Count()
    return r
end function

' wvLayerFetchesContent reports whether a SLIDE layer of this kind names bytes
' in the content origin — i.e. whether it carries an asset_ref this player must
' fetch and content-address-verify before the layer can be drawn.
'
' It is the on-device mirror of wire.LayerFetchesContent (internal/shared/wire/
' lease.go), and it exists for the same reason: the pair of content-bearing
' kinds (image, video) is asked about in more than one place, and a consumer
' that knows about one but not the other fails SILENTLY. Here the failure is
' precise — a video layer whose bytes were never fetched reaches PhotonScene
' with no contentUri, so the slide draws with a blank hole where the video is,
' and nothing anywhere reports an error. Naming the pair once is what stops that
' being possible.
function wvLayerFetchesContent(kind as String) as Boolean
    return kind = "image" or kind = "video"
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
    ' Body-less rejection (the firmware 401 case wv401Code documents): the
    ' Problem-Code header (PLY-008) still names the code, so the console
    ' evidence trail reports the real taxonomy value instead of a bare status.
    if resp.DoesExist("headers")
        hdrCode = wvHeaderValue(resp.headers, "Problem-Code")
        if hdrCode <> ""
            return "program rejected: " + hdrCode + " (from Problem-Code header; response body was empty)"
        end if
    end if
    if resp.startFailed then return "could not reach relay: " + resp.failureReason
    return "program HTTP " + resp.code.toStr() + " " + resp.failureReason
end function

' wv401Code extracts the Error taxonomy `code` (e.g. "CHANNEL_TOKEN_EXPIRED")
' from a Problem-shaped 401 response, preferring the `Problem-Code` response
' header (player/1 PLY-008) and falling back to the Problem body's own `code`
' member (PLY-005). Returns "" when neither carrier yields one — the caller's
' classification then falls back to the same least-destructive handling as an
' unrecognized code.
'
' The header is tried FIRST because on observed Roku firmware it is the only
' carrier that survives: roUrlTransfer hands back an EMPTY body for ANY 401
' regardless of what the relay sent (verified on device for two request shapes,
' against a relay whose 177-byte application/problem+json body curl reads
' correctly), while GetResponseHeaders() delivers that same response's headers
' intact. Reading only the body collapsed CHANNEL_TOKEN_INVALID,
' CHANNEL_TOKEN_REVOKED and CHANNEL_TOKEN_EXPIRED into one branch here and made
' PLY-073's clear-token-and-re-pair requirement unreachable on real hardware:
' a genuinely revoked screen retried forever instead of re-pairing.
'
' The body fallback is retained, not vestigial — PLY-008 makes the two carriers
' byte-identical, so on any client or firmware where the body DOES arrive (and
' for any older relay predating PLY-008) this still classifies correctly. The
' header wins when both are present: they cannot legitimately disagree, and
' preferring one deterministically beats an ordering that varies by platform.
function wv401Code(resp as Object) as String
    fromHeader = ""
    if resp.DoesExist("headers")
        fromHeader = wvHeaderValue(resp.headers, "Problem-Code")
    end if
    if fromHeader <> "" then return fromHeader

    if resp.body = "" then return ""
    pb = ParseJson(resp.body)
    if pb = invalid then return ""
    if pb.code = invalid then return ""
    return wvStr(pb.code)
end function
