import { BrowserRouter, Routes, Route } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";

// The application root. This is the scaffold shell: the widget kit and the real
// routes (the /design gallery, the app shell) layer on top of it in the tasks
// that follow. The ThemeProvider owns the Dusk/Daybreak theme for the whole app
// (default Dusk, persisted, reflected as data-theme on <html>).
function Home() {
  return (
    <main>
      <h1 className="font-display">Waiveo</h1>
      <p>The console is warming up.</p>
    </main>
  );
}

export default function App() {
  return (
    <ThemeProvider>
      <BrowserRouter>
        <Routes>
          <Route path="/" element={<Home />} />
        </Routes>
      </BrowserRouter>
    </ThemeProvider>
  );
}
