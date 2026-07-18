' Asn1.brs — the minimal DER walk needed to extract a certificate's
' SubjectPublicKeyInfo (SPKI), the exact bytes the PLY-052 pairing-pin
' commitment is computed over (tlsboot.SPKIFromCertDER -> RawSubjectPublicKeyInfo
' on the Go side). We must reproduce Go's x509.ParseCertificate(der)
' .RawSubjectPublicKeyInfo byte-for-byte: same SPKI bytes -> same sha256 ->
' same commitment (the golden fixture under testdata/ is the cross-impl oracle).
'
' Only DEFINITE-length DER is handled (X.509 certs are always DER). Structure
' walked (RFC 5280):
'   Certificate ::= SEQUENCE { tbsCertificate, signatureAlgorithm, signature }
'   TBSCertificate ::= SEQUENCE {
'       [0] version OPTIONAL, serialNumber, signature (AlgId), issuer, validity,
'       subject, subjectPublicKeyInfo, ... }
' So SPKI is the child of tbsCertificate immediately AFTER `subject`. We locate
' it positionally: if the first tbsCertificate child is the explicit [0] version
' tag (0xA0) it is child index 6, else index 5 — then take that child's full TLV
' bytes (tag+length+value), which IS RawSubjectPublicKeyInfo.

' wvAsn1ReadTLV parses one DER TLV at byte offset `at` in `ba`.
' Returns { ok, tag, hdrLen, len, valuePos, nextPos, error }.
' valuePos = offset of the value; nextPos = offset just past this TLV.
function wvAsn1ReadTLV(ba as Object, at as Integer) as Object
    r = { ok: false, tag: 0, hdrLen: 0, len: 0, valuePos: 0, nextPos: 0, error: "" }

    total = ba.Count()
    if at < 0 or at >= total
        r.error = "TLV start " + at.toStr() + " out of range (len " + total.toStr() + ")"
        return r
    end if

    r.tag = ba[at]
    lenPos = at + 1
    if lenPos >= total
        r.error = "truncated: no length byte after tag at " + at.toStr()
        return r
    end if

    first = ba[lenPos]
    if first < 128
        ' Short form: length is this byte.
        r.len = first
        r.valuePos = lenPos + 1
    else
        ' Long form: low 7 bits = number of subsequent length bytes.
        numBytes = first - 128
        if numBytes = 0 or numBytes > 4
            r.error = "unsupported long-form length (" + numBytes.toStr() + " bytes) at " + at.toStr()
            return r
        end if
        if lenPos + numBytes >= total
            r.error = "truncated long-form length at " + at.toStr()
            return r
        end if
        acc = 0
        for i = 1 to numBytes
            acc = (acc * 256) + ba[lenPos + i]
        end for
        r.len = acc
        r.valuePos = lenPos + 1 + numBytes
    end if

    r.hdrLen = r.valuePos - at
    r.nextPos = r.valuePos + r.len
    if r.nextPos > total
        r.error = "TLV value overruns buffer: nextPos " + r.nextPos.toStr() + " > len " + total.toStr()
        return r
    end if

    r.ok = true
    return r
end function

' wvSpkiFromCertDer returns { ok, spki: roByteArray, error } — the SPKI's full
' TLV bytes copied out of the certificate DER.
function wvSpkiFromCertDer(certDer as Object) as Object
    out = { ok: false, spki: CreateObject("roByteArray"), error: "" }

    ' Outer Certificate SEQUENCE.
    outer = wvAsn1ReadTLV(certDer, 0)
    if not outer.ok
        out.error = "outer certificate: " + outer.error
        return out
    end if
    if outer.tag <> 48   ' 0x30 SEQUENCE
        out.error = "outer tag is not SEQUENCE (0x30), got " + outer.tag.toStr()
        return out
    end if

    ' First child of Certificate = tbsCertificate SEQUENCE.
    tbs = wvAsn1ReadTLV(certDer, outer.valuePos)
    if not tbs.ok
        out.error = "tbsCertificate: " + tbs.error
        return out
    end if
    if tbs.tag <> 48
        out.error = "tbsCertificate tag is not SEQUENCE, got " + tbs.tag.toStr()
        return out
    end if

    ' Walk tbsCertificate children. SPKI index depends on whether the explicit
    ' [0] version tag (0xA0) is present.
    childPos = tbs.valuePos
    tbsEnd = tbs.nextPos

    firstChild = wvAsn1ReadTLV(certDer, childPos)
    if not firstChild.ok
        out.error = "tbs first child: " + firstChild.error
        return out
    end if

    spkiIndex = 5
    if firstChild.tag = 160   ' 0xA0 context-[0] explicit version
        spkiIndex = 6
    end if

    idx = 0
    cur = childPos
    while idx <= spkiIndex
        tlv = wvAsn1ReadTLV(certDer, cur)
        if not tlv.ok
            out.error = "tbs child " + idx.toStr() + ": " + tlv.error
            return out
        end if
        if idx = spkiIndex
            if tlv.tag <> 48
                out.error = "SPKI (child " + idx.toStr() + ") tag is not SEQUENCE, got " + tlv.tag.toStr()
                return out
            end if
            ' Copy the full TLV bytes [tlv start .. tlv.nextPos).
            spki = CreateObject("roByteArray")
            for b = cur to tlv.nextPos - 1
                spki.Push(certDer[b])
            end for
            out.spki = spki
            out.ok = true
            return out
        end if
        cur = tlv.nextPos
        if cur >= tbsEnd
            out.error = "ran out of tbsCertificate children before reaching SPKI index " + spkiIndex.toStr()
            return out
        end if
        idx = idx + 1
    end while

    out.error = "SPKI not located"
    return out
end function
