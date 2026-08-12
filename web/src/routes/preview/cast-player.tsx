import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ChevronLeft,
  ChevronRight,
  CircleDot,
  Eye,
  Gamepad2,
  Grid2x2Check,
  Pause,
  Play,
  Repeat,
  Repeat1,
  SkipBack,
  SkipForward,
  TriangleAlert,
} from "lucide-react";
import { Badge, Button, KitIcon, Tab, TabList, TabPanel, Tabs, Tooltip, type BadgeTone } from "@/components/kit";
import { MIN_INTERACTIVE_SIDE, type Cast } from "@/api";
import { TvFrame } from "@/routes/studio/studio-chrome";
import type { AssetUrls } from "@/routes/studio/slide-canvas";
import { cn } from "@/lib/utils";
import { PlayerStage } from "./player-stage";
import { describePress, focusTargets, spatialNeighbor, type Direction } from "./focus";
import { fidelityNotes, fidelitySummary, type FidelityLevel } from "./fidelity";
import {
  CANVAS,
  advance,
  dwellSource,
  initialTransport,
  isBelowInteractiveFloor,
  projectCast,
  restartDwell,
  scrubTo,
  seekTo,
  statedDwellMs,
  togglePlay,
  type FocusTarget,
  type Transport,
} from "./playback";

/**
 * CastPlayer — the cast preview surface. Watch a cast play, see what a screen
 * would actually be sent, and work an authored `nav` or `ping` with a remote
 * without standing in front of a television.
 *
 * # Why this is TSX and not a `ui-schema/1` page
 *
 * Step 1.5 of the parity loop asks for a schema document unless the surface is a
 * direct-manipulation application, and asks the exception to NAME the limits
 * that forced it. Four, all structural rather than catalogue gaps:
 *
 *  1. **No clock.** Nothing in the grammar drives host state from elapsed time.
 *     A `switch`/`repeat`/`text` tree renders what a binding holds; the whole
 *     substance of this page is a 60Hz loop that advances a state machine by
 *     wall-clock delta and repaints a progress bar between renders. There is no
 *     widget kind, action kind or Computed for "tick".
 *  2. **No canvas.** The stage is absolutely-positioned children in a 1920×1080
 *     coordinate space under a `transform: scale()`, with a focus ring built
 *     from four clamped rectangles. This is the same limit the `studio-undo`
 *     track reported and the owner accepted for the Studio.
 *  3. **No keyboard binding.** The D-pad is arrow keys and Enter captured at the
 *     document, guarded so they do not fire while a control has focus. UIS-074
 *     gives a `button` a `press` ActionRef; there is no key binding of any kind.
 *  4. **No media element.** A `video` layer has to actually play, looped and
 *     muted, driven imperatively — `HTMLVideoElement.play()` is not a prop, and
 *     the widget catalog has no media kind.
 *
 * The first is the one that would still force TSX if the other three were closed
 * tomorrow. Everything on the page that ISN'T the stage and the transport —
 * the fidelity ledger, the not-playing list, the interaction log — is an
 * ordinary list of rows and would be perfectly happy as a schema document; they
 * are here because they have to sit inside this frame.
 *
 * # What this preview IS
 *
 * Faithful about WHICH SLIDES PLAY, IN WHAT ORDER and FOR HOW LONG: `playback.ts`
 * executes the projector's own drop rules and the player's own dwell arithmetic.
 * Faithful about WHERE a remote can go: `focus.ts` transcribes the player's
 * focus registration and spatial traversal. As close as a browser gets on
 * LAYOUT: `player-stage.tsx` follows the BrightScript's scaling, alignment,
 * wrapping and placeholder rules rather than the editor's.
 *
 * Not a pixel oracle, and it says so on the page. See `fidelity.ts`.
 */

/** How often the progress readout's text is rewritten. The BAR is repainted
 * every frame (a transform on a ref'd node, no React render); the digits change
 * ten times a second, which is as fast as anyone can read them and keeps the
 * imperative write cheap. */
const READOUT_INTERVAL_MS = 100;

/** A slide's live layers re-render every second, exactly as the player's own
 * one-second slide tick does (`m.slideClockTimer.duration = 1`). Only started
 * when the slide carries a ticking layer, for the same reason: "a slide of
 * static text, images and server-resolved widgets needs no timer running behind
 * it at all". */
const TICKING_KINDS = ["clock", "date", "countdown"];

function useNow(active: boolean): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => setNow(new Date()), 1000);
    return () => clearInterval(id);
  }, [active]);
  return now;
}

/** One press the operator made, newest first. The log is the whole answer to
 * "does my button work": a press that does nothing leaves nothing here. */
interface PressRecord {
  at: number;
  text: string;
  kind: "nav" | "ping";
}

export interface CastPlayerProps {
  /** The cast to play. Takes the LIVE document, so the Studio can hand over
   * unsaved edits — see `preview-overlay` in the Studio route. */
  cast: Pick<Cast, "name" | "slides" | "default_duration_ms">;
  /** The content origin's ref→url listing, or null when its answer is unknown. */
  assetUrls: AssetUrls;
  /** Rendered top-left: the door back to wherever this was opened from. */
  door: React.ReactNode;
  /** Set when the origin listing failed, so the page can say why pictures are
   * black rather than letting the operator conclude the bytes are gone. */
  originError?: string | null;
}

export function CastPlayer({ cast, assetUrls, door, originError = null }: CastPlayerProps) {
  const program = useMemo(() => projectCast(cast), [cast]);
  const slides = program.slides;

  const [transport, setTransport] = useState<Transport>(() => initialTransport());
  const [remote, setRemote] = useState(false);
  const [titleSafe, setTitleSafe] = useState(false);
  const [focusIndex, setFocusIndex] = useState(0);
  const [presses, setPresses] = useState<PressRecord[]>([]);

  // The authoritative transport. React state carries the parts a render depends
  // on (which slide, playing, loop, ended); `elapsedMs` lives here and is
  // painted imperatively, because a state write per animation frame is 60
  // renders a second of a 1920×1080 layer tree — the difference between a
  // preview that plays and one that stutters on the very casts worth previewing.
  const live = useRef<Transport>(transport);
  const barRef = useRef<HTMLDivElement>(null);
  const readoutRef = useRef<HTMLSpanElement>(null);
  const scrubRef = useRef<HTMLInputElement>(null);

  const slide = slides[Math.min(transport.index, Math.max(0, slides.length - 1))] ?? null;
  const dwellMs = slide?.dwellMs ?? 0;

  // Keep the ref and the state in step whenever the state is the thing that
  // moved (a click, a seek, a scrub).
  useEffect(() => {
    live.current = transport;
  }, [transport]);

  const paint = useCallback((elapsed: number, dwell: number) => {
    const pct = dwell > 0 ? Math.min(1, elapsed / dwell) : 0;
    if (barRef.current) barRef.current.style.transform = `scaleX(${pct})`;
    if (scrubRef.current && document.activeElement !== scrubRef.current) {
      scrubRef.current.value = String(Math.round(elapsed));
    }
  }, []);

  // ── The clock ─────────────────────────────────────────────────────────────
  // requestAnimationFrame with a WALL-CLOCK delta, not a fixed increment: a
  // throttled background tab or a woken lid hands over a delta of seconds, and
  // `advance` crosses as many slide boundaries as that delta really spans. A
  // fixed per-frame increment would drift further behind the wall the longer the
  // preview ran, which is the one thing a timing preview must not do.
  useEffect(() => {
    if (!transport.playing || slides.length === 0) return;
    let raf = 0;
    let last = performance.now();
    const step = (t: number) => {
      raf = requestAnimationFrame(step);
      const delta = t - last;
      last = t;
      if (delta <= 0) return;
      const before = live.current;
      const after = advance(before, slides, delta);
      live.current = after;
      paint(after.elapsedMs, slides[after.index]?.dwellMs ?? 0);
      // Re-render ONLY when something a render depends on actually changed.
      if (after.index !== before.index || after.ended !== before.ended) setTransport(after);
    };
    raf = requestAnimationFrame(step);
    return () => cancelAnimationFrame(raf);
  }, [transport.playing, slides, paint]);

  // The digits. Separate from the bar because they change ten times a second
  // and the bar changes sixty; one interval is cheaper than formatting a string
  // in every animation frame.
  useEffect(() => {
    if (!transport.playing) return;
    const id = setInterval(() => {
      if (readoutRef.current) {
        readoutRef.current.textContent = formatSeconds(live.current.elapsedMs);
      }
    }, READOUT_INTERVAL_MS);
    return () => clearInterval(id);
  }, [transport.playing]);

  // A command moved the transport: repaint immediately rather than waiting for
  // the next frame, so a paused Next still moves the bar.
  const commit = useCallback(
    (next: Transport) => {
      live.current = next;
      setTransport(next);
      paint(next.elapsedMs, slides[next.index]?.dwellMs ?? 0);
      if (readoutRef.current) readoutRef.current.textContent = formatSeconds(next.elapsedMs);
    },
    [paint, slides],
  );

  // Landing on a new slide puts focus on its first interactive region, exactly
  // as `renderSlide` does — index 0 in Z-order, not nearest a corner.
  useEffect(() => {
    setFocusIndex(0);
  }, [transport.index]);

  const targets = useMemo(() => (slide ? focusTargets(slide.layers) : []), [slide]);
  const focused: FocusTarget | null = remote ? (targets[focusIndex] ?? null) : null;

  const ticking = slide?.layers.some((l) => TICKING_KINDS.includes(l.kind)) ?? false;
  const now = useNow(ticking);

  // ── Commands ──────────────────────────────────────────────────────────────
  const onPlayPause = useCallback(() => commit(togglePlay(live.current)), [commit]);
  const onPrev = useCallback(() => commit(seekTo(live.current, slides, live.current.index - 1)), [commit, slides]);
  const onNext = useCallback(() => commit(seekTo(live.current, slides, live.current.index + 1)), [commit, slides]);
  const onJump = useCallback((i: number) => commit(seekTo(live.current, slides, i)), [commit, slides]);
  const onLoop = useCallback(
    () => commit({ ...live.current, loop: !live.current.loop, ended: false }),
    [commit],
  );
  const onScrub = useCallback((ms: number) => commit(scrubTo(live.current, slides, ms)), [commit, slides]);

  /** Jump to a slide by its cast-local id, as `jumpToSlideId` does — BY ID,
   * never by index, and a target matching nothing is ignored rather than
   * guessed at. The played order is a subset of the authored one, so an id can
   * legitimately resolve to nothing here: a nav item pointing at a slide the
   * projection dropped goes nowhere on the wall too. */
  const jumpToSlideId = useCallback(
    (id: string): boolean => {
      const i = slides.findIndex((s) => s.id === id);
      if (i < 0) return false;
      commit(seekTo(live.current, slides, i));
      return true;
    },
    [commit, slides],
  );

  const slideLabel = useCallback(
    (id: string) => {
      const i = slides.findIndex((s) => s.id === id);
      if (i >= 0) return `slide ${slides[i].authoredIndex + 1}`;
      return `slide "${id}" — which this cast does not play`;
    },
    [slides],
  );

  const onMove = useCallback(
    (direction: Direction): boolean => {
      const next = spatialNeighbor(targets, focusIndex, direction);
      if (next === null) return false;
      setFocusIndex(next);
      // Any consumed key re-arms the dwell (`wvRestartDwell`): somebody working a
      // menu must not have the slide pulled out from under them.
      commit(restartDwell(live.current));
      return true;
    },
    [targets, focusIndex, commit],
  );

  const onPress = useCallback((): boolean => {
    const target = targets[focusIndex];
    if (!target) return false;
    const at = Date.now();
    if (target.press.kind === "nav") {
      const text = describePress(target.press, slideLabel);
      const ok = jumpToSlideId(target.press.targetSlideId);
      const record: PressRecord = {
        at,
        kind: "nav",
        text: ok ? text : `${text} — nothing happens: no such slide plays in this cast`,
      };
      setPresses((p) => [record, ...p].slice(0, 30));
      if (!ok) commit(restartDwell(live.current));
      return true;
    }
    const record: PressRecord = { at, kind: "ping", text: describePress(target.press, slideLabel) };
    setPresses((p) => [record, ...p].slice(0, 30));
    commit(restartDwell(live.current));
    return true;
  }, [targets, focusIndex, slideLabel, jumpToSlideId, commit]);

  // ── Keyboard ──────────────────────────────────────────────────────────────
  // Bound at the document because this is a full-viewport application surface
  // with no single element that should own the keys. Guarded on the active
  // element so typing in a control never steers the deck — the scrub slider is
  // an <input type=range> and owns its own arrows.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = document.activeElement;
      const typing =
        el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement || el instanceof HTMLSelectElement;
      if (typing) return;
      if (e.key === " ") {
        e.preventDefault();
        onPlayPause();
        return;
      }
      if (remote) {
        // The D-pad. The player returns false from onKeyEvent when a direction
        // has no neighbour, deliberately NOT swallowing the key so Home still
        // exits; the same rule here means an arrow at the end of a menu is left
        // to the browser rather than silently eaten.
        const dir: Record<string, Direction> = {
          ArrowUp: "up",
          ArrowDown: "down",
          ArrowLeft: "left",
          ArrowRight: "right",
        };
        if (e.key in dir) {
          if (onMove(dir[e.key])) e.preventDefault();
          return;
        }
        if (e.key === "Enter") {
          if (onPress()) e.preventDefault();
          return;
        }
        return;
      }
      if (e.key === "ArrowLeft") {
        e.preventDefault();
        onPrev();
      } else if (e.key === "ArrowRight") {
        e.preventDefault();
        onNext();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [remote, onPlayPause, onPrev, onNext, onMove, onPress]);

  // ── Fit ───────────────────────────────────────────────────────────────────
  const [viewport, setViewport] = useState<HTMLElement | null>(null);
  const [scale, setScale] = useState(0.4);
  useEffect(() => {
    if (!viewport) return;
    const measure = () => {
      // The bezel eats a little on each axis; the stage is never upscaled past
      // 1:1, because a 1920-wide canvas blown up is a blurrier lie than a small
      // accurate one.
      const w = (viewport.clientWidth - 64) / CANVAS.width;
      const h = (viewport.clientHeight - 96) / CANVAS.height;
      setScale(Math.max(0.05, Math.min(1, Math.min(w, h))));
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(viewport);
    return () => ro.disconnect();
  }, [viewport]);

  const notes = useMemo(() => fidelityNotes(program), [program]);
  const summary = useMemo(() => fidelitySummary(program), [program]);

  if (slides.length === 0) {
    return (
      <PlayerShell door={door} name={cast.name}>
        <div className="flex flex-1 items-center justify-center p-8">
          <div className="max-w-lg space-y-3 text-center">
            <KitIcon icon={TriangleAlert} decorative className="mx-auto size-8 text-[color:var(--wv-warn)]" />
            <p className="text-sm font-medium">Nothing in this cast would play.</p>
            <p className="text-sm text-muted-foreground">
              {program.skipped.length === 0
                ? "The cast has no slides yet."
                : "Every slide is one a screen would be refused. The projection drops a slide whose layers will not validate, silently — this is that list."}
            </p>
            {program.skipped.length > 0 ? <SkippedList skipped={program.skipped} /> : null}
          </div>
        </div>
      </PlayerShell>
    );
  }

  const source = slide ? dwellSource(rawSlide(cast, slide.authoredIndex), cast) : "player-default";

  return (
    <PlayerShell door={door} name={cast.name}>
      <div className="flex min-h-0 flex-1">
        {/* The stage. */}
        <div className="flex min-w-0 flex-1 flex-col">
          <div ref={setViewport} className="flex min-h-0 flex-1 items-center justify-center overflow-hidden p-4">
            {slide ? (
              <TvFrame>
                <PlayerStage
                  slide={slide}
                  scale={scale}
                  now={now}
                  assetUrls={assetUrls}
                  playing={transport.playing}
                  focused={focused}
                  titleSafe={titleSafe}
                />
              </TvFrame>
            ) : null}
          </div>

          <Transport
            transport={transport}
            slides={slides}
            dwellMs={dwellMs}
            dwellSource={source}
            statedMs={slide ? statedDwellMs(rawSlide(cast, slide.authoredIndex), cast) : 0}
            barRef={barRef}
            readoutRef={readoutRef}
            scrubRef={scrubRef}
            onPlayPause={onPlayPause}
            onPrev={onPrev}
            onNext={onNext}
            onLoop={onLoop}
            onScrub={onScrub}
            onJump={onJump}
            remote={remote}
            onRemote={() => setRemote((r) => !r)}
            titleSafe={titleSafe}
            onTitleSafe={() => setTitleSafe((t) => !t)}
            now={now}
            assetUrls={assetUrls}
          />
        </div>

        {/* What the preview is, what will not play, and the remote. */}
        <aside
          data-slot="preview-panels"
          className="hidden w-[22rem] shrink-0 flex-col overflow-hidden border-l border-border bg-[color:var(--wv-surface)] xl:flex"
        >
          <Tabs defaultValue="fidelity" className="flex min-h-0 flex-1 flex-col">
            <TabList aria-label="What the preview can tell you" className="shrink-0">
              <Tab value="fidelity">What this is</Tab>
              <Tab value="program">
                Not playing
                {program.skipped.length > 0 ? (
                  <Badge tone="warn" className="ml-1.5">
                    {program.skipped.length}
                  </Badge>
                ) : null}
              </Tab>
              <Tab value="remote">Remote</Tab>
            </TabList>

            <TabPanel value="fidelity" className="min-h-0 flex-1 overflow-y-auto p-3">
              <p className="mb-3 text-xs text-muted-foreground">{summary}</p>
              <ul className="space-y-2.5">
                {notes.map((note) => (
                  <li key={note.id} className="rounded-md border border-border bg-[color:var(--wv-surface-2)] p-2.5">
                    <div className="mb-1 flex items-start gap-2">
                      <LevelBadge level={note.level} />
                      {note.affected !== undefined ? (
                        <span className="shrink-0 text-[11px] text-muted-foreground">
                          {note.affected} layer{note.affected === 1 ? "" : "s"}
                        </span>
                      ) : null}
                    </div>
                    <p className="text-xs font-medium leading-snug">{note.title}</p>
                    <p className="mt-1 text-[11px] leading-snug text-muted-foreground">{note.detail}</p>
                  </li>
                ))}
              </ul>
            </TabPanel>

            <TabPanel value="program" className="min-h-0 flex-1 overflow-y-auto p-3">
              {originError ? (
                <p className="mb-3 rounded-md border border-[color:var(--wv-warn)]/40 bg-[color:var(--wv-warn)]/10 p-2 text-[11px] leading-snug">
                  The content origin did not answer ({originError}). Pictures are left black rather than reported
                  missing — nothing here says an asset is gone.
                </p>
              ) : null}
              <SkippedList skipped={program.skipped} />
              <DroppedList slides={slides} />
            </TabPanel>

            <TabPanel value="remote" className="min-h-0 flex-1 overflow-y-auto p-3">
              <RemotePanel
                enabled={remote}
                onEnable={() => setRemote(true)}
                targets={targets}
                focusIndex={focusIndex}
                onFocus={setFocusIndex}
                onMove={onMove}
                onPress={onPress}
                slideLabel={slideLabel}
                presses={presses}
              />
            </TabPanel>
          </Tabs>
        </aside>
      </div>
    </PlayerShell>
  );
}

/** The authored slide behind a projected one — needed for the dwell READOUT,
 * which explains where the number came from and so has to see the authored
 * `duration_ms` rather than the resolved one. */
function rawSlide(cast: Pick<Cast, "slides">, authoredIndex: number) {
  return cast.slides[authoredIndex] ?? { id: "", layers: [] };
}

function formatSeconds(ms: number): string {
  return `${(ms / 1000).toFixed(1)}s`;
}

/** The full-viewport frame. `fixed inset-0` for the reason the Studio's shell is:
 * a playback surface must never be the thing that scrolls. */
function PlayerShell({ door, name, children }: { door: React.ReactNode; name: string; children: React.ReactNode }) {
  return (
    <div
      data-slot="preview-shell"
      className="fixed inset-0 z-40 flex h-[100dvh] w-screen flex-col overflow-hidden bg-background text-foreground"
    >
      <header className="flex shrink-0 items-center justify-between gap-3 border-b border-border bg-[color:var(--wv-surface)] px-3 py-2">
        <div className="flex min-w-0 items-center gap-2">
          {door}
          <span className="truncate text-sm font-medium">{name || "Untitled cast"}</span>
        </div>
        {/* The claim, stated where it cannot be missed rather than only in a
            panel. This page has one job an operator will over-trust it for. */}
        <Tooltip tip="Slide order, dwell times and remote focus mirror the player exactly. The drawing is a browser's, and the panel lists every known difference.">
          <span className="flex shrink-0 items-center gap-1.5 text-[11px] text-muted-foreground">
            <KitIcon icon={Eye} decorative className="size-3.5" />
            Not a pixel-exact render of the TV
          </span>
        </Tooltip>
      </header>
      {children}
    </div>
  );
}

function LevelBadge({ level }: { level: FidelityLevel }) {
  // A stand-in is the loudest of the three: it changes what a slide SAYS, not
  // just how it looks, so it wears the warning tone. "Not shown" is neutral —
  // the preview is being honest about an absence, which is not a fault.
  const tone: BadgeTone = level === "stand-in" ? "warn" : level === "not-shown" ? "neutral" : "accent";
  const label = level === "stand-in" ? "Stand-in" : level === "not-shown" ? "Not shown" : "Approximate";
  return <Badge tone={tone}>{label}</Badge>;
}

function SkippedList({ skipped }: { skipped: { id: string; authoredIndex: number; reason: string }[] }) {
  if (skipped.length === 0) {
    return <p className="text-xs text-muted-foreground">Every slide in this cast reaches a screen.</p>;
  }
  return (
    <div className="space-y-2">
      <p className="text-xs font-medium">
        {skipped.length} slide{skipped.length === 1 ? "" : "s"} a screen never receives
      </p>
      <ul className="space-y-2">
        {skipped.map((s) => (
          <li
            key={s.id}
            data-slot="preview-skipped-slide"
            className="rounded-md border border-[color:var(--wv-warn)]/40 bg-[color:var(--wv-warn)]/5 p-2"
          >
            <p className="text-xs font-medium">Slide {s.authoredIndex + 1}</p>
            <p className="mt-0.5 text-[11px] leading-snug text-muted-foreground">{s.reason}</p>
          </li>
        ))}
      </ul>
    </div>
  );
}

function DroppedList({ slides }: { slides: { authoredIndex: number; droppedLayers: number[] }[] }) {
  const withDrops = slides.filter((s) => s.droppedLayers.length > 0);
  if (withDrops.length === 0) return null;
  return (
    <div className="mt-4 space-y-2">
      <p className="text-xs font-medium">Layers the projection omits</p>
      <p className="text-[11px] leading-snug text-muted-foreground">
        A rasterized layer with no PNG yet is dropped and the rest of the slide still plays. Run the renderer over the
        cast and they come back.
      </p>
      <ul className="space-y-1">
        {withDrops.map((s) => (
          <li key={s.authoredIndex} data-slot="preview-dropped-layers" className="text-[11px] text-muted-foreground">
            Slide {s.authoredIndex + 1}: layer
            {s.droppedLayers.length === 1 ? " " : "s "}
            {s.droppedLayers.map((i) => i + 1).join(", ")}
          </li>
        ))}
      </ul>
    </div>
  );
}

// ── The transport bar ────────────────────────────────────────────────────────

interface TransportProps {
  transport: Transport;
  slides: ReturnType<typeof projectCast>["slides"];
  dwellMs: number;
  dwellSource: ReturnType<typeof dwellSource>;
  statedMs: number;
  barRef: React.RefObject<HTMLDivElement | null>;
  readoutRef: React.RefObject<HTMLSpanElement | null>;
  scrubRef: React.RefObject<HTMLInputElement | null>;
  onPlayPause: () => void;
  onPrev: () => void;
  onNext: () => void;
  onLoop: () => void;
  onScrub: (ms: number) => void;
  onJump: (i: number) => void;
  remote: boolean;
  onRemote: () => void;
  titleSafe: boolean;
  onTitleSafe: () => void;
  now: Date;
  assetUrls: AssetUrls;
}

function Transport(p: TransportProps) {
  const current = p.slides[p.transport.index];
  const dwellExplain =
    p.dwellSource === "slide"
      ? `This slide states ${p.statedMs}ms.`
      : p.dwellSource === "cast"
        ? `The cast's default of ${p.statedMs}ms, because this slide states none.`
        : "The player's own 8s default, because neither this slide nor the cast states a duration. It is not on the wire — the player supplies it.";
  const clamped = p.statedMs > 0 && p.dwellMs !== p.statedMs;

  return (
    <div data-slot="preview-transport" className="shrink-0 border-t border-border bg-[color:var(--wv-surface)]">
      {/* Progress. A ref'd bar scaled every frame — no React render per frame. */}
      <div className="relative h-1 w-full overflow-hidden bg-[color:var(--wv-surface-2)]">
        <div
          ref={p.barRef}
          data-slot="preview-progress"
          className="h-full w-full origin-left bg-[color:var(--wv-accent)]"
          style={{ transform: "scaleX(0)" }}
        />
      </div>

      <div className="flex flex-wrap items-center gap-2 px-3 py-2">
        <Tooltip tip="Previous slide (←)">
          <Button variant="ghost" size="sm" icon={SkipBack} aria-label="Previous slide" onClick={p.onPrev} />
        </Tooltip>
        <Tooltip tip={p.transport.playing ? "Pause (Space)" : "Play (Space)"}>
          <Button
            size="sm"
            icon={p.transport.playing ? Pause : Play}
            aria-label={p.transport.playing ? "Pause" : "Play"}
            onClick={p.onPlayPause}
          />
        </Tooltip>
        <Tooltip tip="Next slide (→)">
          <Button variant="ghost" size="sm" icon={SkipForward} aria-label="Next slide" onClick={p.onNext} />
        </Tooltip>

        {/* Scrub, within THIS slide's dwell. Deliberately not a whole-cast
            timeline: a cast has no total length — the wall cycles it forever —
            so a bar that ran out at the end would be describing something that
            does not exist. */}
        <input
          ref={p.scrubRef}
          data-slot="preview-scrub"
          type="range"
          min={0}
          max={Math.max(1, p.dwellMs)}
          defaultValue={0}
          step={50}
          aria-label="Position within this slide"
          className="h-1.5 min-w-[8rem] flex-1 cursor-pointer accent-[color:var(--wv-accent)]"
          onChange={(e) => p.onScrub(Number(e.target.value))}
        />

        <Tooltip tip={dwellExplain}>
          <span className="shrink-0 tabular-nums text-xs text-muted-foreground">
            <span ref={p.readoutRef}>0.0s</span> / {(p.dwellMs / 1000).toFixed(1)}s
          </span>
        </Tooltip>
        {clamped ? (
          <Tooltip tip={`The player raises anything under 500ms to its floor, so ${p.statedMs}ms is held for ${p.dwellMs}ms.`}>
            <Badge tone="warn">clamped</Badge>
          </Tooltip>
        ) : null}

        <span className="shrink-0 text-xs text-muted-foreground">
          Slide {current ? current.authoredIndex + 1 : "—"} · {p.transport.index + 1} of {p.slides.length} playing
        </span>

        <div className="ml-auto flex shrink-0 items-center gap-1">
          <Tooltip
            tip={
              p.transport.loop
                ? "Looping, as the wall does — the player cycles a cast forever and cannot be asked not to."
                : "Stopping at the end. A preview-only convenience: a real screen always loops."
            }
          >
            <Button
              variant={p.transport.loop ? "secondary" : "ghost"}
              size="sm"
              icon={p.transport.loop ? Repeat : Repeat1}
              aria-label={p.transport.loop ? "Looping — stop at the end instead" : "Stopping at the end — loop instead"}
              aria-pressed={p.transport.loop}
              onClick={p.onLoop}
            />
          </Tooltip>
          <Tooltip tip="Show the 5% title-safe inset a television can overscan into">
            <Button
              variant={p.titleSafe ? "secondary" : "ghost"}
              size="sm"
              icon={Grid2x2Check}
              aria-label="Title-safe guide"
              aria-pressed={p.titleSafe}
              onClick={p.onTitleSafe}
            />
          </Tooltip>
          <Tooltip tip="Drive the slide with a remote: arrow keys move focus, Enter presses">
            <Button
              variant={p.remote ? "secondary" : "ghost"}
              size="sm"
              icon={Gamepad2}
              aria-label="Remote"
              aria-pressed={p.remote}
              onClick={p.onRemote}
            >
              Remote
            </Button>
          </Tooltip>
        </div>
      </div>

      {/* Jump to any slide. Thumbnails through the SAME stage the canvas uses,
          so a thumbnail can never disagree with what is about to play. */}
      <div data-slot="preview-filmstrip" className="flex gap-1.5 overflow-x-auto border-t border-border/60 px-3 py-2">
        {p.slides.map((s, i) => (
          <button
            key={s.id}
            type="button"
            aria-label={`Jump to slide ${s.authoredIndex + 1}`}
            aria-current={i === p.transport.index ? "true" : undefined}
            onClick={() => p.onJump(i)}
            className={cn(
              "shrink-0 overflow-hidden rounded border transition-colors",
              i === p.transport.index
                ? "border-[color:var(--wv-accent)]"
                : "border-border hover:border-[color:var(--wv-accent)]/50",
            )}
          >
            <PlayerStage slide={s} scale={0.05} now={p.now} assetUrls={p.assetUrls} playing={false} />
          </button>
        ))}
      </div>
    </div>
  );
}

// ── The remote ───────────────────────────────────────────────────────────────

interface RemotePanelProps {
  enabled: boolean;
  onEnable: () => void;
  targets: FocusTarget[];
  focusIndex: number;
  onFocus: (i: number) => void;
  onMove: (d: Direction) => boolean;
  onPress: () => boolean;
  slideLabel: (id: string) => string;
  presses: PressRecord[];
}

/**
 * The D-pad, on screen as well as on the keyboard.
 *
 * On screen because a key binding nobody can see is a feature nobody uses —
 * legacy's preview put next/previous on the arrow keys alone, behind a hint that
 * faded after five seconds. And because a button can be CLICKED, which is what
 * a test does.
 */
function RemotePanel(p: RemotePanelProps) {
  if (!p.enabled) {
    return (
      <div className="space-y-3">
        <p className="text-xs text-muted-foreground">
          Turn the remote on to move focus with the arrow keys and press with Enter, the way a viewer standing in front
          of the screen would.
        </p>
        <Button size="sm" icon={Gamepad2} onClick={p.onEnable}>
          Turn on the remote
        </Button>
      </div>
    );
  }

  if (p.targets.length === 0) {
    return (
      <p className="text-xs text-muted-foreground">
        Nothing on this slide is focusable. A layer becomes pressable by carrying an event name, and a menu's items are
        always focusable.
      </p>
    );
  }

  const focused = p.targets[p.focusIndex];
  const tooSmall = focused ? isBelowInteractiveFloor(focused.rect, MIN_INTERACTIVE_SIDE) : false;

  return (
    <div className="space-y-4">
      <DPad onMove={p.onMove} onPress={p.onPress} />

      <div>
        <p className="mb-1.5 text-xs font-medium">
          {p.targets.length} focusable region{p.targets.length === 1 ? "" : "s"}, in the order the player registers them
        </p>
        <ul className="space-y-1">
          {p.targets.map((t, i) => (
            <li key={`${t.layerIndex}-${t.itemIndex ?? "x"}`}>
              <button
                type="button"
                onClick={() => p.onFocus(i)}
                aria-current={i === p.focusIndex ? "true" : undefined}
                className={cn(
                  "w-full rounded border px-2 py-1.5 text-left text-[11px] leading-snug transition-colors",
                  i === p.focusIndex
                    ? "border-[color:var(--wv-accent)] bg-[color:var(--wv-accent)]/10"
                    : "border-border hover:border-[color:var(--wv-accent)]/50",
                )}
              >
                {describePress(t.press, p.slideLabel)}
              </button>
            </li>
          ))}
        </ul>
      </div>

      {tooSmall ? (
        <p
          data-slot="preview-focus-too-small"
          className="rounded-md border border-[color:var(--wv-warn)]/40 bg-[color:var(--wv-warn)]/10 p-2 text-[11px] leading-snug"
        >
          This region is smaller than {MIN_INTERACTIVE_SIDE}px on one side. That is the legibility floor for something
          driven by a remote from across a room — and the preview is scaled down, which is exactly what makes it look
          fine here.
        </p>
      ) : null}

      <div>
        <p className="mb-1.5 flex items-center gap-1.5 text-xs font-medium">
          <KitIcon icon={CircleDot} decorative className="size-3.5" />
          What a press would do
        </p>
        <p className="mb-2 text-[11px] leading-snug text-muted-foreground">
          Nothing is sent. A real press reaches the box as a <code>screen.interaction</code> event whose{" "}
          <code>screen_id</code> the relay resolves from the screen's own credential (EVT-055), so a console cannot
          raise one — there is no route that would accept it. This log is what the wall would report.
        </p>
        {p.presses.length === 0 ? (
          <p className="text-[11px] text-muted-foreground">No presses yet.</p>
        ) : (
          <ul data-slot="preview-press-log" className="space-y-1">
            {p.presses.map((r) => (
              <li key={r.at} className="rounded border border-border bg-[color:var(--wv-surface-2)] px-2 py-1 text-[11px] leading-snug">
                {r.text}
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}

function DPad({ onMove, onPress }: { onMove: (d: Direction) => boolean; onPress: () => boolean }) {
  const dir = (d: Direction, label: string, icon: typeof ChevronLeft, cls: string) => (
    <Button
      variant="ghost"
      size="sm"
      icon={icon}
      aria-label={label}
      className={cls}
      onClick={() => onMove(d)}
    />
  );
  return (
    <div data-slot="preview-dpad" className="grid w-[9rem] grid-cols-3 grid-rows-3 gap-0.5">
      <span />
      {dir("up", "Focus up", ChevronLeft, "rotate-90")}
      <span />
      {dir("left", "Focus left", ChevronLeft, "")}
      <Button size="sm" aria-label="Press OK" onClick={() => onPress()}>
        OK
      </Button>
      {dir("right", "Focus right", ChevronRight, "")}
      <span />
      {dir("down", "Focus down", ChevronRight, "rotate-90")}
      <span />
    </div>
  );
}
