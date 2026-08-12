// ui-schema/1 — input widgets (UIS-070): text/number/duration/time-of-day
// inputs, toggle, select, multi-select, entity-picker.
//
// Each renders through the kit FormField (the one form idiom — it wires the
// accessible label, id and aria plumbing from the required `labelMsg`, UIS-075)
// over a native control styled from HORIZON tokens, exactly as the design
// gallery does; the kit-only import boundary forbids reaching into the vendored
// base directly, and a native element needs no import. An input's `bind` is
// read-write by contract (UIS-065): the control shows the scope-resolved value
// and writes edits straight back to that path, firing an optional `on.change`
// ActionRef alongside (never instead of) the intrinsic write. A `number-input`
// writes `null` — never 0 — when cleared (UIS-072). An `entity-picker` binds
// either an EntityRef object or, under `bindShape: "entityId"`, the bare entity
// id `rules/1` inlines — a scalar write that never touches its parent
// (UIS-073a).

import { Combobox, FormField, type FormFieldControl } from "@/components/kit";
import { resolvePath, resolveWriteLocation, resolveOptions } from "../bindings";
import { ENTITY_PICKER_SCALAR_SHAPE } from "../schema";
import { asLiveBinding, useLive } from "../live";
import { runAction } from "../actions";
import { useRenderer } from "../state";
import type { ActionRef } from "../types";
import type { WidgetProps } from "./common";
import { useWizard } from "./wizard-context";

// `.wv-touch` clamps the control to the responsive contract's 44px touch-target
// minimum (Matt, 2026-07-22); its min-height wins over the `h-9` visual height so
// every text/number/duration/time/select control is comfortably tappable.
const FIELD_CLASS =
  "wv-touch flex h-9 w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none transition-[box-shadow,border-color] placeholder:text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring aria-invalid:border-[color:var(--wv-err)]";

/** A widget's bound value plus a writer that performs the intrinsic write-back
 * (UIS-065) and fires any `on.change` ActionRef alongside it. Honors a
 * LiveBinding `bind` (UIS-109). */
function useBoundField(node: WidgetProps["node"], scope: WidgetProps["scope"]) {
  const ctx = useRenderer();
  const wizard = useWizard();
  const live = asLiveBinding(node.bind);
  const bindPath = live ? live.path : String(node.bind);
  const staticValue = resolvePath(bindPath, scope);
  const value = useLive(live ? live.path : null, staticValue);
  const changeAction = node.on?.change as ActionRef | undefined;
  const write = (v: unknown): void => {
    const loc = resolveWriteLocation(bindPath, scope);
    if (loc) ctx.store.write(loc, v);
    if (changeAction) runAction(changeAction, scope, ctx, wizard);
  };
  // A server field-validation error for THIS input's bind path (UIS/api-1: a 422
  // VALIDATION_FAILED's per-field message surfaces on the offending FormField).
  const error = ctx.env.fieldErrors?.[bindPath];
  return { value, write, msg: ctx.env.msg, error };
}

export function TextInputWidget({ node, scope }: WidgetProps) {
  const { value, write, msg, error } = useBoundField(node, scope);
  const label = msg(String(node.props?.labelMsg ?? ""));
  const placeholder = node.props?.placeholderMsg ? msg(String(node.props.placeholderMsg)) : undefined;
  const multiline = node.props?.multiline === true;
  const maxLength = typeof node.props?.maxLength === "number" ? node.props.maxLength : undefined;
  return (
    <FormField label={label} {...(error ? { error } : {})}>
      {(field: FormFieldControl) =>
        multiline ? (
          <textarea
            {...field}
            rows={3}
            maxLength={maxLength}
            placeholder={placeholder}
            className={`${FIELD_CLASS} h-auto py-2`}
            value={value == null ? "" : String(value)}
            onChange={(e) => write(e.target.value)}
          />
        ) : (
          <input
            {...field}
            type="text"
            maxLength={maxLength}
            placeholder={placeholder}
            className={FIELD_CLASS}
            value={value == null ? "" : String(value)}
            onChange={(e) => write(e.target.value)}
          />
        )
      }
    </FormField>
  );
}

export function NumberInputWidget({ node, scope }: WidgetProps) {
  const { value, write, msg, error } = useBoundField(node, scope);
  const label = msg(String(node.props?.labelMsg ?? ""));
  const min = typeof node.props?.min === "number" ? node.props.min : undefined;
  const max = typeof node.props?.max === "number" ? node.props.max : undefined;
  const step = typeof node.props?.step === "number" ? node.props.step : undefined;
  return (
    <FormField label={label} {...(error ? { error } : {})}>
      {(field: FormFieldControl) => (
        <input
          {...field}
          type="number"
          min={min}
          max={max}
          step={step}
          className={FIELD_CLASS}
          value={value == null ? "" : String(value)}
          // UIS-072: clearing writes null, never 0 or a placeholder.
          onChange={(e) => write(e.target.value === "" ? null : Number(e.target.value))}
        />
      )}
    </FormField>
  );
}

const UNIT_PER_SECOND: Record<string, number> = { ms: 1000, s: 1, min: 1 / 60, h: 1 / 3600 };
const UNIT_SUFFIX: Record<string, string> = { ms: "ms", s: "sec", min: "min", h: "hr" };

export function DurationInputWidget({ node, scope }: WidgetProps) {
  const { value, write, msg, error } = useBoundField(node, scope);
  const label = msg(String(node.props?.labelMsg ?? ""));
  const unit = String(node.props?.displayUnit ?? "s");
  const factor = UNIT_PER_SECOND[unit] ?? 1;
  const seconds = typeof value === "number" ? value : Number(value);
  const shown = Number.isFinite(seconds) ? seconds * factor : "";
  const minSeconds = typeof node.props?.min === "number" ? node.props.min : 0;
  return (
    <FormField label={`${label} (${UNIT_SUFFIX[unit] ?? unit})`} {...(error ? { error } : {})}>
      {(field: FormFieldControl) => (
        <input
          {...field}
          type="number"
          min={minSeconds * factor}
          className={FIELD_CLASS}
          value={shown === "" ? "" : String(shown)}
          onChange={(e) => {
            if (e.target.value === "") return write(0);
            // Non-negative integer seconds (UIS-070 bind shape).
            write(Math.max(0, Math.round(Number(e.target.value) / factor)));
          }}
        />
      )}
    </FormField>
  );
}

export function TimeOfDayWidget({ node, scope }: WidgetProps) {
  const { value, write, msg, error } = useBoundField(node, scope);
  const label = msg(String(node.props?.labelMsg ?? ""));
  return (
    <FormField label={label} {...(error ? { error } : {})}>
      {(field: FormFieldControl) => (
        <input
          {...field}
          type="time"
          step={1}
          className={FIELD_CLASS}
          value={value == null ? "" : String(value)}
          onChange={(e) => write(e.target.value)}
        />
      )}
    </FormField>
  );
}

export function ToggleWidget({ node, scope }: WidgetProps) {
  const { value, write, msg, error } = useBoundField(node, scope);
  const label = msg(String(node.props?.labelMsg ?? ""));
  const on = Boolean(value);
  const onLabel = node.props?.onLabelMsg ? msg(String(node.props.onLabelMsg)) : "On";
  const offLabel = node.props?.offLabelMsg ? msg(String(node.props.offLabelMsg)) : "Off";
  return (
    <FormField label={label} {...(error ? { error } : {})}>
      {(field: FormFieldControl) => (
        <div className="flex items-center gap-2.5">
          {/* The clickable control is a >=44px touch target (.wv-touch, the 44px
              minimum the responsive contract mandates) with the visual switch
              track carried by an inner span so the hit area grows without
              stretching the pill itself. */}
          <button
            {...field}
            type="button"
            role="switch"
            aria-checked={on}
            onClick={() => write(!on)}
            className="wv-touch peer inline-flex shrink-0 items-center justify-center rounded-pill outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <span
              aria-hidden="true"
              data-on={on}
              data-off={!on}
              className="inline-flex h-5 w-9 shrink-0 items-center rounded-pill border border-transparent transition-colors data-[on=true]:bg-[image:var(--grad-accent)] data-[off=true]:bg-[color:var(--wv-off-bg)]"
            >
              <span
                className={`block size-4 rounded-pill bg-white shadow-sm transition-transform ${on ? "translate-x-[18px]" : "translate-x-0.5"}`}
              />
            </span>
          </button>
          <span className="text-sm text-muted-foreground">{on ? onLabel : offLabel}</span>
        </div>
      )}
    </FormField>
  );
}

/** Above this many candidates a `select` is rendered as a searchable Combobox.
 *
 * A threshold rather than a prop, for exactly the reason `TableWidget`'s
 * `RENDERER_PAGE_SIZE` is one: ui-schema/1's `select` props are `labelMsg`,
 * `options` and `placeholderMsg` (UIS-070), and a host may not invent a fourth —
 * Negotiation forbids silent host extensions, and the contract puts "the
 * rendering engine's own implementation" out of scope precisely so this kind of
 * decision can be made from the data. Which control paints a closed candidate
 * set IS a presentation decision: the bound value, the write, the validation and
 * the document are identical either way.
 *
 * The number that made this necessary is 446 — the IANA zones the Settings page
 * offers for a site's `tz`, which arrived as one flat scroll. 25 is the same
 * figure the table uses for "long enough to need finding rather than reading",
 * and holding them equal is deliberate: two different answers to "how many rows
 * is too many to scan" on one page would be arbitrary. Below it the native
 * `<select>` is kept — it costs no JavaScript, opens the platform's own picker
 * on a phone, and for six options a search box is chrome with nothing to do. */
export const SELECT_SEARCH_THRESHOLD = 25;

export function SelectWidget({ node, scope }: WidgetProps) {
  const ctx = useRenderer();
  const { value, write, msg, error } = useBoundField(node, scope);
  const label = msg(String(node.props?.labelMsg ?? ""));
  const placeholder = node.props?.placeholderMsg ? msg(String(node.props.placeholderMsg)) : undefined;
  const options = resolveOptions(node.props?.options, scope, ctx.env);
  // A candidate's value is a JSON scalar (UIS-070) — string, number or boolean —
  // and both controls address it by its string key, so the write path is shared:
  // the key goes back through this map to the ORIGINAL scalar, and a numeric
  // option never gets written back as the string "3".
  const byKey = new Map(options.map((o) => [String(o.value), o.value]));
  const writeKey = (key: string): void => write(byKey.has(key) ? byKey.get(key) : key);

  if (options.length > SELECT_SEARCH_THRESHOLD) {
    return (
      <FormField label={label} {...(error ? { error } : {})}>
        {(field: FormFieldControl) => (
          <Combobox
            {...field}
            aria-label={label}
            options={options.map((o) => ({ value: String(o.value), label: o.label }))}
            value={value == null ? "" : String(value)}
            onChange={writeKey}
            {...(placeholder ? { placeholder } : {})}
            searchPlaceholder={`Search ${label.toLocaleLowerCase()}`}
          />
        )}
      </FormField>
    );
  }

  return (
    <FormField label={label} {...(error ? { error } : {})}>
      {(field: FormFieldControl) => (
        <select
          {...field}
          className={FIELD_CLASS}
          value={value == null ? "" : String(value)}
          onChange={(e) => writeKey(e.target.value)}
        >
          <option value="" disabled>
            {placeholder ?? "Select…"}
          </option>
          {options.map((o, i) => (
            <option key={i} value={String(o.value)}>
              {o.label}
            </option>
          ))}
        </select>
      )}
    </FormField>
  );
}

export function MultiSelectWidget({ node, scope }: WidgetProps) {
  const ctx = useRenderer();
  const { value, write, msg } = useBoundField(node, scope);
  const label = msg(String(node.props?.labelMsg ?? ""));
  const options = resolveOptions(node.props?.options, scope, ctx.env);
  const selected = Array.isArray(value) ? value : [];
  const toggle = (v: unknown): void => {
    const has = selected.some((s) => s === v);
    write(has ? selected.filter((s) => s !== v) : [...selected, v]);
  };
  // A fieldset+legend gives the group its required accessible name (UIS-075)
  // without FormField's single-control model.
  return (
    <fieldset className="flex flex-col gap-1.5">
      <legend className="text-sm font-medium">{label}</legend>
      <div className="flex flex-col gap-1">
        {/* Each option row is a >=44px touch target (.wv-touch) so the tiny
            checkbox is comfortably tappable, per the responsive 44px contract. */}
        {options.map((o, i) => (
          <label key={i} className="wv-touch flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              className="size-4 rounded-[4px] border border-border"
              checked={selected.some((s) => s === o.value)}
              onChange={() => toggle(o.value)}
            />
            {o.label}
          </label>
        ))}
      </div>
    </fieldset>
  );
}

const MODE_KEY: Record<string, string> = {
  entity: "entity_id",
  selector: "selector",
  deviceClass: "device_class",
};
const KEY_MODE: Record<string, string> = { entity_id: "entity", selector: "selector", device_class: "deviceClass" };

export function EntityPickerWidget({ node, scope }: WidgetProps) {
  const { value, write, msg, error } = useBoundField(node, scope);
  const label = msg(String(node.props?.labelMsg ?? ""));

  // UIS-073a: WHICH shape `bind` addresses is declared on the node, never
  // sniffed from the bound value. Under `entityId` the bind names the scalar
  // entity id `rules/1` inlines into a trigger/condition/action (RUL-010), so
  // the picker is a single field over that one string — and the write goes to
  // that scalar path alone. It MUST NOT write the parent object: replacing the
  // parent with a freshly-built {entity_id: …}, which is exactly what the
  // `entityRef` branch below does to its own bound object, would erase the
  // record's sibling members (a state trigger's `type`, `to`, `for`).
  if (node.props?.bindShape === ENTITY_PICKER_SCALAR_SHAPE) {
    return (
      <FormField label={label} {...(error ? { error } : {})}>
        {(field: FormFieldControl) => (
          <input
            {...field}
            type="text"
            className={FIELD_CLASS}
            value={value == null ? "" : String(value)}
            onChange={(e) => write(e.target.value)}
          />
        )}
      </FormField>
    );
  }

  const modes: string[] = Array.isArray(node.props?.modes)
    ? (node.props?.modes as string[])
    : ["entity", "selector", "deviceClass"];
  const ref = (value ?? {}) as Record<string, unknown>;
  const activeKey = Object.keys(ref).find((k) => k in KEY_MODE) ?? MODE_KEY[modes[0]];
  const activeMode = KEY_MODE[activeKey] ?? modes[0];
  const currentValue = ref[activeKey];
  return (
    <FormField label={label} {...(error ? { error } : {})}>
      {(field: FormFieldControl) => (
        <div className="flex flex-col gap-2 sm:flex-row">
          <select
            aria-label={`${label} mode`}
            className={`${FIELD_CLASS} sm:w-40`}
            value={activeMode}
            onChange={(e) => write({ [MODE_KEY[e.target.value]]: "" })}
          >
            {modes.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
          <input
            {...field}
            type="text"
            className={FIELD_CLASS}
            value={currentValue == null ? "" : String(currentValue)}
            onChange={(e) => write({ [activeKey]: e.target.value })}
          />
        </div>
      )}
    </FormField>
  );
}
