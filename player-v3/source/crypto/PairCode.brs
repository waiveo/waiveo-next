' PairCode.brs — decode a player/1 pairing code (PLY-024) into its three
' components: the relay dial address {host, port}, the grant_selector, and the
' fingerprint_commitment. MUST mirror internal/shared/paircode/paircode.go
' byte-for-byte.
'
' Wire format (after base32 decode):
'   [1-byte hostLen][host bytes][2-byte big-endian port]
'   [1-byte gsLen][grant_selector bytes][1-byte commitLen][commitment bytes]
'
' The code is presented in hyphen-separated groups for readability; hyphens are
' visual only and stripped before decoding (PLY-024). Decode upper-cases too, so
' a lower-cased rendering still decodes (matching the Go Decode).
'
' fingerprint_commitment is returned as lowercase HEX (the form the local pin
' comparison uses). It is retained locally and NEVER sent to the relay (PLY-054).
'
' Returns { ok, host, port, grantSelector, commitmentHex, error }. Fails closed.

function wvPairCodeDecode(code as String) as Object
    r = { ok: false, host: "", port: 0, grantSelector: "", commitmentHex: "", error: "" }

    if code = invalid
        r.error = "nil code"
        return r
    end if

    stripped = UCase(code.Replace("-", ""))
    if stripped = ""
        r.error = "empty code"
        return r
    end if

    dec = wvB32Decode(stripped)
    if not dec.ok
        r.error = "base32 decode: " + dec.error
        return r
    end if
    buf = dec.bytes
    total = buf.Count()

    at = 0

    ' host (length-prefixed).
    hostRes = wvReadLenPrefixed(buf, at, total)
    if not hostRes.ok
        r.error = "host: " + hostRes.error
        return r
    end if
    r.host = wvBytesToAscii(hostRes.value)
    at = hostRes.nextPos

    ' port (2 bytes big-endian).
    if at + 2 > total
        r.error = "port: truncated"
        return r
    end if
    r.port = (buf[at] * 256) + buf[at + 1]
    at = at + 2

    ' grant_selector (length-prefixed).
    gsRes = wvReadLenPrefixed(buf, at, total)
    if not gsRes.ok
        r.error = "grant_selector: " + gsRes.error
        return r
    end if
    r.grantSelector = wvBytesToAscii(gsRes.value)
    at = gsRes.nextPos

    ' commitment (length-prefixed) — kept as bytes, returned as hex.
    cRes = wvReadLenPrefixed(buf, at, total)
    if not cRes.ok
        r.error = "commitment: " + cRes.error
        return r
    end if
    r.commitmentHex = LCase(cRes.value.ToHexString())
    at = cRes.nextPos

    if at <> total
        r.error = (total - at).toStr() + " trailing byte(s) after all fields"
        return r
    end if

    r.ok = true
    return r
end function

' wvReadLenPrefixed reads a [1-byte length][value] field. Returns
' { ok, value: roByteArray, nextPos, error }.
function wvReadLenPrefixed(buf as Object, at as Integer, total as Integer) as Object
    out = { ok: false, value: CreateObject("roByteArray"), nextPos: at, error: "" }
    if at + 1 > total
        out.error = "missing length prefix"
        return out
    end if
    n = buf[at]
    valStart = at + 1
    if valStart + n > total
        out.error = "value (declared length " + n.toStr() + ") overruns buffer"
        return out
    end if
    val = CreateObject("roByteArray")
    for i = valStart to valStart + n - 1
        val.Push(buf[i])
    end for
    out.value = val
    out.nextPos = valStart + n
    out.ok = true
    return out
end function

' wvBytesToAscii renders a roByteArray as an ASCII/UTF-8 String.
function wvBytesToAscii(ba as Object) as String
    if ba = invalid then return ""
    if ba.Count() = 0 then return ""
    return ba.ToAsciiString()
end function
