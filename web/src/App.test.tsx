import { describe, it, expect, beforeAll, afterAll, afterEach, beforeEach, vi } from "vitest";
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
  http.get("*/api/v1/extensions", empty),
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
    expect(within(nav).getByRole("link", { name: "Upload" })).toBeInTheDocument();
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

describe("App — the toast host is MOUNTED", () => {
  // This exists because the console had every part of a toast system except the
  // part that renders one. `components/kit` exported `Toaster` and `toast`,
  // `components/ui/sonner.tsx` themed it, and `ui.themes.test.tsx` proved it
  // mounts under both themes — but nothing in the application tree ever rendered
  // it. Five production `toast()` calls fired into a void, including
  // `toast.success("Workspace exported")`: the sole confirmation that an
  // operator's backup had actually been written.
  //
  // Every one of those parts passing its own test is exactly why no test failed.
  // A mount assertion inside a test that mounts the Toaster itself proves nothing
  // about the app, so this drives the real thing: raise a toast through the
  // public kit entry point that routes use, against the real <App/>, and require
  // it to reach the document.
  it("renders a toast raised from the same kit entry point the routes use", async () => {
    const { toast } = await import("@/components/kit");
    window.history.pushState({}, "", "/");
    render(<App />);

    // Wait for the app to settle so the assertion cannot pass on a transient
    // tree that a later render would discard.
    await screen.findByRole("navigation", { name: /primary/i });

    toast.success("Workspace exported");

    expect(await screen.findByText("Workspace exported")).toBeInTheDocument();
  });
});

describe("App — a thrown route keeps the navigation, and leaving it clears the error", () => {
  // The boundary's PLACEMENT is the whole design, and placement is exactly what
  // a unit test of the boundary cannot check. Wrapping the shell instead of its
  // content region would answer "this page is broken" by removing every page the
  // operator could go to — so these drive the real App.
  beforeEach(() => {
    vi.spyOn(console, "error").mockImplementation(() => {});
  });
  afterEach(() => vi.restoreAllMocks());

  it("shows the 404 inside the shell, with the rail still there", async () => {
    window.history.pushState({}, "", "/no-such-page");
    render(<App />);

    expect(await screen.findByText("No page at this address")).toBeInTheDocument();
    // The rail must survive: a dead URL is the moment an operator most needs
    // somewhere to go.
    const rail = await screen.findByRole("navigation", { name: /primary/i });
    expect(within(rail).getByRole("link", { name: "Overview" })).toBeInTheDocument();
  });

});

/**
 * The Studio is the one route mounted INSIDE the session gate and OUTSIDE the
 * shell. Both halves are asserted here, against the real <App/>, because both
 * are properties of the route TABLE and nothing else can see them: the Studio's
 * own suite renders the component standalone, where there is no shell to be
 * outside of and no gate to be inside.
 */
describe("App — the Studio is full-screen, and still behind the gate", () => {
  it("paints without the nav rail", async () => {
    server.use(
      http.get("*/api/v1/casts/:id", () =>
        HttpResponse.json(
          {
            id: "01J8Z3K4N5P6Q7R8S9T0V1W2X3",
            scope_node: "01J8Z3K4N5P6Q7R8S9T0V1W2P0",
            name: "Lobby loop",
            slides: [{ id: "s1", layers: [{ kind: "rect", x: 0, y: 0, w: 1920, h: 1080, color: "#101020" }] }],
            labels: {},
            revision: 1,
            created_at: 0,
            updated_at: 0,
          },
          { headers: { ETag: '"1"' } },
        ),
      ),
      http.get("*/api/v1/content", () => HttpResponse.json({ content: [] })),
      http.get("*/api/v1/entities", empty),
    );
    window.history.pushState({}, "", "/studio?id=01J8Z3K4N5P6Q7R8S9T0V1W2X3");
    render(<App />);

    // The editor is up…
    expect(await screen.findByRole("menubar", { name: "Studio menu" })).toBeInTheDocument();
    // …and the shell is not. Asserted on the rail rather than on a class name,
    // because the rail is the thing that was eating the canvas's width — and it
    // is the same landmark every other route in this file asserts is PRESENT, so
    // this cannot pass by querying for something that never existed.
    expect(screen.queryByRole("navigation", { name: /primary/i })).not.toBeInTheDocument();
    // The way OUT is the editor's own, since the rail's is gone.
    expect(screen.getByRole("button", { name: "Back to casts" })).toBeInTheDocument();
  });

  it("still sends an unauthenticated visitor to sign in", async () => {
    // Outside the SHELL is not outside the GATE. /setup is genuinely ungated
    // (nothing has claimed the box yet); the Studio edits a resource behind
    // SEC-005 and must not paint for an anonymous caller.
    server.use(http.get("*/api/v1/auth/session", () => new HttpResponse(null, { status: 401 })));
    window.history.pushState({}, "", "/studio?id=01J8Z3K4N5P6Q7R8S9T0V1W2X3");
    render(<App />);

    expect(await screen.findByRole("button", { name: /sign in/i })).toBeInTheDocument();
    expect(screen.queryByRole("menubar", { name: "Studio menu" })).not.toBeInTheDocument();
  });
});
