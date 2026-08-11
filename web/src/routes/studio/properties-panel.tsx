import { useEffect, useState } from "react";
import { AlertTriangle, ImagePlus, X } from "lucide-react";
import { Button, FormField, KitIcon } from "@/components/kit";
import { SLIDE_CANVAS_HEIGHT, SLIDE_CANVAS_WIDTH, type LayerAlign, type SlideLayer, type SlideProblem } from "@/api";
import { cn } from "@/lib/utils";
import type { LayerPatch } from "./cast-model";
import { CLOCK_PRESETS } from "./go-time-layout";
import { describeLayer } from "./slide-canvas";

/**
 * The properties panel — everything about the selected layer that is not its
 * position on the canvas, plus the geometry as exact numbers for when dragging
 * is not precise enough.
 *
 * It only ever emits a PATCH of changed fields; the reducer clamps and rounds.
 * That split matters: a number input mid-edit is briefly empty or "-", and a
 * panel that owned the clamping would either fight the operator's typing or let
 * an unclampable value reach the save body.
 *
 * The panel is also where a slide's REFUSAL becomes legible. `validateSlide`
 * mirrors the wire's rules, and its messages are shown against the layer that
 * caused them — because the server does not reject an invalid slide back to the
 * author, the projector silently DROPS it, so without this the only symptom is a
 * TV that never shows the slide.
 */

const inputClass =
  "flex min-h-[38px] w-full min-w-0 rounded-input border border-border bg-transparent px-2 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring";

/** Parse a number input's value, ignoring the transient states typing produces
 * (empty, "-", "1e") rather than writing NaN into the model. */
function num(value: string): number | null {
  if (value.trim() === "") return null;
  const n = Number(value);
  return Number.isFinite(n) ? n : null;
}

export interface PropertiesPanelProps {
  layer: SlideLayer | undefined;
  /** Problems for the whole slide; those naming this layer are shown inline. */
  problems: SlideProblem[];
  layerIndex: number | null;
  onPatch: (patch: LayerPatch) => void;
  onPickImage: () => void;
  /** The current slide's hold time, and how to change it. */
  durationMs: number | undefined;
  onDurationChange: (durationMs: number | null) => void;
}

export function PropertiesPanel({
  layer,
  problems,
  layerIndex,
  onPatch,
  onPickImage,
  durationMs,
  onDurationChange,
}: PropertiesPanelProps) {
  const slideProblems = problems.filter((p) => p.index === null);
  const layerProblems = layerIndex === null ? [] : problems.filter((p) => p.index === layerIndex);

  return (
    <section aria-label="Properties" className="flex min-w-0 flex-col gap-4">
      <h2 className="text-sm font-semibold">Properties</h2>

      {/* Slide-level: how long this slide holds the screen. */}
      <FormField
        label="Slide duration (seconds)"
        help="Leave blank to use the playlist's own default rather than a fixed hold."
      >
        {(field) => (
          <input
            {...field}
            type="number"
            min={0}
            step={1}
            className={inputClass}
            value={durationMs === undefined ? "" : String(Math.round(durationMs / 1000))}
            onChange={(e) => {
              const seconds = num(e.target.value);
              onDurationChange(seconds === null ? null : seconds * 1000);
            }}
          />
        )}
      </FormField>

      {slideProblems.map((p, i) => (
        <ProblemNote key={i} message={p.message} />
      ))}

      {!layer ? (
        <p className="text-sm text-muted-foreground">
          Select a layer on the canvas or in the layer list to edit it.
        </p>
      ) : (
        <>
          <p className="text-[13px] font-medium text-muted-foreground">{describeLayer(layer)}</p>

          {layerProblems.map((p, i) => (
            <ProblemNote key={i} message={p.message} />
          ))}

          {/* Geometry — canvas pixels, the same coordinates the player draws in. */}
          <div className="grid grid-cols-2 gap-3">
            <NumberField label="X" value={layer.x} max={SLIDE_CANVAS_WIDTH} onCommit={(x) => onPatch({ x })} />
            <NumberField label="Y" value={layer.y} max={SLIDE_CANVAS_HEIGHT} onCommit={(y) => onPatch({ y })} />
            <NumberField label="Width" value={layer.w} max={SLIDE_CANVAS_WIDTH} onCommit={(w) => onPatch({ w })} />
            <NumberField label="Height" value={layer.h} max={SLIDE_CANVAS_HEIGHT} onCommit={(h) => onPatch({ h })} />
          </div>

          {layer.kind === "text" ? (
            <FormField label="Text">
              {(field) => (
                <textarea
                  {...field}
                  rows={3}
                  className={cn(inputClass, "resize-y")}
                  value={layer.text ?? ""}
                  onChange={(e) => onPatch({ text: e.target.value })}
                />
              )}
            </FormField>
          ) : null}

          {layer.kind === "clock" ? (
            <>
              <FormField
                label="Time format"
                help="A Go reference-time layout — the player formats the live time through it."
              >
                {(field) => (
                  <input
                    {...field}
                    type="text"
                    className={inputClass}
                    value={layer.text ?? ""}
                    onChange={(e) => onPatch({ text: e.target.value })}
                  />
                )}
              </FormField>
              <FormField label="Format preset">
                {(field) => (
                  <select
                    {...field}
                    className={inputClass}
                    value={CLOCK_PRESETS.some((p) => p.layout === layer.text) ? layer.text : ""}
                    onChange={(e) => {
                      if (e.target.value) onPatch({ text: e.target.value });
                    }}
                  >
                    <option value="">Custom…</option>
                    {CLOCK_PRESETS.map((p) => (
                      <option key={p.layout} value={p.layout}>
                        {p.label}
                      </option>
                    ))}
                  </select>
                )}
              </FormField>
            </>
          ) : null}

          {layer.kind === "text" || layer.kind === "clock" ? (
            <>
              <NumberField
                label="Font size (px)"
                value={layer.font_px ?? 48}
                max={SLIDE_CANVAS_HEIGHT}
                onCommit={(font_px) => onPatch({ font_px })}
              />
              <ColorField
                label="Text colour"
                value={layer.color ?? "#FFFFFF"}
                onCommit={(color) => onPatch({ color })}
              />
              <FormField label="Alignment">
                {(field) => (
                  <select
                    {...field}
                    className={inputClass}
                    value={layer.align ?? "left"}
                    onChange={(e) => onPatch({ align: e.target.value as LayerAlign })}
                  >
                    <option value="left">Left</option>
                    <option value="center">Center</option>
                    <option value="right">Right</option>
                  </select>
                )}
              </FormField>
            </>
          ) : null}

          {layer.kind === "rect" ? (
            <ColorField
              label="Fill colour"
              value={layer.color ?? "#F368C4"}
              onCommit={(color) => onPatch({ color })}
            />
          ) : null}

          {layer.kind === "image" ? (
            <div className="flex flex-col gap-2">
              <span className="text-sm font-medium">Image</span>
              {layer.asset_ref ? (
                <code className="min-w-0 truncate rounded-input bg-[color:var(--wv-surface-2)] px-2 py-1 font-mono text-[12px] text-muted-foreground">
                  {layer.asset_ref}
                </code>
              ) : (
                <p className="text-[13px] text-muted-foreground">
                  No image chosen yet — this slide will not render until one is.
                </p>
              )}
              <div className="flex flex-wrap gap-2">
                <Button size="sm" variant="secondary" icon={ImagePlus} onClick={onPickImage}>
                  {layer.asset_ref ? "Replace image" : "Choose image"}
                </Button>
                {layer.asset_ref ? (
                  <Button
                    size="sm"
                    variant="ghost"
                    icon={X}
                    // Both fields are cleared together: the wire requires
                    // asset_ref and url as a pair, so clearing one would leave a
                    // layer that fails validation for a reason the panel is not
                    // showing.
                    onClick={() => onPatch({ asset_ref: undefined, url: undefined })}
                  >
                    Clear
                  </Button>
                ) : null}
              </div>
            </div>
          ) : null}
        </>
      )}
    </section>
  );
}

function ProblemNote({ message }: { message: string }) {
  return (
    <p
      role="status"
      className="flex items-start gap-2 rounded-card border border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] p-2 text-[13px] text-[color:var(--wv-warn)]"
    >
      <KitIcon icon={AlertTriangle} decorative className="mt-0.5 size-4 shrink-0" />
      <span>{message}</span>
    </p>
  );
}

/**
 * A field that lets the operator TYPE freely while only committing values the
 * model can hold.
 *
 * Two things make the naive version — a controlled input bound straight at the
 * model — unusable, and neither is subtle:
 *
 *  1. Clearing the box to retype produces an empty string, which does not
 *     parse, so the input snaps back to the old value and every keystroke after
 *     it fights the operator. The same happens to a colour, where every prefix
 *     of "#0A1B2C" is invalid until the seventh character.
 *  2. The MODEL CLAMPS. Type "5" into a width and the reducer holds it at the
 *     16px floor, which flows back as "16"; the next keystroke then appends to
 *     the clamped value and "512" becomes "1612".
 *
 * So the input owns a draft string and commits only when the draft parses —
 * and, critically, does NOT re-sync from the model **while it has focus**. Focus
 * is the signal that the human owns the field; on blur the model's value (clamp
 * included) is adopted, so what is shown afterwards is always what is stored.
 * Changes from elsewhere — a drag, a different layer selected — land the moment
 * the field is not being typed into, which is every moment that matters.
 */
function useDraft(value: string): {
  draft: string;
  setDraft: (next: string) => void;
  focusProps: { onFocus: () => void; onBlur: () => void };
} {
  const [draft, setDraft] = useState(value);
  const [focused, setFocused] = useState(false);
  useEffect(() => {
    if (!focused) setDraft(value);
  }, [value, focused]);
  return {
    draft,
    setDraft,
    focusProps: { onFocus: () => setFocused(true), onBlur: () => setFocused(false) },
  };
}

/** A committed number input: it reports only parseable values, so a half-typed
 * field never writes NaN into the model. */
function NumberField({
  label,
  value,
  max,
  onCommit,
}: {
  label: string;
  value: number;
  max: number;
  onCommit: (n: number) => void;
}) {
  const { draft, setDraft, focusProps } = useDraft(String(value));
  return (
    <FormField label={label}>
      {(field) => (
        <input
          {...field}
          {...focusProps}
          type="number"
          min={0}
          max={max}
          step={1}
          className={inputClass}
          value={draft}
          onChange={(e) => {
            setDraft(e.target.value);
            const n = num(e.target.value);
            if (n !== null) onCommit(n);
          }}
        />
      )}
    </FormField>
  );
}

/** A colour swatch plus the hex it stands for. The hex field is editable because
 * the wire's colour IS a `#RRGGBB` string — an operator matching a brand colour
 * pastes it; the swatch is for choosing one. Only a well-formed value commits,
 * so a half-typed "#F3" never reaches the model (where it would fail
 * validation and drop the slide). */
function ColorField({
  label,
  value,
  onCommit,
}: {
  label: string;
  value: string;
  onCommit: (color: string) => void;
}) {
  const { draft, setDraft, focusProps } = useDraft(value);
  return (
    <FormField label={label}>
      {(field) => (
        <div className="flex items-center gap-2">
          <input
            {...field}
            type="color"
            className="h-9 w-12 shrink-0 cursor-pointer rounded-input border border-border bg-transparent"
            value={/^#[0-9a-fA-F]{6}$/.test(value) ? value : "#000000"}
            onChange={(e) => onCommit(e.target.value.toUpperCase())}
          />
          <input
            {...focusProps}
            type="text"
            aria-label={`${label} hex`}
            className={cn(inputClass, "font-mono")}
            value={draft}
            onChange={(e) => {
              setDraft(e.target.value);
              const next = e.target.value.trim();
              if (/^#[0-9a-fA-F]{6}$/.test(next)) onCommit(next.toUpperCase());
            }}
          />
        </div>
      )}
    </FormField>
  );
}
