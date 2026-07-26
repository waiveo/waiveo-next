' PlayerTask.brs — the player/1 client thread. Loads persisted state, pairs if
' needed (using the input pairingCode), then pulls its program and fetches the
' image, publishing progress + the final local image path on photonResult.

sub init()
    m.top.functionName = "runPhoton"
end sub

sub runPhoton()
    result = { phase: "start", ok: false, imageUri: "", status: "starting", error: "" }

    state = wvStorageLoad()

    ' An explicitly supplied launch-arg pairing code is deliberate operator
    ' re-provisioning: try it FIRST even when already paired (a re-keyed relay
    ' leaves persisted state stale forever otherwise). Never-wipe still holds:
    ' persisted state is only replaced AFTER the new pairing succeeds; on
    ' failure we fall back to the persisted pairing untouched.
    code = m.top.pairingCode
    if code = invalid then code = ""

    if code <> ""
        print "[player-v3] pairing…"
        pair = wvDoPairing(code)
        if pair.ok
            wvStoragePersistPairing(pair.channelToken, pair.screenId, pair.relayHost, pair.relayPort, pair.trustPem)
            print "[player-v3] paired (screen " + pair.screenId + "), token persisted"
            state = wvStorageLoad()
        else
            print "[player-v3] pairing FAILED: " + pair.error
            if not state.paired
                result.phase = "pair_failed"
                result.status = "pairing failed"
                result.error = pair.error
                m.top.photonResult = result
                return
            end if
            print "[player-v3] keeping existing persisted pairing (never-wipe)"
        end if
    else if not state.paired
        result.phase = "need_code"
        result.status = "waiting for pairing code"
        m.top.photonResult = result
        return
    end if

    print "[player-v3] pulling program…"
    prog = wvDoProgram({ channelToken: state.channelToken, relayHost: state.relayHost, relayPort: state.relayPort, trustPem: state.trustPem })
    if not prog.ok
        if prog.needsRepair
            ' PLY-063/073: clear only the credential, keep the relay address.
            wvStorageClearCredentials()
        end if
        result.phase = "program_failed"
        result.status = "program failed"
        result.error = prog.error
        print "[player-v3] program FAILED: " + prog.error
        m.top.photonResult = result
        return
    end if

    print "[player-v3] PHOTON — image verified + ready to render (" + prog.imageUri + ")"
    result.ok = true
    result.phase = "done"
    result.status = "photon"
    result.imageUri = prog.imageUri
    m.top.photonResult = result
end sub
