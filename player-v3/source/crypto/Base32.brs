' Base32.brs — RFC 4648 standard-alphabet base32 DECODE (no padding).
'
' The player/1 pairing code (PLY-024) is the length-prefixed component blob
' base32-encoded with base32.StdEncoding.WithPadding(NoPadding) on the Go side
' (internal/shared/paircode/paircode.go). Roku's roByteArray offers base64 but
' NOT base32, so we decode it by hand here. This MUST byte-match the Go codec
' or a pairing code minted by the relay will not decode on the screen.
'
' Alphabet: A-Z (values 0..25) then 2-7 (values 26..31). Character values are
' derived via Asc() rather than string indexing to avoid any roString.Mid
' 0/1-base ambiguity across firmware. Input hyphens/whitespace are NOT handled
' here — callers strip and upper-case before calling (PLY-024).
'
' Returns { ok: Boolean, bytes: roByteArray, error: String }. Fails closed
' (ok=false) on any character outside the alphabet — never a partial guess.

function wvB32Decode(s as String) as Object
    result = { ok: false, bytes: CreateObject("roByteArray"), error: "" }

    inBytes = CreateObject("roByteArray")
    inBytes.FromAsciiString(s)
    n = inBytes.Count()

    buffer = 0        ' bit accumulator (MSB-first)
    bitCount = 0      ' number of valid bits currently in `buffer`
    outBytes = CreateObject("roByteArray")

    for i = 0 to n - 1
        code = inBytes[i]
        val = -1
        if code >= 65 and code <= 90        ' 'A'..'Z' -> 0..25
            val = code - 65
        else if code >= 50 and code <= 55   ' '2'..'7' -> 26..31
            val = code - 24
        end if
        if val < 0
            result.error = "invalid base32 character at index " + i.toStr() + " (code " + code.toStr() + ")"
            return result
        end if

        ' Shift the 5 new bits in from the low end.
        buffer = (buffer * 32) + val
        bitCount = bitCount + 5

        ' Emit a byte whenever we have at least 8 bits. Each iteration adds
        ' exactly 5 bits and bitCount is always < 8 on entry, so at most one
        ' byte is emitted per iteration.
        if bitCount >= 8
            bitCount = bitCount - 8
            shift = wvPow2(bitCount)
            outByte = (buffer \ shift) mod 256
            outBytes.Push(outByte)
            buffer = buffer mod shift   ' keep only the low `bitCount` bits
        end if
    end for

    ' Per RFC 4648, leftover bits (< 8) after a no-padding encode are zero
    ' partial bits and carry no whole byte — discarded.
    result.bytes = outBytes
    result.ok = true
    return result
end function

' wvPow2 returns 2^n for small non-negative n (BrightScript has no reliable
' integer left-shift operator across firmware, so multiply).
function wvPow2(n as Integer) as Integer
    p = 1
    for i = 1 to n
        p = p * 2
    end for
    return p
end function
