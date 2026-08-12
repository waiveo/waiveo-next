' Storage.brs — persistent player state under the never-wipe discipline
' (PLY-026/063/130). Uses its own registry section (distinct from any other
' channel's), and rehydrates the pinned trust-anchor PEM to a cachefs file on
' demand (SetCertificatesFile needs a filesystem path, not a registry blob —
' Wave-0 spike finding).
'
' Never-wipe: the relay address is persisted on first successful pairing and
' NEVER cleared on a mere connection failure. A token-only credential problem
' (401 CHANNEL_TOKEN_REVOKED/CHANNEL_TOKEN_INVALID, PLY-073/136) clears ONLY
' the channel token (wvStorageClearChannelToken) — the trust anchor is a
' separate credential class and MUST NOT be destroyed just because a token
' turned out bad. An explicit trust-loss event (PLY-063) is the one case that
' clears both (wvStorageClearTrustAndToken), a deliberately separate and more
' destructive function with no caller in this increment. Either way the
' relay address stays so a re-pair can target the same relay.

function wvStorageSection() as Object
    return CreateObject("roRegistrySection", "waiveo_player_v3")
end function

function wvStorageGet(key as String) as String
    reg = wvStorageSection()
    if not reg.Exists(key) then return ""
    v = reg.Read(key)
    if v = invalid then return ""
    return v
end function

sub wvStorageSet(key as String, value as String)
    reg = wvStorageSection()
    reg.Write(key, value)
    reg.Flush()
end sub

' wvStorageLoad returns the persisted pairing state as an assoc array:
' { paired: Boolean, channelToken, screenId, relayHost, relayPort, trustPem }.
' `paired` means "holds everything a program poll needs", which is BOTH
' credentials — not the channel token alone. A pinned poll rehydrates the trust
' anchor on every attempt (Program.brs) and cannot proceed without it, so a
' screen holding a token and no anchor is not a paired screen that is failing;
' it is an unpaired screen wearing a paired screen's flag.
'
' Defining this on the token alone stranded exactly that screen. It reported
' paired, so PhotonScene started the poll loop and printed "connecting…", every
' poll failed at rehydrate, and the failure carried needsRepair:false — correct
' in itself, since no credential had been REJECTED — so the loop retried
' forever on backoff. The recovery already existed and was unreachable for the
' one reason that mattered: `need_code` and PhotonScene's "enter pairing code to
' begin" are both guarded on `not paired`, and the flag said paired. The screen
' asked for nothing and waited for something that could never arrive.
'
' Widening the predicate is the whole fix, and it costs nothing elsewhere:
' pairing already refuses a response whose trust_anchors carry no pem
' (Pairing.brs), so a redemption cannot produce this state, and clearing a dead
' token deliberately keeps the anchor (PLY-136), which is the other direction.
' What remains is a registry that came back with one of the two — a partial
' write, a wiped key — and for that the honest report is "not paired".
'
' Never-wipe is untouched: this reads, it does not delete. The token stays in
' the registry exactly as persisted, and if the anchor is ever restored the
' screen is paired again with no re-pairing needed.
function wvStorageLoad() as Object
    channelToken = wvStorageGet("channel_token")
    trustPem = wvStorageGet("trust_pem")
    out = {
        paired: (channelToken <> "" and trustPem <> ""),
        channelToken: channelToken,
        screenId: wvStorageGet("screen_id"),
        relayHost: wvStorageGet("relay_host"),
        relayPort: wvStorageGet("relay_port"),
        trustPem: trustPem
    }
    return out
end function

' wvStoragePersistPairing records a completed pairing (never-wipe: overwrites in
' place, only ever set on a verified redemption).
sub wvStoragePersistPairing(channelToken as String, screenId as String, relayHost as String, relayPort as String, trustPem as String)
    reg = wvStorageSection()
    reg.Write("channel_token", channelToken)
    reg.Write("screen_id", screenId)
    reg.Write("relay_host", relayHost)
    reg.Write("relay_port", relayPort)
    reg.Write("trust_pem", trustPem)
    reg.Flush()
end sub

' wvStorageClearChannelToken clears ONLY the channel token — the correct
' (and only) credential-clearing action for a 401
' CHANNEL_TOKEN_REVOKED/CHANNEL_TOKEN_INVALID response (PLY-073/PLY-136).
' The pinned trust anchor is a separate credential class with its own,
' narrower loss condition (PLY-063, wvStorageClearTrustAndToken below) and
' MUST NOT be destroyed here: a bad token says nothing about whether the
' trust anchor a player pinned its connection against is still good. The
' relay address is untouched either way (never-wipe) so a re-pair can
' target the same relay.
sub wvStorageClearChannelToken()
    reg = wvStorageSection()
    reg.Delete("channel_token")
    reg.Flush()
end sub

' wvStorageClearTrustAndToken clears BOTH the channel token and the pinned
' trust anchor. This is deliberately its OWN function, separate from the
' common 401-handling path above (wvStorageClearChannelToken) — it exists
' only for an explicit trust-loss event (PLY-063: storage corruption, a
' factory reset, or any other event that independently leaves the trust
' anchor itself unusable), where the trust material is already gone and
' this call just makes that loss explicit before Pairing redemption. No
' caller in this increment reaches PLY-063's trust-loss condition; the
' function is kept available, under its own explicit name, for whichever
' future caller detects it rather than folding a rare full wipe into the
' ordinary token-clearing path.
sub wvStorageClearTrustAndToken()
    reg = wvStorageSection()
    reg.Delete("channel_token")
    reg.Delete("trust_pem")
    reg.Flush()
end sub

' wvRehydrateTrustFile writes the persisted trust-anchor PEM to a cachefs file
' usable with SetCertificatesFile, and returns its path ("" on failure). The
' file is rewritten every boot because cachefs survives relaunch but the pin
' must be a filesystem path, not the registry blob (Wave-0 spike).
' The three failure returns are all "" because the caller only needs a path or
' nothing, but they are NOT the same condition and the log says which. Callers
' report a bare "could not rehydrate the pinned trust anchor", and read on a
' screen that is failing every poll that sentence names a symptom shared by a
' missing credential (operator must re-pair) and a full cache partition
' (operator must reclaim space) — two different jobs, one message.
function wvRehydrateTrustFile(trustPem as String) as String
    ' Defensive after the wvStorageLoad fix: a screen with no anchor is no
    ' longer `paired`, so the poll loop it used to strand is not reached. This
    ' still guards the InteractionTask path and any future caller that hasn't
    ' consulted `paired` — and if it ever fires, it means something wrote a
    ' program pull past a false `paired`, which the log should say plainly.
    if trustPem = ""
        print "[player-v3] trust rehydrate FAILED — no anchor is persisted for this screen; it must be re-paired (not a filesystem fault)"
        return ""
    end if
    path = "cachefs:/waiveo_player_v3_trust.pem"
    ok = false
    try
        ok = WriteAsciiFile(path, trustPem)
    catch e
        print "[player-v3] trust rehydrate FAILED — writing " + path + " threw; the anchor is intact, so this is a cachefs fault and retrying is right"
        return ""
    end try
    if not ok
        print "[player-v3] trust rehydrate FAILED — writing " + path + " returned false (cachefs full or unavailable); the anchor is intact, retrying is right"
        return ""
    end if
    return path
end function
