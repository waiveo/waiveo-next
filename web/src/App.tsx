import { BrowserRouter, Routes, Route, Link } from "react-router";
import { Button } from "@/components/kit";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { AppShell } from "@/shell/app-shell";
import DesignRoute from "@/routes/design/design-route";

// The application root. The ThemeProvider owns the Dusk/Daybreak theme for the
// whole app (default Dusk, persisted, reflected as data-theme on <html>). The
// AppShell is the responsive frame every route renders inside (via <Outlet/>): a
// locked-left nav rail, a left drawer + hamburger below 1024px, a header, and the
// content region. Real app pages (screens/schedules/automations/…) arrive over
// the api client + the ui-schema renderer in a later wave; for now the shell
// hosts the living design gallery.
function Home() {
  return (
    <main className="mx-auto flex max-w-xl flex-col items-center gap-4 px-6 py-24 text-center">
      <h1 className="font-display text-[32px] font-semibold tracking-[-0.015em]">Waiveo</h1>
      <p className="text-muted-foreground">The console is warming up.</p>
      <Button asChild>
        <Link to="/design">Open the design gallery</Link>
      </Button>
    </main>
  );
}

export default function App() {
  return (
    <ThemeProvider>
      <BrowserRouter>
        <Routes>
          <Route element={<AppShell />}>
            <Route path="/" element={<Home />} />
            <Route path="/design" element={<DesignRoute />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </ThemeProvider>
  );
}
