import { BrowserRouter, Routes, Route } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { Toaster, EmptyState, Button } from "@/components/kit";
import { Compass } from "lucide-react";
import { Link } from "react-router";
import { SessionGate } from "@/auth/session-gate";
import { AppShell } from "@/shell/app-shell";
import LoginRoute from "@/routes/login/login-route";
import SetupRoute from "@/routes/setup/setup-route";
import OverviewRoute from "@/routes/overview/overview-route";
import ScreensRoute from "@/routes/screens/screens-route";
import DevicesRoute from "@/routes/devices/devices-route";
import RokuRoute from "@/routes/roku/roku-route";
import SchedulesRoute from "@/routes/schedules/schedules-route";
import UploadRoute from "@/routes/upload/upload-route";
import MediaRoute from "@/routes/media/media-route";
import CastsRoute from "@/routes/casts/casts-route";
import StudioRoute from "@/routes/studio/studio-route";
import PreviewRoute from "@/routes/preview/preview-route";
import WidgetsRoute from "@/routes/widgets/widgets-route";
import AutomationsRoute from "@/routes/automations/automations-route";
import VariablesRoute from "@/routes/variables/variables-route";
import ActivityRoute from "@/routes/activity/activity-route";
import DesignRoute from "@/routes/design/design-route";
import PagesRoute from "@/routes/pages/pages-route";
import PackPageRoute from "@/routes/packs/pack-page-route";
import ExtensionsRoute from "@/routes/extensions/extensions-route";
import ApiKeysRoute from "@/routes/apikeys/api-keys-route";
import SecurityRoute from "@/routes/security/security-route";
import SystemRoute from "@/routes/system/system-route";
import SettingsRoute from "@/routes/settings/settings-route";
import BackupRoute from "@/routes/backup/backup-route";

// The application root. The ThemeProvider owns the Dusk/Daybreak theme for the
// whole app (default Dusk, persisted, reflected as data-theme on <html>). The
// AppShell is the responsive frame every route renders inside (via <Outlet/>): a
// locked-left nav rail, a left drawer + hamburger below 1024px, a header, and the
// content region. The console pages talk to the feeder over the same-origin
// api/1 client and the ui-schema renderer; Activity streams the live /events/v1
// SSE. The full console navigation is wired here.
//
// Every api/1 route is authenticated (security-model/1 SEC-005), so the whole
// shell sits behind a SessionGate: it probes /auth/session once and either
// renders the console or redirects to /login. /login itself is OUTSIDE the gate
// and outside the shell — a sign-in page behind a sign-in requirement would
// never render, and the shell's own pages all assume a session.
export default function App() {
  return (
    <ThemeProvider>
      <BrowserRouter>
        <Routes>
          <Route path="/login" element={<LoginRoute />} />
          {/* First-boot claim (security-model/1 SEC-120), outside the gate for
              the same reason /login is: it runs before any session exists — it
              is what MINTS the first one. Mounted unconditionally, on a claimed
              box as much as an unclaimed one, because nothing tells the console
              which it is and publishing that would be the disclosure, not the
              route. SEC-121 re-opens the claim window after a factory reset, and
              the PAGE is ready for that second claim because it holds no state
              that could remember the first — but the BOX is not ready until it
              restarts, since the reset destroys rows and the fresh setup grant
              is minted by the next boot (internal/app/auth/destroy.go). The
              route's 401 message names the restart for exactly that reason. */}
          <Route path="/setup" element={<SetupRoute />} />
          {/* The Studio is INSIDE the session gate and OUTSIDE the shell, and it
              is the only route that is both.

              It is a full-screen editor, not a console page: a header, a canvas
              and four docked tool panels that between them want every pixel of
              the viewport. Mounted in the shell's content column it rendered
              beside a 240px nav rail that the operator cannot use while editing
              anyway — the rail's destinations all abandon an unsaved cast — so
              the rail was costing the canvas a sixth of its width and offering
              nothing back. It is declared in the shell's OFF_RAIL_ROUTES for the
              same reason it has no rail entry: it is a tool applied to a cast,
              reached from the Casts library, and it carries its own door back
              (the header's "Back to casts", which asks before discarding).

              The gate still wraps it, because /casts and every read the editor
              makes are authenticated (SEC-005); what it sheds is the CHROME, not
              the authentication. */}
          <Route
            path="/studio"
            element={
              <SessionGate>
                <StudioRoute />
              </SessionGate>
            }
          />
          {/* Cast preview: watch a cast play, see which of its slides a screen
              would actually be sent, and work an authored menu or button with a
              remote. Inside the gate and outside the shell for exactly the
              reasons the Studio is — a 1920×1080 stage, a transport and a panel
              want the viewport, and every rail destination abandons the thing
              you are watching. It carries its own door back.

              The Studio hosts the SAME player in an overlay over its live
              document, because previewing unsaved edits is the loop this
              surface exists for and a navigation would discard them. This route
              is the other half: opening a preview on a SAVED cast from the
              library, without opening an editor on it first, and a link one
              person can send another. See preview-route.tsx. */}
          <Route
            path="/preview"
            element={
              <SessionGate>
                <PreviewRoute />
              </SessionGate>
            }
          />
          <Route
            element={
              <SessionGate>
                <AppShell />
              </SessionGate>
            }
          >
            <Route path="/" element={<OverviewRoute />} />
            <Route path="/screens" element={<ScreensRoute />} />
            {/* The device plane: what the relays have discovered, what has been
                adopted, and the virtual remote. Distinct from /screens, which
                is about displays content is scheduled against — a device is a
                separate row with a separate owner (the relay). */}
            <Route path="/devices" element={<DevicesRoute />} />
            {/* Operating an adopted media player, as distinct from discovering
                and adopting one: live state, the remote, and every dispatch's
                outcome kept on screen. Separate from /devices because the two
                are different jobs — one is a decision, the other is a sequence
                of actions whose results have to stay readable. */}
            <Route path="/roku" element={<RokuRoute />} />
            <Route path="/schedules" element={<SchedulesRoute />} />
            {/* Upload, NOT `/content`: the feeder mounts the content origin at
                `/content/` on this same mux, and ServeMux redirects `/content` to
                it — so a console route there was answered by the origin's 403
                Problem document and the SPA never saw the request. The origin's
                path is on the wire (signed urls, REL-061), so the console moved.
                cmd/waiveo-feeder/consoleroutes_test.go now derives both lists and
                resolves every route below against a real ServeMux. */}
            <Route path="/upload" element={<UploadRoute />} />
            {/* The cast library: the slide documents themselves. Its rows open
                the Studio at `/studio?id=…` — a query rather than a path segment
                because the Studio is a tool APPLIED to a cast, not a sub-resource
                of one, and the same editor opens with no cast at all (it says so,
                and links back here). The editor itself is mounted above, outside
                this shell. */}
            <Route path="/casts" element={<CastsRoute />} />
            {/* /studio is NOT here — see the full-screen note above the gate. */}
            {/* The content origin's READ half. /upload puts bytes on the box and
                shows this session's uploads by filename; this browses everything
                already stored, addressed by digest. */}
            <Route path="/media" element={<MediaRoute />} />
            {/* What a slide can carry, browsable (parity row 8.4). Legacy had a
                Widgets area under Slidecast and this console had none — the
                kinds existed only as an insert menu inside the Studio, which
                meant finding out whether the platform could put the weather on
                a wall required first opening an editor on a cast you had not
                made. Sits under Slidecast in the rail, where legacy had it. */}
            <Route path="/widgets" element={<WidgetsRoute />} />
            <Route path="/automations" element={<AutomationsRoute />} />
            <Route path="/variables" element={<VariablesRoute />} />
            <Route path="/activity" element={<ActivityRoute />} />
            <Route path="/pages" element={<PagesRoute />} />
            {/* The pack lifecycle: what is installed, install/update/uninstall,
                and each pack's install-record provenance. Distinct from the
                `/p/...` routes below, which OPEN an installed pack's own page —
                this one manages the packs themselves. */}
            <Route path="/extensions" element={<ExtensionsRoute />} />
            <Route path="/api-keys" element={<ApiKeysRoute />} />
            {/* An installed pack's page: `/p/{publisher}/{name}/{path}` — the
                pack id is two path segments, the page path a trailing splat. */}
            <Route path="/p/:publisher/:name/*" element={<PackPageRoute />} />
            {/* The caller's own account security — second-factor enrollment
                (security-model/1 SEC-004). Reached from the header rather than
                the primary rail: it acts on the signed-in principal, not on a
                console resource, so it does not belong beside the resource
                families. */}
            <Route path="/security" element={<SecurityRoute />} />
            {/* Operator diagnostics (parity row 7.4): the box's own health and
                its captured log, in one place, so "why is that screen dark"
                does not start with an SSH session. Both reads are owner-only —
                the page renders that refusal as an explanation rather than as a
                blank panel. */}
            <Route path="/system" element={<SystemRoute />} />
            {/* The box's own configuration. Scoped hard on purpose: five of the
                six settings the parity diff attributes to this absence have
                nothing in this build to write to (no log level exists, the log
                buffer's capacity is a compile-time constant, browser sessions
                carry no expiry, the weather provider is keyless by design), and
                the sixth — restart — already lives on /system. What is left is
                the one platform setting everything reads: where a site is and
                what local time it keeps. See the route's docstring. */}
            <Route path="/settings" element={<SettingsRoute />} />
            {/* Workspace backup (parity row 7.5): export to one encrypted
                portable container, get it OFF the box, and restore from it.
                Separate from /system because it is an act on the workspace, not
                a reading of the machine. */}
            <Route path="/backup" element={<BackupRoute />} />
            <Route path="/design" element={<DesignRoute />} />
            {/* The catch-all, INSIDE the shell on purpose. A mistyped or dead
                URL is the one moment an operator most needs the navigation —
                putting the 404 outside the shell would answer "that page does
                not exist" by removing every route they could go to instead.
                It sits last because react-router matches in order. */}
            <Route
              path="*"
              element={
                <EmptyState
                  icon={Compass}
                  title="No page at this address"
                  description="The link may be out of date, or the address mistyped. Nothing is wrong with the box."
                  action={
                    <Button variant="secondary" asChild>
                      <Link to="/">Back to Overview</Link>
                    </Button>
                  }
                />
              }
            />
          </Route>
        </Routes>
      </BrowserRouter>
      {/* The toast HOST. It lives here, above the router and inside the theme,
          for the reason the bug it fixes demonstrates: the console had a themed
          Toaster, kit exports for it, and passing theme tests for it — and it was
          MOUNTED NOWHERE. Five production `toast()` calls across three routes
          (backup export, and the design/pages surfaces) fired into a void, and
          `toast.success("Workspace exported")` was the sharpest: the one signal
          that an operator's backup actually succeeded could never appear.

          Above the router, so a toast survives the navigation that often follows
          the action that raised it. Inside ThemeProvider, because the sonner host
          reads the active Dusk/Daybreak theme from context. Outside SessionGate,
          so a route that toasts before a session resolves is still heard. */}
      <Toaster />
    </ThemeProvider>
  );
}
