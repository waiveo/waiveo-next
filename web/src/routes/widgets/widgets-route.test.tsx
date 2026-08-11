import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { DERIVE_KINDS, LAYER_KINDS } from "@/api";
import { cast, problem } from "@/api/test-support";
import WidgetsRoute, { CATALOG_USAGE_KEYS, countCastUsage } from "./widgets-route";

/**
 * The /widgets route — legacy had a Widgets area under Slidecast and this
 * console had none (parity row 8.4): the kinds existed only as an insert menu
 * inside the Studio, reachable only by first opening a cast.
 *
 * The cases below hold the two things that make it a page rather than a poster:
 * it describes EVERY kind the wire accepts (derived from the kind lists, so a
 * new kind cannot be quietly left off), and it says which of them this box is
 * actually running.
 */

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

const page = (items: unknown[]) => HttpResponse.json({ items, cursor: null });

function renderWidgets(casts: unknown[] = []) {
  server.use(http.get("*/api/v1/casts", () => page(casts)));
  return render(
    <ThemeProvider>
      <MemoryRouter>
        <WidgetsRoute />
      </MemoryRouter>
    </ThemeProvider>,
  );
}

/** The card for one catalog entry, addressed by the key usage is counted on. */
const card = (usageKey: string) =>
  document.querySelector(`[data-slot="widget-card"][data-usage-key="${usageKey}"]`) as HTMLElement;

describe("Widgets route", () => {
  it("describes EVERY layer kind the wire accepts", async () => {
    // Derived from LAYER_KINDS/DERIVE_KINDS rather than listed, because the
    // failure being prevented is silent: a thirteenth kind added to the wire and
    // to the Studio would leave this page rendering eleven cards and looking
    // perfectly correct. `derive` is expanded into its three specs — those are
    // what an operator picks and what the page shows.
    renderWidgets();
    await screen.findByRole("heading", { name: "Live data" });

    const expected = LAYER_KINDS.flatMap((kind) =>
      kind === "derive" ? DERIVE_KINDS.map((d) => `derive:${d}`) : [kind],
    );
    expect([...CATALOG_USAGE_KEYS].sort()).toEqual([...expected].sort());
    for (const key of expected) {
      expect(card(key)).not.toBeNull();
    }
  });

  it("groups the catalog by where a value comes from, live data first", async () => {
    renderWidgets();
    const headings = (await screen.findAllByRole("heading", { level: 2 })).map((h) =>
      h.textContent?.trim(),
    );
    expect(headings).toEqual(["Live data", "Interactive", "Static", "Rasterized"]);

    // The distinction the page exists to make: a Weather card says where the
    // value comes from, not merely that a Weather layer exists.
    expect(within(card("weather")).getByText(/fetched by the box/i)).toBeInTheDocument();
    expect(within(card("entity")).getByText(/live state of one device/i)).toBeInTheDocument();
    // ...and a rasterized card carries its cost warning.
    expect(within(card("derive:qr")).getByText(/No screen can draw one itself/i)).toBeInTheDocument();
  });

  it("says which widgets this box is actually running, counted per cast", async () => {
    renderWidgets([
      // The fixture cast carries rect + text + clock.
      cast({ id: "01J8Z0AAAA0000000000000001" }),
      cast({
        id: "01J8Z0AAAA0000000000000002",
        slides: [
          {
            id: "s1",
            duration_ms: 5_000,
            layers: [
              { kind: "clock", x: 0, y: 0, w: 100, h: 100 },
              // Twice in ONE cast — usage counts casts, not layers, because
              // "used in 3 casts" is what decides whether a change matters.
              { kind: "clock", x: 0, y: 0, w: 100, h: 100 },
              { kind: "weather", x: 0, y: 0, w: 100, h: 100, text: "{temp}°" },
              { kind: "derive", x: 0, y: 0, w: 100, h: 100, derive: { kind: "qr", data: "x" } },
            ],
          },
        ],
      }),
    ]);

    expect(await within(card("clock")).findByText("Used in 2 casts")).toBeInTheDocument();
    expect(within(card("weather")).getByText("Used in 1 cast")).toBeInTheDocument();
    // A derive layer counts under its SPEC kind, not under the shared `derive`
    // layer kind — the qr card is the one an operator would look at.
    expect(within(card("derive:qr")).getByText("Used in 1 cast")).toBeInTheDocument();
    expect(within(card("derive:text")).getByText("Not used yet")).toBeInTheDocument();
    expect(within(card("countdown")).getByText("Not used yet")).toBeInTheDocument();
  });

  it("still renders the whole catalog when the cast library is unreachable", async () => {
    // The catalog IS the page; the counts are an enrichment. A 403 on the cast
    // list must never blank it — that is the failure mode this page replaces
    // (an operator with no way to find out what a widget is).
    server.use(
      http.get("*/api/v1/casts", () =>
        problem(403, "FORBIDDEN", "You do not have access to this scope."),
      ),
    );
    render(
      <ThemeProvider>
        <MemoryRouter>
          <WidgetsRoute />
        </MemoryRouter>
      </ThemeProvider>,
    );

    expect(await screen.findByText(/Usage counts are unavailable/i)).toHaveTextContent(
      /You do not have access to this scope/i,
    );
    expect(card("weather")).not.toBeNull();
    expect(within(card("weather")).getByText(/fetched by the box/i)).toBeInTheDocument();
    // And no card claims a count it does not have.
    expect(screen.queryByText(/Used in/)).toBeNull();
    expect(screen.queryByText("Not used yet")).toBeNull();
  });

  it("sends an operator to the cast library, because a widget lives on a slide", async () => {
    // The one action on the page has to be real. A widget has no standalone
    // identity here — it is a layer on a slide — so the honest next step is the
    // library, and this asserts the link actually points there.
    renderWidgets();
    const link = await screen.findByRole("link", { name: /open the cast library/i });
    expect(link).toHaveAttribute("href", "/casts");
  });
});

describe("countCastUsage", () => {
  it("counts a kind once per cast however many layers carry it", () => {
    const counts = countCastUsage([
      cast({
        slides: [
          { id: "a", layers: [{ kind: "clock" }, { kind: "clock" }] },
          { id: "b", layers: [{ kind: "clock" }] },
        ],
      }),
    ] as never);
    expect(counts.clock).toBe(1);
  });

  it("tolerates a cast with no slides and a slide with no layers", () => {
    // Both members are optional on the wire model, and a console page that threw
    // on an empty cast would break on the most ordinary row there is: one just
    // created.
    const counts = countCastUsage([
      cast({ slides: [] }),
      cast({ slides: [{ id: "a" }] }),
    ] as never);
    expect(counts).toEqual({});
  });
});
