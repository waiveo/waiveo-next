' InteractionTask.brs — posts a viewer's press back to the relay
' (POST /player/v1/interaction; interactive slide layers, parity milestones
' 1.5/3.7).
'
' The whole reason this is a Task and not a call from PhotonScene: an HTTP round
' trip on the render thread blocks the render thread. roUrlTransfer's wait() is
' a blocking wait, and while it runs nothing draws, no Video decodes a frame and
' no key event is processed — so a press on a wall panel would freeze the panel
' for as long as the relay took to answer, which on a busy or half-reachable box
' is seconds. The press is deliberately FIRE AND FORGET from the scene's point of
' view: the scene shows its own immediate feedback (the focus ring flashes) and
' never waits for the network.
'
' Thread hygiene, the rule this player already learned the hard way: exactly ONE
' of these exists for the life of the app. The legacy player created a fresh
' ApiTask per press; Task threads outlive the node that owns them, so a panel
' pressed a few hundred times a day walks into the firmware thread cap. This one
' is created once by PhotonScene, steered by writing its `press` field, and
' stopped cooperatively at shutdown.

' wvStr coerces a possibly-invalid value to a string. Defined HERE rather than
' inherited: a SceneGraph component's scope is exactly the scripts its own XML
' includes, and this component deliberately does not include Pairing.brs (where
' the player's other wvStr lives) because doing so would drag the whole
' pairing/crypto chain into a thread that posts one small JSON body. Http.brs,
' which this component does include, calls wvStr from wvHeaderValue and resolves
' it against this definition through the component's flat scope.
function wvStr(v as Dynamic) as String
    if v = invalid then return ""
    if type(v) = "roString" or type(v) = "String" then return v
    return v.toStr()
end function

sub init()
    m.top.functionName = "runInteraction"
end sub

' runInteraction is one loop for the life of the app. It blocks on a message
' port bound to the `press` field rather than polling it, which is both cheaper
' and — more importantly — LOSSLESS: a second press arriving while the first is
' still being posted is queued by the port and delivered on the next pass. A
' poll of m.top.press would simply read whatever the field last held and silently
' drop the earlier press, which is the one thing this path must not do (a person
' pressed a button; the platform either acts on it or says why).
'
' The wait has a timeout so the two exits below are honored within one tick even
' when nobody is pressing anything — a Task blocked forever in wait() is exactly
' the stranded thread this player refuses to create.
sub runInteraction()
    port = CreateObject("roMessagePort")
    m.top.observeField("press", port)

    print "[player-v3] interaction task started — one Task for the life of this app; posts a viewer press to /player/v1/interaction"

    while true
        if m.top.stopFlag = true
            print "[player-v3] interaction task STOPPED — shutdown requested by the scene"
            return
        end if
        if LCase(m.top.control) <> "run"
            print "[player-v3] interaction task STOPPED — control field is no longer RUN"
            return
        end if

        msg = wait(wvInteractionWaitMs(), port)
        if msg <> invalid
            press = msg.GetData()
            if press <> invalid then wvPostInteraction(press)
        end if
    end while
end sub

' wvInteractionWaitMs is how long the loop blocks between checks of its two
' exits. Long enough that an idle panel costs essentially nothing, short enough
' that a shutdown is honored promptly.
function wvInteractionWaitMs() as Integer
    return 500
end function

' wvPostInteraction posts ONE press over the pinned relay connection — the same
' TLS posture the program poll uses (PLY-090: peer verification ON against the
' pinned relay trust anchor, host verification OFF because the relay is reached
' by IP and its bootstrap certificate carries no matching SAN). The pin is not
' optional here: the request carries the channel token, and the token is the
' credential the relay resolves the SCREEN identity from, so posting it to an
' unverified peer would hand a screen's credential to whatever answered.
'
' Every failure is logged and dropped rather than retried. That is a deliberate
' choice about what a press MEANS: it is an instant of human intent, and a press
' redelivered thirty seconds later would fire an automation the person had
' already given up on and walked away from. The console line is the record; the
' viewer's own recourse is to press it again, which is both immediate and
' something they can actually observe. (A durable, deduplicated press queue would
' need an idempotency key the relay honors and is not built.)
sub wvPostInteraction(press as Object)
    interaction = wvStr(press.interaction)
    leaseId = wvStr(press.lease_id)
    if interaction = "" or leaseId = ""
        print "[player-v3] interaction SKIPPED — a press needs both an interaction name and the lease it was made against"
        return
    end if

    state = wvStorageLoad()
    if not state.paired
        print "[player-v3] interaction '" + interaction + "' DROPPED — this player holds no pairing to report it to"
        return
    end if

    pinFile = wvRehydrateTrustFile(state.trustPem)
    if pinFile = ""
        print "[player-v3] interaction '" + interaction + "' DROPPED — could not rehydrate the pinned trust anchor"
        return
    end if

    body = {
        lease_id: leaseId,
        interaction: interaction
    }
    slideId = wvStr(press.slide_id)
    if slideId <> "" then body.slide_id = slideId

    resp = wvHttpJson({
        method: "POST",
        url: "https://" + state.relayHost + ":" + state.relayPort + "/player/v1/interaction",
        body: FormatJson(body),
        bearer: state.channelToken,
        certFile: pinFile,
        peerVerify: true,
        hostVerify: false,
        timeoutMs: 8000
    })

    if resp.ok
        print "[player-v3] interaction '" + interaction + "' delivered (lease " + leaseId + ")"
    else
        detail = resp.failureReason
        if resp.body <> "" then detail = resp.body
        print "[player-v3] interaction '" + interaction + "' FAILED: HTTP " + resp.code.toStr() + " " + detail
    end if
end sub
