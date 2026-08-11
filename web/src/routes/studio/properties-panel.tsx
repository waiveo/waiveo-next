import { useEffect, useState } from "react";
import { AlertTriangle, FileVideo, ImagePlus, Type, X } from "lucide-react";
import { Button, FormField, KitIcon } from "@/components/kit";
import {
  ENTITY_STATE_TOKEN,
  SLIDE_CANVAS_HEIGHT,
  SLIDE_CANVAS_WIDTH,
  WEATHER_TOKENS,
  isContentKind,
  isLabelKind,
  type DeriveBorder,
  type DeriveSpec,
  type Entity,
  type LayerAlign,
  type SlideLayer,
  type SlideProblem,
} from "@/api";
import { cn } from "@/lib/utils";
import { applyDerivePatch, type DeriveSpecPatch, type LayerPatch } from "./cast-model";
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
  /** Open that same picker for a rasterized TEXT layer's embedded face
   * (`derive.font_asset_ref`). A font file is content in the same origin, so it
   * is chosen the same way — see DeriveFields' custom-face control. */
  onPickFont: () => void;
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
  onPickFont,
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

          {layer.kind === "derive" && layer.derive ? (
            <DeriveFields
              spec={layer.derive}
              rendered={Boolean(layer.asset_ref)}
              onPickFont={onPickFont}
              onSpec={(patch) => {
                const spec = layer.derive;
                if (!spec) return;
                // The whole spec is rewritten on every edit, and `derived_from`
                // is deliberately LEFT ALONE. It records which spec the current
                // raster came from, so the server can tell CURRENT from STALE;
                // clearing it here would report every edited layer as never
                // rendered and drop its picture off the wall until the tool ran.
                onPatch({ derive: applyDerivePatch(spec, patch) });
              }}
            />
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
 * The controls for a RASTERIZED layer's spec.
 *
 * Every member the spec vocabulary carries is editable here, and that is the
 * point rather than thoroughness for its own sake: a layer an operator can
 * insert and not edit is the defect this codebase keeps shipping, and it is
 * worse for a derive layer than for any other kind — a QR whose payload cannot
 * be changed is a sign pointing at example.com, rendered, uploaded, and put on a
 * wall.
 *
 * The render state is stated at the top for the same reason. A derive layer is
 * the ONE kind whose appearance does not follow from saving: it needs
 * `waiveo-derive` to be run, off the box, by a human. An editor that did not say
 * so would leave an operator waiting for a picture nothing was ever going to
 * produce.
 */
function DeriveFields({
  spec,
  rendered,
  onSpec,
  onPickFont,
}: {
  spec: DeriveSpec;
  rendered: boolean;
  onSpec: (patch: DeriveSpecPatch) => void;
  /** Open the content picker for the embedded face. */
  onPickFont: () => void;
}) {
  const fill = spec.fill;
  const shadow = spec.shadow;
  const border = spec.border;
  return (
    <div className="flex flex-col gap-3 rounded-md border border-[color:var(--wv-border)] p-3">
      <p className="text-[13px] text-muted-foreground">
        {rendered
          ? "Rendered. Any edit here needs waiveo-derive run again before the screen shows it; the previous picture keeps playing until then."
          : "Not rendered yet. This layer will not appear on a screen until waiveo-derive has run over it — it runs on your machine, never on the box."}
      </p>

      {spec.kind === "qr" ? (
        <>
          <FormField label="Encoded link or text" help="What a phone receives when the code is scanned.">
            {(field) => (
              <input
                {...field}
                type="text"
                className={inputClass}
                value={spec.data ?? ""}
                onChange={(e) => onSpec({ data: e.target.value })}
              />
            )}
          </FormField>
          <FormField
            label="Error correction"
            help="How much of the code can be obscured and still scan. Higher makes the symbol denser."
          >
            {(field) => (
              <select
                {...field}
                className={inputClass}
                value={spec.ec_level ?? "M"}
                onChange={(e) => onSpec({ ec_level: e.target.value as NonNullable<DeriveSpec["ec_level"]> })}
              >
                <option value="L">L — lowest, densest payload</option>
                <option value="M">M — the default for a sign</option>
                <option value="Q">Q</option>
                <option value="H">H — highest, most robust</option>
              </select>
            )}
          </FormField>
          <ColorField label="Module colour" value={spec.color ?? "#111827"} onCommit={(color) => onSpec({ color })} />
        </>
      ) : null}

      {spec.kind === "text" ? (
        <>
          <FormField label="Text">
            {(field) => (
              <textarea
                {...field}
                rows={2}
                className={cn(inputClass, "resize-y")}
                value={spec.text ?? ""}
                onChange={(e) => onSpec({ text: e.target.value })}
              />
            )}
          </FormField>
          <div className="grid grid-cols-2 gap-3">
            <NumberField
              label="Font size"
              value={spec.font_px ?? 64}
              max={SLIDE_CANVAS_HEIGHT}
              onCommit={(font_px) => onSpec({ font_px })}
            />
            <ColorField label="Text colour" value={spec.color ?? "#FFFFFF"} onCommit={(color) => onSpec({ color })} />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <FormField label="Align">
              {(field) => (
                <select
                  {...field}
                  className={inputClass}
                  value={spec.align ?? "left"}
                  onChange={(e) => onSpec({ align: e.target.value as LayerAlign })}
                >
                  <option value="left">Left</option>
                  <option value="center">Centre</option>
                  <option value="right">Right</option>
                </select>
              )}
            </FormField>
            <FormField label="Vertical">
              {(field) => (
                <select
                  {...field}
                  className={inputClass}
                  value={spec.valign ?? "top"}
                  onChange={(e) => onSpec({ valign: e.target.value as NonNullable<DeriveSpec["valign"]> })}
                >
                  <option value="top">Top</option>
                  <option value="middle">Middle</option>
                  <option value="bottom">Bottom</option>
                </select>
              )}
            </FormField>
          </div>
          <FormField
            label="Font family"
            help="A face the renderer must have, or the name an uploaded font file registers under."
          >
            {(field) => (
              <input
                {...field}
                type="text"
                className={inputClass}
                value={spec.font_family ?? ""}
                onChange={(e) => onSpec({ font_family: e.target.value || undefined })}
              />
            )}
          </FormField>

          {/* CUSTOM TYPOGRAPHY — the whole reason "a font the device does not
              ship" is one of the five things this layer kind exists for.
              Everything downstream of it was already built: the write-time check
              that the face is really in the origin, the retention hold that
              stops the sweep reclaiming it while a cast names it, and the
              renderer's fetch-and-@font-face embed. Without a control the member
              was reachable only by hand-rolled JSON — an operator could upload a
              TTF on the Content page and then had no way at all to attach it,
              which is the half-built shape this panel's own doc promises not to
              ship. */}
          <div className="flex flex-col gap-2">
            <span className="text-sm font-medium">Custom font file</span>
            {spec.font_asset_ref ? (
              <code
                data-slot="derive-font-ref"
                className="min-w-0 truncate rounded-input bg-[color:var(--wv-surface-2)] px-2 py-1 font-mono text-[12px] text-muted-foreground"
              >
                {spec.font_asset_ref}
              </code>
            ) : (
              <p className="text-[13px] text-muted-foreground">
                None — the renderer draws with whatever face its own machine has for that family. Attach an
                uploaded TTF/OTF/WOFF2 to pin it.
              </p>
            )}
            <div className="flex flex-wrap gap-2">
              <Button size="sm" variant="secondary" icon={Type} onClick={onPickFont}>
                {spec.font_asset_ref ? "Replace font file" : "Choose font file"}
              </Button>
              {spec.font_asset_ref ? (
                <Button
                  size="sm"
                  variant="ghost"
                  icon={X}
                  // Only the REF is dropped. `font_family` stays, because
                  // without a file it is still meaningful — the family the
                  // rendering host must already have — and clearing a control
                  // the operator did not touch is its own surprise.
                  onClick={() => onSpec({ font_asset_ref: undefined })}
                >
                  Clear
                </Button>
              ) : null}
            </div>
          </div>
        </>
      ) : null}

      {/* Fill, shadow and border apply to all three kinds — they are the reason
          a layer is rasterized at all. */}
      <FormField label="Fill">
        {(field) => (
          <select
            {...field}
            className={inputClass}
            value={fill?.kind ?? "none"}
            onChange={(e) => {
              const next = e.target.value;
              if (next === "none") return onSpec({ fill: undefined });
              // `to` and `angle_deg` are dropped when switching to solid: the
              // server REFUSES a solid fill carrying a second stop, so keeping
              // them would make the layer unsavable with nothing on screen
              // explaining which control did it.
              if (next === "solid") return onSpec({ fill: { kind: "solid", from: fill?.from ?? "#FFFFFF" } });
              if (next === "radial") {
                return onSpec({ fill: { kind: "radial", from: fill?.from ?? "#7C3AED", to: fill?.to ?? "#0EA5E9" } });
              }
              return onSpec({
                fill: { kind: "linear", from: fill?.from ?? "#7C3AED", to: fill?.to ?? "#0EA5E9", angle_deg: fill?.angle_deg ?? 135 },
              });
            }}
          >
            <option value="none">None (transparent)</option>
            <option value="solid">Solid</option>
            <option value="linear">Linear gradient</option>
            <option value="radial">Radial gradient</option>
          </select>
        )}
      </FormField>
      {fill ? (
        <div className="grid grid-cols-2 gap-3">
          <ColorField
            label={fill.kind === "solid" ? "Fill colour" : "Gradient from"}
            value={fill.from}
            onCommit={(from) => onSpec({ fill: { ...fill, from } })}
          />
          {fill.kind !== "solid" ? (
            <ColorField
              label="Gradient to"
              value={fill.to ?? "#0EA5E9"}
              onCommit={(to) => onSpec({ fill: { ...fill, to } })}
            />
          ) : null}
        </div>
      ) : null}
      {fill?.kind === "linear" ? (
        <NumberField
          label="Gradient angle"
          value={fill.angle_deg ?? 0}
          max={359}
          onCommit={(angle_deg) => onSpec({ fill: { ...fill, angle_deg } })}
        />
      ) : null}

      <FormField
        label="Drop shadow"
        help="The layer box is the shadow's OUTER bounds — the renderer insets the artwork so nothing is clipped, so a big shadow leaves less room for the artwork."
      >
        {(field) => (
          <input
            {...field}
            type="checkbox"
            className="size-4"
            checked={Boolean(shadow)}
            onChange={(e) =>
              onSpec({ shadow: e.target.checked ? { dy: 8, blur: 24, color: "#000000", opacity_pct: 55 } : undefined })
            }
          />
        )}
      </FormField>
      {shadow ? (
        <>
          <div className="grid grid-cols-2 gap-3">
            <NumberField label="Offset X" value={shadow.dx ?? 0} max={200} onCommit={(dx) => onSpec({ shadow: { ...shadow, dx } })} />
            <NumberField label="Offset Y" value={shadow.dy ?? 0} max={200} onCommit={(dy) => onSpec({ shadow: { ...shadow, dy } })} />
            <NumberField label="Blur" value={shadow.blur ?? 0} max={200} onCommit={(blur) => onSpec({ shadow: { ...shadow, blur } })} />
            <NumberField
              label="Opacity %"
              value={shadow.opacity_pct ?? 55}
              max={100}
              onCommit={(opacity_pct) => onSpec({ shadow: { ...shadow, opacity_pct } })}
            />
          </div>
          <ColorField
            label="Shadow colour"
            value={shadow.color ?? "#000000"}
            onCommit={(color) => onSpec({ shadow: { ...shadow, color } })}
          />
        </>
      ) : null}

      <div className="grid grid-cols-2 gap-3">
        <NumberField
          label="Corner radius"
          value={border?.radius ?? 0}
          max={400}
          onCommit={(radius) => onSpec({ border: normaliseBorder({ ...border, radius }) })}
        />
        <NumberField
          label="Border width"
          value={border?.width ?? 0}
          max={100}
          onCommit={(width) => onSpec({ border: normaliseBorder({ ...border, width }) })}
        />
      </div>
      {border?.width ? (
        <ColorField
          label="Border colour"
          value={border.color ?? "#FFFFFF"}
          onCommit={(color) => onSpec({ border: normaliseBorder({ ...border, color }) })}
        />
      ) : null}
    </div>
  );
}

/** A border with neither a width nor a radius does nothing, and the server
 * REFUSES it rather than ignoring it — so zeroing both controls must drop the
 * whole member instead of leaving `{width: 0, radius: 0}` behind to hold the
 * save. A stroked border also gains a default colour here, because a width with
 * no colour is the other refusal. */
function normaliseBorder(b: DeriveBorder): DeriveBorder | undefined {
  const width = b.width ?? 0;
  const radius = b.radius ?? 0;
  if (width === 0 && radius === 0) return undefined;
  const out: DeriveBorder = {};
  if (width > 0) {
    out.width = width;
    out.color = b.color ?? "#FFFFFF";
  }
  if (radius > 0) out.radius = radius;
  return out;
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
