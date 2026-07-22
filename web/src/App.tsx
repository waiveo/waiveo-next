import { BrowserRouter, Routes, Route } from "react-router";

// The application root. This is the scaffold shell: the design system, the
// widget kit and the real routes (the /design gallery, the app shell) layer on
// top of it in the tasks that follow. Kept intentionally minimal so the
// foundation — build, embed, serve, test — is provable on its own.
function Home() {
  return (
    <main>
      <h1>Waiveo</h1>
      <p>The console is warming up.</p>
    </main>
  );
}

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<Home />} />
      </Routes>
    </BrowserRouter>
  );
}
