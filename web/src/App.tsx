import { BrowserRouter, Routes, Route, Link } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import DesignRoute from "@/routes/design/design-route";

// The application root. The ThemeProvider owns the Dusk/Daybreak theme for the
// whole app (default Dusk, persisted, reflected as data-theme on <html>). The
// real routes layer on top: the /design gallery is the living design reference;
// the app shell and its pages arrive in a later wave.
function Home() {
  return (
    <main className="mx-auto flex min-h-screen max-w-xl flex-col items-center justify-center gap-4 px-6 text-center">
      <h1 className="font-display text-[32px] font-semibold tracking-[-0.015em]">Waiveo</h1>
      <p className="text-muted-foreground">The console is warming up.</p>
      <Link
        to="/design"
        className="inline-flex h-9 items-center rounded-btn bg-[image:var(--grad-accent)] px-4 text-[13px] font-medium text-[color:var(--grad-accent-fg)] outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background"
      >
        Open the design gallery
      </Link>
    </main>
  );
}

export default function App() {
  return (
    <ThemeProvider>
      <BrowserRouter>
        <Routes>
          <Route path="/" element={<Home />} />
          <Route path="/design" element={<DesignRoute />} />
        </Routes>
      </BrowserRouter>
    </ThemeProvider>
  );
}
