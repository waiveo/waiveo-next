' Pairing.brs — the on-device player/1 pairing thread (PLY-024/030-033/040/041/
' 052/054/056/057). One verification-disabled bootstrap POST is the ONLY window
' with TLS verification off; it is opened here for a single attempt and never
' reopened by any later call (Program.brs always pins). The fingerprint
' commitment is verified locally over the response's trust_anchors and never
' sent to the relay.
'
' wvDoPairing(pairingCode) returns:
'   { ok, channelToken, screenId, relayHost, relayPort, trustPem, error, phase }
' On ok=true the caller persists via wvStoragePersistPairing. On failure it
' returns ok=false with a human-readable error and never a partial credential.

function wvDoPairing(pairingCode as String) as Object
    r = { ok: false, channelToken: "", screenId: "", relayHost: "", relayPort: "", trustPem: "", error: "", phase: "decode" }

    dec = wvPairCodeDecode(pairingCode)
    if not dec.ok
        r.error = "invalid pairing code: " + dec.error
        return r
    end if
    r.relayHost = dec.host
    r.relayPort = dec.port.toStr()

    ' --- bootstrap POST /player/v1/pair, verification DISABLED (PLY-040/041) ---
    r.phase = "bootstrap"
    base = "https://" + dec.host + ":" + dec.port.toStr()
    ' content_types (PLY-012) MUST match Program.brs's identical declaration
    ' on every later program poll (PLY-013) — see that declaration's own
    ' comment for why "composed" is deliberately absent (no Go producer can
    ' construct one, so declaring it would be a pure over-claim) while
    ' "video" stays declared (PLY-014's content-type floor, unconditional).
    ' "slide" is declared here too (native slide rendering) — this MUST stay
    ' byte-identical to Program.brs's program-poll declaration (PLY-012): the
    ' pair-time and every-poll capability sets are the same set, and a mismatch
    ' is a real bug (PLY-013 filters program assignment against the poll set).
    reqBody = FormatJson({
        hardware_id: wvHardwareId(),
        grant_selector: dec.grantSelector,
        capabilities: { content_types: ["image", "video", "slide"], player_version: "3.0.0" }
    })

    resp = wvHttpJson({
        method: "POST",
        url: base + "/player/v1/pair",
        body: reqBody,
        peerVerify: false,
        hostVerify: false,
        timeoutMs: 8000
    })

    if not resp.ok
        r.error = wvPairErrorText(resp)
        return r
    end if

    parsed = ParseJson(resp.body)
    if parsed = invalid
        r.error = "pair response was not valid JSON"
        return r
    end if

    ' --- verify the OOB commitment over trust_anchors BEFORE trusting anything
    '     in the response (PLY-052/057). On mismatch: discard, do not persist. ---
    r.phase = "pin"
    anchors = parsed.trust_anchors
    if anchors = invalid
        r.error = "pair response missing trust_anchors"
        return r
    end if
    pems = CreateObject("roArray", 0, true)
    playerPem = ""
    for each a in anchors
        if a.pem <> invalid
            pems.Push(a.pem)
            if playerPem = "" then playerPem = a.pem
            if wvCoversPlayer(a) then playerPem = a.pem
        end if
    end for
    if pems.Count() = 0
        r.error = "pair response trust_anchors carried no pem"
        return r
    end if
    if not wvVerifyCommitment(pems, dec.commitmentHex)
        ' PLY-057: possible MITM — the relay reached is not the one the OOB
        ' pairing code was formed for. Refuse to proceed.
        r.error = "fingerprint commitment MISMATCH — refusing to pair (possible MITM)"
        return r
    end if

    ' --- only now consult the redemption result (PLY-038) ---
    r.phase = "redeem"
    status = parsed.pairing_status
    if status <> "redeemed"
        r.error = "pairing not redeemed (status=" + wvStr(status) + ")"
        return r
    end if
    token = parsed.channel_token
    if token = invalid or token = ""
        r.error = "redeemed response carried no channel_token"
        return r
    end if

    r.channelToken = token
    r.screenId = wvStr(parsed.screen_id)
    r.trustPem = playerPem
    r.ok = true
    r.phase = "done"
    return r
end function

' wvCoversPlayer reports whether a trust_anchors entry's covers list includes
' "player" (PLY-042) — the anchor the pinned program-poll connection uses.
function wvCoversPlayer(anchor as Object) as Boolean
    if anchor.covers = invalid then return false
    for each c in anchor.covers
        if c = "player" then return true
    end for
    return false
end function

' wvPairErrorText renders a readable error from a non-2xx pair response, using
' api/1's RFC-9457 problem+json {code,title} when present (PLY-005/036).
function wvPairErrorText(resp as Object) as String
    if resp.body <> ""
        pb = ParseJson(resp.body)
        if pb <> invalid
            if pb.code <> invalid
                return "pairing rejected: " + wvStr(pb.code) + " (" + wvStr(pb.title) + ")"
            end if
        end if
    end if
    if resp.startFailed then return "could not reach relay: " + resp.failureReason
    return "pairing HTTP " + resp.code.toStr() + " " + resp.failureReason
end function

' wvHardwareId returns a stable opaque per-device id for hardware_id (PLY-030).
function wvHardwareId() as String
    di = CreateObject("roDeviceInfo")
    id = di.GetChannelClientId()
    if id = invalid then return "waiveo-player-v3-unknown"
    return id
end function

function wvStr(v as Dynamic) as String
    if v = invalid then return ""
    if type(v) = "roString" or type(v) = "String" then return v
    return v.toStr()
end function
