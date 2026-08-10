import { describe, it, expect, vi, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter, Route, Routes } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import StudioRoute from "./studio-route";
import { TRACE_ID, ULID_A, cast, contentAsset, problem } from "@/api/test-support";

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

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

/** What the last PATCH carried, captured for assertions. */
interface SavedBody {
  name?: string;
  slides?: Array<{ id: string; duration_ms?: number; layers: Array<Record<string, unknown>> }>;
}

function serveCast(body: ReturnType<typeof cast>, saved: { body?: SavedBody; ifMatch?: string | null }) {
  return [
    http.get("*/api/v1/casts/:id", () =>
      HttpResponse.json(body, { headers: { ETag: '"1"', "Trace-Id": TRACE_ID } }),
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

    const duration = screen.getByLabelText(/slide duration/i);
    expect(duration).toHaveValue(10);
    fireEvent.change(duration, { target: { value: "20" } });
    await user.click(saveButton());
    expect((await waitForSave(saved)).slides?.[0]?.duration_ms).toBe(20_000);

    fireEvent.change(screen.getByLabelText(/slide duration/i), { target: { value: "" } });
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
