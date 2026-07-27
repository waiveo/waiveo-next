import { BrowserRouter, Routes, Route } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { SessionGate } from "@/auth/session-gate";
import { AppShell } from "@/shell/app-shell";
import LoginRoute from "@/routes/login/login-route";
import OverviewRoute from "@/routes/overview/overview-route";
import ScreensRoute from "@/routes/screens/screens-route";
import SchedulesRoute from "@/routes/schedules/schedules-route";
import ContentRoute from "@/routes/content/content-route";
import AutomationsRoute from "@/routes/automations/automations-route";
import ActivityRoute from "@/routes/activity/activity-route";
import DesignRoute from "@/routes/design/design-route";
import PagesRoute from "@/routes/pages/pages-route";
import PackPageRoute from "@/routes/packs/pack-page-route";

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
          <Route
            element={
              <SessionGate>
                <AppShell />
              </SessionGate>
            }
          >
            <Route path="/" element={<OverviewRoute />} />
            <Route path="/screens" element={<ScreensRoute />} />
            <Route path="/schedules" element={<SchedulesRoute />} />
            <Route path="/content" element={<ContentRoute />} />
            <Route path="/automations" element={<AutomationsRoute />} />
            <Route path="/activity" element={<ActivityRoute />} />
            <Route path="/pages" element={<PagesRoute />} />
            {/* An installed pack's page: `/p/{publisher}/{name}/{path}` — the
                pack id is two path segments, the page path a trailing splat. */}
            <Route path="/p/:publisher/:name/*" element={<PackPageRoute />} />
            <Route path="/design" element={<DesignRoute />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </ThemeProvider>
  );
}
