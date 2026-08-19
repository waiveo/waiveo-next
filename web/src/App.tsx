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
import DevicesRoute from "@/routes/devices/devices-route";
import ActivityRoute from "@/routes/activity/activity-route";
import DesignRoute from "@/routes/design/design-route";
import PagesRoute from "@/routes/pages/pages-route";
import PackPageRoute from "@/routes/packs/pack-page-route";
import ExtensionsRoute from "@/routes/extensions/extensions-route";
import ApiKeysRoute from "@/routes/apikeys/api-keys-route";
import SecurityRoute from "@/routes/security/security-route";
import SystemRoute from "@/routes/system/system-route";
import SettingsRoute from "@/routes/settings/settings-route";

// The application root. The ThemeProvider owns the Dusk/Daybreak theme for the
// whole app (default Dusk, persisted, reflected as data-theme on <html>). The
// AppShell is the responsive frame every route renders inside (via <Outlet/>): a
// locked-left nav rail, a left drawer + hamburger below 1024px, a header, and the
// content region. The console pages talk to the feeder over the same-origin
// api/1 client and the ui-schema renderer; Activity streams the live /events/v1
// SSE. The full console navigation is wired here.
//
// # What CORE routes, after the 2026-08-19 strip
//
// The owner's rule for this console is that the product ships as PACKS: of the
// nine ship targets (automation, marketplace, backups, system, comms, slidecast,
// roku-integration, device-discovery, device-widgets) only ONE — device-discovery
// — is installed on the box today, and core stopped carrying the other eight's
// console surfaces rather than carrying them until each pack was ready. Thirteen
// route directories left with that decision: the whole Slidecast family (casts,
// screens, schedules, media, upload, widgets, and the full-screen Studio and
// Preview), automations and variables, the Roku console and its remote, and the
// backup surface.
//
// Those thirteen were NOT all decided by the same authority, and conflating them
// would misrepresent the one document everyone will check:
//
//   Map-attributed (plans/2026-08-11-capability-ownership-map.md §3.1 Bucket A,
//   plus §2.2 group E for variables) — `routes/studio` → `waiveo/slidecast`,
//   `routes/automations` → `waiveo/automation-ui`, `routes/roku` +
//   `routes/devices/roku-remote.tsx` + `routes/devices/command-outcome.ts` →
//   `waiveo/roku`, `routes/backup` → `waiveo/backups`, `routes/widgets` →
//   `waiveo/device-widgets`, `routes/variables` → `waiveo/system`. The map is
//   the authority for these six and the strip merely executes it.
//
//   Owner-decided, AGAINST the map — `routes/casts`, `routes/screens`,
//   `routes/schedules`, `routes/media`, `routes/upload`. The map files these
//   under §3.3 "Bucket C — genuinely undecided" and refuses to rule ("This needs
//   an owner decision; I am not guessing it"; §6.2 "8,751 lines hinge on it. I
//   have not picked a side"), while its §2.2 group A target-owner column
//   positively states core KEEPS `/casts`, `/playlists`, `/schedules`,
//   `/screens`. The owner settled it himself on 2026-08-19 by naming the
//   Slidecast group in the rail. Cite HIM for these five, not the map.
//
//   Unattributed — `routes/preview` post-dates the map and appears in it
//   nowhere; it went as part of the Slidecast family under the same owner call.
//
// The map's sixth Bucket-C row, `web/src/api/casts.ts` (651 lines), was
// deliberately KEPT while its five sibling rows went. The instruction was about
// the console surface, and the api/1 families that module types remain core and
// live — `/casts`, `/screens`, `/schedules`, `/playlists` in api/openapi.yaml,
// served by `internal/app/api/casts.go` and siblings — which is precisely what
// §2.2 group A says core keeps. The consequence, stated so nobody has to
// rediscover it: it is a typed client with no console consumer right now, still
// exported from `api/index.ts`, still wired into `api/resources.ts`, and its
// test suite still runs.
//
// What is left is what belongs to no pack: identity (/login, /setup, /security),
// the box itself (/settings, /api-keys, /activity, /system), the pack lifecycle
// (/extensions and the /p/... pack pages), Discovery (/devices), and two
// developer reference surfaces (/pages, /design) that are off the rail.
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
          <Route
            element={
              <SessionGate>
                <AppShell />
              </SessionGate>
            }
          >
            <Route path="/" element={<OverviewRoute />} />
            {/* The device plane: what the relays have discovered and what this
                deployment has adopted. This is device-discovery's surface, and
                the one ship target still installed on the box — it is the reason
                the strip above left the shell, the renderer and the pack-page
                host exactly where they were. */}
            <Route path="/devices" element={<DevicesRoute />} />
            <Route path="/activity" element={<ActivityRoute />} />
            <Route path="/pages" element={<PagesRoute />} />
            {/* The pack lifecycle: what is installed, install/update/uninstall,
                and each pack's install-record provenance. Distinct from the
                `/p/...` routes below, which OPEN an installed pack's own page —
                this one manages the packs themselves.

                It is the marketplace ship target's surface and by the strip's
                own rule it should have gone with the rest. It stays because the
                ownership map's §1.3 names the failure directly: an optional pack
                that is the only way to install packs is a bricking hazard. It is
                also how waiveo/discovery — the one pack that IS installed — was
                installed and is updated. Removing it leaves the CLI and api/1 as
                the only path. */}
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
                blank panel.

                This is NOT the `system` ship target: legacy's system EXTENSION
                owned Variables & Secrets, which was /variables and left with the
                strip. This page is box control, which the ownership map files
                under core. */}
            <Route path="/system" element={<SystemRoute />} />
            {/* The box's own configuration: where a site is and what local time
                it keeps — the one platform setting everything reads. See the
                route's docstring. */}
            <Route path="/settings" element={<SettingsRoute />} />
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
          MOUNTED NOWHERE. Production `toast()` calls across three routes fired
          into a void, and `toast.success("Workspace exported")` was the sharpest:
          the one signal that an operator's backup actually succeeded could never
          appear. (That backup surface has since left core with the strip; the
          lesson about mounting the host has not.)

          Above the router, so a toast survives the navigation that often follows
          the action that raised it. Inside ThemeProvider, because the sonner host
          reads the active Dusk/Daybreak theme from context. Outside SessionGate,
          so a route that toasts before a session resolves is still heard. */}
      <Toaster />
    </ThemeProvider>
  );
}
