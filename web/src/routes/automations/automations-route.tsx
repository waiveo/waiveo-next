import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Braces, Play, Power } from "lucide-react";
import { PageRenderer, isCreateDraftUi, type ActionHandler } from "@/renderer";
import {
  Button,
  FormField,
  PageHeader,
  StatusBadge,
  Toaster,
  toast,
  type Status,
} from "@/components/kit";
import {
  ApiError,
  collectPages,
  createApi,
  etagForRevision,
  updateWithReview,
  type Automation,
  type AutomationCreate,
  type AutomationRunResult,
  type Entity,
  type FieldErrors,
  type ScopeNode,
  type WaiveoApi,
} from "@/api";
import {
  hydrateForBuilder,
  normalizeModeMax,
  pruneBuilderScaffolding,
  ruleBodyJson,
  ruleBodyOf,
  ruleSaveBody,
  ruleUpdateFrom,
} from "./rule-shape";
import automationsPageDoc from "./page.uis.json";

/**
 * The Automations route — the fleet's authored rules/1 rules, edited through a
 * REAL RULE BUILDER rather than a raw JSON textarea.
 *
 * The builder is not bespoke React: it is `page.uis.json`, a ui-schema/1
 * list-detail document rendered through the SAME PageRenderer an extension page
 * takes. That is deliberate — the grammar already carries everything a rule editor
 * needs, and the automation-builder acceptance fixture
 * (`conformance/fixtures/automation-builder/`) exists precisely to prove it can:
 *
 *   • `repeat` + `repeat-add`/`repeat-remove` give the multi-trigger / multi-action
 *     / multi-condition lists their add and remove affordances (UIS-107/162).
 *   • `switch` on `item.type` paints the fields of the SELECTED kind and nothing
 *     else, so the trigger/condition/action editors are driven by the rules/1
 *     closed vocabularies (`rules/1:trigger-kind`, `:condition-leaf-kind`,
 *     `:action-kind`, `:mode`, `:misfire`) the ui-schema vocabRef table already
 *     declares — the builder never invents a second vocabulary (UIS-120/142).
 *   • a self-referential `fragment` gives the condition tree its recursion, so
 *     and / or / not nest to any depth (UIS-181/183) with a fail-closed ceiling.
 *   • `select` over a `data` OptionSource fed from the live `/entities` read model
 *     is the entity picker; `time-of-day` is the time picker; `duration-input` is
 *     the duration picker; the `time_pattern` component fields are the cron picker.
 *
 * This file is the host seam around that document: it feeds the live automations
 * and entities in as bound data, wires the closed action verbs (create = New,
 * submit = save the whole rule, delete) onto the typed api/1 client, and hosts the
 * first-party affordances the grammar genuinely lacks — Run now, enable/disable,
 * and the raw-JSON escape hatch.
 *
 * # The escape hatch, and why it flows one way
 *
 * The builder cannot express every rules/1 shape (a `choose` action's branch tree,
 * an `event` trigger's `match` map, anything a future rules/1 minor adds). Two
 * separate guarantees cover that, and only the first is about the hatch:
 *
 *  1. NOTHING IS LOST WITHOUT IT. The builder binds into the real record, so a
 *     member it has no editor for is carried through an edit and a save untouched —
 *     there is no round trip through a model that would drop it. Proven in the
 *     tests by saving after editing a rule whose actions include a `choose`.
 *  2. IT CAN STILL BE EDITED. "Edit as JSON" opens the rule body; "Apply to
 *     builder" parses it and re-seeds the builder with it. Apply is the ONLY
 *     direction, and the hatch has no save of its own on purpose: two independent
 *     save paths over one record is how an operator loses the edits they made in
 *     whichever surface they did not press Save in. One record, one Save.
 *
 * Every mutation applies the conventions the client owns: creates carry an
 * Idempotency-Key; edits/deletes carry the If-Match derived from the record's
 * revision (no unconditional overwrite); a 412 REVISION_CONFLICT re-reads and
 * surfaces the current state for review rather than silently retrying; a 422
 * VALIDATION_FAILED's field errors surface where the operator can fix them — on the
 * offending FormField when the compiler names a field the builder binds (`name`),
 * and otherwise as the compiler's own message quoting the rule member it rejected.
 *
 * NOTE on `execution_class`: the rules compiler's edge/app classification is a
 * server-internal column, NOT a field of the api/1 Automation resource (openapi
 * Automation is `additionalProperties:false` and omits it). The console therefore
 * edits the wire-available execution `mode` (single/restart/queued/parallel) and
 * badges the `enabled` state.
 */

// The message catalog the document's `msg:` references resolve against. Every key
// here is a reference `page.uis.json` itself declares.
const messages: Record<string, string> = {
  "msg:auto.col.name": "Name",
  "msg:auto.col.mode": "Mode",
  "msg:auto.col.triggers": "Triggers",
  "msg:auto.col.actions": "Actions",
  "msg:auto.detail.title": "Automation",
  "msg:auto.detail.empty": "Select an automation to edit it, or add a new one.",
  "msg:auto.detail.name": "Name",
  "msg:auto.detail.classification": "State",
  "msg:auto.detail.save": "Save changes",
  "msg:auto.detail.delete": "Delete automation",

  // Shared leaf fields.
  "msg:auto.f.entity": "Entity",
  "msg:auto.f.entityPlaceholder": "Pick an entity…",
  "msg:auto.f.entityId": "Entity ID (advanced)",
  "msg:auto.f.attribute": "Attribute",
  "msg:auto.f.attributePlaceholder": "e.g. active_app_id — leave blank for the entity state",
  "msg:auto.f.above": "Above",
  "msg:auto.f.below": "Below",
  "msg:auto.f.timeAfter": "After",
  "msg:auto.f.timeBefore": "Before",
  "msg:auto.f.sunAfterEvent": "After sun event",
  "msg:auto.f.sunAfterOffset": "After offset (seconds)",
  "msg:auto.f.sunBeforeEvent": "Before sun event",
  "msg:auto.f.sunBeforeOffset": "Before offset (seconds)",
  "msg:auto.f.variable": "Variable",
  "msg:auto.f.equals": "Equals",
  "msg:auto.f.statePlaceholder": "Pick a state…",
  "msg:auto.f.expression": "Expression",
  "msg:auto.f.expressionPlaceholder": "e.g. state('01J…') == 'playing'",

  // Triggers.
  "msg:auto.t.title": "Triggers — when this runs",
  "msg:auto.t.empty": "No triggers yet. An automation needs at least one.",
  "msg:auto.t.kind": "Trigger type",
  "msg:auto.t.add": "Add trigger",
  "msg:auto.t.remove": "Remove trigger",
  // rules/1 RUL-021 has TWO forms for a state bound and they are NOT
  // interchangeable — a scalar expands through the device class's semantic groups
  // ("on" also matches playing/paused/idle/buffering, REG-063), an array matches
  // exact literals only. The builder shows whichever form the rule actually holds
  // and labels it as what it is, so an operator can never convert one into the
  // other by accident (see the state-set `switch` in page.uis.json).
  "msg:auto.t.fromState": "From state (or its group)",
  "msg:auto.t.fromStateExact": "From exactly one of",
  "msg:auto.t.toState": "To state (or its group)",
  "msg:auto.t.toStateExact": "To exactly one of",
  "msg:auto.t.for": "Held for",
  "msg:auto.t.at": "At",
  "msg:auto.t.misfire": "If a firing is missed",
  "msg:auto.t.hours": "Hours",
  "msg:auto.t.minutes": "Minutes",
  "msg:auto.t.seconds": "Seconds",
  "msg:auto.t.patternPlaceholder": "exact value, /N for every N, or blank for every",
  "msg:auto.t.sunEvent": "Sun event",
  "msg:auto.t.sunOffset": "Offset (seconds)",
  "msg:auto.t.eventName": "Event name",
  "msg:auto.t.eventPlaceholder": "a pack-declared or platform-reserved event name",

  // Conditions.
  "msg:auto.c.title": "Conditions — only then",
  "msg:auto.c.empty": "No conditions. The actions run whenever a trigger fires.",
  "msg:auto.c.kind": "Condition type",
  "msg:auto.c.add": "Add condition",
  "msg:auto.c.addAll": "Add ALL-of group",
  "msg:auto.c.addAny": "Add ANY-of group",
  "msg:auto.c.addNot": "Add NOT group",
  "msg:auto.c.addLeafHere": "Add condition here",
  "msg:auto.c.addAllHere": "Add ALL-of group here",
  "msg:auto.c.addAnyHere": "Add ANY-of group here",
  "msg:auto.c.remove": "Remove condition",
  "msg:auto.c.andGroup": "All of",
  "msg:auto.c.orGroup": "Any of",
  "msg:auto.c.notGroup": "Not",
  "msg:auto.c.groupEmpty": "This group is empty — add a condition to it.",
  "msg:auto.c.stateIs": "State is (or its group)",
  "msg:auto.c.stateIsOneOf": "State is exactly one of",

  // Actions.
  "msg:auto.a.title": "Actions — do this",
  "msg:auto.a.empty": "No actions yet. An automation needs at least one.",
  "msg:auto.a.kind": "Action type",
  "msg:auto.a.add": "Add action",
  "msg:auto.a.remove": "Remove action",
  "msg:auto.a.command": "Command",
  "msg:auto.a.paramChannel": "Channel",
  "msg:auto.a.paramChannelPlaceholder": "the app/channel id to foreground",
  "msg:auto.a.paramKey": "Key",
  "msg:auto.a.paramKeyPlaceholder": "e.g. Home, Select, Up",
  "msg:auto.a.paramPower": "Power state",
  "msg:auto.a.presetId": "Preset id",
  "msg:auto.a.delay": "Delay",
  "msg:auto.a.message": "Message",
  "msg:auto.a.level": "Level",
  "msg:auto.a.notifyTemplate": "Notification template",
  "msg:auto.a.notifyRecipients": "Recipients selector",
  "msg:auto.a.variableValue": "Value",
  "msg:auto.a.workflowId": "Workflow id",

  // Mode.
  "msg:auto.m.title": "When it is already running",
  "msg:auto.m.mode": "Mode",
  "msg:auto.m.max": "Max parallel runs",

  // rules/1 vocabulary labels (UIS-132 requires one per member).
  "msg:vocab.trig.state": "Entity state change",
  "msg:vocab.trig.numeric": "Numeric threshold",
  "msg:vocab.trig.time": "At a time of day",
  "msg:vocab.trig.timePattern": "Repeating clock pattern",
  "msg:vocab.trig.sun": "Sunrise / sunset",
  "msg:vocab.trig.template": "Expression (template)",
  "msg:vocab.trig.event": "Platform event",
  "msg:vocab.trig.webhook": "Incoming webhook",
  "msg:vocab.cond.state": "Entity state is",
  "msg:vocab.cond.numeric": "Numeric range",
  "msg:vocab.cond.time": "Time of day is between",
  "msg:vocab.cond.sun": "Sun position is between",
  "msg:vocab.cond.variable": "Variable compares",
  "msg:vocab.cond.template": "Expression (template)",
  "msg:vocab.act.deviceCommand": "Send a device command",
  "msg:vocab.act.presetBatch": "Run a preset batch",
  "msg:vocab.act.choose": "Choose (branching)",
  "msg:vocab.act.delay": "Wait",
  "msg:vocab.act.log": "Write a log entry",
  "msg:vocab.act.notify": "Send a notification",
  "msg:vocab.act.variableWrite": "Write a variable",
  "msg:vocab.act.workflowStart": "Start a workflow",
  "msg:vocab.act.packAction": "Pack action",
  "msg:vocab.mode.single": "Single — ignore while running",
  "msg:vocab.mode.restart": "Restart — cancel and start again",
  "msg:vocab.mode.queued": "Queued — run one after another",
  "msg:vocab.mode.parallel": "Parallel — run concurrently",
  "msg:vocab.misfire.catchUpOnce": "Catch up once",
  "msg:vocab.misfire.skip": "Skip it",
  "msg:vocab.misfire.fireEach": "Fire each missed one",

  // device-class-registry/1 media-player state + command vocabularies
  // (REG-061 / REG-066) — the closed sets a Roku screen actually reports.
  "msg:state.on": "on",
  "msg:state.playing": "playing",
  "msg:state.paused": "paused",
  "msg:state.buffering": "buffering",
  "msg:state.idle": "idle",
  "msg:state.off": "off",
  "msg:state.standby": "standby",
  "msg:state.unavailable": "unavailable",
  "msg:sun.sunrise": "Sunrise",
  "msg:sun.sunset": "Sunset",
  "msg:cmd.launch": "launch — foreground an app",
  "msg:cmd.home": "home — return to the home surface",
  "msg:cmd.keypress": "keypress — send one remote key",
  "msg:cmd.power": "power — set the power state",
  "msg:level.info": "info",
  "msg:level.warning": "warning",
  "msg:level.error": "error",
};

/** A run's mode-evaluation disposition (rules/1 RunDisposition) → a StatusBadge
 * tone. `ran` is the live/started outcome (the ok/green lane, reserved for it);
 * `restarted` is a warn; `skipped` reads as off. */
const DISPOSITION: Record<string, { status: Status; label: string }> = {
  ran: { status: "ok", label: "Ran" },
  restarted: { status: "warn", label: "Restarted" },
  skipped: { status: "off", label: "Skipped" },
};

/** One line of the effect report: what the run tried to do to one target, and
 * whether that target took it. */
interface RunTarget {
  /** What was attempted — `launch → 01J8…`, `play_cast → 01J8…`. */
  label: string;
  ok: boolean;
  /** The refusal, when the target did not take it. */
  error?: string | undefined;
}

/**
 * The per-target effect report a run actually produced (RUL-236 / the
 * AutomationRunResult effect arrays), flattened for display.
 *
 * `disposition` alone is the MODE evaluation — whether the rule was allowed to
 * start — and says nothing about what the actions did. A run whose every command
 * was refused (an unadopted entity, a relay that is offline) still answers 200
 * `ran`, so a UI that reads only `disposition` paints a refusal as a success.
 * That is what this exists to prevent: the arrays are the report.
 */
function runTargets(result: AutomationRunResult): RunTarget[] {
  const out: RunTarget[] = [];
  for (const c of result.commands ?? []) {
    out.push({ label: `${c.command} → ${c.entity_id}`, ok: c.ok, error: c.error });
  }
  for (const s of result.signage ?? []) {
    const screens = s.screens ?? [];
    if (screens.length === 0) {
      // A signage action that wrote no screen reports only its own outcome, and
      // an outcome that is not `complete` over zero screens is still a failure —
      // shown as a target rather than dropped for having no rows.
      out.push({
        label: `${s.action} → (no screen matched)`,
        ok: s.outcome === "complete",
        error: s.outcome === "complete" ? undefined : s.outcome,
      });
      continue;
    }
    for (const sc of screens) {
      out.push({ label: `${s.action} → ${sc.screen_id || "(no screen)"}`, ok: sc.ok, error: sc.error });
    }
  }
  return out;
}

/** The chip a finished run gets: the disposition's own tone only while EVERY
 * target took its effect; a partial run warns and a total refusal errors. A run
 * with no targets at all (a rule of pure `log` actions, or one whose conditions
 * did not hold) keeps the disposition's tone — nothing failed. */
function runStatus(result: AutomationRunResult, targets: RunTarget[]): { status: Status; label: string } {
  const base = DISPOSITION[result.disposition] ?? { status: "pending" as Status, label: result.disposition };
  const failed = targets.filter((t) => !t.ok).length;
  if (failed === 0) return base;
  return {
    status: failed === targets.length ? "error" : "warn",
    label: `${base.label} — ${failed} of ${targets.length} failed`,
  };
}

/** A 422 VALIDATION_FAILED's per-field errors, if this failure is one. */
function fieldErrorsOf(err: unknown): FieldErrors | null {
  if (err instanceof ApiError && err.status === 422 && Object.keys(err.fieldErrors).length > 0) {
    return err.fieldErrors;
  }
  return null;
}

/** The compiler's message for a 422, or the Problem detail/code — the single
 * message the rule panel shows, prefixed with the rule member the compiler
 * rejected (`actions[0].type`) so the operator can find it in the builder or the
 * JSON view. */
function compileMessageOf(err: unknown): string | null {
  const fields = fieldErrorsOf(err);
  if (fields) {
    const [field, message] = Object.entries(fields)[0];
    return field ? `${field}: ${message}` : message;
  }
  if (err instanceof ApiError) return err.detail ?? err.code;
  return null;
}

/** Surface a non-2xx Problem as a toast quoting the code + human detail. */
function reportProblem(context: string, err: unknown): void {
  if (err instanceof ApiError) {
    const fieldMsg = Object.values(err.fieldErrors)[0];
    toast.error(`${context}: ${fieldMsg ?? err.detail ?? err.code}`);
  } else {
    toast.error(`${context}: the service is unreachable.`);
  }
}

/** Pull `{id, revision}` off the resource a ui-schema action hands back. */
function idRev(resource: unknown): { id: string; revision: number } | null {
  if (resource && typeof resource === "object") {
    const r = resource as Partial<Automation>;
    if (typeof r.id === "string" && typeof r.revision === "number") {
      return { id: r.id, revision: r.revision };
    }
  }
  return null;
}

export default function AutomationsRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [automations, setAutomations] = useState<Automation[] | null>(null);
  const [scopeNodes, setScopeNodes] = useState<ScopeNode[]>([]);
  const [entities, setEntities] = useState<Entity[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selectedIdRef = useRef<string | null>(null);
  // Whether a create draft was open on the last `$ui` we saw, so a fresh draft
  // opening (New→Cancel→New, which keeps `$ui.selected` null) is detectable below.
  const draftOpenRef = useRef(false);
  // 422 field errors + the 412 review banner for the builder's own form.
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({});
  const [conflictReview, setConflictReview] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [version, setVersion] = useState(0);

  // First-party panel state (out of band of the renderer).
  const [compileError, setCompileError] = useState<string | null>(null);
  const [disposition, setDisposition] = useState<AutomationRunResult | null>(null);
  const [running, setRunning] = useState(false);
  const [togglingEnabled, setTogglingEnabled] = useState(false);
  // The persistent 412 review state for the enable/disable toggle — its own flag
  // alongside the builder form's (conflictReview), so a toggle conflict surfaces
  // the standard toast + re-read + review banner rather than only a fading toast.
  const [enableConflict, setEnableConflict] = useState(false);

  // The raw-JSON escape hatch: whether it is open, its current text, the text it
  // was SEEDED with (so an operator's own typing is distinguishable from an
  // untouched mirror), its parse error, and the per-automation rule bodies "Apply
  // to builder" has staged. An override is what the builder is seeded with on its
  // next mount; it is dropped the moment a reload brings fresh server state (the
  // override HAS been saved, or has been superseded).
  const [jsonOpen, setJsonOpen] = useState(false);
  const [jsonDraft, setJsonDraft] = useState("");
  const [jsonSeed, setJsonSeed] = useState("");
  const [jsonError, setJsonError] = useState<string | null>(null);
  const [overrides, setOverrides] = useState<Record<string, Automation>>({});

  // The LIVE rule the builder holds, lifted out of the renderer through its
  // resource-change seam. Without it the JSON hatch can only mirror server state,
  // and "Apply to builder" then overwrites every unsaved builder edit with a rule
  // that predates them — the silent-loss path the single-save-path design exists
  // to close, left open through Apply. Keyed by record id so a reading can never
  // be shown against a different automation, and cleared whenever the renderer
  // remounts (`version`), since a fresh mount reports nothing until the next edit.
  const [builderRule, setBuilderRule] = useState<{ id: string; json: string } | null>(null);
  useEffect(() => {
    setBuilderRule(null);
  }, [version]);
  const onResourceChange = useCallback((resource: unknown) => {
    const id = selectedIdRef.current;
    if (!id) return;
    const rows = (resource as { automations?: unknown[] } | null)?.automations;
    const row = Array.isArray(rows) ? rows.find((r) => (r as Automation | null)?.id === id) : undefined;
    if (!row) return;
    // The same projection the hatch shows for a server record: the rules/1 keys,
    // with the builder's own scaffolding containers pruned back out.
    setBuilderRule({ id, json: JSON.stringify(pruneBuilderScaffolding(ruleBodyOf(row as Automation)), null, 2) });
  }, []);
  /** The live builder rule for the record currently selected, if we have one. */
  const builderJson = builderRule && builderRule.id === selectedId ? builderRule.json : null;

  // The scope node the next "New" places an automation on; defaults to the first
  // candidate and is chosen explicitly via the first-party picker when the org has
  // more than one node. A ref lets the (stable) create handler read the live
  // choice without being rebuilt on every picker change.
  const [targetScopeId, setTargetScopeId] = useState<string | null>(null);
  const activeScope = targetScopeId ?? scopeNodes[0]?.id ?? null;
  const activeScopeRef = useRef<string | null>(activeScope);
  activeScopeRef.current = activeScope;

  const load = useCallback(async () => {
    try {
      const [autos, scopes] = await Promise.all([
        collectPages<Automation>((cursor) => client.automations.list({ cursor })),
        collectPages<ScopeNode>((cursor) => client.scopeNodes.list({ cursor })),
      ]);
      setAutomations(autos);
      setScopeNodes(scopes);
      setLoadError(null);
    } catch (err) {
      setAutomations([]);
      setScopeNodes([]);
      setLoadError(err instanceof ApiError ? (err.detail ?? err.code) : "The service is unreachable.");
    }
    // Entities are a relay-owned READ MODEL that feeds the entity picker's options
    // — never a reason the page cannot be authored. A relay that is offline (or a
    // deployment with nothing adopted yet) leaves the picker empty and the
    // "Entity ID (advanced)" field still accepts an id, so this load failing is
    // degraded, not fatal: it gets its own try rather than joining the one above.
    try {
      setEntities(await collectPages<Entity>((cursor) => client.entities.list({ cursor })));
    } catch {
      setEntities([]);
    }
  }, [client]);

  useEffect(() => {
    void load();
  }, [load]);

  const reload = useCallback(async () => {
    setFieldErrors({});
    // Fresh server state supersedes anything the JSON hatch staged.
    setOverrides({});
    await load();
    setVersion((v) => v + 1);
  }, [load]);

  const selected = useMemo(
    () => (automations ?? []).find((a) => a.id === selectedId) ?? null,
    [automations, selectedId],
  );

  /** What the builder is bound to: the server's automations, with any staged JSON
   * override substituted, each hydrated so its nested builder binds have somewhere
   * to write (see rule-shape.ts). */
  const boundAutomations = useMemo(
    () => (automations ?? []).map((a) => hydrateForBuilder(overrides[a.id] ?? a)),
    [automations, overrides],
  );

  // Re-seed the JSON hatch whenever the selected automation (or its revision after
  // one of our own saves, or a staged override) changes: the hatch mirrors what the
  // builder was seeded with. `conflictReview` is intentionally NOT cleared here — a
  // 412 reload re-seeds the current values but the review banner must persist until
  // the operator moves on (onUiChange) or re-saves.
  const selId = selected?.id;
  const selRev = selected?.revision;
  const selOverride = selectedId ? overrides[selectedId] : undefined;
  const seedJson = useCallback((text: string) => {
    setJsonDraft(text);
    setJsonSeed(text);
    setJsonError(null);
  }, []);
  useEffect(() => {
    if (!selected) return;
    seedJson(ruleBodyJson(selOverride ?? selected));
    setDisposition(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selId, selRev, selOverride]);

  // …and track the BUILDER while the hatch's text is untouched, so what the hatch
  // shows is what the operator has in front of them rather than what the server
  // last sent. An operator who has typed into the textarea owns it: their text is
  // left alone and the divergence is warned about instead, because silently
  // replacing either surface's work is the failure being fixed here.
  useEffect(() => {
    if (builderJson === null) return;
    if (jsonDraft === jsonSeed) seedJson(builderJson);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [builderJson]);

  /** The builder moved on after the operator started editing the JSON, so Apply
   * would overwrite builder edits the textarea never contained. */
  const builderDiverged = jsonOpen && builderJson !== null && builderJson !== jsonSeed && jsonDraft !== jsonSeed;

  // The renderer owns the selection; moving to a different record retires any
  // captured field errors (keyed by bind path, no record identity) and the conflict
  // review states that belonged to the record just left.
  const onUiChange = useCallback((ui: Record<string, unknown>) => {
    const next = typeof ui.selected === "string" ? ui.selected : null;
    // Entering a FRESH create draft (UIS-021) resets the same out-of-band state — a
    // prior failed attempt's field error must not leak onto the new blank draft,
    // which keeps `$ui.selected` null across its whole New→Cancel→New lifecycle.
    const draftOpen = isCreateDraftUi(ui);
    const enteringDraft = draftOpen && !draftOpenRef.current;
    draftOpenRef.current = draftOpen;
    if (next === selectedIdRef.current && !enteringDraft) return;
    selectedIdRef.current = next;
    setSelectedId(next);
    setFieldErrors({});
    setConflictReview(false);
    setEnableConflict(false);
    setCompileError(null);
  }, []);

  /** Route a failed save: a 422 naming a field the builder BINDS (`name`) lands on
   * that FormField; anything else — every rule member the compiler rejects — lands
   * in the rule panel's alert quoting the member's own path. */
  const reportSaveFailure = useCallback((context: string, err: unknown) => {
    const fields = fieldErrorsOf(err);
    if (fields && "name" in fields) {
      setFieldErrors(fields);
      toast.error(`${context} — please fix the highlighted fields.`);
      return;
    }
    const message = compileMessageOf(err);
    if (message) {
      setCompileError(message);
      toast.error(`${context}: ${message}`);
      return;
    }
    reportProblem(context, err);
  }, []);

  const handler: ActionHandler = useMemo(
    () => ({
      // "New": create an automation from the document's starter itemDefault, placed
      // on the chosen scope node, then reload so the fresh row is selectable and its
      // rule is authored in the detail pane. An automation MUST have a scope_node the
      // newAction itemDefault can't carry, so with no candidate node there is nothing
      // to place it on — say so plainly rather than POST a body the server would
      // reject. The create carries an Idempotency-Key (the client owns that
      // convention).
      create: async (_target, itemDefault) => {
        setConflictReview(false);
        setEnableConflict(false);
        setCompileError(null);
        const scope = activeScopeRef.current;
        if (!scope) {
          toast.error("Add a site before creating an automation — an automation is placed on a scope node.");
          return;
        }
        // The draft went through the same builder as an existing record, so it gets
        // the same treatment on the way out: scaffolding pruned, mode/max coupled.
        const draft = itemDefault as unknown as Automation;
        const rule = normalizeModeMax(pruneBuilderScaffolding(ruleBodyOf(draft)));
        const body: AutomationCreate = {
          name: typeof itemDefault.name === "string" ? itemDefault.name : "New automation",
          scope_node: scope,
          enabled: itemDefault.enabled !== false,
          mode: rule.mode ?? "single",
          max: rule.max,
          triggers: rule.triggers ?? [],
          conditions: rule.conditions ?? [],
          actions: rule.actions ?? [],
        };
        try {
          const created = await client.automations.create(body);
          toast.success(`Added ${created.data.name}`);
          // The freshly created row becomes the selection (UIS-021): the detail
          // pane reopens on it so its rule keeps being authored, rather than
          // collapsing to the empty prompt.
          selectedIdRef.current = created.data.id;
          setSelectedId(created.data.id);
          await reload();
        } catch (err) {
          reportSaveFailure("Couldn't create the automation", err);
        }
      },
      // "Save changes": persist the WHOLE authored rule — name plus the rules/1
      // projection the builder edited — under its If-Match (the standard optimistic
      // concurrency flow). A 412 re-reads and surfaces the current state for review;
      // a 422's message lands where the operator can act on it.
      submit: async (_target, resource) => {
        const meta = idRev(resource);
        if (!meta) return;
        setFieldErrors({});
        setConflictReview(false);
        setCompileError(null);
        const patch = ruleSaveBody(resource as Automation);
        try {
          const outcome = await updateWithReview(client.automations, meta.id, patch, etagForRevision(meta.revision));
          if (outcome.status === "conflict") {
            toast.error("This automation changed elsewhere. Review the current rule and try again.");
            setConflictReview(true);
          } else {
            toast.success("Saved changes");
          }
          await reload();
        } catch (err) {
          reportSaveFailure("Couldn't save the automation", err);
        }
      },
      remove: async (_target, resource) => {
        const meta = idRev(resource);
        if (!meta) return;
        setConflictReview(false);
        try {
          await client.automations.remove(meta.id, etagForRevision(meta.revision));
          toast.success("Deleted automation");
          if (selectedIdRef.current === meta.id) {
            selectedIdRef.current = null;
            setSelectedId(null);
          }
          await reload();
        } catch (err) {
          reportProblem("Couldn't delete the automation", err);
        }
      },
    }),
    [client, reload, reportSaveFailure],
  );

  // ── First-party affordances for the selected automation ────────────────────

  const runNow = useCallback(async () => {
    if (!selected) return;
    setRunning(true);
    setDisposition(null);
    try {
      const result = await client.automations.run(selected.id);
      setDisposition(result);
      // What the run DID, not merely that it was allowed to start: a run whose
      // targets all refused is a failure the operator has to be told about, and it
      // comes back 200 `ran`.
      const targets = runTargets(result);
      const { label } = runStatus(result, targets);
      if (targets.some((t) => !t.ok)) toast.error(`Run ${label.toLowerCase()}`);
      else toast.success(`Run ${label.toLowerCase()}`);
    } catch (err) {
      reportProblem("Couldn't run the automation", err);
    } finally {
      setRunning(false);
    }
  }, [client, selected]);

  const toggleEnabled = useCallback(async () => {
    if (!selected) return;
    const wasEnabled = selected.enabled;
    setEnableConflict(false);
    setTogglingEnabled(true);
    try {
      const outcome = await updateWithReview(
        client.automations,
        selected.id,
        { enabled: !wasEnabled },
        etagForRevision(selected.revision),
      );
      if (outcome.status === "conflict") {
        // The standard conflict UX, same as the rule save: a toast, the re-read
        // current state (reload re-seeds the badge), AND a persistent review banner
        // — never a silent retry-overwrite. The selection is preserved across the
        // remount so the review state survives the reload.
        toast.error("This automation changed elsewhere. Review it and try again.");
        setEnableConflict(true);
      } else {
        toast.success(wasEnabled ? "Disabled automation" : "Enabled automation");
      }
      await reload();
    } catch (err) {
      reportProblem("Couldn't change the automation", err);
    } finally {
      setTogglingEnabled(false);
    }
  }, [client, selected, reload]);

  /** Open/close the hatch.
   *
   * Opening re-mirrors the live builder — the hatch is a view of the record being
   * edited, and a view that opens onto anything else is how an Apply destroys
   * work that was never on screen — but ONLY when the hatch holds no unapplied
   * draft. An operator who has typed into the textarea owns it, closed or open:
   * re-seeding on every open throws that text away silently, which is the same
   * loss in the other direction, and "Hide JSON" is a fold, not a discard.
   *
   * There is no third case to decide between: a draft the operator has NOT
   * touched (`jsonDraft === jsonSeed`) is already tracking the builder through
   * the seed effect above, and a draft they HAVE touched survives the fold with
   * the divergence warning (`builderDiverged`) telling them the builder moved on
   * while it was closed — which is the same warning, for the same reason, as one
   * that moves while it is open. */
  const toggleJson = useCallback(() => {
    const opening = !jsonOpen;
    setJsonOpen(opening);
    if (!opening) return;
    if (jsonDraft !== jsonSeed) return;
    seedJson(builderJson ?? (selected ? ruleBodyJson(selOverride ?? selected) : ""));
  }, [jsonOpen, jsonDraft, jsonSeed, builderJson, selected, selOverride, seedJson]);

  /** "Apply to builder": parse the hatch's JSON and stage it as the record the
   * builder is seeded with, then remount the renderer so the builder shows it. No
   * network call — Save is still the only thing that writes. */
  const applyJson = useCallback(() => {
    if (!selected) return;
    setJsonError(null);
    setCompileError(null);
    let parsed: unknown;
    try {
      parsed = JSON.parse(jsonDraft);
    } catch (e) {
      setJsonError(`The rule isn't valid JSON: ${(e as Error).message}`);
      return;
    }
    const patch = ruleUpdateFrom(parsed);
    if (!patch) {
      setJsonError("The rule must be a JSON object with the rule's mode / triggers / conditions / actions.");
      return;
    }
    const base = overrides[selected.id] ?? selected;
    setOverrides((prev) => ({ ...prev, [selected.id]: { ...base, ...patch } }));
    setVersion((v) => v + 1);
    toast.success("Applied to the builder — press Save changes to persist it.");
  }, [selected, jsonDraft, overrides]);

  // The last run's per-target report and the chip that summarises it.
  const runReport = useMemo(() => (disposition ? runTargets(disposition) : []), [disposition]);
  const runChip = disposition ? runStatus(disposition, runReport) : null;

  const editorClasses =
    "wv-touch min-h-[11rem] w-full min-w-0 rounded-input border border-border bg-[color:var(--wv-surface-2)] px-3 py-2 font-mono text-[13px] leading-relaxed text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring";

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Automations"
          description="Author the fleet's rules — when a screen turns on, run a playlist; at sunset, power a display. Build the trigger, the conditions and the actions by picking them; drop to raw JSON only for what the builder cannot express."
        />

        {loadError ? (
          <p role="alert" className="text-sm text-[color:var(--wv-err)]">
            Couldn't load automations — {loadError}
          </p>
        ) : null}

        {/* grammar-gap: an automation's create needs a scope_node the list-detail
            newAction cannot carry (like a screen's parent or a schedule's node), and
            it is fixed after create — so when the org spans more than one node the
            operator MUST pick where a new automation lands. A first-party target
            picker until the grammar grows a create-form with a placement selector;
            the New button itself is the dogfooded newAction verb. */}
        {scopeNodes.length > 1 ? (
          <div className="max-w-sm">
            <FormField label="Add new automations on">
              {(field) => (
                <select
                  {...field}
                  className="wv-touch flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  value={activeScope ?? ""}
                  onChange={(e) => setTargetScopeId(e.target.value)}
                >
                  {scopeNodes.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name}
                    </option>
                  ))}
                </select>
              )}
            </FormField>
          </div>
        ) : null}

        {conflictReview ? (
          <p
            role="status"
            className="rounded-card border border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] p-4 text-sm text-[color:var(--wv-warn)]"
          >
            This automation was changed elsewhere while you were editing. The current rule is shown below —
            review it, then save again to apply your change.
          </p>
        ) : null}

        {/* The compiler's refusal, quoting the rule member it named. It sits ABOVE
            the renderer rather than inside the selected-automation panel below,
            because a CREATE that fails to compile has no selection to hang it on —
            the draft form is the only thing on screen, and a refusal that rendered
            only for an already-saved record would be invisible in exactly the case
            an operator most needs it. */}
        {compileError ? (
          <p
            role="alert"
            data-testid="compile-error"
            className="rounded-card border border-[color:var(--wv-err)] bg-[color:var(--wv-err-bg)] p-4 text-sm text-[color:var(--wv-err)]"
          >
            The rule was refused — {compileError}
          </p>
        ) : null}

        <main className="min-w-0">
          {automations === null ? (
            <p className="text-sm text-muted-foreground">Loading automations…</p>
          ) : (
            <PageRenderer
              key={version}
              doc={automationsPageDoc}
              data={{ automations: boundAutomations, entities }}
              initialUi={{ selected: selectedId }}
              messages={messages}
              handler={handler}
              fieldErrors={fieldErrors}
              onUiChange={onUiChange}
              onResourceChange={onResourceChange}
            />
          )}
        </main>

        {/* The first-party affordances the ui-schema grammar genuinely lacks for the
            SELECTED automation: running it, enabling/disabling it, and the raw-JSON
            escape hatch (there is no widget in UIS-070's input catalog that edits a
            free-form rules/1 member, and the compiler is the authority on that
            body's shape). Everything ELSE that used to live here — the rule itself —
            is now the ui-schema builder above. */}
        {selected ? (
          <section
            aria-label={`Rule and actions for ${selected.name}`}
            className="flex flex-col gap-4 rounded-card border border-border bg-card p-4 wv-elevation sm:p-5"
          >
            <div className="flex flex-wrap items-center justify-between gap-3">
              <h2 className="font-display text-[17px] font-semibold">Run &amp; advanced</h2>
              <div className="flex flex-wrap items-center gap-2">
                <Button
                  variant="secondary"
                  size="sm"
                  icon={Play}
                  className="wv-touch"
                  disabled={running}
                  onClick={() => void runNow()}
                >
                  Run now
                </Button>
                <Button
                  variant="secondary"
                  size="sm"
                  icon={Power}
                  className="wv-touch"
                  disabled={togglingEnabled}
                  onClick={() => void toggleEnabled()}
                >
                  {selected.enabled ? "Disable" : "Enable"}
                </Button>
                <Button
                  variant="secondary"
                  size="sm"
                  icon={Braces}
                  className="wv-touch"
                  aria-expanded={jsonOpen}
                  onClick={toggleJson}
                >
                  {jsonOpen ? "Hide JSON" : "Edit as JSON"}
                </Button>
                {runChip ? (
                  <span data-testid="run-disposition" data-status={runChip.status} className="inline-flex items-center">
                    <StatusBadge status={runChip.status}>{runChip.label}</StatusBadge>
                  </span>
                ) : null}
              </div>
            </div>

            {enableConflict ? (
              <p
                role="status"
                className="rounded-card border border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] p-3 text-sm text-[color:var(--wv-warn)]"
              >
                This automation's enabled state was changed elsewhere. The current state is shown above — review
                it, then toggle again to apply your change.
              </p>
            ) : null}

            {/* What the last run actually DID. The chip above is the mode
                evaluation; this is the effect report — every device command and
                every signage write, with the refusal quoted on any that did not
                take. Without it a run whose targets all refused (an unadopted
                entity, an offline relay) reads as a plain success. */}
            {disposition ? (
              <div data-testid="run-report" className="flex flex-col gap-2 rounded-card border border-border p-3">
                <p className="text-[13px] text-muted-foreground">
                  Run {disposition.run_id}
                  {disposition.dry_run ? " · effects withheld (dry run)" : ""}
                  {disposition.delays_collapsed
                    ? ` · ${disposition.delays_collapsed} delay${disposition.delays_collapsed === 1 ? "" : "s"} passed through without waiting`
                    : ""}
                </p>
                {runReport.length === 0 ? (
                  <p className="text-sm text-muted-foreground">
                    No device or signage effects — this run changed nothing.
                  </p>
                ) : (
                  <ul className="flex flex-col gap-1">
                    {runReport.map((t, i) => (
                      <li
                        key={i}
                        data-testid="run-target"
                        data-ok={t.ok}
                        className="flex flex-wrap items-center gap-2 text-sm"
                      >
                        <StatusBadge status={t.ok ? "ok" : "error"}>{t.ok ? "Done" : "Failed"}</StatusBadge>
                        <span className="font-mono text-[13px]">{t.label}</span>
                        {t.error ? <span className="text-[color:var(--wv-err)]">{t.error}</span> : null}
                      </li>
                    ))}
                  </ul>
                )}
                {(disposition.logs ?? []).length > 0 ? (
                  <ul className="flex flex-col gap-1">
                    {(disposition.logs ?? []).map((l, i) => (
                      <li key={i} data-testid="run-log" className="text-sm text-muted-foreground">
                        <span className="font-mono text-[12px] uppercase">{l.level}</span> {l.message}
                      </li>
                    ))}
                  </ul>
                ) : null}
              </div>
            ) : null}

            {jsonOpen ? (
              <>
                {builderDiverged ? (
                  <p
                    role="status"
                    data-testid="json-diverged"
                    className="rounded-card border border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] p-3 text-sm text-[color:var(--wv-warn)]"
                  >
                    The builder has changed since you edited this JSON. Applying will replace those builder edits
                    with what is written here.{" "}
                    <button
                      type="button"
                      className="underline underline-offset-2"
                      onClick={() => seedJson(builderJson ?? jsonDraft)}
                    >
                      Reload from the builder
                    </button>
                  </p>
                ) : null}
                <FormField
                  label="Rule body (JSON)"
                  help="The whole rules/1 rule, mirroring what the builder holds right now — including edits you have not saved. Nothing here is written until you apply it to the builder and press Save changes: the builder is the single save path, so an edit can never be lost to the other surface."
                  {...(jsonError ? { error: jsonError } : {})}
                >
                  {(field) => (
                    <textarea
                      {...field}
                      value={jsonDraft}
                      onChange={(e) => setJsonDraft(e.target.value)}
                      spellCheck={false}
                      rows={14}
                      className={editorClasses}
                    />
                  )}
                </FormField>
                <div className="flex justify-end">
                  <Button className="wv-touch" onClick={applyJson}>
                    Apply to builder
                  </Button>
                </div>
              </>
            ) : null}
          </section>
        ) : null}
      </div>

      <Toaster />
    </div>
  );
}
