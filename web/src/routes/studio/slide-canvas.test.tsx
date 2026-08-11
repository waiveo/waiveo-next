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

/** Render ONE layer and return the node it drew. Each call gets its own
 * container, so the queries below can never read a previous case's paint. */
function draw(l: SlideLayer, slot = `layer-${l.kind}`): HTMLElement {
  const { container } = render(<LayerView layer={l} now={NOW} />);
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
    expect(draw(layer({ kind: "image", url: "blob:asset" }))).toHaveAttribute("src", "blob:asset");
    // No bytes chosen yet: a labelled outline, not nothing — an invisible object
    // could not be found again on the canvas.
    expect(draw(layer({ kind: "image" }), "layer-image-empty")).not.toBeNull();
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
    for (const kind of ["text", "rect", "image", "clock", "date", "countdown", "weather", "entity"] as const) {
      expect(typeof describeLayer(layer({ kind }))).toBe("string");
    }
  });
});
