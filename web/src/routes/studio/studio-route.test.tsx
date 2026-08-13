import { describe, it, expect, vi, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter, Route, Routes } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import StudioRoute from "./studio-route";
import { DUPLICATE_OFFSET } from "./cast-model";
import { TRACE_ID, ULID_A, ULID_B, ULID_C, cast, contentAsset, problem } from "@/api/test-support";

/**
 * The Studio, driven the way an operator drives it: click a layer, drag it,
 * type in the properties panel, pick an image, reorder the stack, save — and
 * then assert on THE BODY THAT WENT TO THE SERVER.
 *
 * That last part is the point. This repo has been burned by UI tests that
 * asserted something rendered while the control underneath did nothing, so
 * almost every case here ends at the PATCH: the proof that a drag moved a layer
 * is that the saved slide carries the new coordinates, not that a box moved in a
 * DOM with no layout.
 *
 * Two environment facts make the drags exact. jsdom reports `clientWidth: 0`, so
 * the canvas falls back to 1:1 scale (slide-canvas.tsx says so explicitly) and a
 * 100px pointer delta IS 100 canvas pixels. And the drag is computed from the
 * box the layer had at pointer-down, so a single pointermove is a complete,
 * deterministic gesture.
 */

// The editor reads the content origin's listing ON MOUNT, not only when the
// picker is opened: a layer's fetch `url` is DERIVED, so this listing is how an
// authored `asset_ref` becomes something the canvas can draw. An EMPTY listing
// is the base handler — a case that needs bytes to resolve says so with its own
// `server.use`, which takes precedence — so a case about text layers never has
// to know about the content origin, and no case reaches the network unhandled.
const server = setupServer(
  http.get("*/api/v1/content", () => HttpResponse.json({ content: [] })),
);
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

/** What the last PATCH carried, captured for assertions. */
interface SavedBody {
  name?: string;
  default_duration_ms?: number | null;
  slides?: Array<{ id: string; duration_ms?: number; layers: Array<Record<string, unknown>> }>;
}

/** The entities the editor reads on mount for the `entity` widget's subject
 * picker. Two, with different states, so a test that picked the wrong one is
 * visible in the saved body rather than passing on a single-row list. */
const STUDIO_ENTITIES = [
  { id: ULID_B, external_id: null, device_id: ULID_C, relay_id: "relay-1", device_class: "media-player", name: "Lobby TV", scope_node: ULID_A, labels: {}, state: "on" },
  { id: ULID_C, external_id: null, device_id: ULID_C, relay_id: "relay-1", device_class: "media-player", name: "Cafe TV", scope_node: ULID_A, labels: {}, state: "off" },
];

function serveCast(body: ReturnType<typeof cast>, saved: { body?: SavedBody; ifMatch?: string | null }) {
  return [
    http.get("*/api/v1/casts/:id", () =>
      HttpResponse.json(body, { headers: { ETag: '"1"', "Trace-Id": TRACE_ID } }),
    ),
    // The editor reads the entity list on mount, for the entity widget's
    // subject picker. Stubbed for EVERY case rather than only the widget ones,
    // because `onUnhandledRequest: "error"` would otherwise turn an ordinary
    // text-layer test into a failure about the device plane.
    http.get("*/api/v1/entities", () =>
      HttpResponse.json({ items: STUDIO_ENTITIES, cursor: null }, { headers: { "Trace-Id": TRACE_ID } }),
    ),
    http.patch("*/api/v1/casts/:id", async ({ request }) => {
      saved.ifMatch = request.headers.get("If-Match");
      saved.body = (await request.json()) as SavedBody;
      return HttpResponse.json(
        { ...body, ...saved.body, revision: 2 },
        { headers: { ETag: '"2"', "Trace-Id": TRACE_ID } },
      );
    }),
  ];
}

function renderStudio(id = ULID_A) {
  return render(
    <ThemeProvider>
      <MemoryRouter initialEntries={[`/studio?id=${id}`]}>
        <Routes>
          <Route path="/studio" element={<StudioRoute />} />
          <Route path="/casts" element={<h1>Casts</h1>} />
        </Routes>
      </MemoryRouter>
    </ThemeProvider>,
  );
}

/** The canvas hit target for a layer (1-based, as the operator sees it). */
function canvasLayer(n: number, description: RegExp | string) {
  return screen.getByRole("button", {
    name: typeof description === "string" ? `Layer ${n}: ${description}` : description,
  });
}

const saveButton = () => screen.getByRole("button", { name: /save changes/i });

describe("Studio — opening a cast", () => {
  it("draws every layer of the first slide on the canvas", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    renderStudio();

    expect(await screen.findByRole("button", { name: /Layer 1: Rectangle/ })).toBeInTheDocument();
    expect(canvasLayer(2, /Layer 2: Text — Welcome/)).toBeInTheDocument();
    expect(canvasLayer(3, /Layer 3: Clock/)).toBeInTheDocument();
    // The text layer's literal is painted, not just described — twice over,
    // because the filmstrip thumbnail is the SAME renderer at a smaller scale.
    expect(screen.getAllByText("Welcome")).toHaveLength(2);
  });

  it("says so plainly when no cast is addressed", () => {
    render(
      <ThemeProvider>
        <MemoryRouter initialEntries={["/studio"]}>
          <Routes>
            <Route path="/studio" element={<StudioRoute />} />
          </Routes>
        </MemoryRouter>
      </ThemeProvider>,
    );
    expect(screen.getByRole("alert")).toHaveTextContent(/no cast selected/i);
  });

  it("surfaces a load failure instead of an empty editor", async () => {
    server.use(http.get("*/api/v1/casts/:id", () => problem(404, "NOT_FOUND", "No such cast.")));
    renderStudio();
    expect(await screen.findByRole("alert")).toHaveTextContent(/no such cast/i);
  });
});

describe("Studio — inserting and editing a layer", () => {
  it("inserts a text layer on top, selects it, and saves it in the stack", async () => {
    const saved: { body?: SavedBody; ifMatch?: string | null } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Text" }));

    // It is on the canvas as layer 4 (top of the stack) and selected.
    expect(canvasLayer(4, /Layer 4: Text — New text/)).toHaveAttribute("aria-pressed", "true");
    // …and the properties panel is editing it.
    expect(screen.getByLabelText("Text")).toHaveValue("New text");

    await user.click(saveButton());
    const layers = (await waitForSave(saved)).slides?.[0]?.layers ?? [];
    expect(layers).toHaveLength(4);
    expect(layers[3]).toMatchObject({ kind: "text", text: "New text" });
  });

  it("carries the If-Match derived from the read revision", async () => {
    const saved: { body?: SavedBody; ifMatch?: string | null } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Rectangle" }));
    await user.click(saveButton());
    await waitForSave(saved);
    expect(saved.ifMatch).toBe('"1"');
  });

  it("edits the selected layer's text, size, colour and alignment", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));

    const textarea = screen.getByLabelText("Text");
    await user.clear(textarea);
    await user.type(textarea, "Open at 9");
    fireEvent.change(screen.getByLabelText("Font size (px)"), { target: { value: "72" } });
    fireEvent.change(screen.getByLabelText("Text colour hex"), { target: { value: "#00ff00" } });
    await user.selectOptions(screen.getByLabelText("Alignment"), "center");

    await user.click(saveButton());
    const layer = (await waitForSave(saved)).slides?.[0]?.layers?.[1];
    expect(layer).toMatchObject({
      kind: "text",
      text: "Open at 9",
      font_px: 72,
      color: "#00FF00",
      align: "center",
    });
  });

  it("deletes the selected layer from the stack", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Delete Text — Welcome" }));
    await user.click(saveButton());
    const layers = (await waitForSave(saved)).slides?.[0]?.layers ?? [];
    expect(layers.map((l) => l.kind)).toEqual(["rect", "clock"]);
  });

  it("reorders z-order — 'bring forward' moves a layer later in the array", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    // The background rect is index 0 (drawn first). Bring it one step forward.
    await user.click(screen.getByRole("button", { name: /Bring Rectangle/ }));
    await user.click(saveButton());
    const layers = (await waitForSave(saved)).slides?.[0]?.layers ?? [];
    expect(layers.map((l) => l.kind)).toEqual(["text", "rect", "clock"]);
  });
});

describe("Studio — moving and resizing on the canvas", () => {
  it("drags a layer and saves its new position", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const target = canvasLayer(2, /Layer 2: Text — Welcome/);
    await user.click(target);
    expect(screen.getByLabelText("X")).toHaveValue(120);

    fireEvent.pointerDown(target, { clientX: 300, clientY: 300 });
    fireEvent.pointerMove(window, { clientX: 400, clientY: 350 });
    fireEvent.pointerUp(window);

    // The panel agrees with the model immediately…
    expect(screen.getByLabelText("X")).toHaveValue(220);
    expect(screen.getByLabelText("Y")).toHaveValue(250);

    // …and so does what gets saved.
    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.layers?.[1]).toMatchObject({ x: 220, y: 250 });
  });

  it("clamps a drag at the canvas edge instead of pushing the layer off it", async () => {
    server.use(...serveCast(cast(), {}));
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const target = canvasLayer(3, /Layer 3: Clock/);
    fireEvent.click(target);
    fireEvent.pointerDown(target, { clientX: 0, clientY: 0 });
    fireEvent.pointerMove(window, { clientX: 5000, clientY: 5000 });
    fireEvent.pointerUp(window);

    // The clock is 420x120; its far edge lands exactly on the canvas boundary.
    expect(screen.getByLabelText("X")).toHaveValue(1920 - 420);
    expect(screen.getByLabelText("Y")).toHaveValue(1080 - 120);
  });

  it("resizes from the bottom-right grip with the origin pinned", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    const grip = screen.getByRole("button", { name: /Resize layer 2 from the bottom right/ });
    fireEvent.pointerDown(grip, { clientX: 1000, clientY: 1000 });
    fireEvent.pointerMove(window, { clientX: 900, clientY: 1040 });
    fireEvent.pointerUp(window);

    expect(screen.getByLabelText("X")).toHaveValue(120);
    expect(screen.getByLabelText("Y")).toHaveValue(200);
    expect(screen.getByLabelText("Width")).toHaveValue(800);
    expect(screen.getByLabelText("Height")).toHaveValue(200);

    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.layers?.[1]).toMatchObject({ w: 800, h: 200 });
  });

  it("nudges the focused layer with the arrow keys (and finely with Alt)", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const target = canvasLayer(2, /Layer 2: Text — Welcome/);
    await user.click(target);
    fireEvent.keyDown(target, { key: "ArrowRight" });
    expect(screen.getByLabelText("X")).toHaveValue(128);
    fireEvent.keyDown(target, { key: "ArrowLeft", altKey: true });
    expect(screen.getByLabelText("X")).toHaveValue(127);
    fireEvent.keyDown(target, { key: "ArrowDown", shiftKey: true });
    expect(screen.getByLabelText("Height")).toHaveValue(168);
  });

  it("lets a number field be cleared and retyped a character at a time", async () => {
    // The naive controlled field rejects the empty string mid-retype and snaps
    // back to the old value, so every keystroke after the first is fought. This
    // drives the exact gesture: select all, clear, type.
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    const width = screen.getByLabelText("Width");
    await user.clear(width);
    await user.type(width, "512");
    expect(width).toHaveValue(512);

    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.layers?.[1]).toMatchObject({ w: 512 });
  });

  it("shows the CLAMPED value once the field is left, so the field never lies", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    const width = screen.getByLabelText("Width");
    await user.clear(width);
    await user.type(width, "2");
    // Still typing: the field holds what was typed, NOT the clamped model value
    // (that is what lets "512" be typed one digit at a time).
    expect(width).toHaveValue(2);
    await user.tab();
    // Left the field: the 16px floor the model actually stored is now shown.
    expect(width).toHaveValue(16);
  });

  it("lets a colour be retyped as hex without fighting the caret", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(1, /Layer 1: Rectangle/));
    const hex = screen.getByLabelText("Fill colour hex");
    await user.clear(hex);
    await user.type(hex, "#0A1B2C");
    expect(hex).toHaveValue("#0A1B2C");

    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.layers?.[0]).toMatchObject({ color: "#0A1B2C" });
  });

  it("edits geometry precisely from the properties panel", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    fireEvent.change(screen.getByLabelText("X"), { target: { value: "640" } });
    fireEvent.change(screen.getByLabelText("Width"), { target: { value: "600" } });

    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.layers?.[1]).toMatchObject({ x: 640, w: 600 });
  });
});

describe("Studio — the save gate", () => {
  // The PATCH ships the WHOLE slides array and the server validates every
  // member, so ONE undrawable slide loses the edits to all the others. The gate
  // must therefore be on the DOCUMENT, not on the slide being looked at: an
  // operator who broke slide 1, moved on, and polished slide 2 for an hour must
  // not be allowed to press a button that discards both.
  it("holds the save for a slide the operator is not even looking at, and says why", async () => {
    const saved: { body?: SavedBody } = {};
    const two = cast({
      slides: [
        { id: "slide-1", layers: [{ kind: "text", x: 0, y: 0, w: 600, h: 200, text: "Front", font_px: 96 }] },
        { id: "slide-2", layers: [{ kind: "text", x: 0, y: 0, w: 600, h: 200, text: "Back", font_px: 96 }] },
      ],
    });
    server.use(...serveCast(two, saved));
    const user = userEvent.setup();
    renderStudio();

    // Break slide 1 by emptying its only text layer…
    await user.click(await screen.findByRole("button", { name: /Layer 1: Text/ }));
    await user.clear(screen.getByLabelText("Text"));
    expect(saveButton()).toBeDisabled();
    expect(screen.getByText(/one slide won't draw yet/i)).toBeInTheDocument();

    // …then go and edit slide 2. The save is STILL held — the body would carry
    // the broken slide 1 and be refused whole.
    await user.click(screen.getByRole("button", { name: "Slide 2" }));
    expect(saveButton()).toBeDisabled();

    await user.click(screen.getByRole("button", { name: "Slide 1 — needs attention" }));
    await user.click(screen.getByRole("button", { name: /Layer 1: Text/ }));
    await user.type(screen.getByLabelText("Text"), "Fixed");
    await waitFor(() => expect(saveButton()).toBeEnabled());
    expect(screen.queryByText(/won't draw yet/i)).toBeNull();

    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.layers?.[0]?.text).toBe("Fixed");
  });
});

describe("Studio — the image layer and the media picker", () => {
  it("picks an asset and writes the REF ALONE — never the derived url", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(
      ...serveCast(cast(), saved),
      http.get("*/api/v1/content", () =>
        HttpResponse.json({ content: [contentAsset(), contentAsset({ asset_ref: "sha256:ff99", url: "/content/ff99" })] }),
      ),
    );
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Image" }));
    // A fresh image layer is deliberately incomplete, and the panel says why.
    expect(screen.getByText(/no image chosen yet/i)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /choose image/i }));
    const dialog = await screen.findByRole("dialog");
    await user.click(await within(dialog).findByRole("button", { name: "Use sha256:ff99" }));

    expect(await screen.findByText("sha256:ff99")).toBeInTheDocument();

    await user.click(saveButton());
    const layers = (await waitForSave(saved)).slides?.[0]?.layers ?? [];
    expect(layers[3]).toMatchObject({ kind: "image", asset_ref: "sha256:ff99" });
    // The url is DERIVED and this picker used to be the one writer that put one
    // on an authored layer. It must not: producers mint a fetch url at
    // projection time, and one frozen into a row is a value nothing re-checks —
    // it survives an export/restore and it outlives a signed url's expiry, so a
    // canvas that trusts it draws dead bytes and reports nothing missing. The
    // canvas resolves the ref against the origin's listing instead, which is the
    // answer for every layer written by anything but this picker.
    expect(layers[3]).not.toHaveProperty("url");
  });

  it("draws an image layer that carries only an asset_ref", async () => {
    // The MIRROR of the derive defect, and the same root cause. `asset_ref` is
    // the only AUTHORED half of a content-bearing layer — the wire says so, and
    // `validateSlide` deliberately does not demand a url — so a cast written by
    // a pack import, a workspace restore or a plain API call carries the ref
    // alone. Keyed off `url`, the canvas drew every one of those as the "no
    // bytes chosen yet" outline: a lie about a layer that is finished.
    const withImage = cast({
      slides: [{
        id: "slide-1",
        layers: [{ kind: "image", x: 0, y: 0, w: 640, h: 360, asset_ref: "sha256:aa11" }],
      }],
    });
    const saved: { body?: SavedBody } = {};
    server.use(
      ...serveCast(withImage, saved),
      http.get("*/api/v1/content", () =>
        HttpResponse.json({ content: [contentAsset({ asset_ref: "sha256:aa11", url: "/content/aa11" })] }),
      ),
    );
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Image/ });

    await waitFor(() => {
      const imgs = document.querySelectorAll('[data-slot="layer-image"]');
      expect(imgs).toHaveLength(2);
      expect(imgs[0]).toHaveAttribute("src", "/content/aa11");
    });
    expect(document.querySelectorAll('[data-slot="layer-image-empty"]')).toHaveLength(0);
  });

  // Clearing leaves an image layer naming no bytes, which the SERVER refuses at
  // authoring time (wire.validateSlideLayers: "image: asset_ref is required")
  // — and it refuses the whole cast, not that one slide. So the console holds
  // the save and says why, instead of sending a body it knows will 422 and
  // losing every other edit in the document with it. (That the patch removes
  // BOTH keys rather than writing `undefined` is pinned on the model, in
  // cast-model.test.ts, where it can be asserted without a save.)
  it("holds the save after an image is cleared, rather than sending a cast the server refuses", async () => {
    const saved: { body?: SavedBody } = {};
    const withImage = cast({
      slides: [
        {
          id: "slide-1",
          layers: [{ kind: "image", x: 0, y: 0, w: 640, h: 360, asset_ref: "sha256:aa11", url: "/content/aa11" }],
        },
      ],
    });
    server.use(
      ...serveCast(withImage, saved),
      http.get("*/api/v1/content", () =>
        HttpResponse.json({ content: [contentAsset({ asset_ref: "sha256:ff99", url: "/content/ff99" })] }),
      ),
    );
    const user = userEvent.setup();
    renderStudio();

    await user.click(await screen.findByRole("button", { name: /Layer 1: Image/ }));
    await user.click(screen.getByRole("button", { name: /clear/i }));

    expect(saveButton()).toBeDisabled();
    expect(screen.getByText(/won't draw yet/i)).toBeInTheDocument();
    // Held, not silently swallowed: nothing was sent.
    expect(saved.body).toBeUndefined();

    // …and choosing bytes again releases it, so the gate is a gate and not a
    // dead end.
    await user.click(screen.getByRole("button", { name: /choose image/i }));
    const dialog = await screen.findByRole("dialog");
    await user.click(await within(dialog).findByRole("button", { name: "Use sha256:ff99" }));
    await waitFor(() => expect(saveButton()).toBeEnabled());
    await user.click(saveButton());
    const layer = (await waitForSave(saved)).slides?.[0]?.layers?.[0] ?? {};
    expect(layer).toMatchObject({ asset_ref: "sha256:ff99" });
    // The fixture this started from carried a legacy `url` alongside its ref.
    // Re-picking must CLEAR it, not leave the old one beside a new digest —
    // which would be a layer pointing at one asset and drawing another.
    expect(layer).not.toHaveProperty("url");
  });

  // ── The url a cast was saved with must never be drawn ─────────────────────
  //
  // The regression HV-1's fix introduced on the authoring surface. Content urls
  // became SIGNED and EXPIRING; the picker handed the Studio one of those, the
  // Studio patched it into the layer, and the save persisted it. Nothing reached
  // a screen — both projections re-mint from the asset_ref — but an operator who
  // built a cast today and reopened it tomorrow was looking at broken images and
  // a dead link in the properties panel.
  //
  // The fixture therefore carries a REAL signed url shape on both sides: an
  // expired one on the stored layer (the kind a cast saved yesterday holds) and
  // a live one in the listing. Anything that reads the layer's own url draws the
  // dead one and fails here.
  it("draws a stored image from a FRESHLY minted url, never the one saved with the cast", async () => {
    const stale = "https://origin.example/content/aa11?exp=1700000000000&sig=deadbeef";
    const fresh = "https://origin.example/content/aa11?exp=9999999999999&sig=cafebabe";
    const saved: { body?: SavedBody } = {};
    const withImage = cast({
      slides: [
        {
          id: "slide-1",
          layers: [{ kind: "image", x: 0, y: 0, w: 640, h: 360, asset_ref: "sha256:aa11", url: stale }],
        },
      ],
    });
    server.use(
      ...serveCast(withImage, saved),
      http.get("*/api/v1/content", () =>
        HttpResponse.json({ content: [contentAsset({ asset_ref: "sha256:aa11", url: fresh })] }),
      ),
    );
    renderStudio();

    await screen.findByRole("button", { name: /Layer 1: Image/ });
    // The canvas AND the filmstrip thumbnail — the same renderer at two scales,
    // so a fix applied to one and not the other is visible here.
    await waitFor(() => {
      const drawn = Array.from(document.querySelectorAll('[data-slot="layer-image"]'));
      expect(drawn.length).toBeGreaterThan(0);
      for (const el of drawn) expect(el.getAttribute("src")).toBe(fresh);
    });

    // And the dead url is not merely out-drawn — it is gone from the document
    // being edited, so a save cannot put it back.
    const user = userEvent.setup();
    await user.click(canvasLayer(1, /Layer 1: Image/));
    await user.type(screen.getByLabelText("X"), "{selectall}24");
    await waitFor(() => expect(saveButton()).toBeEnabled());
    await user.click(saveButton());
    const layer = (await waitForSave(saved)).slides?.[0]?.layers?.[0] ?? {};
    expect(layer).toMatchObject({ asset_ref: "sha256:aa11" });
    expect(layer).not.toHaveProperty("url");
  });
});

/**
 * The VIDEO layer (parity row 1.5-video).
 *
 * `video` is the ninth layer kind. It landed on the wire, in the projector and
 * in the player, and the console was left believing there were eight — with two
 * consequences, and the second is the one that mattered. It could not be
 * inserted, which is a missing feature; and any cast that ALREADY held one was
 * reported by the console's own mirror as carrying an "Unknown layer kind",
 * which sets `invalidSlideCount > 0`, which disables Save for the whole
 * document. An operator opening that cast could edit nothing and save nothing,
 * with a message blaming a slide the server was perfectly happy with.
 *
 * Both halves are driven here as an operator drives them, and both end at the
 * PATCH body rather than at something rendering.
 */
describe("Studio — the video layer", () => {
  it("inserts a video, picks its bytes, and saves the layer", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(
      ...serveCast(cast(), saved),
      http.get("*/api/v1/content", () =>
        HttpResponse.json({ content: [contentAsset({ asset_ref: "sha256:cc77", url: "/content/cc77" })] }),
      ),
    );
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    // The toolbar offers it directly, beside Image: a clip is content chosen
    // from the library, not a live widget that needs explaining.
    await user.click(screen.getByRole("button", { name: "Video" }));
    expect(screen.getByText(/no video chosen yet/i)).toBeInTheDocument();

    // A fresh video layer names no bytes, so the save is held — exactly as an
    // image's is, and for the same reason.
    expect(saveButton()).toBeDisabled();

    await user.click(screen.getByRole("button", { name: /choose video/i }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/choose a video/i)).toBeInTheDocument();
    await user.click(await within(dialog).findByRole("button", { name: "Use sha256:cc77" }));

    await waitFor(() => expect(saveButton()).toBeEnabled());
    await user.click(saveButton());
    const layers = (await waitForSave(saved)).slides?.[0]?.layers ?? [];
    expect(layers[3]).toMatchObject({ kind: "video", asset_ref: "sha256:cc77" });
    // Same rule as an image, from the same picker: the ref is authored, the url
    // is not.
    expect(layers[3]).not.toHaveProperty("url");
  });

  it("saves a cast that already carries a video layer instead of calling its kind unknown", async () => {
    const saved: { body?: SavedBody } = {};
    const withVideo = cast({
      slides: [
        {
          id: "slide-1",
          layers: [
            { kind: "video", x: 0, y: 0, w: 1920, h: 1080, asset_ref: "sha256:cc77", url: "/content/cc77" },
            { kind: "text", x: 120, y: 200, w: 900, h: 160, text: "Now showing", font_px: 96, color: "#ffffff" },
          ],
        },
      ],
    });
    server.use(...serveCast(withVideo, saved));
    const user = userEvent.setup();
    renderStudio();

    // It is a real, selectable object in the stack — not a mystery layer.
    await user.click(await screen.findByRole("button", { name: /Layer 1: Video/ }));
    // Nothing is held, and no phantom problem is shown against a layer the
    // server stored happily.
    expect(screen.queryByText(/won't draw yet/i)).toBeNull();
    expect(screen.queryByText(/unknown layer kind/i)).toBeNull();

    // And an edit to the OTHER layer really can be saved, which is the whole
    // point: the defect made the document read-only.
    await user.click(canvasLayer(2, /Layer 2: Text — Now showing/));
    const textarea = screen.getByRole("textbox", { name: /^text$/i });
    await user.clear(textarea);
    await user.type(textarea, "Feature presentation");

    await waitFor(() => expect(saveButton()).toBeEnabled());
    await user.click(saveButton());
    const layers = (await waitForSave(saved)).slides?.[0]?.layers ?? [];
    expect(layers[0]).toMatchObject({ kind: "video", asset_ref: "sha256:cc77" });
    expect(layers[1]).toMatchObject({ kind: "text", text: "Feature presentation" });
  });
});

describe("Studio — slides", () => {
  it("adds, duplicates, reorders and deletes slides", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: /add slide/i }));
    // A brand-new slide is DRAWABLE, so the filmstrip does not flag it and the
    // save is not held: the operator can add a slide and keep working. (A
    // zero-layer slide would have been flagged here — and would have made the
    // save below fail for slide 1 as well, because the PATCH is atomic.)
    expect(screen.getByRole("button", { name: "Slide 2" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Slide 2 — needs attention" })).toBeNull();
    expect(saveButton()).toBeEnabled();

    // The new slide is selected and the canvas follows it — onto its seeded text.
    expect(screen.getByLabelText("Text")).toHaveValue("New text");

    await user.click(screen.getByRole("button", { name: "Move slide 2 earlier" }));
    await user.click(saveButton());
    let slides = (await waitForSave(saved)).slides ?? [];
    expect(slides[0]?.layers).toHaveLength(1);
    expect(slides[1]?.layers).toHaveLength(3);

    await user.click(screen.getByRole("button", { name: "Delete slide 1" }));
    await user.click(saveButton());
    slides = (await waitForSave(saved)).slides ?? [];
    expect(slides).toHaveLength(1);
    expect(slides[0]?.layers).toHaveLength(3);
  });

  it("duplicates a slide by value — the copy is independent", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Duplicate slide 1" }));
    // The duplicate is now selected; edit its text layer only.
    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    const textarea = screen.getByLabelText("Text");
    await user.clear(textarea);
    await user.type(textarea, "Second");

    await user.click(saveButton());
    const slides = (await waitForSave(saved)).slides ?? [];
    expect(slides).toHaveLength(2);
    expect(slides[0]?.layers[1]?.text).toBe("Welcome");
    expect(slides[1]?.layers[1]?.text).toBe("Second");
  });

  it("sets a slide's hold time, and clearing it REMOVES the key", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const duration = screen.getByLabelText("Slide duration (seconds)");
    expect(duration).toHaveValue(10);
    fireEvent.change(duration, { target: { value: "20" } });
    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.duration_ms).toBe(20_000);

    fireEvent.change(screen.getByLabelText("Slide duration (seconds)"), { target: { value: "" } });
    await user.click(saveButton());
    expect("duration_ms" in ((await waitForSave(saved)).slides?.[0] ?? {})).toBe(false);
  });
});

describe("Studio — refusals the projector would make silently", () => {
  it("warns on the layer, and badges the slide, when a text layer has no text", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    await user.clear(screen.getByLabelText("Text"));

    expect(await screen.findByText(/text is required/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Slide 1 — needs attention/ })).toBeInTheDocument();
  });
});

describe("Studio — saving, conflicts and unsaved work", () => {
  it("the save button is inert until something changes, and settles back after", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    expect(screen.getByRole("button", { name: "Saved" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Rectangle" }));
    await user.click(saveButton());
    expect(await screen.findByRole("button", { name: "Saved" })).toBeDisabled();
  });

  it("on a concurrent edit keeps the draft and offers both explicit choices", async () => {
    // The FIRST GET is the editor opening the cast; the second is the re-read
    // the conflict flow performs after the 412, and it answers with the version
    // saved elsewhere. One counted handler, because the flow's own re-read
    // happens inside the save — there is no moment between them to swap in a
    // different one.
    let reads = 0;
    server.use(
      http.get("*/api/v1/casts/:id", () => {
        reads += 1;
        return reads === 1
          ? HttpResponse.json(cast(), { headers: { ETag: '"1"', "Trace-Id": TRACE_ID } })
          : HttpResponse.json(cast({ revision: 7, name: "Renamed elsewhere" }), {
              headers: { ETag: '"7"', "Trace-Id": TRACE_ID },
            });
      }),
      http.patch("*/api/v1/casts/:id", () =>
        problem(412, "REVISION_CONFLICT", "The resource was modified concurrently.", { current_revision: 7 }),
      ),
    );
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Text" }));
    await user.click(saveButton());

    const banner = await screen.findByRole("status");
    expect(banner).toHaveTextContent(/changed elsewhere/i);
    expect(screen.getByLabelText("Cast name")).toHaveValue("Lobby loop");
    // The inserted layer survived the conflict.
    expect(screen.getByRole("button", { name: /Layer 4: Text — New text/ })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /load the current version/i }));
    expect(screen.getByLabelText("Cast name")).toHaveValue("Renamed elsewhere");
    expect(screen.queryByRole("button", { name: /Layer 4:/ })).not.toBeInTheDocument();
  });

  it("guards leaving with unsaved work, and lets the operator through when they mean it", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Rectangle" }));
    await user.click(screen.getByRole("button", { name: /back to casts/i }));

    expect(await screen.findByRole("dialog")).toHaveTextContent(/leave without saving/i);
    await user.click(screen.getByRole("button", { name: /discard and leave/i }));
    expect(await screen.findByRole("heading", { name: "Casts" })).toBeInTheDocument();
  });

  it("leaves immediately when there is nothing to lose", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: /back to casts/i }));
    expect(await screen.findByRole("heading", { name: "Casts" })).toBeInTheDocument();
  });

  it("renames the cast and saves the new name", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const name = screen.getByLabelText("Cast name");
    await user.clear(name);
    await user.type(name, "Front window");
    await user.click(saveButton());
    expect((await waitForSave(saved)).name).toBe("Front window");
  });
});

/**
 * UNDO AND REDO, driven with the pointer and the keyboard.
 *
 * The reducer's own cases live in edit-history.test.ts; these exist because the
 * pure model being right proves nothing about the wiring. Two of these cases are
 * here specifically because they are the ways this feature ships broken while
 * every unit test passes: a real multi-event drag unwinding one frame at a time,
 * and a document-level key handler that keeps editing a cast after the operator
 * has left the page.
 */
describe("Studio — undo and redo", () => {
  /** ⌘Z, delivered to whatever currently has focus, as a browser would. */
  const undoChord = "{Meta>}z{/Meta}";
  const redoChord = "{Meta>}{Shift>}z{/Shift}{/Meta}";

  it("brings back a deleted slide, and the save carries it", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: /add slide/i }));
    expect(screen.getByRole("button", { name: "Slide 2" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Delete slide 1" }));
    expect(screen.queryByRole("button", { name: "Slide 2" })).toBeNull();

    await user.keyboard(undoChord);

    // The slide is back — and the operator is left where they were when they
    // deleted it (on slide 2), because a step restores the state that preceded
    // it rather than inventing a new selection.
    expect(screen.getByRole("button", { name: "Slide 2" })).toHaveAttribute("aria-current", "true");
    // It came back WITH its three layers, not as a placeholder of the right
    // shape: open it and the clock is there.
    await user.click(screen.getByRole("button", { name: "Slide 1" }));
    expect(screen.getByRole("button", { name: /Layer 3: Clock/ })).toBeInTheDocument();

    await user.click(saveButton());
    const slides = (await waitForSave(saved)).slides ?? [];
    expect(slides).toHaveLength(2);
    expect(slides[0]?.layers.map((l) => l.kind)).toEqual(["rect", "text", "clock"]);
  });

  it("undoes a 20-frame drag in ONE press, back to where the layer started", async () => {
    // The failure this asserts against is the usual one: a stack that records
    // every pointermove, so ⌘Z crawls the layer back a few pixels at a time.
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const target = canvasLayer(2, /Layer 2: Text — Welcome/);
    await user.click(target);
    expect(screen.getByLabelText("X")).toHaveValue(120);
    expect(screen.getByLabelText("Y")).toHaveValue(200);

    fireEvent.pointerDown(target, { clientX: 300, clientY: 300 });
    for (let frame = 1; frame <= 20; frame += 1) {
      fireEvent.pointerMove(window, { clientX: 300 + frame * 10, clientY: 300 + frame * 5 });
    }
    fireEvent.pointerUp(window);
    expect(screen.getByLabelText("X")).toHaveValue(320);
    expect(screen.getByLabelText("Y")).toHaveValue(300);

    // ONE press. The affordance agrees about what it is going to do.
    expect(screen.getByRole("button", { name: "Undo move layer" })).toBeEnabled();
    await user.keyboard(undoChord);

    expect(screen.getByLabelText("X")).toHaveValue(120);
    expect(screen.getByLabelText("Y")).toHaveValue(200);
    // …and nothing is left behind it, so the drag really was one step.
    expect(screen.getByRole("button", { name: "Undo" })).toBeDisabled();

    // Redo puts the whole drag back, also in one press.
    await user.keyboard(redoChord);
    expect(screen.getByLabelText("X")).toHaveValue(320);

    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.layers?.[1]).toMatchObject({ x: 320, y: 300 });
  });

  it("keeps a drag the operator PAUSED in the middle of as one undo", async () => {
    // The coalescing window cannot save this one — five seconds is many times
    // its length — so this is the case that proves the canvas is announcing the
    // gesture's edges and the history is honouring them. An operator who holds
    // the button still while lining a layer up against another has not
    // performed two drags.
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const target = canvasLayer(2, /Layer 2: Text — Welcome/);
    await user.click(target);

    // The clock the coalescing window reads, driven rather than waited on.
    let clock = Date.now();
    const now = vi.spyOn(Date, "now").mockImplementation(() => clock);
    try {
      fireEvent.pointerDown(target, { clientX: 300, clientY: 300 });
      fireEvent.pointerMove(window, { clientX: 340, clientY: 300 });
      clock += 5_000;
      fireEvent.pointerMove(window, { clientX: 400, clientY: 340 });
      fireEvent.pointerUp(window);
    } finally {
      now.mockRestore();
    }

    expect(screen.getByLabelText("X")).toHaveValue(220);
    expect(screen.getByLabelText("Y")).toHaveValue(240);

    await user.keyboard(undoChord);
    expect(screen.getByLabelText("X")).toHaveValue(120);
    expect(screen.getByLabelText("Y")).toHaveValue(200);
    expect(screen.getByRole("button", { name: "Undo" })).toBeDisabled();
  });

  it("does not swallow the next edit into the drag that just finished", async () => {
    // The other half of the gesture contract. If pointerup went unreported the
    // flag would stay raised and everything after it would fold into the drag —
    // a stack that looks healthy and holds one step for the whole session.
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const target = canvasLayer(2, /Layer 2: Text — Welcome/);
    await user.click(target);
    fireEvent.pointerDown(target, { clientX: 300, clientY: 300 });
    fireEvent.pointerMove(window, { clientX: 400, clientY: 300 });
    fireEvent.pointerUp(window);
    expect(screen.getByLabelText("X")).toHaveValue(220);

    const textarea = screen.getByLabelText("Text");
    await user.clear(textarea);
    await user.type(textarea, "Open at 9");

    const row = screen.getByRole("button", { name: "Text — Open at 9" });
    await user.click(row);
    await user.keyboard(undoChord);

    // One press took back the typing and LEFT the drag: two edits, two steps.
    expect(screen.getByLabelText("Text")).toHaveValue("Welcome");
    expect(screen.getByLabelText("X")).toHaveValue(220);
  });

  it("undoes a typed phrase in ONE press, not one character at a time", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    const textarea = screen.getByLabelText("Text");
    await user.clear(textarea);
    await user.type(textarea, "Open at 9");
    expect(canvasLayer(2, "Text — Open at 9")).toBeInTheDocument();

    // Leave the field the way an operator does — click the layer's row in the
    // Layers list — so the keystroke belongs to the editor and not to the
    // textarea's own undo. (Clicking the CANVAS also works now: the hit target
    // preventDefaults its pointerdown to stop the browser dragging the artwork
    // and then moves focus itself. It did not, once, and the arrow-key nudge was
    // unreachable by pointer for exactly that reason.)
    const row = screen.getByRole("button", { name: "Text — Open at 9" });
    await user.click(row);
    expect(row).toHaveFocus();

    await user.keyboard(undoChord);

    expect(canvasLayer(2, "Text — Welcome")).toBeInTheDocument();
    expect(screen.getByLabelText("Text")).toHaveValue("Welcome");
  });

  it("leaves ⌘Z alone while the caret is in a text field", async () => {
    // The field has its own undo, over the characters in it. Reverting a slide
    // delete out from under someone mid-word would be a worse surprise than
    // having no shortcut at all.
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    const textarea = screen.getByLabelText("Text");
    await user.clear(textarea);
    await user.type(textarea, "Zzz");
    expect(textarea).toHaveFocus();

    // Dispatched rather than typed, so the assertion is about the handler and
    // not about how the emulation renders a held modifier. `fireEvent` returns
    // false only when something cancelled the event, so a `true` here IS the
    // claim: nothing intercepted it, and it reached the field's own undo.
    expect(fireEvent.keyDown(textarea, { key: "z", metaKey: true })).toBe(true);

    // And the MODEL is untouched — the canvas still describes the typed text.
    expect(canvasLayer(2, "Text — Zzz")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Undo edit the text" })).toBeEnabled();
  });

  it("leaves ⌘Z alone while a dialog is in front of the editor", async () => {
    // A modal is a different context. Reverting the document behind it, out of
    // sight, on a keystroke aimed at the dialog is the wrong answer.
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Text" }));
    expect(screen.getByRole("button", { name: /Layer 4: Text — New text/ })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /back to casts/i }));
    expect(await screen.findByRole("dialog")).toHaveTextContent(/leave without saving/i);

    expect(fireEvent.keyDown(document, { key: "z", metaKey: true })).toBe(true);

    // Dismiss it — the editor behind a modal is aria-hidden, so the layer can
    // only be looked at once the dialog is gone — and the inserted layer is
    // still there.
    await user.keyboard("{Escape}");
    expect(await screen.findByRole("button", { name: /Layer 4: Text — New text/ })).toBeInTheDocument();
  });

  it("names the step on the buttons, and disables them at the ends", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    // Freshly opened: nothing either way.
    expect(screen.getByRole("button", { name: "Undo" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Redo" })).toBeDisabled();

    await user.click(screen.getByRole("button", { name: /add slide/i }));
    await user.click(screen.getByRole("button", { name: "Delete slide 1" }));

    const undo = screen.getByRole("button", { name: "Undo delete slide" });
    expect(undo).toBeEnabled();
    await user.click(undo);

    expect(screen.getByRole("button", { name: "Slide 2" })).toBeInTheDocument();
    const redo = screen.getByRole("button", { name: "Redo delete slide" });
    expect(redo).toBeEnabled();
    // Behind the delete is the add — the label follows the stack rather than
    // being a fixed string.
    expect(screen.getByRole("button", { name: "Undo add slide" })).toBeEnabled();

    await user.click(redo);
    expect(screen.queryByRole("button", { name: "Slide 2" })).toBeNull();
  });

  it("undoes past a SAVE, and the save button comes back so the undo can be kept", async () => {
    // The half that is easy to get wrong: after a save the draft is clean, so an
    // undo has to report the draft dirty AGAIN or the operator is left looking
    // at a reverted cast they cannot persist.
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const name = screen.getByLabelText("Cast name");
    await user.clear(name);
    await user.type(name, "Front window");
    await user.click(saveButton());
    expect((await waitForSave(saved)).name).toBe("Front window");
    expect(await screen.findByRole("button", { name: "Saved" })).toBeDisabled();

    // Focus is on the save button, not in a field, so the chord is the editor's.
    await user.keyboard(undoChord);

    expect(screen.getByLabelText("Cast name")).toHaveValue("Lobby loop");
    await user.click(saveButton());
    expect((await waitForSave(saved)).name).toBe("Lobby loop");
  });

  it("undoing back to the opened cast reports it CLEAN again", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Rectangle" }));
    expect(saveButton()).toBeEnabled();

    await user.keyboard(undoChord);
    expect(screen.getByRole("button", { name: "Saved" })).toBeDisabled();
  });

  it("adopting the server's version after a conflict empties the stack", async () => {
    // That version is a DIFFERENT document. An undo across it would write one
    // cast's slides into another's draft.
    let reads = 0;
    server.use(
      http.get("*/api/v1/casts/:id", () => {
        reads += 1;
        return reads === 1
          ? HttpResponse.json(cast(), { headers: { ETag: '"1"', "Trace-Id": TRACE_ID } })
          : HttpResponse.json(cast({ revision: 7, name: "Renamed elsewhere" }), {
              headers: { ETag: '"7"', "Trace-Id": TRACE_ID },
            });
      }),
      http.patch("*/api/v1/casts/:id", () =>
        problem(412, "REVISION_CONFLICT", "The resource was modified concurrently.", { current_revision: 7 }),
      ),
    );
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Text" }));
    expect(screen.getByRole("button", { name: "Undo add text layer" })).toBeEnabled();

    await user.click(saveButton());
    await screen.findByRole("status");
    await user.click(screen.getByRole("button", { name: /load the current version/i }));

    expect(screen.getByRole("button", { name: "Undo" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Redo" })).toBeDisabled();
  });

  it("gives ⌘Z back to the browser the moment the Studio unmounts", async () => {
    // A document-level handler that outlives its page is a live editor nobody
    // is looking at. `fireEvent` returns false when the event was cancelled, so
    // this asserts on the handler's actual effect rather than on a spy.
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    const view = renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });
    await user.click(screen.getByRole("button", { name: "Rectangle" }));

    expect(fireEvent.keyDown(document, { key: "z", metaKey: true })).toBe(false);

    view.unmount();
    expect(fireEvent.keyDown(document, { key: "z", metaKey: true })).toBe(true);
  });
});

/** Wait until the PATCH handler has recorded a body, then return it. */
async function waitForSave(saved: { body?: SavedBody | undefined }): Promise<SavedBody> {
  const body = await vi.waitFor(() => {
    if (!saved.body) throw new Error("no save observed");
    return saved.body;
  });
  saved.body = undefined;
  return body;
}

/**
 * The WIDGET PICKER and the four live widget kinds (parity row 3.6).
 *
 * Every case here goes all the way to the PATCH body, because the failure this
 * feature exists to fix was precisely a rendered capability nothing could
 * author: the four kinds shipped on the wire, in the box's resolver and in the
 * player while the Studio's toolbar and the request schema still knew four.
 * "The picker opened" and "a countdown reached the server with its target" are
 * different claims, and only the second one is worth anything.
 */
describe("Studio — the widget picker", () => {
  /** Open the picker and choose one widget by its name in the dialog. */
  async function insertWidget(user: ReturnType<typeof userEvent.setup>, name: RegExp) {
    await user.click(screen.getByRole("button", { name: /add widget/i }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name }));
  }

  it("offers every live kind, with what each one draws from", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: /add widget/i }));
    const dialog = await screen.findByRole("dialog");
    const choices = within(dialog).getAllByRole("button", { name: /date|countdown|weather|entity/i });
    expect(choices).toHaveLength(4);
    // The blurbs are the reason the picker is a dialog and not four more
    // toolbar buttons: they say where the value comes from.
    expect(within(dialog).getByText(/fetched by the box/i)).toBeInTheDocument();
  });

  it("inserts a DATE layer, takes a format preset, and saves the layout", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await insertWidget(user, /^Date$/);
    // It landed on top of the stack and is selected, so the panel is already
    // editing it — the same contract every other insert has.
    expect(canvasLayer(4, /Layer 4: Date/)).toHaveAttribute("aria-pressed", "true");

    await user.selectOptions(screen.getByLabelText("Date format preset"), "Jan 2, 2006");
    await user.click(saveButton());

    const layer = (await waitForSave(saved)).slides?.[0]?.layers[3];
    expect(layer).toMatchObject({ kind: "date", text: "Jan 2, 2006" });
  });

  it("inserts a COUNTDOWN and saves the target as an absolute epoch-ms instant", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await insertWidget(user, /^Countdown$/);
    // It arrives VALID — a target is already set, so the save gate is not
    // holding and the widget is visibly counting rather than reading 00:00:00.
    expect(saveButton()).toBeEnabled();

    // A local wall time in, an absolute instant out. Asserting against
    // Date.parse of the same string is the whole conversion under test: any
    // UTC-vs-local slip shows up as a several-hour difference.
    fireEvent.change(screen.getByLabelText(/counts down to/i), { target: { value: "2027-01-01T18:30" } });
    await user.selectOptions(screen.getByLabelText("Remaining-time format preset"), "DD:HH:MM:SS");
    await user.click(saveButton());

    const layer = (await waitForSave(saved)).slides?.[0]?.layers[3];
    expect(layer).toMatchObject({
      kind: "countdown",
      target_ms: Date.parse("2027-01-01T18:30"),
      text: "DD:HH:MM:SS",
    });
  });

  it("inserts a WEATHER widget and appends a token by clicking it", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await insertWidget(user, /^Weather$/);
    const template = screen.getByLabelText("Display template");
    await user.clear(template);
    await user.type(template, "Now ");
    await user.click(screen.getByRole("button", { name: "Insert {tempc}" }));

    await user.click(saveButton());
    const layer = (await waitForSave(saved)).slides?.[0]?.layers[3];
    expect(layer).toMatchObject({ kind: "weather", text: "Now {tempc}" });
  });

  it("inserts an ENTITY widget, lists the box's REAL entities, and saves the chosen one", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await insertWidget(user, /entity state/i);

    // Until a subject is chosen the slide will not draw, so the save is HELD
    // and the panel says which layer is at fault — an entity id nobody chose
    // would otherwise be accepted and resolve to a dash on the wall forever.
    expect(saveButton()).toBeDisabled();
    expect(screen.getAllByRole("status").some((n) => /which entity/i.test(n.textContent ?? ""))).toBe(true);

    // The options are the rows GET /entities returned, named and stated — not
    // ids typed by hand.
    const picker = screen.getByLabelText("Entity");
    expect(within(picker).getByRole("option", { name: /Lobby TV — on/ })).toBeInTheDocument();
    expect(within(picker).getByRole("option", { name: /Cafe TV — off/ })).toBeInTheDocument();

    await user.selectOptions(picker, ULID_C);
    expect(saveButton()).toBeEnabled();
    await user.click(saveButton());

    const layer = (await waitForSave(saved)).slides?.[0]?.layers[3];
    expect(layer).toMatchObject({ kind: "entity", entity_id: ULID_C, text: "{state}" });
  });

  it("degrades the entity picker to a plain id field when the box knows none", async () => {
    const saved: { body?: SavedBody } = {};
    // The empty override goes FIRST: msw resolves the first matching handler,
    // and serveCast registers a populated /entities of its own.
    server.use(
      http.get("*/api/v1/entities", () =>
        HttpResponse.json({ items: [], cursor: null }, { headers: { "Trace-Id": TRACE_ID } }),
      ),
      ...serveCast(cast(), saved),
    );
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: /add widget/i }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: /entity state/i }));

    // An empty dropdown with no explanation is the worse dead end: a slide
    // authored before the devices are adopted must still be finishable.
    const field = await screen.findByLabelText("Entity");
    expect(field.tagName).toBe("INPUT");
    fireEvent.change(field, { target: { value: ULID_B } });
    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.layers[3]).toMatchObject({ entity_id: ULID_B });
  });
});

/**
 * Cast-level playback settings (parity row 1.7).
 *
 * The CLEAR is the half that would otherwise ship broken, and it is asserted
 * explicitly: the PATCH body shallow-merges over the stored row, so a save that
 * simply omitted the member when the field was blanked would leave the old
 * default in place forever — a control that accepts the work and never performs
 * it, which is this repo's recurring defect shape.
 */
describe("Studio — cast playback settings", () => {
  it("saves a cast-wide default duration, and an explicit null when it is cleared", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast({ default_duration_ms: 8000 }), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const field = screen.getByLabelText("Default slide duration (seconds)");
    expect(field).toHaveValue(8);

    fireEvent.change(field, { target: { value: "15" } });
    await user.click(saveButton());
    expect((await waitForSave(saved)).default_duration_ms).toBe(15_000);

    fireEvent.change(screen.getByLabelText("Default slide duration (seconds)"), { target: { value: "" } });
    await user.click(saveButton());
    const cleared = await waitForSave(saved);
    // null, not absent: absent means "leave it alone" to a merging server.
    expect(cleared.default_duration_ms).toBeNull();
    expect("default_duration_ms" in cleared).toBe(true);
  });

  it("keeps the per-slide duration and the cast default as separate controls", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    // The fixture slide holds 10s of its own; setting a cast default must not
    // disturb it, and vice versa — they are different rows of the resolution.
    fireEvent.change(screen.getByLabelText("Default slide duration (seconds)"), { target: { value: "6" } });
    fireEvent.change(screen.getByLabelText("Slide duration (seconds)"), { target: { value: "25" } });
    await user.click(saveButton());

    const body = await waitForSave(saved);
    expect(body.default_duration_ms).toBe(6000);
    expect(body.slides?.[0]?.duration_ms).toBe(25_000);
  });
});

/**
 * The two INTERACTIVE kinds (tracker rows 1.5 and 3.7), driven the way an
 * operator drives them and asserted on the body that reaches the server.
 *
 * These matter more than most Studio cases because the thing being authored is
 * BEHAVIOUR, not appearance: a button whose event name never reaches the save
 * body looks identical on the canvas to one whose does, and the difference only
 * shows up as an automation that never fires.
 */
describe("Studio — interactive layers", () => {
  it("inserts a Button, names its event, and saves both halves", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Button" }));

    // It lands COMPLETE — label and event name both set — so it is drawable and
    // pressable the moment it is inserted.
    const label = screen.getByLabelText("Button label");
    await user.clear(label);
    await user.type(label, "Call for service");

    const name = screen.getByLabelText("Event name");
    await user.clear(name);
    // Typed the way a person types it; the field normalises to the slug the
    // wire enforces rather than reporting an error on every keystroke.
    await user.type(name, "Call Service");
    expect(name).toHaveValue("call_service");

    await user.click(saveButton());
    const layers = (await waitForSave(saved)).slides?.[0]?.layers ?? [];
    expect(layers[3]).toMatchObject({
      kind: "ping",
      text: "Call for service",
      ping_name: "call_service",
    });
  });

  it("makes an ordinary widget pressable by giving it an event name (row 3.7)", async () => {
    // The interactive-WIDGET half: no new kind, the same one field on a layer
    // that already exists. Driven on the fixture's clock layer.
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(3, /Layer 3: Clock/));
    await user.type(screen.getByLabelText(/Event name/), "clock_tapped");

    await user.click(saveButton());
    const layers = (await waitForSave(saved)).slides?.[0]?.layers ?? [];
    expect(layers[2]).toMatchObject({ kind: "clock", ping_name: "clock_tapped" });
  });

  it("builds a Menu whose item jumps to another slide of the same cast", async () => {
    const twoSlides = cast({
      slides: [
        {
          id: "slide-1",
          layers: [{ kind: "text", x: 120, y: 200, w: 900, h: 160, text: "Welcome" }],
        },
        {
          id: "slide-2",
          layers: [{ kind: "text", x: 120, y: 200, w: 900, h: 160, text: "Rooms" }],
        },
      ],
    });
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(twoSlides, saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Text — Welcome/ });

    await user.click(screen.getByRole("button", { name: "Menu" }));
    // A menu lands EMPTY of items: its targets can only be slides of this cast,
    // which the layer default cannot know.
    expect(screen.getByText(/No items yet/)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /Add item/ }));
    const itemLabel = screen.getByLabelText("Item 1 label");
    await user.clear(itemLabel);
    await user.type(itemLabel, "Rooms");
    // The target is a SELECT over the cast's real slides, so a dead-end target
    // is unreachable from this surface.
    await user.selectOptions(screen.getByLabelText("Jumps to"), "slide-2");

    await user.click(saveButton());
    const layers = (await waitForSave(saved)).slides?.[0]?.layers ?? [];
    expect(layers[1]).toMatchObject({
      kind: "nav",
      items: [{ label: "Rooms", target_slide_id: "slide-2" }],
    });
  });

  it("holds the save when a menu item has no target yet", async () => {
    // The gate the whole nav design rests on: a menu item that would accept a
    // press and perform nothing must never reach the server.
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Menu" }));
    await user.click(screen.getByRole("button", { name: /Add item/ }));

    expect(saveButton()).toBeDisabled();
    expect(screen.getByText(/needs a slide to jump to/i)).toBeInTheDocument();
  });
});

/**
 * The RASTERIZED FALLBACK, authored the way an operator authors it (parity row
 * 2.4). Every case here ends at the PATCH body, because the whole risk in this
 * feature is a control that renders and does nothing: a QR whose payload cannot
 * be changed would still look right on the canvas and would still save — as a
 * sign pointing at example.com.
 */
describe("Studio — rasterized (derive) layers", () => {
  async function insertDerive(user: ReturnType<typeof userEvent.setup>, label: string) {
    await user.click(screen.getByRole("button", { name: "Add rasterized" }));
    await user.click(await screen.findByRole("button", { name: new RegExp(label) }));
  }

  it("inserts a QR layer, lets the payload be changed, and saves the spec", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await insertDerive(user, "QR code");

    // It lands on the canvas, selected, and SAYS it is not rendered — the one
    // kind whose appearance does not follow from saving.
    expect(canvasLayer(4, /Layer 4: QR/)).toHaveAttribute("aria-pressed", "true");
    // Twice: the canvas and the filmstrip thumbnail are the same renderer.
    expect(screen.getAllByText("NEEDS RENDER")).toHaveLength(2);

    const payload = screen.getByLabelText("Encoded link or text");
    await user.clear(payload);
    await user.type(payload, "https://waiveo.local/pair/ABCD-1234");
    await user.selectOptions(screen.getByLabelText("Error correction"), "H");

    await user.click(saveButton());
    const layers = (await waitForSave(saved)).slides?.[0]?.layers ?? [];
    expect(layers).toHaveLength(4);
    expect(layers[3]).toMatchObject({
      kind: "derive",
      derive: { kind: "qr", data: "https://waiveo.local/pair/ABCD-1234", ec_level: "H" },
    });
    // The raster is the RENDERER's to produce: an authored layer never invents
    // an asset_ref, and one here would name bytes nobody uploaded.
    expect(layers[3]).not.toHaveProperty("asset_ref");
  });

  it("edits a styled panel's gradient, shadow and corner radius", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await insertDerive(user, "Styled panel");

    fireEvent.change(screen.getByLabelText("Gradient from hex"), { target: { value: "#112233" } });
    fireEvent.change(screen.getByLabelText("Gradient to hex"), { target: { value: "#445566" } });
    fireEvent.change(screen.getByLabelText("Gradient angle"), { target: { value: "45" } });
    fireEvent.change(screen.getByLabelText("Corner radius"), { target: { value: "16" } });
    await user.click(screen.getByLabelText(/Drop shadow/));

    await user.click(saveButton());
    const layer = (await waitForSave(saved)).slides?.[0]?.layers?.[3] as Record<string, unknown>;
    expect(layer.derive).toMatchObject({
      kind: "rect",
      fill: { kind: "linear", from: "#112233", to: "#445566", angle_deg: 45 },
      border: { radius: 16 },
      shadow: { dy: 8, blur: 24, opacity_pct: 55 },
    });
  });

  it("drops the gradient's second stop when the fill is switched to solid", async () => {
    // The server REFUSES a solid fill carrying a `to`, so a switch that left the
    // old stop behind would make the cast unsavable — and the control that did
    // it would no longer be on screen for the operator to undo.
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await insertDerive(user, "Styled panel");
    await user.selectOptions(screen.getByLabelText("Fill"), "solid");

    await user.click(saveButton());
    const layer = (await waitForSave(saved)).slides?.[0]?.layers?.[3] as Record<string, unknown>;
    expect(layer.derive).toMatchObject({ fill: { kind: "solid" } });
    expect((layer.derive as Record<string, Record<string, unknown>>).fill).not.toHaveProperty("to");
    expect((layer.derive as Record<string, Record<string, unknown>>).fill).not.toHaveProperty("angle_deg");
  });

  it("draws the real picture, and stops warning, once a raster exists", async () => {
    // A cast whose derive layer has already been through waiveo-derive — and the
    // layer is written EXACTLY as that tool writes it: `asset_ref` and
    // `derived_from`, and nothing else. There is deliberately no `url` here,
    // because no run of the rasterizer has ever produced one: `url` is a DERIVED
    // member that producers mint at projection time, and the only reason any
    // authored cast carries one is that this console's media picker writes it.
    // A fixture that invented one would let a canvas that waits for an authored
    // url pass this test while showing every real raster as unrendered forever.
    //
    // So the url comes from where the real one comes from: the content origin's
    // own listing, which the editor already reads.
    const rendered = cast();
    rendered.slides[0]!.layers.push({
      kind: "derive", x: 1400, y: 120, w: 400, h: 400,
      asset_ref: "sha256:aa11",
      derived_from: "digest-1",
      derive: { kind: "qr", data: "https://waiveo.local/pair" },
    } as never);
    const saved: { body?: SavedBody } = {};
    server.use(
      ...serveCast(rendered, saved),
      http.get("*/api/v1/content", () =>
        HttpResponse.json({
          content: [contentAsset({ asset_ref: "sha256:aa11", url: "https://origin.example/content/aa11" })],
        }),
      ),
    );
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await waitFor(() => {
      // Twice: the canvas and the filmstrip thumbnail are the same renderer, so
      // a fix applied to one of them is not a fix.
      const drawn = document.querySelectorAll('[data-slot="layer-derive"]');
      expect(drawn).toHaveLength(2);
      expect(drawn[0]!.querySelector("img")).toHaveAttribute("src", "https://origin.example/content/aa11");
    });
    expect(screen.queryAllByText("NEEDS RENDER")).toHaveLength(0);
    expect(screen.queryAllByText("BYTES MISSING")).toHaveLength(0);
    // …and no re-render warning either: nothing in this session has touched the
    // spec or the geometry, so the PNG on screen IS the picture of this layer.
    expect(screen.queryAllByText("NEEDS RE-RENDER")).toHaveLength(0);
  });

  it("says the raster is missing, not unrendered, when the origin has no such bytes", async () => {
    // A layer that HAS been through the tool but whose PNG the origin is no
    // longer serving — swept, or restored from an export without its assets.
    // "NEEDS RENDER" would send an operator to run a tool that has already run,
    // and running it again changes nothing, because the layer still reads
    // CURRENT to the queue.
    const rendered = cast();
    rendered.slides[0]!.layers.push({
      kind: "derive", x: 1400, y: 120, w: 400, h: 400,
      asset_ref: "sha256:gone", derived_from: "digest-1",
      derive: { kind: "qr", data: "https://waiveo.local/pair" },
    } as never);
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(rendered, saved));
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await waitFor(() => expect(screen.getAllByText("BYTES MISSING")).toHaveLength(2));
    expect(screen.queryAllByText("NEEDS RENDER")).toHaveLength(0);
  });

  // ── The origin has THREE states, and the canvas must model three ───────────
  //
  // "In flight", "answered" and "FAILED" are not two states. Collapsing the
  // third into "answered, and the origin holds nothing" makes a failed read
  // indistinguishable from a swept asset, so the canvas puts BYTES MISSING on
  // every finished layer in the cast — a lie about a finished layer, which is
  // exactly the class the badge was added to end, moved into the state nobody
  // modelled. An operator who opens the Studio while the box is briefly
  // unreachable, or with a principal that can read casts but not list content,
  // concludes the retention sweep ate their assets and goes and re-uploads or
  // re-renders bytes that were never gone.
  for (const origin of [
    { name: "refuses the listing", handler: () => HttpResponse.json(problem(403, "FORBIDDEN", "This principal may not list content."), { status: 403 }) },
    { name: "is unreachable", handler: () => HttpResponse.error() },
  ]) {
    it(`says nothing is missing, and says WHY, when the origin ${origin.name}`, async () => {
      const rendered = cast();
      rendered.slides[0]!.layers.push(
        {
          kind: "derive", x: 1400, y: 120, w: 400, h: 400,
          asset_ref: "sha256:aa11", derived_from: "digest-1",
          derive: { kind: "qr", data: "https://waiveo.local/pair" },
        } as never,
        { kind: "image", x: 0, y: 0, w: 640, h: 360, asset_ref: "sha256:bb22" } as never,
      );
      const saved: { body?: SavedBody } = {};
      server.use(...serveCast(rendered, saved), http.get("*/api/v1/content", origin.handler));
      renderStudio();
      await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

      // The status line is the whole answer: said ONCE, about the origin, not
      // per-layer about assets it knows nothing about.
      await waitFor(() => expect(screen.getByText(/couldn't read the content library/i)).toBeInTheDocument());
      expect(screen.queryAllByText("BYTES MISSING")).toHaveLength(0);
      // …and not "unrendered" either: the rasterizer HAS run on that layer.
      expect(screen.queryAllByText("NEEDS RENDER")).toHaveLength(0);
      // The editor is still an editor — a text-layout session is not taken down
      // because the content origin blinked.
      expect(screen.getByRole("button", { name: /Layer 2: Text — Welcome/ })).toBeInTheDocument();
    });
  }

  // ── The listing is authoritative; an authored url is at most a fallback ────
  it("draws from the origin's listing, not from a url frozen into the row", async () => {
    // `url` is DERIVED. A row can still carry one — written by an older console,
    // carried in from a workspace export — and it is a value nothing has
    // re-checked: on the branch that makes content urls signed and expiring it
    // is simply dead. Preferring it over the listing draws the dead bytes AND
    // reports missing:false, so there is no badge either.
    const rendered = cast();
    rendered.slides[0]!.layers.push({
      kind: "image", x: 0, y: 0, w: 640, h: 360,
      asset_ref: "sha256:aa11", url: "https://origin.example/content/aa11?sig=EXPIRED",
    } as never);
    const saved: { body?: SavedBody } = {};
    server.use(
      ...serveCast(rendered, saved),
      http.get("*/api/v1/content", () =>
        HttpResponse.json({
          content: [contentAsset({ asset_ref: "sha256:aa11", url: "https://origin.example/content/aa11?sig=FRESH" })],
        }),
      ),
    );
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await waitFor(() => {
      const imgs = document.querySelectorAll('[data-slot="layer-image"]');
      expect(imgs).toHaveLength(2);
    });
    for (const img of document.querySelectorAll('[data-slot="layer-image"]')) {
      expect(img).toHaveAttribute("src", "https://origin.example/content/aa11?sig=FRESH");
    }
  });

  it("badges a layer whose frozen url outlived the bytes, rather than drawing it", async () => {
    // The same rule at the other end: the listing ANSWERED and does not carry
    // this digest. The bytes are gone; the url on the row is a url to nothing.
    // Drawing it would show a broken image with no explanation, and reporting
    // missing:false would leave the operator with no badge at all.
    const rendered = cast();
    rendered.slides[0]!.layers.push({
      kind: "image", x: 0, y: 0, w: 640, h: 360,
      asset_ref: "sha256:gone", url: "https://origin.example/content/gone?sig=EXPIRED",
    } as never);
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(rendered, saved));
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    // Twice — the canvas and the filmstrip are the same renderer.
    await waitFor(() => expect(screen.getAllByText("BYTES MISSING")).toHaveLength(2));
    expect(document.querySelectorAll('[data-slot="layer-image"]')).toHaveLength(0);
  });

  // ── A raster the operator has just invalidated ─────────────────────────────
  it("says NEEDS RE-RENDER once an edit makes the drawn raster a picture of the old spec", async () => {
    // A derive layer's PNG is rendered at its exact spec and pixel size, so
    // editing either makes what is on the canvas a picture of the previous
    // design. It keeps drawing — never blanking over an edit nobody has rendered
    // yet — but drawing it with nothing said is a lie about a finished layer.
    const rendered = cast();
    rendered.slides[0]!.layers.push({
      kind: "derive", x: 1400, y: 120, w: 400, h: 400,
      asset_ref: "sha256:aa11", derived_from: "digest-1",
      derive: { kind: "text", text: "SCAN TO PAIR", font_px: 96 },
    } as never);
    const saved: { body?: SavedBody } = {};
    server.use(
      ...serveCast(rendered, saved),
      http.get("*/api/v1/content", () =>
        HttpResponse.json({
          content: [contentAsset({ asset_ref: "sha256:aa11", url: "https://origin.example/content/aa11" })],
        }),
      ),
    );
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    // Untouched: the drawn PNG is this layer's picture, and nothing is claimed.
    await waitFor(() => expect(document.querySelectorAll('[data-slot="layer-derive"]')).toHaveLength(2));
    expect(screen.queryAllByText("NEEDS RE-RENDER")).toHaveLength(0);

    await user.click(await screen.findByRole("button", { name: /Layer 4: Styled text/ }));
    const size = screen.getByLabelText(/font size/i);
    await user.clear(size);
    await user.type(size, "120");

    // Still drawn — and now it says what it is.
    await waitFor(() => expect(screen.getAllByText("NEEDS RE-RENDER").length).toBeGreaterThan(0));
    expect(document.querySelectorAll('[data-slot="layer-derive"]')).toHaveLength(2);
    expect(screen.queryAllByText("BYTES MISSING")).toHaveLength(0);
  });

  it("attaches an uploaded font file to a rasterized text layer", async () => {
    // CUSTOM TYPOGRAPHY — one of the five things this layer kind exists for, and
    // the one whose enforcement shipped without its authoring half: the
    // write-time existence check, the retention hold and the renderer's
    // @font-face embed were all built while `font_asset_ref` had no control
    // anywhere in the console, so an operator could upload a TTF and had no way
    // to attach it.
    const saved: { body?: SavedBody } = {};
    server.use(
      ...serveCast(cast(), saved),
      http.get("*/api/v1/content", () =>
        HttpResponse.json({ content: [contentAsset({ asset_ref: "sha256:f0nt", url: "/content/f0nt" })] }),
      ),
    );
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await insertDerive(user, "Styled text");
    expect(screen.getByText(/None — the renderer draws with whatever face/i)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /choose font file/i }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/choose a font file/i)).toBeInTheDocument();
    await user.click(await within(dialog).findByRole("button", { name: "Use sha256:f0nt" }));

    // It is shown on the layer it belongs to…
    expect(await screen.findByText("sha256:f0nt")).toBeInTheDocument();
    // …and it reaches the SERVER, inside the spec, which is the only thing that
    // makes the face reach the renderer.
    await user.click(saveButton());
    const layer = (await waitForSave(saved)).slides?.[0]?.layers?.[3] as Record<string, unknown>;
    expect(layer.derive).toMatchObject({ kind: "text", font_asset_ref: "sha256:f0nt" });

    // And it can be taken off again, without taking the family with it.
    await user.click(screen.getByRole("button", { name: /^clear$/i }));
    await user.click(saveButton());
    const after = (await waitForSave(saved)).slides?.[0]?.layers?.[3] as Record<string, unknown>;
    expect(after.derive).not.toHaveProperty("font_asset_ref");
  });

  it("keeps derived_from across an unrelated edit", async () => {
    // `derived_from` is what tells the server CURRENT from STALE. A Studio that
    // dropped it on save would report every layer as never rendered, and its
    // picture would come off the wall until somebody ran the tool again.
    const rendered = cast();
    // Written the way waiveo-derive writes it: the pair, and nothing else.
    rendered.slides[0]!.layers.push({
      kind: "derive", x: 1400, y: 120, w: 400, h: 400,
      asset_ref: "sha256:aa11",
      derived_from: "digest-1",
      derive: { kind: "qr", data: "https://waiveo.local/pair" },
    } as never);
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(rendered, saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    const textarea = screen.getByLabelText("Text");
    await user.clear(textarea);
    await user.type(textarea, "Open at 9");

    await user.click(saveButton());
    const layer = (await waitForSave(saved)).slides?.[0]?.layers?.[3] as Record<string, unknown>;
    expect(layer.derived_from).toBe("digest-1");
    expect(layer.asset_ref).toBe("sha256:aa11");
  });
});

/**
 * THE FULL-SCREEN EDITOR — the menu bar, the docked panels, the zoom, the
 * arrange commands, the History palette and the keyboard.
 *
 * Every case here PRESSES something and then asserts on the model, the saved
 * PATCH body, or a region appearing and disappearing. None of them assert that
 * a thing rendered: this console has shipped a control that rendered perfectly
 * and did nothing, and the layout it renders in is not what makes any of these
 * correct.
 */

/** Open one of the menu-bar menus and return its panel. */
async function openMenu(user: ReturnType<typeof userEvent.setup>, label: string) {
  await user.click(screen.getByRole("menuitem", { name: label }));
  return screen.getByRole("menu", { name: label });
}

/** The insert group's button for a kind — disambiguated from the layer rows and
 * the canvas hit targets, which carry the same words. */
function insertTool(name: string) {
  return within(screen.getByRole("group", { name: "Insert a layer" })).getByRole("button", { name });
}

describe("Studio — the menu bar", () => {
  it("opens a menu, runs a command from it, and closes", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const insert = await openMenu(user, "Insert");
    await user.click(within(insert).getByRole("menuitem", { name: "Rectangle" }));

    // The command RAN: a fourth layer is on the canvas.
    expect(await screen.findByRole("button", { name: /Layer 4: Rectangle/ })).toBeInTheDocument();
    // …and the menu closed behind it, rather than staying over the panel that
    // asks for the new layer's fields.
    expect(screen.queryByRole("menu", { name: "Insert" })).not.toBeInTheDocument();
  });

  it("closes on Escape without running anything", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await openMenu(user, "Insert");
    await user.keyboard("{Escape}");

    expect(screen.queryByRole("menu", { name: "Insert" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Layer 4:/ })).not.toBeInTheDocument();
  });

  it("disables a command that cannot run, and enables it when it can", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    // Nothing is selected, so there is nothing to duplicate.
    let edit = await openMenu(user, "Edit");
    expect(within(edit).getByRole("menuitem", { name: /Duplicate layer/ })).toBeDisabled();
    await user.keyboard("{Escape}");

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    edit = await openMenu(user, "Edit");
    expect(within(edit).getByRole("menuitem", { name: /Duplicate layer/ })).toBeEnabled();
  });

  it("toggles a dock panel from View, and the panel's CONTENT goes with it", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    expect(screen.getByRole("list", { name: "Layers" })).toBeInTheDocument();

    const view = await openMenu(user, "View");
    await user.click(within(view).getByRole("menuitem", { name: "Layers panel" }));
    // Collapsed, not merely restyled: the list is GONE from the tree, which is
    // the only version of "hidden" a screen reader agrees with.
    expect(screen.queryByRole("list", { name: "Layers" })).not.toBeInTheDocument();

    await user.click(within(await openMenu(user, "View")).getByRole("menuitem", { name: "Layers panel" }));
    expect(screen.getByRole("list", { name: "Layers" })).toBeInTheDocument();
  });

  it("hides the slide rail from View and brings it back", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    expect(screen.getByRole("region", { name: "Slides" })).toBeInTheDocument();
    await user.click(within(await openMenu(user, "View")).getByRole("menuitem", { name: "Slides panel" }));
    expect(screen.queryByRole("region", { name: "Slides" })).not.toBeInTheDocument();
  });
});

describe("Studio — zoom", () => {
  it("zooms in, zooms out, and goes back to fitting", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    // jsdom reports a zero-sized viewport, so the fit falls back to 1:1 — see
    // canvas-zoom.fitToViewport, which is where that rule is asserted directly.
    const readout = () => screen.getByRole("button", { name: /Zoom is \d+ percent/ });
    expect(readout()).toHaveTextContent("100%");

    await user.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(readout()).toHaveTextContent("150%");
    await user.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(readout()).toHaveTextContent("200%");
    await user.click(screen.getByRole("button", { name: "Zoom out" }));
    expect(readout()).toHaveTextContent("150%");

    await user.click(screen.getByRole("button", { name: "Fit to window" }));
    expect(readout()).toHaveTextContent("100%");
    // Fitting is a MODE, not a value: the button reads as engaged so an operator
    // can tell "100% because it fits" from "100% because I asked for it".
    expect(screen.getByRole("button", { name: "Fit to window" })).toHaveAttribute("aria-pressed", "true");

    await user.click(screen.getByRole("button", { name: "Actual size" }));
    expect(screen.getByRole("button", { name: "Fit to window" })).toHaveAttribute("aria-pressed", "false");
  });

  it("zooms the drawn stage, not just the readout", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    const { container } = renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    // Scoped to the CANVAS's stage. The slide rail draws its thumbnails through
    // the very same component, and its first one comes earlier in the document —
    // an unscoped query measures a 152px thumbnail and passes whatever the zoom
    // does.
    const stage = () =>
      container.querySelector<HTMLElement>('[data-slot="slide-canvas"] [data-slot="slide-stage"]');
    expect(stage()?.style.width).toBe("1920px");

    await user.click(screen.getByRole("button", { name: "Zoom in" }));
    // 1920 × 1.5. Asserted on the DRAWN size because a zoom control that moves a
    // number and not the artwork is exactly the dead control this suite exists
    // for.
    expect(stage()?.style.width).toBe("2880px");
  });

  it("zooms from the keyboard", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.keyboard("{Meta>}={/Meta}");
    expect(screen.getByRole("button", { name: /Zoom is \d+ percent/ })).toHaveTextContent("150%");
    await user.keyboard("{Meta>}0{/Meta}");
    expect(screen.getByRole("button", { name: /Zoom is \d+ percent/ })).toHaveTextContent("100%");
  });
});

describe("Studio — arrange", () => {
  it("aligns the selected layer to the canvas, and the save carries it", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    // The text layer is x:120 w:900 — centring puts it at (1920-900)/2 = 510.
    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    await user.click(screen.getByRole("button", { name: "Align horizontal centre" }));
    expect(screen.getByLabelText("X")).toHaveValue(510);
    // And ONLY x moved: an align that also re-centres vertically is the bug a
    // "set x and y" implementation ships.
    expect(screen.getByLabelText("Y")).toHaveValue(200);

    await user.click(screen.getByRole("button", { name: "Align bottom" }));
    expect(screen.getByLabelText("Y")).toHaveValue(920);

    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.layers?.[1]).toMatchObject({ x: 510, y: 920 });
  });

  it("brings a layer to the front, and the SAVED ARRAY ORDER changes", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    // Array order IS z-order on this wire — there is no z-index field — so the
    // only proof a "bring to front" worked is where the layer lands in `slides`.
    await user.click(canvasLayer(1, /Layer 1: Rectangle/));
    await user.click(screen.getByRole("button", { name: "Bring to front" }));

    await user.click(saveButton());
    const body = await waitForSave(saved);
    expect(body.slides?.[0]?.layers.map((l) => l.kind)).toEqual(["text", "clock", "rect"]);
  });

  it("sends a layer to the back from the keyboard", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(3, /Layer 3: Clock/));
    // Dispatched rather than typed: userEvent's keyboard grammar reads "[" as
    // the start of a key-CODE descriptor. And the key it sends is the SHIFTED
    // glyph, "{", which is what a US layout really produces for the chord — the
    // exact case `unshift` exists for.
    fireEvent.keyDown(document.body, { key: "{", metaKey: true, shiftKey: true });

    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.layers.map((l) => l.kind)).toEqual([
      "clock",
      "rect",
      "text",
    ]);
  });

  it("greys out the order buttons the selected layer cannot use", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(1, /Layer 1: Rectangle/));
    expect(screen.getByRole("button", { name: "Send to back" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Bring to front" })).toBeEnabled();

    await user.click(canvasLayer(3, /Layer 3: Clock/));
    expect(screen.getByRole("button", { name: "Bring to front" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Send to back" })).toBeEnabled();
  });
});

describe("Studio — the keyboard acts on the SELECTION, wherever focus is", () => {
  it("nudges after the layer was picked from the Layers panel", async () => {
    // The regression this exists for: the nudge used to be an onKeyDown on the
    // canvas hit target, and the hit target preventDefaults its own pointerdown
    // (so the browser does not drag the artwork) — which also stops it taking
    // focus. So the arrows worked only if the operator had TABBED to the layer,
    // and did nothing at all after either of the two ways a layer is actually
    // selected. Focus here is on a layer-list row, deliberately.
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const row = screen.getByRole("button", { name: "Text — Welcome" });
    await user.click(row);
    expect(row).toHaveFocus();

    await user.keyboard("{ArrowRight}{ArrowRight}");
    expect(screen.getByLabelText("X")).toHaveValue(136);
    await user.keyboard("{Alt>}{ArrowLeft}{/Alt}");
    expect(screen.getByLabelText("X")).toHaveValue(135);
    await user.keyboard("{Shift>}{ArrowDown}{/Shift}");
    expect(screen.getByLabelText("Height")).toHaveValue(168);

    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.layers?.[1]).toMatchObject({ x: 135, h: 168 });
  });

  it("focuses the layer a pointer press selected, so the two rings agree", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const target = canvasLayer(2, /Layer 2: Text — Welcome/);
    await user.click(target);
    expect(target).toHaveFocus();
  });

  it("does not touch the arrows when nothing is selected", async () => {
    // With no selection the arrows belong to the browser, which scrolls the
    // canvas viewport with them. Claiming them would leave an operator unable to
    // scroll a zoomed-in canvas from the keyboard at all.
    server.use(...serveCast(cast(), {}));
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    expect(fireEvent.keyDown(document.body, { key: "ArrowRight" })).toBe(true);
  });

  it("leaves the arrows alone while the caret is in a text field", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    const textarea = screen.getByLabelText("Text");
    await user.click(textarea);
    expect(fireEvent.keyDown(textarea, { key: "ArrowRight" })).toBe(true);
    // The caret moved; the layer did not.
    expect(screen.getByLabelText("X")).toHaveValue(120);
  });

  it("deletes the selected layer with Delete, from anywhere in the editor", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Clock — 3:04 PM" }));
    await user.keyboard("{Delete}");
    expect(screen.queryByRole("button", { name: /Layer 3: Clock/ })).not.toBeInTheDocument();

    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.layers).toHaveLength(2);
  });

  it("duplicates the selected layer with ⌘D, offset so the copy is findable", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    await user.keyboard("{Meta>}d{/Meta}");

    await user.click(saveButton());
    const layers = (await waitForSave(saved)).slides?.[0]?.layers ?? [];
    expect(layers).toHaveLength(4);
    // Appended (top of the stack) and moved, not stacked exactly on top of the
    // original where it would look as if nothing happened.
    expect(layers[3]).toMatchObject({ kind: "text", text: "Welcome", x: 152, y: 232 });
  });

  it("saves with ⌘S", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(insertTool("Rectangle"));
    await user.keyboard("{Meta>}s{/Meta}");
    expect((await waitForSave(saved)).slides?.[0]?.layers).toHaveLength(4);
  });

  it("deselects with Escape", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    expect(screen.getByLabelText("X")).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(screen.queryByLabelText("X")).not.toBeInTheDocument();
  });
});

describe("Studio — the layer clipboard", () => {
  const copyChord = "{Meta>}c{/Meta}";
  const cutChord = "{Meta>}x{/Meta}";
  const pasteChord = "{Meta>}v{/Meta}";

  // The gap: an operator who wanted the same masthead on slide 2 rebuilt it by
  // hand. Duplicate has always existed and only ever copies WITHIN a slide.
  it("carries a layer to another slide, at the same coordinates", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Text — Welcome" }));
    const x = (screen.getByLabelText("X") as HTMLInputElement).value;
    await user.keyboard(copyChord);

    await user.click(screen.getByRole("button", { name: /add slide/i }));
    await user.click(screen.getByRole("button", { name: "Slide 2" }));
    await user.keyboard(pasteChord);

    // It is on the new slide, and at the SAME place — a cross-slide paste that
    // nudged would make every one of them a two-step operation. Queried by the
    // EXACT layer-row name: the canvas hit target is "Layer 1: Text — Welcome",
    // so a regex would match both and prove nothing about which one exists.
    expect(await screen.findByRole("button", { name: "Text — Welcome" })).toBeInTheDocument();
    expect(screen.getByLabelText("X")).toHaveValue(Number(x));

    // And it SAVES: the second slide's layer reaches the wire, which is the only
    // proof that a paste edited the document rather than the view. A new slide
    // arrives with one layer of its own (a slide with an empty stack is one the
    // projector drops and the server refuses), so the pasted one is the SECOND —
    // appended, because array order is z-order and a paste lands on top.
    await user.click(saveButton());
    const body = await waitForSave(saved);
    expect(body.slides?.[1]?.layers).toHaveLength(2);
    expect(body.slides?.[1]?.layers?.[1]).toMatchObject({ kind: "text" });
  });

  // On the SAME slide a paste offsets, for duplicate's reason: a layer dropped
  // exactly on its original is indistinguishable from nothing happening.
  it("offsets a paste onto the slide it was copied from", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Text — Welcome" }));
    const before = Number((screen.getByLabelText("X") as HTMLInputElement).value);
    await user.keyboard(copyChord);
    await user.keyboard(pasteChord);

    expect(screen.getByLabelText("X")).toHaveValue(before + DUPLICATE_OFFSET);
  });

  // Cut copies BEFORE it deletes. The order matters: a cut that deleted first
  // and then failed would lose the layer entirely.
  it("cut removes the layer and still pastes it back", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Text — Welcome" }));
    await user.keyboard(cutChord);
    expect(screen.queryByRole("button", { name: "Text — Welcome" })).toBeNull();

    await user.keyboard(pasteChord);
    expect(await screen.findByRole("button", { name: "Text — Welcome" })).toBeInTheDocument();
  });

  // A paste is an ordinary edit, so it rides the history that already exists —
  // no new machinery, and ⌘Z takes it back like anything else.
  it("a paste is undoable", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Text — Welcome" }));
    expect(screen.getAllByRole("button", { name: "Text — Welcome" })).toHaveLength(1);
    await user.keyboard(copyChord);
    await user.keyboard(pasteChord);
    expect(screen.getAllByRole("button", { name: "Text — Welcome" })).toHaveLength(2);

    await user.keyboard("{Meta>}z{/Meta}");
    expect(screen.getAllByRole("button", { name: "Text — Welcome" })).toHaveLength(1);
  });

  // Copying changes NOTHING about the cast, so it must not enter the history —
  // an undo step that undoes nothing visible is the most confusing thing an undo
  // stack can hold. (Nor may it dirty the document: a copy is not an edit.)
  it("copying adds no undo step and does not dirty the cast", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    // A clean document's save control reads "Saved"; a dirty one reads "Save
    // changes". Asserting on the LABEL is what makes this a statement about the
    // dirty flag rather than about a disabled attribute.
    await user.click(screen.getByRole("button", { name: "Text — Welcome" }));
    expect(screen.getByRole("button", { name: /^saved$/i })).toBeInTheDocument();
    await user.keyboard(copyChord);
    expect(screen.getByRole("button", { name: /^saved$/i })).toBeInTheDocument();
    // …and a PASTE does dirty it, so the assertion above is not vacuous.
    await user.keyboard(pasteChord);
    expect(saveButton()).toBeInTheDocument();
  });

  // Paste is gated on the CLIPBOARD, not the selection: "copy a layer, click
  // empty canvas, paste" is the ordinary way to paste onto another slide, and
  // gating on the selection would make it impossible.
  it("the Edit menu offers Paste only once something is on the clipboard", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("menuitem", { name: "Edit" }));
    expect(screen.getByRole("menuitem", { name: /Paste layer/ })).toBeDisabled();
    await user.keyboard("{Escape}");

    await user.click(screen.getByRole("button", { name: "Text — Welcome" }));
    await user.click(screen.getByRole("menuitem", { name: "Edit" }));
    await user.click(screen.getByRole("menuitem", { name: /Copy layer/ }));

    // Deselect, so the only thing that can enable Paste is the clipboard.
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("menuitem", { name: "Edit" }));
    expect(screen.getByRole("menuitem", { name: /Paste layer/ })).toBeEnabled();
  });

  // The document handler returns before any table when the keystroke landed in
  // a text field, so ⌘C over a cast name still copies text. Asserted through
  // the effect: no layer is added and the document stays clean.
  it("leaves copy and paste alone inside a text field", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(screen.getByRole("button", { name: "Text — Welcome" }));
    await user.keyboard(copyChord);

    const before = screen.getAllByRole("button", { name: "Text — Welcome" }).length;
    await user.click(screen.getByLabelText("Cast name"));
    await user.keyboard(pasteChord);

    expect(screen.getAllByRole("button", { name: "Text — Welcome" })).toHaveLength(before);
  });
});

describe("Studio — the History panel", () => {
  it("lists every step, marks where the operator is, and jumps on a click", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    await user.click(screen.getByRole("button", { name: "Align right" }));
    await user.click(insertTool("Rectangle"));

    const list = screen.getByRole("list", { name: "Edit history" });
    expect(within(list).getAllByRole("button")).toHaveLength(3);
    expect(within(list).getByRole("button", { name: /Step 2: add rectangle layer — current/ })).toBeInTheDocument();

    // Jump back TWO steps in one click — the thing ⌘Z cannot do.
    await user.click(within(list).getByRole("button", { name: /Step 0: open the cast/ }));

    expect(screen.queryByRole("button", { name: /Layer 4: Rectangle/ })).not.toBeInTheDocument();
    await user.click(canvasLayer(2, /Layer 2: Text — Welcome/));
    expect(screen.getByLabelText("X")).toHaveValue(120);
    // The undone steps stay on the list, reachable, rather than being discarded.
    expect(
      within(screen.getByRole("list", { name: "Edit history" })).getByRole("button", {
        name: /Step 2: add rectangle layer — undone/,
      }),
    ).toBeInTheDocument();
  });

  it("jumps FORWARD again, and the save carries the state jumped to", async () => {
    const saved: { body?: SavedBody } = {};
    server.use(...serveCast(cast(), saved));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(insertTool("Rectangle"));
    await user.click(insertTool("Text"));

    const rows = () => within(screen.getByRole("list", { name: "Edit history" }));
    await user.click(rows().getByRole("button", { name: /Step 0: open the cast/ }));
    expect(screen.queryByRole("button", { name: /Layer 4:/ })).not.toBeInTheDocument();

    await user.click(rows().getByRole("button", { name: /Step 2: add text layer/ }));
    expect(screen.getByRole("button", { name: /Layer 5: Text — New text/ })).toBeInTheDocument();

    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.layers).toHaveLength(5);
  });

  it("reports the document CLEAN again when the jump lands on the opened state", async () => {
    // The property that makes the panel safe to use: jumping back to where the
    // cast was opened is not "undone edits still pending", it IS the saved
    // document, and the Save affordance has to say so.
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(insertTool("Rectangle"));
    expect(screen.getByRole("button", { name: /save changes/i })).toBeEnabled();

    await user.click(
      within(screen.getByRole("list", { name: "Edit history" })).getByRole("button", {
        name: /Step 0: open the cast/,
      }),
    );
    expect(screen.getByRole("button", { name: "Saved" })).toBeDisabled();
  });
});

describe("Studio — the workspace chrome", () => {
  it("shows and hides the composition guides", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    const { container } = renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    expect(container.querySelector('[data-slot="canvas-guides"]')).toBeNull();
    await user.click(within(await openMenu(user, "View")).getByRole("menuitem", { name: "Guides" }));
    expect(container.querySelector('[data-slot="canvas-guides"]')).not.toBeNull();
    await user.click(within(await openMenu(user, "View")).getByRole("menuitem", { name: "Guides" }));
    expect(container.querySelector('[data-slot="canvas-guides"]')).toBeNull();
  });

  it("resizes the panel column from the keyboard", async () => {
    // A drag handle nobody can reach without a mouse is a control half the
    // operators do not have. The pointer path and this one call the same
    // setter.
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    const { container } = renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    const handle = screen.getByRole("separator", { name: "Resize the panels" });
    const sidebar = container.querySelector<HTMLElement>('[data-slot="studio-sidebar"]');
    expect(sidebar?.style.width).toBe("304px");

    handle.focus();
    await user.keyboard("{ArrowLeft}");
    expect(sidebar?.style.width).toBe("316px");
    expect(handle).toHaveAttribute("aria-valuenow", "316");

    // …and it stops at the ends rather than collapsing the column to nothing.
    await user.keyboard("{Shift>}{ArrowRight}{/Shift}".repeat(6));
    expect(sidebar?.style.width).toBe("248px");
  });

  it("prints the keyboard sheet from the same table the menus print", async () => {
    server.use(...serveCast(cast(), {}));
    const user = userEvent.setup();
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    await user.click(within(await openMenu(user, "Help")).getByRole("menuitem", { name: /Keyboard shortcuts/ }));
    const sheet = await screen.findByRole("dialog");
    expect(within(sheet).getByText("Bring to front")).toBeInTheDocument();
    expect(within(sheet).getByText("Fit to window")).toBeInTheDocument();
  });

  it("states the fixed canvas as a READOUT, not as a control", async () => {
    // Legacy's canvas chip opens a size dialog. This wire has no such setting —
    // 1920×1080 is a constant of the player contract — so a button here would be
    // a control that accepts the gesture and cannot perform it.
    server.use(...serveCast(cast(), {}));
    renderStudio();
    await screen.findByRole("button", { name: /Layer 1: Rectangle/ });

    expect(screen.getByText("1920 × 1080")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /1920/ })).not.toBeInTheDocument();
  });

  it("keeps a door out in the error state, when there is no cast to edit", async () => {
    server.use(
      http.get("*/api/v1/casts/:id", () =>
        HttpResponse.json(problem(404, "NOT_FOUND", "no such cast"), { status: 404 }),
      ),
      http.get("*/api/v1/entities", () => HttpResponse.json({ items: [], cursor: null })),
    );
    renderStudio();

    expect(await screen.findByRole("alert")).toHaveTextContent(/couldn't open this cast/i);
    // No confirm to get past: there is nothing unsaved in an editor that never
    // opened, and a guard there would be a dead end.
    expect(screen.getByRole("link", { name: /back to casts/i })).toHaveAttribute("href", "/casts");
  });
});
