' Main.brs — player-v3 entry point (waiveo-next first photon).
'
' Two modes:
'   - launched with selfcheck=1 -> run the golden-vector crypto self-check
'     (SelfCheck.brs) console-only and idle. This is the §10 on-hardware
'     confirmation of the pairing-pin primitives, readable entirely from the
'     debug console (no screen needed).
'   - otherwise -> run the SceneGraph app: show PhotonScene, hand it the pairing
'     code from the launch args (deep link launch/dev?pairingCode=...), and let
'     it pair, pull its program, fetch the image direct, and render it.

sub Main(args as Dynamic)
    print "###################################################################"
    print "# WAIVEO PLAYER v3 - START (first-photon build)"
    print "###################################################################"

    params = wvNormalizeArgs(args)

    if wvArgEquals(params, "selfcheck", "1")
        try
            wvRunSelfCheck()
        catch e
            print "[main] EXCEPTION during self-check: " + e.message
        end try
        wvIdle()
        return
    end if

    screen = CreateObject("roSGScreen")
    port = CreateObject("roMessagePort")
    screen.SetMessagePort(port)
    scene = screen.CreateScene("PhotonScene")
    screen.Show()

    code = wvArgValue(params, "pairingCode")
    if code <> ""
        print "[main] pairing code supplied via launch args"
        scene.pairingCode = code
    end if

    while true
        msg = wait(0, port)
        if type(msg) = "roSGScreenEvent"
            if msg.isScreenClosed()
                ' Tell the scene to stop its Task threads before we leave. A
                ' Task thread survives the node that owns it, so leaving
                ' without this is the shape that leaks threads.
                scene.callFunc("shutdown")
                return
            end if
        end if
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

function wvArgEquals(params as Dynamic, key as String, value as String) as Boolean
    if params = invalid then return false
    if type(params) <> "roAssociativeArray" then return false
    if not params.DoesExist(key) then return false
    return (params[key] = value)
end function

function wvArgValue(params as Dynamic, key as String) as String
    if params = invalid then return ""
    if type(params) <> "roAssociativeArray" then return ""
    if not params.DoesExist(key) then return ""
    v = params[key]
    if v = invalid then return ""
    return v
end function

sub wvIdle()
    print "###################################################################"
    print "# player-v3 idling (Home to exit)."
    print "###################################################################"
    p = CreateObject("roMessagePort")
    while true
        msg = wait(0, p)
    end while
end sub
