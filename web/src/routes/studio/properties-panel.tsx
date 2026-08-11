import { useEffect, useState } from "react";
import { AlertTriangle, FileVideo, ImagePlus, X } from "lucide-react";
import { Button, FormField, KitIcon } from "@/components/kit";
import {
  ENTITY_STATE_TOKEN,
  SLIDE_CANVAS_HEIGHT,
  SLIDE_CANVAS_WIDTH,
  WEATHER_TOKENS,
  isContentKind,
  isLabelKind,
  type Entity,
  type LayerAlign,
  type SlideLayer,
  type SlideProblem,
} from "@/api";
import { cn } from "@/lib/utils";
import type { LayerPatch } from "./cast-model";
import { CLOCK_PRESETS, COUNTDOWN_PRESETS, DATE_PRESETS } from "./go-time-layout";
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
  /** Open the content-origin picker for the selected content-bearing layer
   * (`image` or `video` — one picker, because the origin holds one kind of
   * thing: bytes with a digest). */
  onPickAsset: () => void;
  /** The current slide's hold time, and how to change it. */
  durationMs: number | undefined;
  onDurationChange: (durationMs: number | null) => void;
  /** Every entity this box knows about, for an `entity` widget's subject picker.
   * Read from GET /entities by the host route — REAL rows, because an entity id
   * typed by hand is one typo away from a widget that shows a dash forever and
   * never says why. Empty while the read is in flight, or if it failed. */
  entities: Entity[];
}

export function PropertiesPanel({
  layer,
  problems,
  layerIndex,
  onPatch,
  onPickAsset,
  durationMs,
  onDurationChange,
  entities,
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

          {layer.kind === "date" ? (
            <FormatField
              label="Date format"
              help="The same Go reference-time layout a clock uses — a date is a time format on this wire, so the screen runs both through one formatter."
              value={layer.text ?? ""}
              presets={DATE_PRESETS}
              onCommit={(text) => onPatch({ text })}
            />
          ) : null}

          {layer.kind === "countdown" ? (
            <>
              <CountdownTargetField
                targetMs={layer.target_ms}
                onCommit={(target_ms) => onPatch({ target_ms })}
              />
              <FormatField
                label="Remaining-time format"
                help="DD/D days, HH/H hours, MM/M minutes, SS/S seconds — the doubled form zero-padded. Anything else is drawn literally. This is NOT a clock layout: a duration has no hour of day."
                value={layer.text ?? ""}
                presets={COUNTDOWN_PRESETS}
                onCommit={(text) => onPatch({ text })}
              />
            </>
          ) : null}

          {layer.kind === "weather" ? (
            <TemplateField
              label="Display template"
              help={`The box substitutes ${WEATHER_TOKENS.join(", ")} and draws the rest exactly as typed. A token you misspell shows as itself rather than blanking the widget.`}
              value={layer.text ?? ""}
              tokens={WEATHER_TOKENS}
              onCommit={(text) => onPatch({ text })}
            />
          ) : null}

          {layer.kind === "entity" ? (
            <>
              <EntityPicker
                entityId={layer.entity_id}
                entities={entities}
                onCommit={(entity_id) => onPatch({ entity_id })}
              />
              <TemplateField
                label="Display template"
                help={`The box substitutes ${ENTITY_STATE_TOKEN} with the entity's current state. Leave it as just the token to show the state alone.`}
                value={layer.text ?? ""}
                tokens={[ENTITY_STATE_TOKEN]}
                onCommit={(text) => onPatch({ text })}
              />
            </>
          ) : null}

          {isLabelKind(layer.kind) ? (
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

          {/* The two content-bearing kinds share one control, because they are
              one gesture: pick bytes out of the content origin. The origin has
              no MIME metadata (it is content-addressed — see media-library.tsx),
              so there is nothing to branch on but the WORD, and duplicating the
              block to change that word is how `video` gets forgotten the next
              time this file changes. */}
          {isContentKind(layer.kind) ? (
            <div className="flex flex-col gap-2">
              <span className="text-sm font-medium">{layer.kind === "video" ? "Video" : "Image"}</span>
              {layer.asset_ref ? (
                <code className="min-w-0 truncate rounded-input bg-[color:var(--wv-surface-2)] px-2 py-1 font-mono text-[12px] text-muted-foreground">
                  {layer.asset_ref}
                </code>
              ) : (
                <p className="text-[13px] text-muted-foreground">
                  No {layer.kind} chosen yet — this slide will not render until one is.
                </p>
              )}
              <div className="flex flex-wrap gap-2">
                <Button size="sm" variant="secondary" icon={layer.kind === "video" ? FileVideo : ImagePlus} onClick={onPickAsset}>
                  {layer.asset_ref ? `Replace ${layer.kind}` : `Choose ${layer.kind}`}
                </Button>
                {layer.asset_ref ? (
                  <Button
                    size="sm"
                    variant="ghost"
                    icon={X}
                    // Both fields are cleared together: a served slide's layer
                    // carries asset_ref and url as a pair, so clearing one would
                    // leave a layer whose bytes are named but unfetchable.
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

/**
 * A free-text FORMAT field with a preset dropdown beside it — the shape every
 * generated kind's format control takes (`clock`, `date`, `countdown`).
 *
 * Both halves are needed and neither is redundant. The presets are what make the
 * grammar discoverable: nobody guesses that a Go layout spells "the day" as `2`,
 * or that a countdown spells hours `HH` and not `15`. The free field is what
 * makes the grammar REACHABLE — the layouts are open sets, and a dropdown alone
 * would cap the editor at whatever list happened to ship.
 *
 * The select shows "Custom…" whenever the current value is not one of the
 * presets, so an operator can always see which of the two they are in.
 */
function FormatField({
  label,
  help,
  value,
  presets,
  onCommit,
}: {
  label: string;
  help: string;
  value: string;
  presets: ReadonlyArray<{ layout: string; label: string }>;
  onCommit: (layout: string) => void;
}) {
  return (
    <>
      <FormField label={label} help={help}>
        {(field) => (
          <input
            {...field}
            type="text"
            className={inputClass}
            value={value}
            onChange={(e) => onCommit(e.target.value)}
          />
        )}
      </FormField>
      <FormField label={`${label} preset`}>
        {(field) => (
          <select
            {...field}
            className={inputClass}
            value={presets.some((p) => p.layout === value) ? value : ""}
            onChange={(e) => {
              if (e.target.value) onCommit(e.target.value);
            }}
          >
            <option value="">Custom…</option>
            {presets.map((p) => (
              <option key={p.layout} value={p.layout}>
                {p.label}
              </option>
            ))}
          </select>
        )}
      </FormField>
    </>
  );
}

/**
 * A substitution TEMPLATE field (`weather`, `entity`) with one-click token
 * insertion.
 *
 * A template is not a format string the operator picks from a list — it is
 * prose with holes in it ("Now: {temp}° {cond}"), so the useful affordance is
 * appending a token to whatever they are writing rather than replacing the
 * whole value. The tokens are a closed set the BOX substitutes; anything else,
 * including a misspelt token, is drawn literally, which is why the buttons
 * matter: a widget showing "{tmp}" on a wall is a typo nobody sees for a week.
 */
function TemplateField({
  label,
  help,
  value,
  tokens,
  onCommit,
}: {
  label: string;
  help: string;
  value: string;
  tokens: readonly string[];
  onCommit: (template: string) => void;
}) {
  return (
    <div className="flex flex-col gap-2">
      <FormField label={label} help={help}>
        {(field) => (
          <input
            {...field}
            type="text"
            className={inputClass}
            value={value}
            onChange={(e) => onCommit(e.target.value)}
          />
        )}
      </FormField>
      <div role="group" aria-label={`${label} tokens`} className="flex flex-wrap gap-1">
        {tokens.map((token) => (
          <Button
            key={token}
            size="sm"
            variant="ghost"
            aria-label={`Insert ${token}`}
            onClick={() => onCommit(value + token)}
          >
            <span className="font-mono text-[12px]">{token}</span>
          </Button>
        ))}
      </div>
    </div>
  );
}

/**
 * A countdown's target instant, edited as a LOCAL date and time and stored as
 * Unix epoch milliseconds.
 *
 * The conversion is the whole job and it goes only one way safely.
 * `<input type="datetime-local">` speaks a timezone-less wall time, which is
 * what an operator means ("the doors open at 7pm"); the wire needs an ABSOLUTE
 * instant, because the player subtracts its own clock from it and must not have
 * to know the authoring timezone. `new Date("YYYY-MM-DDTHH:mm")` resolves such a
 * string in the RUNTIME's local zone, which is the author's, so the round trip
 * is faithful for anyone authoring in the site's own timezone — and any other
 * reading of a wall time would be a guess.
 *
 * A value that does not parse commits NOTHING. Clearing the field mid-edit
 * otherwise produces NaN, and NaN reaches the wire as a target of zero, which
 * the server refuses and which renders as a countdown that has already finished.
 */
function CountdownTargetField({
  targetMs,
  onCommit,
}: {
  targetMs: number | undefined;
  onCommit: (targetMs: number) => void;
}) {
  return (
    <FormField
      label="Counts down to"
      help="Your local date and time. It is stored as an absolute instant, so the screen counts down correctly whatever clock it has."
    >
      {(field) => (
        <input
          {...field}
          type="datetime-local"
          className={inputClass}
          value={targetMs ? toDatetimeLocal(targetMs) : ""}
          onChange={(e) => {
            const ms = Date.parse(e.target.value);
            if (Number.isFinite(ms) && ms > 0) onCommit(ms);
          }}
        />
      )}
    </FormField>
  );
}

/** Format an epoch-ms instant as the `YYYY-MM-DDTHH:mm` a datetime-local input
 * reads — in LOCAL time, which is why this is hand-built rather than
 * `toISOString().slice(0,16)` (that would silently shift the displayed time by
 * the UTC offset and an operator would "correct" it into the wrong instant). */
function toDatetimeLocal(ms: number): string {
  const d = new Date(ms);
  const p2 = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}T${p2(d.getHours())}:${p2(d.getMinutes())}`;
}

/**
 * An `entity` widget's SUBJECT, chosen from the entities this box actually has.
 *
 * A select over real rows rather than a text field, because the failure of a
 * wrong id is completely silent: the box resolves an unknown entity to an em
 * dash, exactly as it resolves a known one whose state it has not observed yet,
 * so a typo and a device that is merely quiet look identical on the wall. The
 * only place that difference is visible is here, at the moment of choosing.
 *
 * When the list is empty the control degrades to a plain text input rather than
 * to an empty select: an operator authoring a slide before their devices are
 * adopted should still be able to write the id down, and an empty dropdown with
 * no explanation is the worse dead end. The list also carries each entity's
 * last-observed state, which is what tells an operator they picked the TV they
 * meant rather than its neighbour.
 */
function EntityPicker({
  entityId,
  entities,
  onCommit,
}: {
  entityId: string | undefined;
  entities: Entity[];
  onCommit: (entityId: string) => void;
}) {
  const known = entities.some((e) => e.id === entityId);
  return (
    <FormField
      label="Entity"
      help={
        entities.length === 0
          ? "No entities are known to this box yet — adopt a device on the Devices page, or type an id if you know it."
          : "The device whose live state this widget shows."
      }
    >
      {(field) =>
        entities.length === 0 ? (
          <input
            {...field}
            type="text"
            className={inputClass}
            placeholder="Entity id"
            value={entityId ?? ""}
            onChange={(e) => onCommit(e.target.value.trim())}
          />
        ) : (
          <select
            {...field}
            className={inputClass}
            value={entityId ?? ""}
            onChange={(e) => onCommit(e.target.value)}
          >
            <option value="">Choose an entity…</option>
            {/* An id the list does not contain is still shown as the selected
                option — otherwise opening a slide authored before a device was
                removed would silently reset the widget to "choose one" and the
                operator would save that reset without noticing. */}
            {entityId && !known ? <option value={entityId}>{entityId} (not on this box)</option> : null}
            {entities.map((e) => (
              <option key={e.id} value={e.id}>
                {e.name}
                {e.state ? ` — ${e.state}` : ""}
              </option>
            ))}
          </select>
        )
      }
    </FormField>
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
