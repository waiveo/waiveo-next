import { BrowserRouter, Routes, Route } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { AppShell } from "@/shell/app-shell";
import OverviewRoute from "@/routes/overview/overview-route";
import ScreensRoute from "@/routes/screens/screens-route";
import SchedulesRoute from "@/routes/schedules/schedules-route";
import ContentRoute from "@/routes/content/content-route";
import AutomationsRoute from "@/routes/automations/automations-route";
import ActivityRoute from "@/routes/activity/activity-route";
import DesignRoute from "@/routes/design/design-route";
import PagesRoute from "@/routes/pages/pages-route";

// The application root. The ThemeProvider owns the Dusk/Daybreak theme for the
// whole app (default Dusk, persisted, reflected as data-theme on <html>). The
// AppShell is the responsive frame every route renders inside (via <Outlet/>): a
// locked-left nav rail, a left drawer + hamburger below 1024px, a header, and the
// content region. The console pages talk to the feeder over the same-origin
// api/1 client and the ui-schema renderer; Activity streams the live /events/v1
// SSE. The full console navigation is wired here.
export default function App() {
  return (
    <ThemeProvider>
      <BrowserRouter>
        <Routes>
          <Route element={<AppShell />}>
            <Route path="/" element={<OverviewRoute />} />
            <Route path="/screens" element={<ScreensRoute />} />
            <Route path="/schedules" element={<SchedulesRoute />} />
            <Route path="/content" element={<ContentRoute />} />
            <Route path="/automations" element={<AutomationsRoute />} />
            <Route path="/activity" element={<ActivityRoute />} />
            <Route path="/pages" element={<PagesRoute />} />
            <Route path="/design" element={<DesignRoute />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </ThemeProvider>
  );
}
