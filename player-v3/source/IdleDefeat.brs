' IdleDefeat.brs — PLY-158's player-local obligation: whenever this player is
' actively assigned non-blank content, this platform's own idle/inactivity
' surface (here, the Roku screensaver) MUST NOT be allowed to cover that
' content. PLY-158 is explicitly independent of the relay-side recovery
' mechanism and of server connectivity — and PLY-152 makes that independence
' load-bearing rather than stylistic: a relay MUST NOT attempt recovery
' against a device reporting `app_type: "screensaver"`, so a screen the
' platform screensaver has covered is, by contract, one nothing else will
' rescue. A player that lets that happen stays covered forever.
'
' THE OBSERVED DEFECT. On real hardware, a player left with no remote input —
' which is every production signage screen, permanently — has the platform
' screensaver drawn over its cast after the device's idle delay elapses. The
' player is still the foreground app the whole time (a device-plane query
' reports this channel AND a screensaver surface simultaneously) and the cast
' keeps cycling underneath; a single keypress dismisses the screensaver and
' the cast is there. It is a display-layer defect, not a playback one.
'
' WHY THE MANIFEST FLAG IS NOT THE FIX. This player's manifest carries
' `screensaver_private=true`, and on hardware that demonstrably does not
' prevent the above. That attribute declares that a screensaver a channel
' SHIPS is private to that channel; it is a property of a channel-provided
' screensaver, not a suppression switch for the platform's own. This player
' ships no screensaver at all, so the flag has nothing to qualify and no value
' of it can satisfy PLY-158.
'
' WHY A LAST-INPUT-TIME REFRESH. The platform starts its idle surface from the
' time of the last user input, and PLY-158 names precisely why that is the
' wrong clock for a player: "a player showing static content generates no
' interaction a platform's own idle detection would otherwise observe."
' roAppManager.UpdateLastKeyPressTime() exists for exactly this case — an app
' whose genuine activity the platform's input-idle detection cannot see.
' Refreshing it on a cadence shorter than the shortest idle delay a device can
' be configured with means that clock never reaches its threshold while this
' player is presenting content, whatever the device's own setting happens to
' be (the defect was observed at roughly an hour; the shortest selectable
' delay is a minute, and this cadence covers that case too).
'
' WHY NOT THE HIDDEN-VIDEO TRICK. A hidden, permanently-looping Video node
' carrying disableScreenSaver=true also defeats the screensaver, and is what
' this fleet's earlier player did. It is rejected here as the primary
' mechanism because it holds a decoder open and plays forever, for the entire
' life of a channel whose content is images — an unbounded side effect in a
' player deliberately kept minimal, and one that would run permanently in
' production, where content is always assigned. disableScreenSaver IS set on
' the Video nodes this player already creates for genuine video items
' (PhotonScene), where it costs nothing and covers a video item for free; it
' is simply not a mechanism for a static image cast.
'
' BOUNDED BY CONSTRUCTION. The refresh rides a single repeating SceneGraph
' Timer declared in PhotonScene's own XML, on the render thread: no Task node,
' no thread, and nothing created per rendered item — this fleet has already
' been bitten by per-render spawned Tasks leaking until the firmware thread
' cap. The timer is started only while content is assigned and stopped when it
' is not, so a player showing nothing never holds the platform awake.

' wvIdleDefeatShouldEngage decides, from a program result alone, whether
' PLY-158's obligation is currently in force. A player is "actively assigned
' non-blank content" exactly when it holds a cast with at least one item to
' present. Everything else leaves the obligation off: a failed or not-yet-made
' program pull, a program that produced no items, and — once this player
' renders PLY-093's `display: "blank"`, which it does not yet — an intentional
' blank, which PLY-158 excludes by its own wording and PLY-155 treats the same
' way relay-side.
function wvIdleDefeatShouldEngage(contentType as Dynamic, items as Dynamic) as Boolean
    if contentType = invalid then return false
    if contentType <> "cast" then return false
    if items = invalid then return false
    return items.Count() > 0
end function

' wvIdleDefeatIntervalSeconds is how often the last-input-time refresh runs
' while engaged. It must stay comfortably below the shortest idle delay a
' device can be configured with (a minute) so the platform's idle clock is
' reset at least once inside even that window; 30s leaves 2x margin at that
' extreme and is nothing at all against the observed hour-long delay. The cost
' per refresh is one object creation and one call on the render thread.
function wvIdleDefeatIntervalSeconds() as Integer
    return 30
end function

' wvIdleDefeatHeartbeatTicks is how many refreshes pass between console
' heartbeat lines (onIdleDefeatTick) — one line per refresh would be 120 an
' hour and would bury everything else, while no line at all would leave
' on-device verification unable to tell "still refreshing an hour in" from
' "silently stopped". Every 10th refresh is a line every five minutes.
function wvIdleDefeatHeartbeatTicks() as Integer
    return 10
end function

' wvIdleDefeatPing performs one refresh of the platform's last-input time.
' Wrapped in try/catch on purpose: this is the single platform-specific call
' in the whole mechanism, and a firmware that did not expose it must degrade
' to "the screensaver may appear" — never to a render-thread exception that
' takes the cast down with it, which would be strictly worse than the defect
' this fixes.
sub wvIdleDefeatPing()
    try
        appManager = CreateObject("roAppManager")
        if appManager <> invalid then appManager.UpdateLastKeyPressTime()
    catch e
        print "[player-v3] idle-defeat: last-input-time refresh unavailable on this firmware (" + e.message + ")"
    end try
end sub
