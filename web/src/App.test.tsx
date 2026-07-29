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
    // Serve the 401 the REAL visitor to this page draws. That is not a detail of
    // the fixture: a box with no owner yet has nobody who could be signed in, so
    // an unauthenticated caller is the only caller /setup ever has. Under the
    // file's default handler — a valid session — the gate resolves to signed-in
    // and renders its children, so a /setup mounted INSIDE the gate would paint
    // the form too and this test would pass on the arrangement it exists to
    // forbid. With the 401 the gate redirects to /login instead, which is the
    // very absence this page exists to fix, and the assertions below fail.
    let probes = 0;
    server.use(
      http.get("*/api/v1/auth/session", () => {
        probes += 1;
        return new HttpResponse(null, { status: 401 });
      }),
    );
    window.history.pushState({}, "", "/setup");
    render(<App />);
    expect(await screen.findByRole("button", { name: /set up this box/i })).toBeInTheDocument();
    expect(screen.getByLabelText(/setup code/i)).toBeInTheDocument();
    // And the gate never ran at all: no session probe was issued. A route inside
    // the gate probes before it renders anything, so this pins "outside" rather
    // than merely "reachable while signed out" — which a gate that default-
    // permitted on a failed probe would also satisfy.
    expect(probes).toBe(0);
  });
});
