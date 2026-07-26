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
function wvStorageLoad() as Object
    channelToken = wvStorageGet("channel_token")
    out = {
        paired: (channelToken <> ""),
        channelToken: channelToken,
        screenId: wvStorageGet("screen_id"),
        relayHost: wvStorageGet("relay_host"),
        relayPort: wvStorageGet("relay_port"),
        trustPem: wvStorageGet("trust_pem")
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
function wvRehydrateTrustFile(trustPem as String) as String
    if trustPem = "" then return ""
    path = "cachefs:/waiveo_player_v3_trust.pem"
    ok = false
    try
        ok = WriteAsciiFile(path, trustPem)
    catch e
        return ""
    end try
    if not ok then return ""
    return path
end function
