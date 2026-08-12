import { describe, it, expect, vi, afterEach } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { ThemeProvider } from "@/components/theme/theme-provider";
import type { Cast, CastSlide, SlideLayer } from "@/api";
import { CastPlayer } from "./cast-player";

/**
 * The preview, DRIVEN — every case here presses something and then asserts on
 * what the page does about it.
 *
 * This console shipped a dead New button that passed its render test, so a case
 * that only checks a control exists is not a case. And a timed player is the
 * sharpest version of that failure: the transport can be perfect, the model can
 * be perfect, and the page can still never advance because nothing wired the
 * clock to it. So the clock is driven here too: vitest's fake timers fake
 * `requestAnimationFrame` as well as `Date`, so the component's real animation
 * loop runs at its real ~16ms cadence and every assertion lands on a real frame.
 *
 * Interaction is `fireEvent` rather than `userEvent`, and that is forced rather
 * than preferred: `userEvent` awaits a real timer between its synthetic events,
 * which a frozen clock never fires, so every case here hung to the 5s budget —
 * measured, with and without the animation loop running, so it is `userEvent`
 * and fake timers that are incompatible and not anything on this page.
 * `fireEvent` dispatches the same DOM events on the same elements through the
 * same handlers; what it gives up is the pointer-move choreography, which none
 * of these controls reads.
 */

afterEach(() => {
  vi.useRealTimers();
});

const layer = (over: Partial<SlideLayer> = {}): SlideLayer => ({
  kind: "text",
  x: 0,
  y: 0,
  w: 600,
  h: 200,
  text: "Hello",
  ...over,
});

const slide = (id: string, over: Partial<CastSlide> = {}): CastSlide => ({
  id,
  layers: [layer({ text: `Slide ${id}` })],
  ...over,
});

function castOf(slides: CastSlide[], defaultDurationMs: number | null = 1000): Pick<Cast, "name" | "slides" | "default_duration_ms"> {
  return { name: "Lobby loop", slides, default_duration_ms: defaultDurationMs };
}

function renderPlayer(cast: Pick<Cast, "name" | "slides" | "default_duration_ms">) {
  return render(
    <ThemeProvider>
      <CastPlayer cast={cast} assetUrls={new Map()} door={<button type="button">Back</button>} />
    </ThemeProvider>,
  );
}

/** Advance wall time under fake timers, letting the animation loop run.
 * vitest fakes `requestAnimationFrame`, so this drives the real loop at the real
 * ~16ms cadence rather than a stub. */
function tick(ms: number) {
  act(() => {
    vi.advanceTimersByTime(ms);
  });
}

/** Press a control BY ITS ACCESSIBLE NAME — the name a screen reader reads and a
 * voice-control user says. A control that renders without one cannot be found
 * here, which is the point. */
function press(name: string | RegExp) {
  act(() => {
    fireEvent.click(screen.getByRole("button", { name }));
  });
}

/** A kit Tab wraps Radix's TabsTrigger, which selects on MOUSEDOWN rather than
 * click — so a bare `fireEvent.click` leaves the panel unchanged and every
 * assertion inside it fails as "not found". */
function pressTab(name: string | RegExp) {
  act(() => {
    const tab = screen.getByRole("tab", { name });
    fireEvent.mouseDown(tab, { button: 0, ctrlKey: false });
    fireEvent.click(tab);
  });
}

/** A key on the document, which is where the page binds them. */
function key(k: string) {
  act(() => {
    fireEvent.keyDown(document, { key: k });
  });
}

/** The STAGE, scoped — the filmstrip draws every slide through the same
 * component, so an unscoped text query matches the thumbnail of every slide in
 * the cast as well as the one playing. Scoping to the TV frame is what makes
 * "which slide is on screen" a real assertion. */
function stage() {
  return within(document.querySelector('[data-slot="tv-frame"]') as HTMLElement);
}

const currentSlide = () => stage().getByText(/^Slide /);
const progressBar = (c: HTMLElement) => c.querySelector('[data-slot="preview-progress"]') as HTMLElement;

describe("watching a cast play", () => {
  it("ADVANCES on its own, at the dwell each slide is authored with", () => {
    // The whole feature in one case. Nothing is clicked: the deck must move by
    // itself, and it must move when the dwell elapses and not before.
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a", { duration_ms: 1000 }), slide("b", { duration_ms: 2000 }), slide("c")]));

    expect(currentSlide()).toHaveTextContent("Slide a");
    tick(900);
    expect(currentSlide()).toHaveTextContent("Slide a");
    tick(200);
    expect(currentSlide()).toHaveTextContent("Slide b");
    // b is authored at 2000, so it must NOT turn over at a's 1000.
    tick(1100);
    expect(currentSlide()).toHaveTextContent("Slide b");
    tick(1000);
    expect(currentSlide()).toHaveTextContent("Slide c");
  });

  it("holds slide c for the CAST's default, since it states none of its own", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a", { duration_ms: 500 }), slide("b")], 3000));
    tick(600);
    expect(currentSlide()).toHaveTextContent("Slide b");
    tick(2500);
    expect(currentSlide()).toHaveTextContent("Slide b");
    tick(600);
    expect(currentSlide()).toHaveTextContent("Slide a");
  });

  it("LOOPS back to the first slide, the way the wall always does", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a"), slide("b")], 1000));
    tick(1100);
    expect(currentSlide()).toHaveTextContent("Slide b");
    tick(1100);
    expect(currentSlide()).toHaveTextContent("Slide a");
  });

  it("paints the progress bar as the dwell runs down", () => {
    // The bar is written imperatively from the animation loop rather than
    // through a render, which is exactly the arrangement that can silently paint
    // nothing. Assert on the transform it actually carries.
    vi.useFakeTimers();
    const { container } = renderPlayer(castOf([slide("a", { duration_ms: 1000 })]));
    expect(progressBar(container).style.transform).toBe("scaleX(0)");
    tick(500);
    const half = Number(progressBar(container).style.transform.match(/scaleX\(([\d.]+)\)/)![1]);
    expect(half).toBeGreaterThan(0.4);
    expect(half).toBeLessThan(0.6);
  });

  it("shows a running elapsed readout", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a", { duration_ms: 4000 })]));
    expect(screen.getByText("0.0s")).toBeInTheDocument();
    tick(1000);
    expect(screen.getByText(/^1\.[01]s$/)).toBeInTheDocument();
  });
});

describe("the transport — every control, pressed", () => {
  it("PAUSES, and the deck stops moving", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a"), slide("b")], 1000));

    press("Pause");
    tick(5000);
    expect(currentSlide()).toHaveTextContent("Slide a");
    // And resuming starts it again — a pause that could not be undone would
    // pass the case above just as well.
    press("Play");
    tick(1100);
    expect(currentSlide()).toHaveTextContent("Slide b");
  });

  it("steps to the NEXT slide on the button, without waiting for the dwell", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a"), slide("b"), slide("c")], 60_000));
    press("Next slide");
    expect(currentSlide()).toHaveTextContent("Slide b");
  });

  it("steps BACK, and wraps past the first slide the way the cycle does", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a"), slide("b"), slide("c")], 60_000));
    press("Previous slide");
    expect(currentSlide()).toHaveTextContent("Slide c");
  });

  it("RESTARTS the dwell on a step, so the new slide gets its whole time", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a"), slide("b")], 1000));
    tick(900);
    press("Next slide");
    expect(currentSlide()).toHaveTextContent("Slide b");
    // If the step had carried the 900ms over, b would turn to a here.
    tick(500);
    expect(currentSlide()).toHaveTextContent("Slide b");
  });

  it("JUMPS to a slide from the filmstrip", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a"), slide("b"), slide("c")], 60_000));
    press("Jump to slide 3");
    expect(currentSlide()).toHaveTextContent("Slide c");
  });

  it("STOPS at the end when looping is switched off, and says the deck is spent", () => {
    vi.useFakeTimers();
    const { container } = renderPlayer(castOf([slide("a"), slide("b")], 1000));

    press(/Looping/);
    tick(2500);
    expect(currentSlide()).toHaveTextContent("Slide b");
    // Resting ON the last slide with the bar full, not snapped back to the first.
    expect(progressBar(container).style.transform).toBe("scaleX(1)");
    tick(5000);
    expect(currentSlide()).toHaveTextContent("Slide b");
  });

  it("restarts a finished deck when Play is pressed again", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a"), slide("b")], 1000));
    press(/Looping/);
    tick(2500);
    press("Play");
    expect(currentSlide()).toHaveTextContent("Slide a");
  });

  it("SCRUBS within the current slide, and the deck resumes from where it was dropped", () => {
    vi.useFakeTimers();
    const { container } = renderPlayer(castOf([slide("a", { duration_ms: 4000 }), slide("b")], 4000));
    const scrub = container.querySelector('[data-slot="preview-scrub"]') as HTMLInputElement;
    // The range spans THIS slide's dwell, not the cast's total — a cast has no
    // total, because the wall cycles it forever.
    expect(scrub.max).toBe("4000");
    act(() => {
      fireEvent.change(scrub, { target: { value: "3800" } });
    });
    expect(currentSlide()).toHaveTextContent("Slide a");
    tick(300);
    expect(currentSlide()).toHaveTextContent("Slide b");
  });

  it("drives from the keyboard: Space pauses, arrows step", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a"), slide("b"), slide("c")], 60_000));

    key("ArrowRight");
    expect(currentSlide()).toHaveTextContent("Slide b");
    key("ArrowLeft");
    expect(currentSlide()).toHaveTextContent("Slide a");
    key(" ");
    expect(screen.getByRole("button", { name: "Play" })).toBeInTheDocument();
  });

  it("names the slide by its AUTHORED number, not its position in the played order", () => {
    // Slide 1 is refused, so the first thing that plays is authored slide 2. A
    // preview that called it "slide 1" would send the operator to edit the wrong
    // one.
    vi.useFakeTimers();
    renderPlayer(
      castOf([{ id: "dead", layers: [layer({ text: "" })] }, slide("b")], 60_000),
    );
    expect(screen.getByText(/Slide 2 · 1 of 1 playing/)).toBeInTheDocument();
  });
});

describe("what the preview says about itself", () => {
  it("states on the header that it is not a pixel-exact render", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a")]));
    expect(screen.getByText(/Not a pixel-exact render of the TV/)).toBeInTheDocument();
  });

  it("declares a weather layer as a STAND-IN rather than showing it as fact", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([{ id: "w", layers: [layer({ kind: "weather", text: "{temp}° {cond}" })] }]));
    expect(screen.getByText(/Weather and entity readings are stand-ins/)).toBeInTheDocument();
    // And the count is the cast's, not a constant.
    const note = screen.getByText(/Weather and entity readings are stand-ins/).closest("li")!;
    expect(within(note).getByText("1 layer")).toBeInTheDocument();
  });

  it("does NOT raise the stand-in note for a cast with no live widgets", () => {
    // A ledger that said everything about every cast would be read once and
    // never again.
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a")]));
    expect(screen.queryByText(/Weather and entity readings are stand-ins/)).not.toBeInTheDocument();
  });

  it("warns about a `_2` day token, which the player's formatter does not have", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([{ id: "d", layers: [layer({ kind: "date", text: "_2 Jan" })] }]));
    expect(screen.getByText(/`_2` day token renders differently/)).toBeInTheDocument();
  });

  it("LISTS the slides a screen would never be sent, with the reason", () => {
    vi.useFakeTimers();
    renderPlayer(
      castOf([
        slide("good"),
        { id: "bad", layers: [layer({ kind: "image", asset_ref: "" })] },
      ]),
    );
    pressTab(/Not playing/);
    const row = screen.getByText("Slide 2").closest('[data-slot="preview-skipped-slide"]')!;
    expect(within(row as HTMLElement).getByText(/Pick an image from the media library/)).toBeInTheDocument();
  });

  it("reports a dropped derive layer WITHOUT dropping the slide", () => {
    vi.useFakeTimers();
    renderPlayer(
      castOf([
        {
          id: "s",
          layers: [layer({ text: "Still here" }), layer({ kind: "derive", derive: { kind: "qr", data: "x" } })],
        },
      ]),
    );
    // The slide plays.
    expect(stage().getByText("Still here")).toBeInTheDocument();
    pressTab(/Not playing/);
    expect(screen.getByText("Slide 1: layer 2")).toBeInTheDocument();
  });

  it("says so plainly when NOTHING in the cast would play", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([{ id: "bad", layers: [] }]));
    expect(screen.getByText("Nothing in this cast would play.")).toBeInTheDocument();
    expect(
      screen.getByText("Nothing left to draw — every layer on it was dropped, or it never had one."),
    ).toBeInTheDocument();
  });

  it("flags a dwell the player CLAMPS, so 100ms is not read as 100ms", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([slide("a", { duration_ms: 100 })]));
    expect(screen.getByText("clamped")).toBeInTheDocument();
    expect(screen.getByText(/\/ 0\.5s/)).toBeInTheDocument();
  });
});

describe("working a nav and a ping with the remote", () => {
  const menuCast = castOf(
    [
      {
        id: "home",
        layers: [
          layer({ text: "Home" }),
          layer({
            kind: "nav",
            x: 200,
            y: 800,
            w: 900,
            h: 120,
            items: [
              { label: "Hours", target_slide_id: "hours" },
              { label: "Map", target_slide_id: "map" },
            ],
          }),
          layer({ kind: "ping", x: 1500, y: 800, w: 300, h: 120, text: "Call staff", ping_name: "front_desk" }),
        ],
      },
      slide("hours"),
      slide("map"),
    ],
    60_000,
  );

  function withRemote() {
    const view = renderPlayer(menuCast);
    pressTab("Remote");
    press("Turn on the remote");
    return view;
  }

  it("lists every focusable region, in the order the player registers them", () => {
    vi.useFakeTimers();
    withRemote();
    const items = screen.getAllByRole("button", { name: /Menu item|Button fires/ });
    expect(items.map((b) => b.textContent)).toEqual([
      "Menu item “Hours” → jumps to slide 2",
      "Menu item “Map” → jumps to slide 3",
      "Button fires screen.interaction with interaction = “front_desk”",
    ]);
  });

  it("DRAWS one focus ring, and moves it with the arrow keys", () => {
    vi.useFakeTimers();
    const { container } = withRemote();
    // ONE ring on the stage, not an outline per interactive layer the way the
    // editor canvas draws it: the wall shows exactly one.
    const rings = () => container.querySelectorAll('[data-slot="player-focus-ring"]');
    expect(rings()).toHaveLength(1);

    // Scoped to the panel: the FILMSTRIP also marks its playing slide with
    // aria-current, and it means a different thing there.
    const focused = () =>
      within(document.querySelector('[data-slot="preview-panels"]') as HTMLElement)
        .getAllByRole("button")
        .filter((b) => b.getAttribute("aria-current") === "true")
        .map((b) => b.textContent);

    // Focus lands on the FIRST nav item — index 0 in Z-order, as renderSlide's
    // setFocusIndex(0) does, not on whatever is nearest a corner.
    expect(focused()).toEqual(["Menu item “Hours” → jumps to slide 2"]);
    key("ArrowRight");
    expect(focused()).toEqual(["Menu item “Map” → jumps to slide 3"]);
    // And on across the gap to the ping button, which is a different LAYER —
    // spatial traversal does not stop at the menu's edge.
    key("ArrowRight");
    expect(focused()).toEqual(["Button fires screen.interaction with interaction = “front_desk”"]);
    // Still exactly one ring after two moves.
    expect(rings()).toHaveLength(1);
  });

  it("JUMPS to the target slide when OK is pressed on a menu item", () => {
    vi.useFakeTimers();
    withRemote();
    press("Press OK");
    expect(currentSlide()).toHaveTextContent("Slide hours");
  });

  it("jumps by SLIDE ID, so reordering the deck does not break the menu", () => {
    // jumpToSlideId scans for the id and never uses an index. Put the target
    // last and the jump must still land on it.
    vi.useFakeTimers();
    renderPlayer(
      castOf(
        [
          {
            id: "home",
            layers: [
              layer({
                kind: "nav",
                w: 900,
                h: 120,
                items: [{ label: "Go", target_slide_id: "target" }],
              }),
            ],
          },
          slide("filler"),
          slide("target"),
        ],
        60_000,
      ),
    );
    pressTab("Remote");
    press("Turn on the remote");
    press("Press OK");
    expect(currentSlide()).toHaveTextContent("Slide target");
  });

  it("LOGS what a ping would raise, and says nothing was sent", () => {
    vi.useFakeTimers();
    withRemote();
    key("ArrowRight");
    key("ArrowRight");
    key("Enter");
    expect(
      screen.getByText("Button fires screen.interaction with interaction = “front_desk”", {
        selector: '[data-slot="preview-press-log"] li',
      }),
    ).toBeInTheDocument();
    expect(screen.getByText(/Nothing is sent/)).toBeInTheDocument();
  });

  it("RE-ARMS the dwell on a press, so a menu is not pulled out from under you", () => {
    // wvRestartDwell. This is the behaviour that makes a nav usable on a wall,
    // and a preview that let the slide turn over mid-menu would misreport it.
    vi.useFakeTimers();
    renderPlayer(
      castOf(
        [
          {
            id: "home",
            layers: [layer({ kind: "ping", w: 300, h: 120, text: "Call", ping_name: "call" })],
          },
          slide("next"),
        ],
        1000,
      ),
    );
    pressTab("Remote");
    press("Turn on the remote");
    tick(900);
    press("Press OK");
    // Without the re-arm the slide would turn over 100ms from here.
    tick(600);
    expect(stage().getByText("Call")).toBeInTheDocument();
  });

  it("reports a menu item pointing at a slide that does not play, rather than doing nothing quietly", () => {
    vi.useFakeTimers();
    renderPlayer(
      castOf(
        [
          {
            id: "home",
            layers: [
              layer({ kind: "nav", w: 900, h: 120, items: [{ label: "Gone", target_slide_id: "nope" }] }),
            ],
          },
        ],
        60_000,
      ),
    );
    pressTab("Remote");
    press("Turn on the remote");
    press("Press OK");
    expect(screen.getByText(/nothing happens: no such slide plays in this cast/)).toBeInTheDocument();
  });

  it("does not offer a remote for a slide the WIRE refuses as unaimable", () => {
    // A 300×40 pressable layer is below wire.MinInteractiveSide, so the serve
    // gate refuses the slide and no screen ever receives it. The preview reports
    // that in the not-playing list rather than playing it with a caution — a
    // caution about a slide that never airs is advice about nothing.
    vi.useFakeTimers();
    renderPlayer(
      castOf([{ id: "s", layers: [layer({ kind: "ping", w: 300, h: 40, text: "Tiny", ping_name: "tiny" })] }], 60_000),
    );
    expect(screen.getByText("Nothing in this cast would play.")).toBeInTheDocument();
    expect(screen.getByText(/at least 48/)).toBeInTheDocument();
  });

  it("does NOT steer the deck with the arrows while the remote is on", () => {
    // The arrows are the D-pad then, not prev/next — the wall's meaning wins.
    vi.useFakeTimers();
    withRemote();
    key("ArrowRight");
    // Still on the menu slide: the arrow moved FOCUS, not the deck.
    expect(stage().getByText("Home")).toBeInTheDocument();
  });

  it("says nothing is focusable on a slide that carries no interactive layer", () => {
    vi.useFakeTimers();
    renderPlayer(castOf([slide("plain")], 60_000));
    pressTab("Remote");
    press("Turn on the remote");
    expect(screen.getByText(/Nothing on this slide is focusable/)).toBeInTheDocument();
  });
});
