' Commitment.brs — the player/1 out-of-band pairing-pin (PLY-052/053/054/056).
'
' Given the trust_anchors[].pem strings the relay returns in the PairingResponse
' body, compute the commitment locally and compare it, byte-exact, against the
' fingerprint_commitment carried out-of-band in the pairing code. The commitment
' is sha256 over the ordered concatenation of each anchor's SubjectPublicKeyInfo
' (SPKI), truncated to 16 bytes (128 bits). We express the 16-byte truncation as
' the first 32 hex characters of the digest, so no byte-slicing is needed and
' the value compares directly against the OOB hex.
'
' MITM defense (PLY-056): the commitment is ALWAYS the out-of-band value from
' the pairing code, recomputed here from the SPKI of the cert(s) actually
' fetched — never a value the relay put on the wire. Callers pass the OOB
' commitment in; nothing here derives an "expected" value from the connection.

' wvPemToDer strips one PEM certificate's armor and base64-decodes its body to
' DER bytes. Returns { ok, der: roByteArray, error }.
function wvPemToDer(pem as String) as Object
    out = { ok: false, der: CreateObject("roByteArray"), error: "" }

    if pem = invalid
        out.error = "nil PEM"
        return out
    end if

    b64 = ""
    lines = pem.Split(Chr(10))
    for each rawLine in lines
        line = wvTrim(rawLine)
        keep = true
        if line = "" then keep = false
        if keep and line.Instr("-----") >= 0 then keep = false   ' armor line (BEGIN/END)
        if keep then b64 = b64 + line
    end for

    if b64 = ""
        out.error = "no base64 body found between PEM armor"
        return out
    end if

    der = CreateObject("roByteArray")
    der.FromBase64String(b64)
    if der.Count() = 0
        out.error = "base64 body decoded to zero bytes"
        return out
    end if

    out.der = der
    out.ok = true
    return out
end function

' wvSha256Hex returns the lowercase hex sha256 of a roByteArray, via the
' firmware roEVPDigest component. This is the one primitive whose on-device
' byte-correctness the golden self-check (SelfCheck.brs) confirms before pairing
' ever depends on it (player-1.md:162 draft-note; spike findings.md:294).
function wvSha256Hex(bytes as Object) as String
    digest = CreateObject("roEVPDigest")
    digest.Setup("sha256")
    out = digest.Process(bytes)
    if out = invalid then return ""
    ' roEVPDigest.Process returns a hex String on documented firmware; guard the
    ' roByteArray shape too so a firmware difference FAILs the self-check cleanly
    ' rather than crashing on LCase of a non-String.
    if type(out) = "roByteArray" then return LCase(out.ToHexString())
    return LCase(out)
end function

' wvCommitmentHex computes the PLY-052 commitment (first 16 bytes / 32 hex chars
' of sha256 over the ordered SPKI concatenation) for an roArray of SPKI
' roByteArrays. Returns "" on any failure.
function wvCommitmentHex(spkiList as Object) as String
    if spkiList = invalid then return ""
    if spkiList.Count() = 0 then return ""

    concat = CreateObject("roByteArray")
    for each spki in spkiList
        if spki = invalid then return ""
        if spki.Count() = 0 then return ""
        concat.Append(spki)
    end for

    full = wvSha256Hex(concat)
    if full.Len() < 32 then return ""
    return LCase(full.Left(32))
end function

' wvVerifyCommitment reports whether the trust-anchor PEMs commit to `oobHex`
' (the pairing code's fingerprint_commitment, lowercase hex). Fails closed on
' any parse/extract/hash error or length mismatch. Comparison walks the full
' string length (no early-out on first mismatch) to avoid leaking a match-prefix
' length, mirroring the Go constant-time compare.
function wvVerifyCommitment(pems as Object, oobHex as String) as Boolean
    if pems = invalid then return false
    if pems.Count() = 0 then return false
    if oobHex = invalid then return false
    if oobHex.Len() = 0 then return false

    spkiList = CreateObject("roArray", 0, true)
    for each pem in pems
        derRes = wvPemToDer(pem)
        if not derRes.ok then return false
        spkiRes = wvSpkiFromCertDer(derRes.der)
        if not spkiRes.ok then return false
        spkiList.Push(spkiRes.spki)
    end for

    got = wvCommitmentHex(spkiList)
    if got = "" then return false
    return wvHexEqualConstant(got, LCase(oobHex))
end function

' wvHexEqualConstant compares two lowercase-hex strings without an early return,
' so timing does not reveal how many leading characters matched.
function wvHexEqualConstant(a as String, b as String) as Boolean
    if a.Len() <> b.Len() then return false
    diff = 0
    n = a.Len()
    for i = 0 to n - 1
        if a.Mid(i, 1) <> b.Mid(i, 1) then diff = diff + 1
    end for
    return (diff = 0)
end function

' wvTrim strips leading/trailing spaces, CR, and tabs (BrightScript String has
' no built-in Trim on all firmware).
function wvTrim(s as String) as String
    if s = invalid then return ""
    out = s
    ' Trailing.
    while out.Len() > 0
        c = out.Right(1)
        if c = " " or c = Chr(13) or c = Chr(9)
            out = out.Left(out.Len() - 1)
        else
            exit while
        end if
    end while
    ' Leading.
    while out.Len() > 0
        c = out.Left(1)
        if c = " " or c = Chr(13) or c = Chr(9)
            out = out.Mid(1)
        else
            exit while
        end if
    end while
    return out
end function
