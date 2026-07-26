' SelfCheck.brs — on-device confirmation of the player/1 pairing crypto against
' the committed cross-implementation golden vectors (testdata/). This retires the
' ONE unknown the off-device mirror could not: that roEVPDigest("sha256") and the
' BrightScript runtime produce byte-identical results to the Go tlsboot/paircode
' codecs on real firmware (player-1.md:162 draft-note; spike findings.md:294).
'
' Runs on boot when launched with selfcheck=1 (or always in DEBUG). Prints
' "[RESULT] <name> = PASS|FAIL :: <detail>" — grep the debug console (telnet
' 8085) for "[RESULT]". No SceneGraph, no network: pure local crypto vs. the
' same golden bytes the Go TestGoldenFixture* tests assert against.

sub wvRunSelfCheck()
    print "=================================================================="
    print "player-v3 crypto self-check vs. committed golden vectors"
    print "=================================================================="

    goldenPem = wvReadPkg("pkg:/testdata/golden-relay-cert.pem")
    goldenCommit = wvTrim(wvReadPkg("pkg:/testdata/golden-relay-spki-commitment.hex"))
    pcJsonRaw = wvReadPkg("pkg:/testdata/golden-paircode.json")

    if goldenPem = ""
        wvResult("fixture-load: golden-relay-cert.pem", false, "empty/missing")
        return
    end if
    if goldenCommit = ""
        wvResult("fixture-load: golden-relay-spki-commitment.hex", false, "empty/missing")
        return
    end if

    ' --- empty input fails closed by contract: roEVPDigest on observed firmware
    ' (15.2.4) cannot digest zero-length input (Process(empty) and Final()
    ' without update both return ""), so wvSha256Hex returns "" deterministically
    ' and every caller treats "" as failure. ---
    empty = CreateObject("roByteArray")
    emptyHex = wvSha256Hex(empty)
    wvResult("wvSha256Hex(empty) fails closed (returns empty)", emptyHex = "", "got=" + emptyHex)

    ' --- roEVPDigest sanity on the Process() path pairing actually uses: a
    ' non-empty NIST vector, sha256("abc"). ---
    abc = CreateObject("roByteArray")
    abc.FromAsciiString("abc")
    abcHex = wvSha256Hex(abc)
    wantAbc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
    wvResult("roEVPDigest sha256(abc) vs known vector", abcHex = wantAbc, "got=" + abcHex)

    ' --- PEM -> DER -> SPKI -> commitment == golden committed hex ---
    derRes = wvPemToDer(goldenPem)
    if not derRes.ok
        wvResult("pem->der (golden cert)", false, derRes.error)
        return
    end if
    wvResult("pem->der (golden cert)", true, derRes.der.Count().toStr() + " DER bytes")

    spkiRes = wvSpkiFromCertDer(derRes.der)
    if not spkiRes.ok
        wvResult("asn1 SPKI extract (golden cert)", false, spkiRes.error)
        return
    end if
    wvResult("asn1 SPKI extract (golden cert)", spkiRes.spki.Count() = 44, spkiRes.spki.Count().toStr() + " SPKI bytes (want 44 for ed25519)")

    spkiList = CreateObject("roArray", 0, true)
    spkiList.Push(spkiRes.spki)
    gotCommit = wvCommitmentHex(spkiList)
    wvResult("commitment(SPKI) == golden hex", gotCommit = goldenCommit, "got=" + gotCommit + " want=" + goldenCommit)

    ' --- the full local pin path: verify the golden PEM against its OOB hex ---
    pems = CreateObject("roArray", 0, true)
    pems.Push(goldenPem)
    verified = wvVerifyCommitment(pems, goldenCommit)
    wvResult("wvVerifyCommitment(golden pem, golden hex) accepts", verified, "")

    ' --- fail-closed: a one-nibble-corrupted commitment must be rejected (MITM) ---
    tampered = wvFlipFirstNibble(goldenCommit)
    rejected = not wvVerifyCommitment(pems, tampered)
    wvResult("wvVerifyCommitment rejects corrupted hex (fail-closed)", rejected, "tampered=" + tampered)

    ' --- pairing-code decode round-trips the Go-encoded golden vector ---
    if pcJsonRaw <> ""
        pcv = ParseJson(pcJsonRaw)
        if pcv <> invalid
            dec = wvPairCodeDecode(pcv.pairing_code)
            if dec.ok
                hostOk = (dec.host = pcv.host)
                portOk = (dec.port = pcv.port)
                gsOk = (dec.grantSelector = pcv.grant_selector)
                cOk = (dec.commitmentHex = pcv.commitment_hex)
                allOk = hostOk and portOk and gsOk and cOk
                wvResult("paircode decode round-trips golden vector", allOk, "host=" + wvBoolS(hostOk) + " port=" + wvBoolS(portOk) + " gs=" + wvBoolS(gsOk) + " commit=" + wvBoolS(cOk))
                ' The decoded code's commitment must equal the golden cert's — a
                ' screen decoding this code then verifying the golden PEM passes.
                bindOk = (dec.commitmentHex = goldenCommit)
                wvResult("paircode commitment binds golden cert", bindOk, "code=" + dec.commitmentHex + " cert=" + goldenCommit)
            else
                wvResult("paircode decode round-trips golden vector", false, dec.error)
            end if
        else
            wvResult("paircode fixture parse", false, "ParseJson returned invalid")
        end if
    end if

    ' --- storage-scoping regression (PLY-073/PLY-136): a 401
    ' CHANNEL_TOKEN_REVOKED/INVALID clears ONLY the channel token, never the
    ' pinned trust anchor; only the separate, explicit-trust-loss function
    ' (PLY-063) clears both. This exercises the REAL registry section
    ' production Storage.brs uses (wvStorageSection), so it saves whatever
    ' is already persisted there first and restores it byte-for-byte
    ' afterward — this check must never be what corrupts a real paired
    ' screen's credentials if selfcheck=1 is ever run against one. ---
    reg = wvStorageSection()
    savedToken = wvStorageGet("channel_token")
    savedTrust = wvStorageGet("trust_pem")

    wvStorageSet("channel_token", "selfcheck-dummy-token")
    wvStorageSet("trust_pem", "selfcheck-dummy-trust-pem")
    wvStorageClearChannelToken()
    tokAfter = wvStorageGet("channel_token")
    trustAfter = wvStorageGet("trust_pem")
    wvResult("wvStorageClearChannelToken clears token, keeps trust_pem", tokAfter = "" and trustAfter = "selfcheck-dummy-trust-pem", "token=[" + tokAfter + "] trust=[" + trustAfter + "]")

    wvStorageSet("channel_token", "selfcheck-dummy-token")
    wvStorageSet("trust_pem", "selfcheck-dummy-trust-pem")
    wvStorageClearTrustAndToken()
    tokAfter2 = wvStorageGet("channel_token")
    trustAfter2 = wvStorageGet("trust_pem")
    wvResult("wvStorageClearTrustAndToken clears both", tokAfter2 = "" and trustAfter2 = "", "token=[" + tokAfter2 + "] trust=[" + trustAfter2 + "]")

    ' --- 401 taxonomy must survive a body-less response (PLY-008/PLY-073) ---
    ' BrightScript has no unit runner, so this is the on-device expression of
    ' the property the Go side asserts in
    ' internal/relay/playerserver/authcode_test.go: a 401 whose BODY IS EMPTY —
    ' which is every 401 on this firmware, measured — must still classify,
    ' via the Problem-Code response header. Before the header path existed
    ' wv401Code returned "" for exactly this input and all three CHANNEL_TOKEN_*
    ' codes collapsed into the single non-destructive branch.
    '
    ' The header map is built CASE-SENSITIVE with a lowercased key on purpose:
    ' that is the hostile shape for a lookup helper (a plain headers["Problem-Code"]
    ' index misses it), and a helper that survives it survives whatever casing
    ' and lookup mode the firmware's own GetResponseHeaders() map happens to use.
    hdrs = CreateObject("roAssociativeArray")
    hdrs.SetModeCaseSensitive(true)
    hdrs.AddReplace("content-type", "application/problem+json")
    hdrs.AddReplace("problem-code", "CHANNEL_TOKEN_REVOKED")

    wvResult("wvHeaderValue finds a differently-cased header key", wvHeaderValue(hdrs, "Problem-Code") = "CHANNEL_TOKEN_REVOKED", "got=[" + wvHeaderValue(hdrs, "Problem-Code") + "]")
    wvResult("wvHeaderValue returns empty for an absent header", wvHeaderValue(hdrs, "Trace-Id") = "", "got=[" + wvHeaderValue(hdrs, "Trace-Id") + "]")
    wvResult("wvHeaderValue tolerates an invalid header map", wvHeaderValue(invalid, "Problem-Code") = "", "")

    emptyBody401 = { code: 401, body: "", headers: hdrs }
    gotCode = wv401Code(emptyBody401)
    wvResult("wv401Code reads the code from headers when the body is empty", gotCode = "CHANNEL_TOKEN_REVOKED", "got=[" + gotCode + "]")

    noHdr401 = { code: 401, body: "{""type"":""about:blank"",""code"":""CHANNEL_TOKEN_EXPIRED"",""title"":""Channel Token Expired""}", headers: {} }
    gotCode2 = wv401Code(noHdr401)
    wvResult("wv401Code falls back to the body when no header is present", gotCode2 = "CHANNEL_TOKEN_EXPIRED", "got=[" + gotCode2 + "]")

    neither401 = { code: 401, body: "", headers: {} }
    gotCode3 = wv401Code(neither401)
    wvResult("wv401Code yields empty when neither carrier has a code", gotCode3 = "", "got=[" + gotCode3 + "]")

    if savedToken = ""
        reg.Delete("channel_token")
    else
        wvStorageSet("channel_token", savedToken)
    end if
    if savedTrust = ""
        reg.Delete("trust_pem")
    else
        wvStorageSet("trust_pem", savedTrust)
    end if
    reg.Flush()

    print "=================================================================="
    print "self-check complete — grep [RESULT] lines above"
    print "=================================================================="
end sub

function wvReadPkg(path as String) as String
    s = ""
    try
        v = ReadAsciiFile(path)
        if v <> invalid then s = v
    catch e
        print "[selfcheck] ReadAsciiFile(" + path + ") THREW: " + e.message
        return ""
    end try
    return s
end function

sub wvResult(name as String, pass as Boolean, detail as String)
    status = "FAIL"
    if pass then status = "PASS"
    print "[RESULT] " + name + " = " + status + " :: " + detail
end sub

function wvBoolS(b as Boolean) as String
    if b then return "ok"
    return "MISMATCH"
end function

' wvFlipFirstNibble flips the first hex character to a different value, producing
' a commitment guaranteed to differ by exactly one nibble.
function wvFlipFirstNibble(hex as String) as String
    if hex.Len() = 0 then return hex
    first = hex.Left(1)
    repl = "f"
    if first = "f" then repl = "0"
    return repl + hex.Mid(1)
end function
