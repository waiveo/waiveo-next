import {
  SLIDE_CANVAS_HEIGHT,
  SLIDE_CANVAS_WIDTH,
  isContentKind,
  validateSlide,
  type Cast,
  type CastSlide,
  type SlideLayer,
} from "@/api";

/**
 * The cast-preview PLAYBACK MODEL: what a cast becomes when a box projects it
 * for a screen, and how a transport walks that projection in time.
 *
 * # Why this is a module and not a component's state
 *
 * The one thing a preview must not do is agree with the editor. The editor draws
 * every slide an operator authored; a SCREEN plays the subset the projector
 * emits, for the dwell times the projector resolves, in the order the player
 * cycles. Those three facts live in Go — `internal/feeder/snapshot`
 * (`resolveLayers`/`castContent`), `internal/datamodel` (`SlideDwellMS`) and
 * `player-v3/components/PhotonScene.brs` (`wvClampCastDurationMs`, `advanceCast`)
 * — and this file is their console-side mirror, transcribed rule for rule with
 * the Go named beside each one.
 *
 * Keeping it separate from the surface is what makes it TESTABLE against those
 * rules rather than against a rendered DOM. A timed player is exactly the kind of
 * thing whose component test passes while the real page never advances, so the
 * arithmetic is proved here and the surface is proved by clicking it.
 *
 * # What this mirror is, and is not
 *
 * It is faithful about WHICH SLIDES PLAY, IN WHAT ORDER, and FOR HOW LONG. Those
 * are decided by rules this file can execute exactly.
 *
 * It says nothing about PIXELS. The wall is painted by BrightScript
 * (`player-v3/`); the console draws with `SlideStage`. They are two
 * implementations of one contract and they have disagreed in production. See
 * `fidelity.ts`, which is the ledger of where the preview is known to differ —
 * and which the preview SHOWS rather than hides.
 */

// ── The player's own timing constants ────────────────────────────────────────

/**
 * The dwell a slide gets when nothing in the chain states one — the PLAYER's own
 * default, not a contract-level one.
 *
 * `player-v3/components/PhotonScene.brs:582` `wvDefaultImageDurationMs()` returns
 * 8000, and its comment says why the number is 8000 rather than anything else:
 * PLY-083b fixes no contract default, so the player picked the same reasonable
 * signage dwell the feeder's demo cast uses. A preview that invented its own
 * default here would show a slide holding for a length of time no wall ever
 * gives it.
 */
export const PLAYER_DEFAULT_DWELL_MS = 8000;

/**
 * The floor the player clamps a positive dwell up to
 * (`PhotonScene.brs:488` `wvMinCastTimerDurationMs()`).
 *
 * It matters to a preview more than it looks. An operator can author
 * `duration_ms: 100` and the console will store it — the data model only
 * requires it be positive — but the TV holds that slide for half a second. A
 * preview that honoured the 100 would flick past a slide the wall dwells on, and
 * the operator would "fix" a problem that does not exist.
 */
export const PLAYER_MIN_DWELL_MS = 500;

/**
 * One slide's dwell in milliseconds, exactly as the wall will time it.
 *
 * Two Go rules composed, in the order the real path composes them:
 *
 *  1. `datamodel.SlideDwellMS(slide, cast, itemDurationMS)` (rows.go:462) —
 *     DAT-042's resolution order: slide `duration_ms`, then the referencing
 *     playlist item's `duration_seconds`, then the cast's `default_duration_ms`,
 *     then 0 meaning "state nothing".
 *  2. `wvClampCastDurationMs` on the player (PhotonScene.brs:480) — a stated 0
 *     becomes the player's default; a positive value below the floor is raised
 *     to it.
 *
 * `itemDurationMS` is 0 here and that is not a simplification: a cast preview is
 * the projection of a cast with no playlist item to carry an override, which is
 * precisely the shape `overrideProgram` (screenprograms.go:247) already has —
 * "it takes no duration override … so each slide's own `duration_ms` governs,
 * falling back to the cast's `default_duration_ms` and then the player's own
 * default". Previewing a cast AS SCHEDULED, where an item's `duration_seconds`
 * could outrank the cast default, is a different question this surface does not
 * claim to answer, and the fidelity ledger says so.
 */
export function playerDwellMs(slide: CastSlide, cast: Pick<Cast, "default_duration_ms">): number {
  const stated = statedDwellMs(slide, cast);
  if (stated <= 0) return PLAYER_DEFAULT_DWELL_MS;
  if (stated < PLAYER_MIN_DWELL_MS) return PLAYER_MIN_DWELL_MS;
  return stated;
}

/**
 * `datamodel.SlideDwellMS` alone — what the WIRE will carry, before the player
 * applies its own default and floor. 0 means "the projection omits the key".
 *
 * Exported because the difference between this and `playerDwellMs` is the thing
 * a transport has to be able to EXPLAIN: an operator who set nothing and sees 8
 * seconds needs to be told the 8 came from the player, not from their cast.
 */
export function statedDwellMs(slide: CastSlide, cast: Pick<Cast, "default_duration_ms">): number {
  if ((slide.duration_ms ?? 0) > 0) return slide.duration_ms!;
  if ((cast.default_duration_ms ?? 0) > 0) return cast.default_duration_ms!;
  return 0;
}

/** Where a slide's dwell came from — the three answers `playerDwellMs` composes
 * from, so the transport can name the source instead of just the number. */
export type DwellSource = "slide" | "cast" | "player-default";

export function dwellSource(slide: CastSlide, cast: Pick<Cast, "default_duration_ms">): DwellSource {
  if ((slide.duration_ms ?? 0) > 0) return "slide";
  if ((cast.default_duration_ms ?? 0) > 0) return "cast";
  return "player-default";
}

// ── The projection: which slides reach a screen at all ───────────────────────

/** A `sha256:` reference the content origin can mint a fetch URL for.
 *
 * `contenturl.Signer.Mint` (contenturl.go:390) trims the `sha256:` prefix and
 * hands the rest to `URL(...)`, which refuses anything that is not lowercase
 * hex and returns no URL at all — deliberately, because "the UNSIGNED form
 * instead would be worse … it 403s, which reads to an operator as an
 * authorization fault rather than as the malformed asset_ref it is". No URL then
 * fails the SERVE gate, and the whole slide is dropped.
 *
 * LOWERCASE HEX OF ANY LENGTH, not 64 characters. `isLowerHex`
 * (contenturl.go:253) imposes no length at all, and a mirror stricter than the
 * server is not a safety margin here any more than it is in `validateSlide`: it
 * would report a slide the box happily serves as one that will never play, and
 * send an operator to fix an asset that is fine. The rule it DOES carry is the
 * one that is real — the case fold — because "two spellings of one digest that
 * both verify would be two distinct URLs for the same signed capability". */
const MINTABLE_ASSET_REF = /^(sha256:)?[0-9a-f]+$/;

/** One slide as it reaches the wire, with what the projection did to it. */
export interface ProjectedSlide {
  /** The cast-local id — the same value a `nav` item targets. */
  id: string;
  /** Its position in the AUTHORED deck, which is what the Studio's filmstrip
   * numbers. Kept because the played order skips slides, so "slide 3 of 5" in
   * the preview and "slide 4" in the editor must be reconcilable. */
  authoredIndex: number;
  /** How long the player holds it. Already clamped — see `playerDwellMs`. */
  dwellMs: number;
  /** The layer stack the wire carries: derive layers rewritten into the `image`
   * they become (`wire.DeriveProjection`), unrendered ones gone. */
  layers: SlideLayer[];
  /** Authored layer indices the projection OMITTED — an unrendered `derive`.
   * The slide still plays; that layer is simply not on it. */
  droppedLayers: number[];
}

/** A slide that will not reach a screen at all, and the reason in the operator's
 * terms rather than the validator's. */
export interface SkippedSlide {
  id: string;
  authoredIndex: number;
  reason: string;
}

/**
 * What a screen would actually be sent for this cast.
 *
 * The two halves are separate on purpose: `slides` is what plays, `skipped` is
 * what an operator has to be TOLD about. A preview that quietly played four of
 * five slides would reproduce the exact silence this projection is famous for —
 * `castContent`'s own comment says a malformed slide "costs its own slot and not
 * the whole cast", and nothing anywhere tells the author.
 */
export interface Program {
  slides: ProjectedSlide[];
  skipped: SkippedSlide[];
}

/**
 * Project a cast the way `internal/feeder/snapshot.castContent` does for a
 * screen with no playlist item — the `overrideProgram` shape.
 *
 * Per slide, in the projector's own order (`resolveLayers`,
 * screenprograms.go:488):
 *
 *  1. Every layer through `wire.DeriveProjection`: a non-derive passes through;
 *     a derive WITH an `asset_ref` becomes a plain `image` at the same geometry;
 *     a derive with none is DROPPED — "a layer whose PNG has not been rendered
 *     yet is dropped rather than dropping the whole slide".
 *  2. Content-bearing layers have their URL minted. A ref that will not sign
 *     mints nothing, and the serve gate then refuses the slide.
 *  3. `wire.ValidateSlideLayers` over what is left. Failure drops the WHOLE
 *     SLIDE — silently, on the box.
 *
 * It deliberately takes NO content-origin listing, and the omission is the
 * decision rather than an oversight. Bytes absent from the origin do not drop a
 * slide: `Mint` (contenturl.go:390) signs any well-formed digest without asking
 * whether the origin holds it, so the projection succeeds and the PLAYER gets a
 * URL that 404s — a fetch failure at the screen, which the player degrades per
 * layer (PLY-087), not a projection failure at the box. Feeding the listing in
 * here so that "missing bytes" could join this list would be a different lie
 * from the one it was trying to fix: the stage draws the player's own degraded
 * placeholder for that case, which is what the wall does.
 */
export function projectCast(cast: Pick<Cast, "slides" | "default_duration_ms">): Program {
  const slides: ProjectedSlide[] = [];
  const skipped: SkippedSlide[] = [];

  cast.slides.forEach((slide, authoredIndex) => {
    const layers: SlideLayer[] = [];
    const droppedLayers: number[] = [];

    slide.layers.forEach((layer, i) => {
      if (layer.kind === "derive") {
        if (!layer.asset_ref) {
          droppedLayers.push(i);
          return;
        }
        // wire.DeriveProjection: geometry and asset survive, EVERYTHING ELSE
        // does not — the projected layer is constructed field by field, so a
        // `ping_name` hung on a derive layer never reaches the wire. Mirrored
        // literally rather than by spreading the original, because spreading
        // would carry members the wall will never see and the preview would
        // then draw an interactive affordance on a layer that is inert there.
        layers.push({ kind: "image", x: layer.x, y: layer.y, w: layer.w, h: layer.h, asset_ref: layer.asset_ref });
        return;
      }
      layers.push(layer);
    });

    const reason = slideRefusal(slide.id, layers);
    if (reason !== null) {
      skipped.push({ id: slide.id, authoredIndex, reason });
      return;
    }
    slides.push({
      id: slide.id,
      authoredIndex,
      dwellMs: playerDwellMs(slide, cast),
      layers,
      droppedLayers,
    });
  });

  return { slides, skipped };
}

/** Why the serve gate would refuse this projected stack, or `null` if it would
 * not. The message is what an operator can act on, not the validator's text. */
function slideRefusal(id: string, layers: SlideLayer[]): string | null {
  if (layers.length === 0) {
    return "Nothing left to draw — every layer on it was dropped, or it never had one.";
  }
  // The console's mirror of the authoring gate. It is the same gate the serve
  // side runs (`wire.validateSlideLayers`) bar one rule, so running it here is
  // running the projector's own refusal — see validateSlide's doc for why the
  // console mirrors the authoring arm rather than the serving one.
  const problems = validateSlide({ id, layers });
  if (problems.length > 0) {
    const first = problems[0];
    return first.index === null
      ? first.message
      : `Layer ${first.index + 1} would be refused: ${first.message}`;
  }
  // The one serve-side rule the authoring mirror does not carry: a content
  // layer's URL is minted at projection time, and a ref the signer will not
  // sign mints nothing — which the serve gate refuses, taking the slide with it.
  const bad = layers.findIndex((l) => isContentKind(l.kind) && !MINTABLE_ASSET_REF.test(l.asset_ref ?? ""));
  if (bad >= 0) {
    return `Layer ${bad + 1} names bytes by a reference the content origin cannot sign, so the box cannot build a fetch URL for it.`;
  }
  return null;
}

// ── The transport ────────────────────────────────────────────────────────────

/**
 * The player's own cycle: after the last item, back to index 0.
 * `PhotonScene.brs:494` `advanceCast` — `(m.castIndex + 1) mod count`, which
 * PLY-083a calls a "continuously repeating cycle".
 *
 * This is why `loop` on the transport is a PREVIEW affordance and not a cast
 * setting: the wall always loops, with no way to ask it not to. Turning it off
 * here stops the preview at the end so an operator can study the last slide —
 * and the transport says that is what it is doing rather than implying the
 * screen would stop too.
 */
export function nextIndex(index: number, count: number, loop: boolean): number | null {
  if (count <= 0) return null;
  const next = index + 1;
  if (next < count) return next;
  return loop ? 0 : null;
}

/** The transport's own state. Deliberately plain data: every transition below is
 * a pure function of it, so the surface holds one value and the rules are
 * provable without a DOM. */
export interface Transport {
  /** Index into `Program.slides` — the PLAYED order, not the authored one. */
  index: number;
  /** How far into the current slide's dwell, in ms. */
  elapsedMs: number;
  playing: boolean;
  loop: boolean;
  /** Set when playback ran off the end with `loop` off: the deck is finished and
   * the surface says so rather than sitting on a paused last frame that looks
   * identical to a stall. */
  ended: boolean;
}

export function initialTransport(over: Partial<Transport> = {}): Transport {
  return { index: 0, elapsedMs: 0, playing: true, loop: true, ended: false, ...over };
}

/**
 * Advance the transport by `deltaMs` of wall time, crossing as many slide
 * boundaries as that delta actually spans.
 *
 * The loop rather than a single subtraction is the load-bearing part. A
 * background tab throttles rAF to about once a second and a laptop lid produces
 * a delta of minutes; with a single subtraction the leftover time is thrown away
 * and the preview drifts further behind the wall the longer it runs. A cast of
 * half-second slides needs to cross several boundaries in one 5-second delta,
 * and it does.
 *
 * The `remaining` accumulator is bounded by the dwell floor
 * (`PLAYER_MIN_DWELL_MS` — every dwell is at least 500ms), so the worst case is
 * delta/500 iterations rather than unbounded.
 */
export function advance(state: Transport, slides: readonly ProjectedSlide[], deltaMs: number): Transport {
  if (!state.playing || state.ended || slides.length === 0 || deltaMs <= 0) return state;

  let { index, elapsedMs } = state;
  let remaining = deltaMs;

  for (;;) {
    const dwell = slides[index]?.dwellMs ?? PLAYER_DEFAULT_DWELL_MS;
    const left = dwell - elapsedMs;
    if (remaining < left) return { ...state, index, elapsedMs: elapsedMs + remaining };
    remaining -= left;
    const next = nextIndex(index, slides.length, state.loop);
    if (next === null) {
      // Rest ON the last slide rather than jumping back: the operator asked not
      // to loop, so the thing to leave on screen is the frame they were watching.
      return { ...state, index, elapsedMs: dwell, playing: false, ended: true };
    }
    index = next;
    elapsedMs = 0;
    // A zero-length delta after landing exactly on a boundary must not spin.
    if (remaining === 0) return { ...state, index, elapsedMs: 0 };
  }
}

/** Step to a slide by played-index, wrapping in both directions the way the
 * player's own cycle does. Always clears `ended` — an operator who presses Next
 * on a finished deck means "keep going", not "stay finished". */
export function seekTo(state: Transport, slides: readonly ProjectedSlide[], index: number): Transport {
  if (slides.length === 0) return state;
  const wrapped = ((index % slides.length) + slides.length) % slides.length;
  return { ...state, index: wrapped, elapsedMs: 0, ended: false };
}

/** Scrub within the CURRENT slide's dwell, clamped to it. Separate from `seekTo`
 * because they answer different questions — "which slide" and "how far into
 * it" — and a scrub must not silently roll into the next slide, which would make
 * the handle jump out from under the pointer at the right-hand end. */
export function scrubTo(state: Transport, slides: readonly ProjectedSlide[], elapsedMs: number): Transport {
  const dwell = slides[state.index]?.dwellMs ?? PLAYER_DEFAULT_DWELL_MS;
  return { ...state, elapsedMs: Math.max(0, Math.min(dwell, elapsedMs)), ended: false };
}

/** Play/pause. Resuming a FINISHED deck restarts it from the top, because the
 * alternative — resuming onto a last slide with zero time left — advances
 * instantly and looks like the button did nothing. */
export function togglePlay(state: Transport): Transport {
  if (state.ended) return { ...state, index: 0, elapsedMs: 0, playing: true, ended: false };
  return { ...state, playing: !state.playing };
}

/**
 * Re-arm the current slide's dwell without moving — the console-side twin of the
 * player's `wvRestartDwell`.
 *
 * `PhotonScene.brs:72-75` keeps `m.currentDwellMs` for exactly this: "somebody
 * working a menu must not have the slide pulled" out from under them, so an
 * interaction restarts the timer. A preview that let the dwell run out under a
 * menu the operator is walking would misreport the one behaviour that makes
 * `nav` usable on a wall.
 */
export function restartDwell(state: Transport): Transport {
  return { ...state, elapsedMs: 0, ended: false };
}

// ── Focus: the D-pad, as the player moves it ─────────────────────────────────

/** One focusable region on a slide — a whole layer, or one cell of a `nav`. */
export interface FocusTarget {
  /** Index into the projected slide's `layers`. */
  layerIndex: number;
  /** Which `nav` item, or `null` for a layer focused as a whole. */
  itemIndex: number | null;
  /** Canvas-space box, for drawing the ring where the player would put it. */
  rect: [number, number, number, number];
  /** What pressing OK on it does. */
  press: PressOutcome;
}

/** What the box would see when this target is pressed. */
export type PressOutcome =
  | { kind: "nav"; label: string; targetSlideId: string }
  | { kind: "ping"; pingName: string };

/** Whether a focus target is too small for the player to accept.
 * `wire.MinInteractiveSide` — a legibility floor, because the canvas is shown on
 * a wall and driven from across a room. The Studio's canvas is scaled DOWN,
 * "which is precisely what makes the mistake easy to make here and impossible to
 * see", and a preview scaled down the same way inherits the same blindness. */
export function isBelowInteractiveFloor(rect: [number, number, number, number], floor: number): boolean {
  return rect[2] < floor || rect[3] < floor;
}

/** The canvas the geometry is expressed in, re-exported so the surface does not
 * reach past this module for it. */
export const CANVAS = { width: SLIDE_CANVAS_WIDTH, height: SLIDE_CANVAS_HEIGHT } as const;
