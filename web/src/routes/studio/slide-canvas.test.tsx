import { describe, expect, it, afterEach, vi } from "vitest";
import { act, render } from "@testing-library/react";
import type { CastSlide, SlideLayer } from "@/api";
import { LayerView, SlideStage, describeLayer } from "./slide-canvas";

/**
 * What the Studio canvas actually DRAWS for each layer kind.
 *
 * The gap this closes: the four live-widget kinds are previewed through
 * formatters that mirror the player (`formatGoTimeLayout`,
 * `formatCountdownLayout`) or through a stand-in substitution the box performs
 * for real, and nothing asserted a single character of any of it — a total
 * mutant of the preview survived the whole suite. A wrong preview ships green
 * and the operator lays the layer out against a string the wall will never show,
 * which is the one failure a WYSIWYG editor exists to prevent.
 *
 * `now` is injected (LayerView takes it) so every assertion is an exact string,
 * never a "looks like a time" regex.
 */

// Monday, 2 February 2026, 15:04:05 local time.
const NOW = new Date(2026, 1, 2, 15, 4, 5);

function layer(over: Partial<SlideLayer> & { kind: SlideLayer["kind"] }): SlideLayer {
  return { x: 100, y: 200, w: 400, h: 120, ...over };
}

/** The content library every content-bearing case below draws through. A
 * layer's fetch url is NOT part of the layer — it is minted per response and
 * expires — so the canvas resolves it from here by `asset_ref`, and a test that
 * set `layer.url` would be exercising a path the component no longer has. */
const LIBRARY: ReadonlyMap<string, string> = new Map([
  ["sha256:aa11", "https://origin.example/content/aa11?exp=9999999999999&sig=beef"],
  ["sha256:cc77", "https://origin.example/content/cc77?exp=9999999999999&sig=cafe"],
]);

/** Render ONE layer and return the node it drew. Each call gets its own
 * container, so the queries below can never read a previous case's paint. */
function draw(l: SlideLayer, slot = `layer-${l.kind}`): HTMLElement {
  const { container } = render(<LayerView layer={l} now={NOW} assetUrls={LIBRARY} />);
  return container.querySelector(`[data-slot="${slot}"]`) as HTMLElement;
}

afterEach(() => {
  vi.useRealTimers();
});

describe("the canvas preview of each LIVE widget kind", () => {
  it("draws a clock through its Go reference-time layout, not the raw layout string", () => {
    expect(draw(layer({ kind: "clock", text: "3:04 PM" }))).toHaveTextContent("3:04 PM");
    expect(draw(layer({ kind: "clock", text: "15:04:05" }))).toHaveTextContent("15:04:05");
  });

  it("draws a date through the SAME grammar — a date is a time format on this wire", () => {
    expect(draw(layer({ kind: "date", text: "Monday, January 2" }))).toHaveTextContent("Monday, February 2");
    expect(draw(layer({ kind: "date", text: "2006-01-02" }))).toHaveTextContent("2026-02-02");
  });

  it("draws a countdown as the remaining time, in the layout's own grammar", () => {
    const target = NOW.getTime() + 2 * 86_400_000 + 3 * 3_600_000 + 4 * 60_000 + 5000;
    expect(draw(layer({ kind: "countdown", text: "DD:HH:MM:SS", target_ms: target }))).toHaveTextContent("02:03:04:05");
    // A countdown with no authored layout falls to the player's own default…
    expect(draw(layer({ kind: "countdown", text: "", target_ms: target }))).toHaveTextContent("51:04:05");
    // …and one whose target has passed reads zero, never a negative.
    expect(draw(layer({ kind: "countdown", text: "HH:MM:SS", target_ms: NOW.getTime() - 5000 }))).toHaveTextContent(
      "00:00:00",
    );
    // A countdown with no target at all is a placed, unfinished object — it still
    // draws (clamped), rather than vanishing off the canvas.
    expect(draw(layer({ kind: "countdown", text: "HH:MM:SS" }))).toHaveTextContent("00:00:00");
  });

  it("draws a weather template with a STAND-IN value, never a pretended forecast", () => {
    // The box substitutes these at Lease issuance; the console is not asking a
    // forecast service here. The preview shows the layout, the font and the box
    // the widget needs — with a value in the shape of a real answer.
    const el = draw(layer({ kind: "weather", text: "{temp}° {cond}" }));
    expect(el).toHaveTextContent("72° Clear");
    expect(draw(layer({ kind: "weather", text: "{tempc}°C" }))).toHaveTextContent("22°C");
    // A token the box does not substitute is literal on the wall, so it is
    // literal here too — a typo shows as itself rather than blanking the widget.
    expect(draw(layer({ kind: "weather", text: "{temperature}" }))).toHaveTextContent("{temperature}");
  });

  it("draws an entity template with a stand-in state, and shows the bare token when none is authored", () => {
    expect(draw(layer({ kind: "entity", text: "Lobby is {state}" }))).toHaveTextContent("Lobby is on");
    expect(draw(layer({ kind: "entity" }))).toHaveTextContent("on");
  });

  it("draws a text layer literally — the one kind whose text is not a format", () => {
    // "15:04" is a clock LAYOUT and a perfectly ordinary string; only the kind
    // decides which. A text layer that rendered as a clock would silently rewrite
    // the operator's copy.
    expect(draw(layer({ kind: "text", text: "15:04" }))).toHaveTextContent("15:04");
    expect(draw(layer({ kind: "text" }))).toBeEmptyDOMElement();
  });

  it("places and styles every Label kind in CANVAS pixels, the way the player will", () => {
    const el = draw(layer({ kind: "countdown", text: "SS", target_ms: NOW.getTime(), font_px: 96, color: "#f368c4", align: "center" }));
    expect(el).toHaveStyle({ left: "100px", top: "200px", width: "400px", height: "120px" });
    expect(el).toHaveStyle({ fontSize: "96px", color: "#f368c4", justifyContent: "center" });
  });

  it("draws rect and image without a text branch, and marks an unfinished image", () => {
    expect(draw(layer({ kind: "rect", color: "#101020" }))).toHaveStyle({ backgroundColor: "#101020" });
    // ASSET_REF is the authored half; the url is DERIVED, and the listing is
    // what resolves it. A layer carrying a url and NO ref is not a layer with
    // bytes, it is a layer the server would refuse, and the canvas draws it as
    // unfinished for the same reason describeLayer calls it "(none chosen)".
    expect(draw(layer({ kind: "image", asset_ref: "sha256:aa11" }))).toHaveAttribute(
      "src",
      LIBRARY.get("sha256:aa11"),
    );
    // No bytes chosen yet: a labelled outline, not nothing — an invisible object
    // could not be found again on the canvas.
    expect(draw(layer({ kind: "image" }), "layer-image-empty")).not.toBeNull();
    expect(draw(layer({ kind: "image", url: "blob:asset" }), "layer-image-empty")).not.toBeNull();
  });

  it("draws the LIBRARY's url for an asset_ref, and ignores a url carried on the layer", () => {
    // The regression this closes. A content url is minted per response and
    // EXPIRES (internal/feeder/contenturl), so one saved with the cast is a link
    // that dies — an operator reopening the cast the next day met a canvas of
    // broken images. The layer holds the content-addressed asset_ref and nothing
    // else; the url is resolved at render time, every time.
    const stale = "https://origin.example/content/aa11?exp=1&sig=dead";
    const el = draw(layer({ kind: "image", asset_ref: "sha256:aa11", url: stale }));
    expect(el).toHaveAttribute("src", LIBRARY.get("sha256:aa11"));
    expect(el).not.toHaveAttribute("src", stale);
  });

  it("draws the unfinished outline for an asset the library no longer holds", () => {
    // A ref the content origin has swept: there is nothing to draw, and saying
    // so is better than an <img> pointed at a 404. A layer carrying a url for it
    // must not resurrect it either.
    const gone = layer({ kind: "image", asset_ref: "sha256:beef", url: "https://origin.example/content/beef?exp=1&sig=dead" });
    expect(draw(gone, "layer-image-empty")).not.toBeNull();
    expect(draw(gone)).toBeNull();
  });

  // ── video ────────────────────────────────────────────────────────────────
  // The kind whose canvas rendering had NO assertion at all: the layer list and
  // the PATCH body were covered, so reverting the content-kind branch to
  // `layer.kind === "image"` — which drops a video straight through to the
  // Label/text branch and paints an empty div where the clip should be — left
  // the whole suite green. What a WYSIWYG canvas DRAWS is the thing the editor
  // exists for, so it is asserted here for video exactly as it is for image.
  it("draws a video layer as a real video element, never as a Label or an <img>", () => {
    const el = draw(layer({ kind: "video", asset_ref: "sha256:cc77" }));
    expect(el).not.toBeNull();
    expect(el.tagName).toBe("VIDEO");
    expect(el).toHaveAttribute("src", LIBRARY.get("sha256:cc77"));
    // The still-frame posture: the filmstrip draws every slide through this same
    // component, so a canvas that autoplayed would start one decoder per slide
    // the moment a cast is opened.
    expect(el).not.toHaveAttribute("autoplay");
    expect(el).toHaveAttribute("preload", "metadata");
    // Placed in CANVAS pixels like every other kind.
    expect(el).toHaveStyle({ left: "100px", top: "200px", width: "400px", height: "120px" });

    // …and it is NOT drawn through either of the two branches that would swallow
    // it silently: the image element, or the Label branch's own div.
    const { container } = render(
      <LayerView layer={layer({ kind: "video", asset_ref: "sha256:cc77" })} now={NOW} assetUrls={LIBRARY} />,
    );
    expect(container.querySelector('[data-slot="layer-image"]')).toBeNull();
    expect(container.querySelector("div[data-slot='layer-video']")).toBeNull();
  });

  it("marks a video layer with no clip chosen with its OWN placeholder, not the image one", () => {
    const empty = draw(layer({ kind: "video" }), "layer-video-empty");
    expect(empty).not.toBeNull();
    // The VideoOff glyph, not ImageOff: the two placeholders are the only thing
    // telling an operator which unfinished box is which.
    expect(empty.querySelector("svg.lucide-video-off")).not.toBeNull();
    expect(empty.querySelector("svg.lucide-image-off")).toBeNull();
    // An unfinished video draws no <video> — there is no src to give it.
    const { container } = render(<LayerView layer={layer({ kind: "video" })} now={NOW} />);
    expect(container.querySelector('[data-slot="layer-video"]')).toBeNull();
  });
});

describe("the stage as a whole", () => {
  const slide: CastSlide = {
    id: "s1",
    layers: [
      layer({ kind: "rect", color: "#000000" }),
      layer({ kind: "clock", text: "15:04" }),
      layer({ kind: "countdown", text: "MM:SS", target_ms: Date.now() + 90_000 }),
      layer({ kind: "weather", text: "{temp}° {cond}" }),
    ],
  };

  it("draws every layer of the slide, in order, inside the scaled stage", () => {
    const { container } = render(<SlideStage slide={slide} scale={0.5} />);
    const stage = container.querySelector('[data-slot="slide-stage"]') as HTMLElement;
    expect(stage).toHaveStyle({ width: "960px", height: "540px" });
    const drawn = Array.from(stage.querySelectorAll("[data-slot^='layer-']")).map((n) => n.getAttribute("data-slot"));
    expect(drawn).toEqual(["layer-rect", "layer-clock", "layer-countdown", "layer-weather"]);
  });

  it("TICKS: a live widget re-renders as the clock advances, so no slide is a still", () => {
    // A frozen preview is the single most common way a slide looks right in an
    // editor and wrong on the wall. The countdown below must count DOWN on its
    // own, with no interaction.
    vi.useFakeTimers();
    vi.setSystemTime(new Date(2026, 1, 2, 15, 4, 5));
    const ticking: CastSlide = {
      id: "s1",
      layers: [layer({ kind: "countdown", text: "MM:SS", target_ms: Date.now() + 90_000 })],
    };
    const { container } = render(<SlideStage slide={ticking} scale={1} />);
    const el = () => container.querySelector('[data-slot="layer-countdown"]') as HTMLElement;
    expect(el()).toHaveTextContent("01:30");
    act(() => {
      vi.advanceTimersByTime(5000);
    });
    expect(el()).toHaveTextContent("01:25");
  });

  it("does NOT tick for a slide of purely static layers", () => {
    vi.useFakeTimers();
    const still: CastSlide = { id: "s1", layers: [layer({ kind: "text", text: "Welcome" })] };
    render(<SlideStage slide={still} scale={1} />);
    expect(vi.getTimerCount()).toBe(0);
  });
});

describe("describeLayer — the accessible name of every kind", () => {
  it("names each kind by what distinguishes one instance from another", () => {
    expect(describeLayer(layer({ kind: "text", text: "Welcome" }))).toBe("Text — Welcome");
    expect(describeLayer(layer({ kind: "text" }))).toBe("Text — (empty)");
    expect(describeLayer(layer({ kind: "clock", text: "15:04" }))).toBe("Clock — 15:04");
    expect(describeLayer(layer({ kind: "date", text: "Jan 2" }))).toBe("Date — Jan 2");
    expect(describeLayer(layer({ kind: "rect", color: "#fff" }))).toBe("Rectangle — #fff");
    expect(describeLayer(layer({ kind: "image" }))).toBe("Image — (none chosen)");
    expect(describeLayer(layer({ kind: "weather", text: "{temp}°" }))).toBe("Weather — {temp}°");
    expect(describeLayer(layer({ kind: "entity", entity_id: "01J8Z" }))).toBe("Entity — 01J8Z");
    // A countdown is named by its TARGET, not its layout: two countdowns on one
    // slide differ by what they count to.
    expect(describeLayer(layer({ kind: "countdown" }))).toBe("Countdown — (no target set)");
    expect(describeLayer(layer({ kind: "countdown", target_ms: 1 }))).toMatch(/^Countdown — to /);
  });

  it("never returns undefined for a kind the wire accepts", () => {
    // The trap: an exhaustive switch with no default returns `undefined` for any
    // kind added to the wire but not to it, and `aria-label` then reads
    // "Layer 1: undefined".
    for (const kind of ["text", "rect", "image", "video", "clock", "date", "countdown", "weather", "entity"] as const) {
      expect(typeof describeLayer(layer({ kind }))).toBe("string");
    }
  });
});
