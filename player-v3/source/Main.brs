' Main.brs — player-v3 entry point (waiveo-next first photon).
'
' CURRENT INCREMENT: crypto core + on-device self-check only. Main runs the
' golden-vector crypto self-check (SelfCheck.brs) and idles so the debug console
' stays attachable. This is the §10 on-hardware confirmation of the pairing pin
' primitives BEFORE the pairing state machine and SceneGraph render depend on
' them.
'
' NEXT INCREMENTS (not yet wired here):
'   - Pairing.brs: decode code -> bootstrap fetch -> local OOB pin -> redeem ->
'     persist channel token (never-wipe, PLY-026).
'   - Program.brs + PhotonScene: program poll -> lease -> direct image fetch ->
'     Poster render.
' Main will then create an roSGScreen and drive the pairing/program flow; the
' self-check stays reachable via launch arg selfcheck=1.

sub Main(args as Dynamic)
    print "###################################################################"
    print "# WAIVEO PLAYER v3 - START (first-photon build)"
    print "###################################################################"

    params = wvNormalizeArgs(args)

    runSelfCheck = false
    #if DEBUG
        runSelfCheck = true
    #end if
    if params <> invalid
        if type(params) = "roAssociativeArray"
            if params.DoesExist("selfcheck")
                if params.selfcheck = "1" then runSelfCheck = true
                if params.selfcheck = "0" then runSelfCheck = false
            end if
        end if
    end if

    if runSelfCheck
        try
            wvRunSelfCheck()
        catch e
            print "[main] EXCEPTION during self-check: " + e.message
        end try
    end if

    ' TODO(next increment): create roSGScreen + PhotonScene and drive
    ' Pairing/Program here. For now, idle to keep the console attachable.
    print "###################################################################"
    print "# player-v3 idling (Home to exit). Pairing/render land next increment."
    print "###################################################################"
    port = CreateObject("roMessagePort")
    while true
        msg = wait(0, port)
    end while
end sub

function wvNormalizeArgs(args as Dynamic) as Dynamic
    if args = invalid then return invalid
    if type(args) = "roArray"
        if args.Count() > 0 then return args[0]
        return invalid
    end if
    return args
end function
