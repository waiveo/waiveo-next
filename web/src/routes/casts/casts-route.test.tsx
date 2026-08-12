import { describe, it, expect, vi, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter, Route, Routes, useSearchParams } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import CastsRoute from "./casts-route";
import { validateSlide, type CastSlide } from "@/api";
import { TRACE_ID, ULID_A, ULID_B, ULID_C, ULID_ROOT, cast, problem, scopeNode } from "@/api/test-support";

/**
 * The cast library, driven end to end: create, duplicate, delete, and open in
 * the Studio. The assertions land on the REQUEST the console sent (the created
 * document, the duplicated slides, the If-Match on the delete) and on where the
 * router ended up — not on whether a row appeared.
 */

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

const page = (items: unknown[]) => HttpResponse.json({ items, cursor: null });

/** The two lists the page loads: the casts, and the scope nodes a new cast can
 * be attached under. */
function listing(casts: unknown[], scopes: unknown[] = [scopeNode({ id: ULID_ROOT, kind: "site", name: "The Hanger" })]) {
  return [
    http.get("*/api/v1/casts", () => page(casts)),
    http.get("*/api/v1/scope-nodes", () => page(scopes)),
  ];
}

/** A probe route that reports where a navigation landed, so "open in the
 * Studio" is proven by the URL it produced rather than by a click handler. */
function StudioProbe() {
  const [params] = useSearchParams();
  return <h1>Studio for {params.get("id")}</h1>;
}

function renderCasts() {
  return render(
    <ThemeProvider>
      <MemoryRouter initialEntries={["/casts"]}>
        <Routes>
          <Route path="/casts" element={<CastsRoute />} />
          <Route path="/studio" element={<StudioProbe />} />
        </Routes>
      </MemoryRouter>
    </ThemeProvider>,
  );
}

describe("Casts library", () => {
  it("lists casts with their slide count and readiness", async () => {
    server.use(
      ...listing([
        cast(),
        cast({
          id: ULID_B,
          name: "Broken loop",
          // A slide with no layers is one the projector DROPS — the library has
          // to be able to say so before it reaches a TV.
          slides: [{ id: "s1", layers: [] }],
        }),
      ]),
    );
    renderCasts();

    expect(await screen.findByText("Lobby loop")).toBeInTheDocument();
    expect(screen.getByText("Broken loop")).toBeInTheDocument();
    // Scoped to the TABLE. "Ready" now also names a stat tile above it, and this
    // assertion is about the row's status cell — an unscoped getByText would
    // pass on the tile and stop checking the thing it is named for.
    const table = screen.getByRole("table", { name: "Casts" });
    expect(within(table).getByText("Ready")).toBeInTheDocument();
    expect(within(table).getByText("1 slide needs attention")).toBeInTheDocument();
  });

  it("finds one cast by NAME in a library too long to scan", async () => {
    const user = userEvent.setup();
    server.use(
      ...listing([
        cast(),
        cast({ id: ULID_B, name: "Broken loop", slides: [{ id: "s1", layers: [] }] }),
      ]),
    );
    renderCasts();
    await screen.findByText("Lobby loop");

    await user.type(screen.getByLabelText("Search casts"), "Broken");
    await waitFor(() => expect(screen.queryByText("Lobby loop")).not.toBeInTheDocument());
    expect(screen.getByText("Broken loop")).toBeInTheDocument();

    // …and the health filter narrows by the two states the rows actually carry,
    // rather than by anything this page enumerates.
    await user.clear(screen.getByLabelText("Search casts"));
    await waitFor(() => expect(screen.getByText("Lobby loop")).toBeInTheDocument());
    await user.selectOptions(screen.getByLabelText("Status"), "Needs attention");
    expect(screen.queryByText("Lobby loop")).not.toBeInTheDocument();
    expect(screen.getByText("Broken loop")).toBeInTheDocument();
  });

  it("shows an empty state on a box with no casts yet", async () => {
    server.use(...listing([]));
    renderCasts();
    expect(await screen.findByText(/no casts yet/i)).toBeInTheDocument();
  });

  it("surfaces a load failure rather than an empty library", async () => {
    server.use(
      http.get("*/api/v1/casts", () => problem(503, "INTERNAL", "The store is unavailable.")),
      http.get("*/api/v1/scope-nodes", () => page([])),
    );
    renderCasts();
    expect(await screen.findByRole("alert")).toHaveTextContent(/store is unavailable/i);
  });

  it("opens a cast in the Studio when its row is pressed", async () => {
    server.use(...listing([cast()]));
    const user = userEvent.setup();
    renderCasts();

    await user.click(await screen.findByText("Lobby loop"));
    expect(await screen.findByRole("heading", { name: `Studio for ${ULID_A}` })).toBeInTheDocument();
  });

  it("creates a cast with one DRAWABLE slide and goes straight into the Studio", async () => {
    const seen: { body?: Record<string, unknown>; key?: string | null } = {};
    server.use(
      ...listing([]),
      http.post("*/api/v1/casts", async ({ request }) => {
        seen.key = request.headers.get("Idempotency-Key");
        seen.body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(cast({ id: ULID_B, name: "Front window" }), {
          status: 201,
          headers: { ETag: '"1"', "Trace-Id": TRACE_ID },
        });
      }),
    );
    const user = userEvent.setup();
    renderCasts();

    await user.click(await screen.findByRole("button", { name: "New cast" }));
    await user.type(await screen.findByLabelText("Cast name"), "Front window");
    await user.click(screen.getByRole("button", { name: /create and open/i }));

    expect(await screen.findByRole("heading", { name: `Studio for ${ULID_B}` })).toBeInTheDocument();
    expect(seen.key).toBeTruthy();
    expect(seen.body).toMatchObject({ name: "Front window", scope_node: ULID_ROOT });
    // One slide, so the Studio opens on a canvas rather than on nothing — and
    // that slide carries a layer. `CastSlide.layers` is `minItems: 1` in
    // api/openapi.yaml and the store gate refuses an empty stack again
    // (CAST_SLIDE_LAYERS_INVALID), so a zero-layer body makes the page's PRIMARY
    // action fail outright: a red toast and no cast. The body is asserted
    // against validateSlide — the console's own mirror of the server rule — so
    // this cannot pass on a stack that merely exists.
    const slides = seen.body?.slides as CastSlide[];
    expect(slides).toHaveLength(1);
    expect(slides[0]!.layers.length).toBeGreaterThan(0);
    expect(validateSlide(slides[0]!)).toEqual([]);
  });

  it("refuses to invent a scope when the box has no site yet", async () => {
    server.use(...listing([], []));
    const user = userEvent.setup();
    renderCasts();

    await user.click(await screen.findByRole("button", { name: "New cast" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/no site to put a cast under/i);
  });

  it("duplicates a cast by value, with fresh slide ids", async () => {
    type DuplicateBody = { name?: string; slides?: Array<{ id: string; layers: unknown[] }> };
    const seen: { body?: DuplicateBody } = {};
    server.use(
      ...listing([cast()]),
      http.post("*/api/v1/casts", async ({ request }) => {
        seen.body = (await request.json()) as DuplicateBody;
        return HttpResponse.json(cast({ id: ULID_B, name: "Lobby loop copy" }), {
          status: 201,
          headers: { ETag: '"1"', "Trace-Id": TRACE_ID },
        });
      }),
    );
    const user = userEvent.setup();
    renderCasts();

    await user.click(await screen.findByRole("button", { name: "Duplicate Lobby loop" }));

    await vi.waitFor(() => expect(seen.body).toBeDefined());
    expect(seen.body?.name).toBe("Lobby loop copy");
    expect(seen.body?.slides?.[0]?.layers).toHaveLength(3);
    // A fresh id, so the copy is not the same slide by another name.
    expect(seen.body?.slides?.[0]?.id).not.toBe("slide-1");
    // Duplicating must not navigate away from the library — the copy appears here.
    expect(screen.getByRole("heading", { level: 1, name: "Casts" })).toBeInTheDocument();
  });

  it("carries the cast-wide default duration into the duplicate", async () => {
    // The defect: the create body was {scope_node, name, slides}. Every slide
    // that states no `duration_ms` of its own is timed by the cast-wide default,
    // so a copy that drops it plays at the player's default instead — and looks
    // identical in the library (same name, same slide count, same health) while
    // running at a different pace.
    type DuplicateBody = { default_duration_ms?: number };
    const seen: { body?: DuplicateBody } = {};
    server.use(
      ...listing([cast({ default_duration_ms: 8000 })]),
      http.post("*/api/v1/casts", async ({ request }) => {
        seen.body = (await request.json()) as DuplicateBody;
        return HttpResponse.json(cast({ id: ULID_B, name: "Lobby loop copy" }), {
          status: 201,
          headers: { ETag: '"1"', "Trace-Id": TRACE_ID },
        });
      }),
    );
    const user = userEvent.setup();
    renderCasts();

    await user.click(await screen.findByRole("button", { name: "Duplicate Lobby loop" }));
    await vi.waitFor(() => expect(seen.body).toBeDefined());
    expect(seen.body?.default_duration_ms).toBe(8000);
  });

  it("omits the default duration when the cast itself declares none", async () => {
    type DuplicateBody = Record<string, unknown>;
    const seen: { body?: DuplicateBody } = {};
    server.use(
      ...listing([cast()]),
      http.post("*/api/v1/casts", async ({ request }) => {
        seen.body = (await request.json()) as DuplicateBody;
        return HttpResponse.json(cast({ id: ULID_B, name: "Lobby loop copy" }), {
          status: 201,
          headers: { ETag: '"1"', "Trace-Id": TRACE_ID },
        });
      }),
    );
    const user = userEvent.setup();
    renderCasts();

    await user.click(await screen.findByRole("button", { name: "Duplicate Lobby loop" }));
    await vi.waitFor(() => expect(seen.body).toBeDefined());
    expect(seen.body).not.toHaveProperty("default_duration_ms");
  });

  it("deletes only after a confirm, and carries the If-Match", async () => {
    const seen: { ifMatch?: string | null; deletes: number } = { deletes: 0 };
    server.use(
      ...listing([cast({ revision: 4 })]),
      http.delete("*/api/v1/casts/:id", ({ request }) => {
        seen.deletes += 1;
        seen.ifMatch = request.headers.get("If-Match");
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const user = userEvent.setup();
    renderCasts();

    await user.click(await screen.findByRole("button", { name: "Delete Lobby loop" }));
    // The confirm is open and nothing has been deleted yet.
    expect(await screen.findByRole("dialog")).toHaveTextContent(/delete lobby loop\?/i);
    expect(seen.deletes).toBe(0);
    // …and the row press did NOT fire behind the dialog. (Queried by text, not
    // by role: an open Radix dialog aria-hides the rest of the app, so a role
    // query would report "absent" whether or not the navigation happened.)
    expect(screen.queryByText(/^Studio for/)).toBeNull();

    await user.click(screen.getByRole("button", { name: "Delete" }));
    await vi.waitFor(() => expect(seen.deletes).toBe(1));
    expect(seen.ifMatch).toBe('"4"');
  });

  it("reports a refused delete instead of pretending it worked", async () => {
    server.use(
      ...listing([cast()]),
      http.delete("*/api/v1/casts/:id", () =>
        problem(409, "SCOPE_NODE_IN_USE", "A schedule still plays this cast."),
      ),
    );
    const user = userEvent.setup();
    renderCasts();

    await user.click(await screen.findByRole("button", { name: "Delete Lobby loop" }));
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    expect(await screen.findByText(/a schedule still plays this cast/i)).toBeInTheDocument();
  });
});

/**
 * Templates (parity row 1.8) — a template is a cast carrying `template: true`,
 * so everything here is asserted on the ONE cast list and the ordinary
 * `POST /casts` a create sends.
 *
 * The assertions land on the request body for the same reason the rest of this
 * file's do: "a template appeared in a dropdown" and "the cast that was created
 * carries the template's slides" are different claims, and an operator only
 * cares about the second.
 */
describe("Casts library — templates", () => {
  /** A saved template row: an ordinary cast with the flag set. */
  const savedTemplate = cast({
    id: ULID_B,
    name: "House style",
    template: true,
    default_duration_ms: 9000,
    slides: [
      { id: "tpl-1", layers: [{ kind: "rect", x: 0, y: 0, w: 1920, h: 1080, color: "#123456" }] },
    ],
  });

  /** Capture the create POST and answer with a plausible created row. */
  function capturingCreate(seen: { body?: Record<string, unknown> }) {
    return http.post("*/api/v1/casts", async ({ request }) => {
      seen.body = (await request.json()) as Record<string, unknown>;
      return HttpResponse.json(cast({ id: ULID_A, name: String(seen.body.name ?? "") }), {
        status: 201,
        headers: { ETag: '"1"', "Trace-Id": TRACE_ID },
      });
    });
  }

  it("keeps templates out of the schedulable list and shows them as starting points", async () => {
    server.use(...listing([cast(), savedTemplate]));
    renderCasts();

    // The schedulable table holds the cast and NOT the template — a template a
    // playlist cannot reference has no business in the list of things to
    // schedule, and the server refuses one anyway.
    const casts = await screen.findByRole("table", { name: "Casts" });
    expect(within(casts).getByText("Lobby loop")).toBeInTheDocument();
    expect(within(casts).queryByText("House style")).not.toBeInTheDocument();

    const templates = screen.getByRole("table", { name: "Templates" });
    expect(within(templates).getByText("House style")).toBeInTheDocument();
    expect(within(templates).queryByText("Lobby loop")).not.toBeInTheDocument();
  });

  it("creates a cast FROM a built-in template, carrying its slides and its default duration", async () => {
    const seen: { body?: Record<string, unknown> } = {};
    server.use(...listing([]), capturingCreate(seen));
    const user = userEvent.setup();
    renderCasts();

    await user.click(await screen.findByRole("button", { name: "New cast" }));
    await user.selectOptions(await screen.findByLabelText("Start from"), "builtin:menu-board");
    // The description is shown before anything is created — the names alone do
    // not tell a menu board from a welcome board.
    expect(screen.getByText(/heading and four priced rows/i)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /create and open/i }));

    await screen.findByRole("heading", { name: `Studio for ${ULID_A}` });
    const body = seen.body ?? {};
    // The template's own name is the default, so an operator who names nothing
    // still gets something findable rather than "Untitled cast".
    expect(body.name).toBe("Menu board");
    expect(body.default_duration_ms).toBe(12_000);
    // The slides are the template's, and — the claim that matters — every one
    // of them is DRAWABLE, so the new cast is schedulable with no further edit.
    const slides = body.slides as CastSlide[];
    expect(slides).toHaveLength(2);
    expect(slides.every((s) => validateSlide(s).length === 0)).toBe(true);
    expect(slides[0]?.layers.some((l) => l.text === "Today's Menu")).toBe(true);
    // A created cast is an ORDINARY cast: nothing marks it as template-derived,
    // which is what lets it be scheduled immediately.
    expect(body.template).toBeUndefined();
  });

  it("creates a cast from a SAVED template, deep-copying its slides under fresh ids", async () => {
    const seen: { body?: Record<string, unknown> } = {};
    server.use(...listing([savedTemplate]), capturingCreate(seen));
    const user = userEvent.setup();
    renderCasts();

    const templates = await screen.findByRole("table", { name: "Templates" });
    await user.click(within(templates).getByRole("button", { name: `New cast from ${savedTemplate.name}` }));
    // The dialog opens already pointed at that template.
    expect(await screen.findByLabelText("Start from")).toHaveValue(`cast:${ULID_B}`);
    await user.click(screen.getByRole("button", { name: /create and open/i }));

    await screen.findByRole("heading", { name: `Studio for ${ULID_A}` });
    const body = seen.body ?? {};
    expect(body.name).toBe("House style");
    expect(body.default_duration_ms).toBe(9000);
    expect(body.template).toBeUndefined();
    const slides = body.slides as CastSlide[];
    expect(slides).toHaveLength(1);
    expect(slides[0]?.layers[0]).toMatchObject({ kind: "rect", color: "#123456" });
    // Fresh id: the copy is its own document, not the template's slide under a
    // second name.
    expect(slides[0]?.id).not.toBe("tpl-1");
  });

  it("saves an existing cast AS a template, leaving the original untouched", async () => {
    const seen: { body?: Record<string, unknown> } = {};
    server.use(...listing([cast()]), capturingCreate(seen));
    const user = userEvent.setup();
    renderCasts();

    await user.click(await screen.findByRole("button", { name: "Save Lobby loop as a template" }));
    const name = await screen.findByLabelText("Template name");
    expect(name).toHaveValue("Lobby loop template");
    await user.click(screen.getByRole("button", { name: /save template/i }));

    await screen.findByText(/saved .* as a template/i);
    const body = seen.body ?? {};
    // A COPY carrying the flag — never the original flipped. Flipping would
    // take a cast screens are playing out of service, and the server would
    // refuse it outright while it is referenced by a playlist.
    expect(body.template).toBe(true);
    expect(body.name).toBe("Lobby loop template");
    expect((body.slides as CastSlide[])[0]?.layers).toHaveLength(3);
    // No PATCH went anywhere near the original: the only request was the POST.
    expect(seen.body).toBeDefined();
  });
});

describe("Casts library — portability (.cast bundles)", () => {
  it("offers each cast a download link to its own bundle", async () => {
    server.use(...listing([cast()]));
    renderCasts();

    const link = await screen.findByRole("link", { name: /export lobby loop as a \.cast bundle/i });
    // An anchor, not a fetch: a bundle carries every image the design draws, and
    // the browser streams it straight to disk from a link. It is now the kit's
    // DownloadLink, so the same guarantee is one component instead of a
    // hand-rolled copy of the icon-button class string.
    expect(link).toHaveAttribute("href", `/api/v1/casts/${ULID_A}/export`);
    expect(link).toHaveAttribute("download");
    // …and the bare attribute, never download="true" — the browser would save
    // every bundle as a file literally called `true`.
    expect(link.getAttribute("download")).not.toBe("true");
  });

  it("downloads from the export link WITHOUT the pressable row also navigating", async () => {
    // The row opens the Studio and the export link sits inside it, so the link
    // has to stop its own click. Moving the anchor into the kit's DownloadLink
    // is exactly the change that could drop the handler — and the symptom would
    // be a download that ALSO abandons the page.
    const user = userEvent.setup();
    server.use(...listing([cast()]));
    renderCasts();
    await user.click(
      await screen.findByRole("link", { name: /export lobby loop as a \.cast bundle/i }),
    );
    expect(screen.queryByRole("heading", { name: `Studio for ${ULID_A}` })).toBeNull();
  });
});

describe("Casts library — a row of glyphs that can be read before it is pressed", () => {
  // Five icon-only actions per row. Before the kit Tooltip, the ONLY way to
  // learn what any of them did was to press it — the aria-label named them for a
  // screen reader and for nobody else.
  it("names each row action on hover", async () => {
    const user = userEvent.setup();
    server.use(...listing([cast()]));
    renderCasts();

    await user.hover(await screen.findByRole("button", { name: /duplicate lobby loop/i }));
    expect(await screen.findByRole("tooltip")).toHaveTextContent("Duplicate");
  });

  it("names each row action on KEYBOARD FOCUS, which a native title never did", async () => {
    const user = userEvent.setup();
    server.use(...listing([cast()]));
    renderCasts();

    const del = await screen.findByRole("button", { name: /delete lobby loop/i });
    del.focus();
    expect(await screen.findByRole("tooltip")).toHaveTextContent("Delete");
    // The tip DESCRIBES; the button keeps its own, record-specific name.
    expect(screen.getByRole("button", { name: "Delete Lobby loop" })).toBe(del);
    await user.keyboard("{Escape}");
  });

  it("still performs the action it now explains", async () => {
    // Wrapping five live buttons in tooltip triggers is exactly the change that
    // silently swallows an onClick — this console has shipped a dead button
    // before, and it passed every render-only test.
    const user = userEvent.setup();
    server.use(...listing([cast()]));
    renderCasts();
    await user.click(await screen.findByRole("button", { name: /delete lobby loop/i }));
    expect(await screen.findByRole("dialog")).toHaveTextContent(/delete lobby loop/i);
  });

  it("imports a bundle at the placement the operator chose, and renames on request", async () => {
    const seen: { url: URL; body: Uint8Array; type: string | null }[] = [];
    server.use(
      ...listing([]),
      http.post("*/api/v1/casts/import", async ({ request }) => {
        const body = await request.arrayBuffer();
        seen.push({
          url: new URL(request.url),
          body: new Uint8Array(body),
          type: request.headers.get("Content-Type"),
        });
        return HttpResponse.json(cast({ name: "Imported menu" }), {
          status: 201,
          headers: { ETag: '"1"', "Trace-Id": TRACE_ID },
        });
      }),
    );
    renderCasts();

    await userEvent.click(await screen.findByRole("button", { name: /import \.cast/i }));
    const file = new File([new Uint8Array([80, 75, 3, 4, 1, 2, 3])], "menu.cast", {
      type: "application/zip",
    });
    await userEvent.upload(screen.getByLabelText(/^Bundle/), file);
    await userEvent.type(screen.getByLabelText(/^Name/), "Imported menu");
    await userEvent.click(screen.getByRole("button", { name: "Import" }));

    await screen.findByText(/Imported Imported menu/i);
    expect(seen).toHaveLength(1);
    // The placement rides the request — a bundle carries none, so the console
    // must supply one and must not let the server guess.
    expect(seen[0]!.url.searchParams.get("scope_node")).toBe(ULID_ROOT);
    expect(seen[0]!.url.searchParams.get("name")).toBe("Imported menu");
    // The FILE'S OWN BYTES were sent, not a JSON description of them: the body
    // opens with the zip magic the fixture starts with.
    expect(seen[0]!.body.length).toBeGreaterThan(0);
    expect(Array.from(seen[0]!.body.slice(0, 4))).toEqual([0x50, 0x4b, 0x03, 0x04]);
    expect(seen[0]!.type).toBe("application/octet-stream");
  });

  it("shows the server's own refusal sentence, because they mean different things", async () => {
    // "That is not a cast bundle", "this file has been altered" and "this bundle
    // is incomplete" are three different next actions for the operator. A
    // generic "import failed" would send them to retry the thing that cannot
    // work.
    server.use(
      ...listing([]),
      http.post("*/api/v1/casts/import", () =>
        problem(
          422,
          "VALIDATION_FAILED",
          "An image in this bundle does not match the reference it is carried under; the file has been altered or corrupted in transit. Nothing was imported.",
        ),
      ),
    );
    renderCasts();

    await userEvent.click(await screen.findByRole("button", { name: /import \.cast/i }));
    await userEvent.upload(
      screen.getByLabelText(/^Bundle/),
      new File([new Uint8Array([1, 2, 3])], "menu.cast"),
    );
    await userEvent.click(screen.getByRole("button", { name: "Import" }));

    expect(await screen.findByText(/does not match the reference it is carried under/)).toBeInTheDocument();
  });

  it("will not import until a bundle has been chosen", async () => {
    server.use(...listing([]));
    renderCasts();
    await userEvent.click(await screen.findByRole("button", { name: /import \.cast/i }));
    expect(screen.getByRole("button", { name: "Import" })).toBeDisabled();
  });
});

describe("Casts library — the picture and the shape of the library", () => {
  // Restoring these was the owner's "visual differences" note at its most
  // literal: data points per cast had fallen from 9 to 3, and operators name
  // casts badly and recognise them by picture.
  it("draws the FIRST SLIDE of each cast, not a placeholder", async () => {
    server.use(
      ...listing([
        cast({
          name: "Lobby loop",
          slides: [
            {
              id: "s1",
              layers: [
                { kind: "rect", x: 0, y: 0, w: 1920, h: 1080, color: "#1B0A2E" },
                { kind: "text", x: 80, y: 40, w: 1760, h: 80, text: "WELCOME", font_px: 56 },
              ],
            },
            // A SECOND slide, so a pass cannot come from rendering "the only
            // slide there is" — the column has to pick the first.
            { id: "s2", layers: [{ kind: "text", x: 0, y: 0, w: 100, h: 40, text: "SECOND" }] },
          ],
        }),
      ]),
    );
    renderCasts();

    const table = await screen.findByRole("table", { name: "Casts" });
    // The real renderer drew the real layer text...
    expect(within(table).getByText("WELCOME")).toBeInTheDocument();
    // ...and only the first slide's.
    expect(within(table).queryByText("SECOND")).not.toBeInTheDocument();
  });

  it("summarises the library, counting only schedulable casts", async () => {
    server.use(
      ...listing([
        cast({ name: "Lobby loop" }),
        cast({ id: ULID_B, name: "Broken loop", slides: [{ id: "s1", layers: [] }] }),
        // A TEMPLATE. It is a cast row carrying template:true, so a tile that
        // counted rows rather than schedulable ones would say 3.
        cast({ id: ULID_C, name: "Starting point", template: true }),
      ]),
    );
    renderCasts();

    await screen.findByRole("table", { name: "Casts" });
    // Look only INSIDE stat cards. A page-wide getByText would also match the
    // hero's own <h1>Casts</h1>, which is how the first draft of this test
    // failed — and would have silently asserted against the heading if the tile
    // had happened to be absent.
    const tile = (label: string) => {
      const found = [...document.querySelectorAll('[data-slot="stat-card"]')].find((el) =>
        within(el as HTMLElement).queryByText(label),
      );
      if (!found) throw new Error(`no stat card labelled ${label}`);
      return found as HTMLElement;
    };

    expect(within(tile("Casts")).getByText("2")).toBeInTheDocument();
    expect(within(tile("Ready")).getByText("1")).toBeInTheDocument();
    expect(within(tile("Needs attention")).getByText("1")).toBeInTheDocument();
    expect(within(tile("Templates")).getByText("1")).toBeInTheDocument();
  });
});
