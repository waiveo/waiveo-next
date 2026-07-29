import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import App from "@/App";

// The application root wires the console: the locked-left shell with the live
// Overview at "/". The Overview reads live counts over the same-origin api/1
// client, so the smoke serves empty pages (msw) and asserts the wired home
// renders inside the shell rather than depending on a real backend.
//
// The shell now sits behind a SessionGate — every api/1 route is authenticated
// (security-model/1 SEC-005) — so the smoke also serves a live session. Without
// one the gate redirects to /login, which is itself the correct behaviour and is
// asserted in its own test below.

const empty = () => HttpResponse.json({ items: [], cursor: null });
const session = () =>
  HttpResponse.json({
    principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
    kind: "user",
    role: "owner",
    aal: "standard",
    session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
  });
const server = setupServer(
  http.get("*/api/v1/auth/session", session),
  http.get("*/api/v1/scope-nodes", empty),
  http.get("*/api/v1/schedules", empty),
  http.get("*/api/v1/automations", empty),
  http.get("*/api/v1/playlists", empty),
  // The shell resolves its Extensions nav over the installed-packs list.
  http.get("*/api/v1/packs", empty),
);
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
  // The routes are mounted on a BrowserRouter, so a test that navigates does it
  // by pushing real history; put it back so the next test starts at the console.
  window.history.pushState({}, "", "/");
});
afterAll(() => server.close());

describe("App", () => {
  it("renders the console shell with the Overview home", async () => {
    render(<App />);
    // The Overview page is the "/" route, painted inside the shell.
    expect(await screen.findByRole("heading", { name: "Overview" })).toBeInTheDocument();
    // The primary nav rail carries the brand and the console destinations.
    const nav = screen.getAllByRole("navigation", { name: /primary/i })[0];
    expect(within(nav).getByRole("link", { name: "Screens" })).toBeInTheDocument();
    expect(within(nav).getByRole("link", { name: "Content" })).toBeInTheDocument();
  });

  it("sends an unauthenticated visitor to the sign-in page instead of the console", async () => {
    server.use(http.get("*/api/v1/auth/session", () => new HttpResponse(null, { status: 401 })));
    render(<App />);
    // SEC-005 refuses rather than default-permits, so there is nothing for the
    // console to render without a session — the shell must not paint at all.
    expect(await screen.findByRole("button", { name: /sign in/i })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Overview" })).not.toBeInTheDocument();
  });

  it("serves the first-boot setup form at /setup, outside the session gate", async () => {
    window.history.pushState({}, "", "/setup");
    render(<App />);
    // Outside the gate, like /login: it runs before any session exists because
    // it is what mints the first one. A /setup mounted INSIDE the gate would be
    // redirected to /login by the very absence it exists to fix, and no session
    // probe should even be issued here.
    expect(await screen.findByRole("button", { name: /set up this box/i })).toBeInTheDocument();
    expect(screen.getByLabelText(/setup code/i)).toBeInTheDocument();
  });
});
