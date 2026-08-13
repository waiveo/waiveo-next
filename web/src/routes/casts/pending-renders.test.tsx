import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { PendingRenders, locate } from "./pending-renders";
import { createApi, type DerivePendingLayer } from "@/api";

// The library-wide render queue. `GET /derive/pending` has existed with NO
// consumer anywhere in the console: per-layer status was legible only inside the
// Studio, for the one cast already open, so finding every unrendered or stale
// layer meant opening every cast one at a time or curling the endpoint.

const TEST_BASE = "http://api.test/api/v1";
const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

/** A pending-render fixture.
 *
 * An override of `undefined` OMITS the member rather than setting it to
 * undefined — which is both what the wire actually does (an absent optional is
 * absent, not null) and what `exactOptionalPropertyTypes` requires. Writing
 * `slide_id: undefined` to mean "a playlist job has no slide id" is the natural
 * thing to reach for and does not typecheck without this. */
function job(over: Record<string, unknown> = {}): DerivePendingLayer {
  const base: Record<string, unknown> = {
    source: "cast",
    resource_id: "01J8ZCAST00000000000000001",
    resource_name: "Lobby loop",
    slide_id: "s1",
    layer_index: 0,
    state: "pending",
    spec_digest: "sha256:abc",
    w: 640,
    h: 360,
    spec: { kind: "text", text: "Hello" },
    ...over,
  };
  for (const [k, v] of Object.entries(base)) if (v === undefined) delete base[k];
  return base as DerivePendingLayer;
}

function seed(jobs: DerivePendingLayer[]) {
  server.use(
    http.get(`${TEST_BASE}/derive/pending`, () =>
      HttpResponse.json({ derive_jobs: jobs }, { headers: { "Trace-Id": "t" } }),
    ),
  );
}

function renderPanel() {
  render(
    <ThemeProvider>
      <PendingRenders api={createApi({ baseUrl: TEST_BASE })} />
    </ThemeProvider>,
  );
}

describe("locate — exactly one of the two locators is present", () => {
  // A cast job is located by its slide's document-local id; a playlist job by
  // the index of the item whose INLINE slide carries the layer, because an
  // inline slide has no id of its own.
  it("names a cast job by slide and a playlist job by item index", () => {
    expect(locate(job({ slide_id: "intro", layer_index: 2 }))).toBe("slide intro, layer 2");
    expect(locate(job({ source: "playlist", slide_id: undefined, item_index: 3, layer_index: 0 }))).toBe(
      "item 3, layer 0",
    );
  });

  // Item index 0 is a real position. A truthiness test would render it as
  // "unidentified", which is the classic off-by-falsy on a zero index.
  it("treats item index 0 as a position, not as absent", () => {
    expect(locate(job({ source: "playlist", slide_id: undefined, item_index: 0, layer_index: 1 }))).toBe(
      "item 0, layer 1",
    );
  });

  // A row missing its locator renders as words rather than as "undefined". The
  // schema says one is always present, so this is the malformed-row fallback.
  it("degrades to words when the locator the source promises is missing", () => {
    expect(locate(job({ slide_id: undefined }))).toBe("slide (unidentified), layer 0");
    expect(locate(job({ source: "playlist", slide_id: undefined, item_index: undefined }))).toBe(
      "item (unidentified), layer 0",
    );
  });
});

describe("Waiting to be rendered", () => {
  it("lists every outstanding layer, across casts AND playlists", async () => {
    seed([
      job(),
      job({
        source: "playlist",
        resource_id: "01J8ZPLIST0000000000000001",
        resource_name: "Morning rotation",
        slide_id: undefined,
        item_index: 2,
        layer_index: 1,
        state: "stale",
        w: 1920,
        h: 1080,
      }),
    ]);
    renderPanel();

    const table = await screen.findByRole("table", { name: "Waiting to be rendered" });
    expect(within(table).getByText("Lobby loop")).toBeInTheDocument();
    expect(within(table).getByText("slide s1, layer 0")).toBeInTheDocument();
    // Both authored shapes that carry a layer stack reach a screen, so both are
    // listed — a panel that showed only casts would hide half the queue.
    expect(within(table).getByText("Morning rotation")).toBeInTheDocument();
    expect(within(table).getByText("item 2, layer 1")).toBeInTheDocument();
    expect(within(table).getByText("1920×1080")).toBeInTheDocument();
  });

  // The two states are different facts about what is on a screen RIGHT NOW, and
  // flattening them would hide the more dangerous one: a stale layer looks
  // correct and is out of date, where a pending layer is visibly absent.
  it("separates stale from pending, and counts what is currently wrong", async () => {
    seed([job({ state: "stale" }), job({ slide_id: "s2", state: "stale" }), job({ slide_id: "s3" })]);
    renderPanel();

    const table = await screen.findByRole("table", { name: "Waiting to be rendered" });
    expect(within(table).getAllByText("stale")).toHaveLength(2);
    expect(within(table).getAllByText("pending")).toHaveLength(1);
    expect(await screen.findByRole("status")).toHaveTextContent(
      "2 layers are showing an out-of-date picture right now",
    );
  });

  it("says nothing about staleness when nothing is stale", async () => {
    seed([job()]);
    renderPanel();
    await screen.findByRole("table", { name: "Waiting to be rendered" });
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  // The console cannot start a rasterization — `waiveo-derive` is a separate
  // binary and api/1 exposes no endpoint. Naming the tool is what stops the
  // queue reading as something the console is about to handle by itself.
  it("names the tool when there is work, and does not nag when there is none", async () => {
    seed([job()]);
    renderPanel();
    expect(await screen.findByText(/run waiveo-derive against this box/i)).toBeInTheDocument();

    server.resetHandlers();
    seed([]);
    renderPanel();
    expect(await screen.findAllByText(/everything is rendered/i)).not.toHaveLength(0);
  });

  it("falls back to the resource id when a job carries no name", async () => {
    seed([job({ resource_name: undefined })]);
    renderPanel();
    const table = await screen.findByRole("table", { name: "Waiting to be rendered" });
    expect(within(table).getByText("01J8ZCAST00000000000000001")).toBeInTheDocument();
  });

  it("says the queue could not be read rather than claiming everything is rendered", async () => {
    server.use(
      http.get(`${TEST_BASE}/derive/pending`, () =>
        HttpResponse.json(
          { type: "about:blank", title: "Forbidden", status: 403, code: "FORBIDDEN", detail: "not yours", trace_id: "t" },
          { status: 403, headers: { "Content-Type": "application/problem+json", "Trace-Id": "t" } },
        ),
      ),
    );
    renderPanel();

    // "Everything is rendered" would be a claim about the library; this is a
    // claim about the read, which is the only one the console can support.
    expect(await screen.findByText("The render queue could not be read")).toBeInTheDocument();
    expect(screen.queryByText(/everything is rendered/i)).not.toBeInTheDocument();
  });
});
