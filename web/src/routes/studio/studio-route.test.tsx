import { describe, it, expect, vi, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter, Route, Routes } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import StudioRoute from "./studio-route";
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
  it("picks an asset from the content origin and sets asset_ref AND url together", async () => {
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
    expect(layers[3]).toMatchObject({ kind: "image", asset_ref: "sha256:ff99", url: "/content/ff99" });
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
    expect(layer).toMatchObject({ asset_ref: "sha256:ff99", url: "/content/ff99" });
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
    expect(layers[3]).toMatchObject({ kind: "video", asset_ref: "sha256:cc77", url: "/content/cc77" });
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
      const imgs = document.querySelectorAll('[data-slot="layer-derive"]');
      expect(imgs).toHaveLength(2);
      expect(imgs[0]).toHaveAttribute("src", "https://origin.example/content/aa11");
    });
    expect(screen.queryAllByText("NEEDS RENDER")).toHaveLength(0);
    expect(screen.queryAllByText("BYTES MISSING")).toHaveLength(0);
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
