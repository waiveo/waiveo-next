' Storage.brs — persistent player state under the never-wipe discipline
' (PLY-026/063/130). Uses its own registry section (distinct from any other
' channel's), and rehydrates the pinned trust-anchor PEM to a cachefs file on
' demand (SetCertificatesFile needs a filesystem path, not a registry blob —
' Wave-0 spike finding).
'
' Never-wipe: the relay address is persisted on first successful pairing and
' NEVER cleared on a mere connection failure. Only an explicit trust loss clears
' the channel token + trust anchor (wvStorageClearCredentials), and even then the
' address stays so a re-pair can target the same relay.

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

' wvStorageClearCredentials clears ONLY the channel token + trust anchor on an
' explicit trust loss (PLY-063), keeping the relay address (never-wipe).
sub wvStorageClearCredentials()
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
