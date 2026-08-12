import { describe, it, expect } from "vitest";
import type { CastSlide, SlideLayer } from "@/api";
import {
  PLAYER_DEFAULT_DWELL_MS,
  PLAYER_MIN_DWELL_MS,
  advance,
  dwellSource,
  initialTransport,
  isBelowInteractiveFloor,
  nextIndex,
  playerDwellMs,
  projectCast,
  restartDwell,
  scrubTo,
  seekTo,
  statedDwellMs,
  togglePlay,
} from "./playback";

/**
 * The playback model, proved against the RULES IT MIRRORS rather than against a
 * rendered page.
 *
 * Every case here names the Go or BrightScript it is a mirror of, because that
 * is the only thing that makes it a real test: an assertion that
 * `playerDwellMs` returns 8000 proves nothing unless 8000 is the number
 * `wvDefaultImageDurationMs` returns. Where the mirrored source is quoted, the
 * quote is the test's oracle.
 */

const layer = (over: Partial<SlideLayer> = {}): SlideLayer => ({
  kind: "text",
  x: 0,
  y: 0,
  w: 400,
  h: 100,
  text: "Hello",
  ...over,
});

const slide = (over: Partial<CastSlide> = {}): CastSlide => ({
  id: "s1",
  layers: [layer()],
  ...over,
});

describe("dwell — the four-step resolution, then the player's own clamp", () => {
  it("prefers the slide's own duration_ms (datamodel.SlideDwellMS step 1)", () => {
    expect(playerDwellMs(slide({ duration_ms: 12_000 }), { default_duration_ms: 5000 })).toBe(12_000);
    expect(dwellSource(slide({ duration_ms: 12_000 }), { default_duration_ms: 5000 })).toBe("slide");
  });

  it("falls back to the cast's default_duration_ms (step 3)", () => {
    expect(playerDwellMs(slide(), { default_duration_ms: 5000 })).toBe(5000);
    expect(dwellSource(slide(), { default_duration_ms: 5000 })).toBe("cast");
  });

  it("falls back to the PLAYER's own 8000ms when nothing states one", () => {
    // wvDefaultImageDurationMs() → 8000 (PhotonScene.brs:582). PLY-083b fixes no
    // contract-level default, so this number is the player's and not the wire's:
    // statedDwellMs reports 0 (the projection omits the key) while the wall
    // still holds the slide for eight seconds.
    expect(statedDwellMs(slide(), {})).toBe(0);
    expect(playerDwellMs(slide(), {})).toBe(PLAYER_DEFAULT_DWELL_MS);
    expect(PLAYER_DEFAULT_DWELL_MS).toBe(8000);
    expect(dwellSource(slide(), {})).toBe("player-default");
  });

  it("raises a positive dwell below the floor up to it, rather than honouring it", () => {
    // wvClampCastDurationMs (PhotonScene.brs:480): `< wvMinCastTimerDurationMs()`
    // → the floor, which is 500. An operator CAN author 100 — the data model only
    // requires positive — and the wall holds it for half a second. A preview that
    // flicked past it would send them to fix a problem they do not have.
    expect(playerDwellMs(slide({ duration_ms: 100 }), {})).toBe(PLAYER_MIN_DWELL_MS);
    expect(PLAYER_MIN_DWELL_MS).toBe(500);
    // Exactly at the floor is not below it.
    expect(playerDwellMs(slide({ duration_ms: 500 }), {})).toBe(500);
  });

  it("treats a cast default below the floor the same way a slide duration is treated", () => {
    // The clamp is applied by the PLAYER, after the resolution — so it catches a
    // short value whichever step produced it. A clamp applied only to the slide's
    // own field would let a 50ms cast-wide default through.
    expect(playerDwellMs(slide(), { default_duration_ms: 50 })).toBe(PLAYER_MIN_DWELL_MS);
  });

  it("ignores a zero or absent duration_ms rather than treating it as a dwell", () => {
    expect(playerDwellMs(slide({ duration_ms: 0 }), { default_duration_ms: 3000 })).toBe(3000);
  });

  it("ignores a null cast default — the value a PATCH clears with", () => {
    expect(playerDwellMs(slide(), { default_duration_ms: null })).toBe(PLAYER_DEFAULT_DWELL_MS);
  });
});

describe("projection — which slides a screen is actually sent", () => {
  it("carries an ordinary slide through with its dwell resolved", () => {
    const program = projectCast({ slides: [slide({ duration_ms: 4000 })], default_duration_ms: null });
    expect(program.skipped).toEqual([]);
    expect(program.slides).toHaveLength(1);
    expect(program.slides[0]).toMatchObject({ id: "s1", authoredIndex: 0, dwellMs: 4000, droppedLayers: [] });
  });

  it("DROPS an unrendered derive layer and still plays the slide", () => {
    // screenprograms.go:496 — "A layer whose PNG has not been rendered yet is
    // dropped rather than dropping the whole slide." The Studio draws a CSS
    // approximation of it; the wall shows nothing there at all.
    const program = projectCast({
      slides: [
        slide({
          layers: [
            layer({ kind: "rect", color: "#101020", w: 1920, h: 1080 }),
            layer({ kind: "derive", derive: { kind: "qr", data: "https://example.test" } }),
          ],
        }),
      ],
      default_duration_ms: null,
    });
    expect(program.skipped).toEqual([]);
    expect(program.slides[0].droppedLayers).toEqual([1]);
    expect(program.slides[0].layers.map((l) => l.kind)).toEqual(["rect"]);
  });

  it("rewrites a RENDERED derive into the plain image the player draws", () => {
    // wire.DeriveProjection (derive.go:508) constructs the projected layer field
    // by field — geometry and asset only.
    const program = projectCast({
      slides: [
        slide({
          layers: [
            layer({
              kind: "derive",
              x: 40,
              y: 60,
              w: 300,
              h: 300,
              asset_ref: "sha256:aa11bb22cc33",
              derive: { kind: "qr", data: "x" },
            }),
          ],
        }),
      ],
      default_duration_ms: null,
    });
    expect(program.slides[0].layers[0]).toEqual({
      kind: "image",
      x: 40,
      y: 60,
      w: 300,
      h: 300,
      asset_ref: "sha256:aa11bb22cc33",
    });
  });

  it("does NOT carry a ping_name off a derive layer, because the projection does not", () => {
    // The projected Layer is built member by member in derive.go:521-529, so a
    // PingName hung on a derive layer never reaches the wire. Spreading the
    // original here would have the preview draw a pressable affordance on a
    // layer the TV cannot focus — a fidelity lie in the direction that costs an
    // operator a working button.
    const program = projectCast({
      slides: [
        slide({
          layers: [layer({ kind: "derive", asset_ref: "sha256:aa11", ping_name: "front_desk", derive: { kind: "rect" } })],
        }),
      ],
      default_duration_ms: null,
    });
    expect(program.slides[0].layers[0].ping_name).toBeUndefined();
  });

  it("SKIPS a whole slide whose only layer was an unrendered derive", () => {
    // Dropping every layer leaves an empty stack, which wire.validateSlideLayers
    // refuses ("slide has no layers") — and the projector drops the slide.
    const program = projectCast({
      slides: [slide({ id: "empty", layers: [layer({ kind: "derive", derive: { kind: "rect" } })] })],
      default_duration_ms: null,
    });
    expect(program.slides).toEqual([]);
    expect(program.skipped).toEqual([
      { id: "empty", authoredIndex: 0, reason: "Nothing left to draw — every layer on it was dropped, or it never had one." },
    ]);
  });

  it("SKIPS a slide the serve gate would refuse, and names the offending layer", () => {
    const program = projectCast({
      slides: [
        slide({ id: "good" }),
        slide({ id: "bad", layers: [layer({ kind: "image", asset_ref: "" })] }),
      ],
      default_duration_ms: null,
    });
    expect(program.slides.map((s) => s.id)).toEqual(["good"]);
    expect(program.skipped).toHaveLength(1);
    expect(program.skipped[0].id).toBe("bad");
    expect(program.skipped[0].reason).toMatch(/Layer 1 would be refused/);
  });

  it("keeps the AUTHORED index on a slide that survives a skip before it", () => {
    // The Studio's filmstrip numbers the authored deck. A preview that renumbered
    // after dropping one would say "slide 2" about the operator's slide 3, and
    // they would go and edit the wrong one.
    const program = projectCast({
      slides: [
        slide({ id: "bad", layers: [layer({ kind: "text", text: "" })] }),
        slide({ id: "good" }),
      ],
      default_duration_ms: null,
    });
    expect(program.slides).toHaveLength(1);
    expect(program.slides[0]).toMatchObject({ id: "good", authoredIndex: 1 });
  });

  it("SKIPS a slide naming bytes by a reference the origin cannot sign", () => {
    // contenturl.Signer.Mint returns no URL for a digest that is not lowercase
    // hex (contenturl.go:394-406, isLowerHex :253), and the SERVE gate then
    // refuses the slide for having no url. This is the one rule the console's
    // authoring mirror does not carry.
    const program = projectCast({
      slides: [slide({ id: "shouty", layers: [layer({ kind: "image", asset_ref: "sha256:AA11BB" })] })],
      default_duration_ms: null,
    });
    expect(program.slides).toEqual([]);
    expect(program.skipped[0].reason).toMatch(/cannot sign/);
  });

  it("accepts a short lowercase digest, because isLowerHex imposes no length", () => {
    // A mirror stricter than the server reports a slide that plays perfectly as
    // one that will never play. The fixture digests in this repo are short.
    const program = projectCast({
      slides: [slide({ layers: [layer({ kind: "image", asset_ref: "sha256:aa11bb22cc33" })] })],
      default_duration_ms: null,
    });
    expect(program.skipped).toEqual([]);
    expect(program.slides).toHaveLength(1);
  });

  it("accepts a bare hex digest with no sha256: prefix, because Mint trims and accepts one", () => {
    const program = projectCast({
      slides: [slide({ layers: [layer({ kind: "image", asset_ref: "aa11bb22cc33" })] })],
      default_duration_ms: null,
    });
    expect(program.skipped).toEqual([]);
  });

  it("reports an empty cast as an empty program rather than throwing", () => {
    expect(projectCast({ slides: [], default_duration_ms: null })).toEqual({ slides: [], skipped: [] });
  });
});

describe("the cycle — advanceCast, and the one place the preview diverges", () => {
  it("wraps after the last slide, the way the player always does", () => {
    // PhotonScene.brs:494 — `(m.castIndex + 1) mod count`, PLY-083a's
    // "continuously repeating cycle". There is no way to ask a wall not to loop.
    expect(nextIndex(2, 3, true)).toBe(0);
    expect(nextIndex(0, 3, true)).toBe(1);
  });

  it("stops at the end when the OPERATOR turned looping off — a preview-only state", () => {
    expect(nextIndex(2, 3, false)).toBeNull();
    expect(nextIndex(0, 3, false)).toBe(1);
  });

  it("reports nothing to advance to on an empty program", () => {
    expect(nextIndex(0, 0, true)).toBeNull();
  });
});

describe("advance — wall time into slide changes", () => {
  const slides = projectCast({
    slides: [
      slide({ id: "a", duration_ms: 1000 }),
      slide({ id: "b", duration_ms: 2000 }),
      slide({ id: "c", duration_ms: 1000 }),
    ],
    default_duration_ms: null,
  }).slides;

  it("accumulates within a slide without advancing", () => {
    const s = advance(initialTransport(), slides, 400);
    expect(s).toMatchObject({ index: 0, elapsedMs: 400 });
  });

  it("advances exactly when the dwell is reached, not a frame later", () => {
    const s = advance(initialTransport({ elapsedMs: 600 }), slides, 400);
    expect(s).toMatchObject({ index: 1, elapsedMs: 0 });
  });

  it("carries the OVERSHOOT into the next slide rather than discarding it", () => {
    // A single subtraction throws the leftover away, and the preview drifts
    // further behind the wall the longer it runs.
    const s = advance(initialTransport(), slides, 1300);
    expect(s).toMatchObject({ index: 1, elapsedMs: 300 });
  });

  it("crosses SEVERAL boundaries in one delta — a throttled tab, or a woken lid", () => {
    // 1000 + 2000 + 1000 = 4000 is exactly one full cycle, so 4500 crosses FOUR
    // boundaries and lands 500ms into slide a of the second pass. A player that
    // could only cross one boundary per tick would be sitting on slide b while
    // the wall had already come back round to a.
    const s = advance(initialTransport(), slides, 4500);
    expect(s).toMatchObject({ index: 0, elapsedMs: 500 });
    // And one that crosses two but not four is equally wrong — 2500 is inside b.
    expect(advance(initialTransport(), slides, 2500)).toMatchObject({ index: 1, elapsedMs: 1500 });
  });

  it("does not move while paused", () => {
    const paused = initialTransport({ playing: false });
    expect(advance(paused, slides, 5000)).toBe(paused);
  });

  it("does not move on a zero or negative delta", () => {
    const s = initialTransport({ elapsedMs: 100 });
    expect(advance(s, slides, 0)).toBe(s);
    expect(advance(s, slides, -50)).toBe(s);
  });

  it("does not move once the deck has ended", () => {
    const done = initialTransport({ ended: true, playing: false });
    expect(advance(done, slides, 5000)).toBe(done);
  });

  it("ENDS on the last slide with looping off, rather than snapping back to the first", () => {
    const s = advance(initialTransport({ index: 2, loop: false }), slides, 1200);
    expect(s).toMatchObject({ index: 2, elapsedMs: 1000, playing: false, ended: true });
  });

  it("keeps cycling forever with looping on, however long the delta", () => {
    const s = advance(initialTransport({ index: 2 }), slides, 1200);
    expect(s).toMatchObject({ index: 0, elapsedMs: 200, ended: false });
  });

  it("lands cleanly on a boundary without spinning when the delta ends exactly there", () => {
    const s = advance(initialTransport(), slides, 1000);
    expect(s).toMatchObject({ index: 1, elapsedMs: 0 });
  });

  it("survives an empty program", () => {
    const s = initialTransport();
    expect(advance(s, [], 1000)).toBe(s);
  });
});

describe("transport commands", () => {
  const slides = projectCast({
    slides: [slide({ id: "a", duration_ms: 1000 }), slide({ id: "b", duration_ms: 2000 })],
    default_duration_ms: null,
  }).slides;

  it("seeks to a slide and restarts its dwell", () => {
    expect(seekTo(initialTransport({ elapsedMs: 700 }), slides, 1)).toMatchObject({ index: 1, elapsedMs: 0 });
  });

  it("wraps a seek past the end, and a seek before the start", () => {
    expect(seekTo(initialTransport(), slides, 2).index).toBe(0);
    expect(seekTo(initialTransport(), slides, -1).index).toBe(1);
  });

  it("clears ENDED on a seek, so Next on a finished deck keeps going", () => {
    const done = initialTransport({ index: 1, ended: true, playing: false });
    expect(seekTo(done, slides, 0)).toMatchObject({ index: 0, ended: false });
  });

  it("scrubs within the current slide and clamps to its dwell", () => {
    expect(scrubTo(initialTransport(), slides, 400).elapsedMs).toBe(400);
    expect(scrubTo(initialTransport(), slides, 99_999).elapsedMs).toBe(1000);
    expect(scrubTo(initialTransport(), slides, -5).elapsedMs).toBe(0);
  });

  it("scrubs against the CURRENT slide's dwell, not the first one's", () => {
    expect(scrubTo(initialTransport({ index: 1 }), slides, 99_999).elapsedMs).toBe(2000);
  });

  it("toggles play and pause", () => {
    expect(togglePlay(initialTransport()).playing).toBe(false);
    expect(togglePlay(initialTransport({ playing: false })).playing).toBe(true);
  });

  it("RESTARTS a finished deck rather than resuming onto a spent last frame", () => {
    // Resuming with elapsed === dwell advances on the very next tick, which reads
    // as the button having done nothing.
    const done = initialTransport({ index: 1, elapsedMs: 2000, playing: false, ended: true });
    expect(togglePlay(done)).toMatchObject({ index: 0, elapsedMs: 0, playing: true, ended: false });
  });

  it("re-arms the dwell without moving — the player's wvRestartDwell", () => {
    // PhotonScene.brs:1181. Somebody working a menu must not have the slide
    // pulled out from under them.
    const s = restartDwell(initialTransport({ index: 1, elapsedMs: 1900 }));
    expect(s).toMatchObject({ index: 1, elapsedMs: 0 });
  });
});

describe("the interactive floor", () => {
  it("flags a region below wire.MinInteractiveSide on either side", () => {
    expect(isBelowInteractiveFloor([0, 0, 47, 200], 48)).toBe(true);
    expect(isBelowInteractiveFloor([0, 0, 200, 47], 48)).toBe(true);
    expect(isBelowInteractiveFloor([0, 0, 48, 48], 48)).toBe(false);
  });
});
