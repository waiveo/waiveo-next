import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Play, Power, Save } from "lucide-react";
import { PageRenderer, type ActionHandler } from "@/renderer";
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
  type AutomationUpdate,
  type FieldErrors,
  type ScopeNode,
  type WaiveoApi,
} from "@/api";
import automationsPageDoc from "./page.uis.json";

/**
 * The Automations route — the fleet's authored rules/1 rules, as a DOGFOODED
 * ui-schema/1 list-detail (name column + execution-mode column; the detail badges
 * the mode and the enabled state) rendered through the SAME PageRenderer an
 * extension page takes, plus the three sanctioned first-party affordances the
 * grammar genuinely lacks. This file is the host seam: it feeds the live
 * automations in as the page's bound data, wires the dogfood verbs (create=New,
 * submit=save name, delete) onto the typed api/1 client, and hosts the first-party
 * controls beneath the renderer for the selected automation — Run, enable/disable,
 * and the raw rule-body code editor. Creation is dogfooded: the document's
 * `newAction` verb creates the automation from a starter template placed on the
 * scope chosen by a first-party target picker (the same grammar gap Screens and
 * Schedules host), and the operator authors the real rule in the detail pane.
 *
 * Every mutation applies the conventions the client owns: creates carry an
 * Idempotency-Key; edits/deletes carry the If-Match derived from the record's
 * revision (no unconditional overwrite); a 412 REVISION_CONFLICT re-reads and
 * surfaces the current state for review rather than silently retrying; a 422
 * VALIDATION_FAILED's field errors surface where the operator can fix them — for
 * an authored rule that means the compiler's message lands on the rule-body field.
 *
 * NOTE on `execution_class`: the rules compiler's edge/app classification is a
 * server-internal column, NOT a field of the api/1 Automation resource (openapi
 * Automation is `additionalProperties:false` and omits it — confirmed against the
 * live feeder). The console therefore badges the wire-available execution `mode`
 * (single/restart/queued/parallel) and the `enabled` state; surfacing edge/app
 * would require the api/1 resource to expose it (out of this UI task's scope).
 */

// The message catalog the document's `msg:` references resolve against.
const messages: Record<string, string> = {
  "msg:auto.col.name": "Name",
  "msg:auto.col.mode": "Mode",
  "msg:auto.detail.title": "Automation",
  "msg:auto.detail.empty": "Select an automation to edit it, or add a new one.",
  "msg:auto.detail.name": "Name",
  "msg:auto.detail.classification": "Mode & state",
  "msg:auto.detail.save": "Save changes",
  "msg:auto.detail.delete": "Delete automation",
};

/** A run's mode-evaluation disposition (rules/1 RunDisposition) → a StatusBadge
 * tone. `ran` is the live/started outcome (the ok/green lane, reserved for it);
 * `restarted` is a warn; `skipped` reads as off. */
const DISPOSITION: Record<string, { status: Status; label: string }> = {
  ran: { status: "ok", label: "Ran" },
  restarted: { status: "warn", label: "Restarted" },
  skipped: { status: "off", label: "Skipped" },
};

/** A 422 VALIDATION_FAILED's per-field errors, if this failure is one. */
function fieldErrorsOf(err: unknown): FieldErrors | null {
  if (err instanceof ApiError && err.status === 422 && Object.keys(err.fieldErrors).length > 0) {
    return err.fieldErrors;
  }
  return null;
}

/** The compiler's message for a 422, or the Problem detail/code — the single
 * message the rule-body field shows (the whole rule is one editor, so any member
 * error, e.g. `actions[0].type`, pins to the body). */
function compileMessageOf(err: unknown): string | null {
  const fields = fieldErrorsOf(err);
  if (fields) return Object.values(fields)[0] ?? null;
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

/** The authored rule projection (rules/1 vocabulary only) an automation carries,
 * pretty-printed for the code editor. Identity/placement/baseline (id, scope_node,
 * revision, timestamps) are NOT the rule — they are edited elsewhere. */
function ruleBodyJson(a: Automation): string {
  return JSON.stringify(
    { mode: a.mode, max: a.max ?? null, triggers: a.triggers, conditions: a.conditions, actions: a.actions },
    null,
    2,
  );
}

/** Turn a parsed rule-body object into an AutomationUpdate carrying only the rule
 * vocabulary keys present. Returns null when the parsed value is not a JSON object
 * (an array or scalar is not a rule). */
function ruleUpdateFrom(parsed: unknown): AutomationUpdate | null {
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return null;
  const o = parsed as Record<string, unknown>;
  // Cast via the (required, non-optional) Automation field types so the assigned
  // values carry no `undefined` — exactOptionalPropertyTypes forbids assigning an
  // explicit `undefined` to AutomationUpdate's optional keys.
  const patch: AutomationUpdate = {};
  if ("mode" in o) patch.mode = o.mode as Automation["mode"];
  if ("max" in o) patch.max = o.max as Automation["max"];
  if ("triggers" in o) patch.triggers = o.triggers as Automation["triggers"];
  if ("conditions" in o) patch.conditions = o.conditions as Automation["conditions"];
  if ("actions" in o) patch.actions = o.actions as Automation["actions"];
  return patch;
}

export default function AutomationsRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [automations, setAutomations] = useState<Automation[] | null>(null);
  const [scopeNodes, setScopeNodes] = useState<ScopeNode[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selectedIdRef = useRef<string | null>(null);
  // 422 field errors + the 412 review banner for the dogfood name form.
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({});
  const [conflictReview, setConflictReview] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [version, setVersion] = useState(0);

  // First-party rule panel state (out of band of the renderer).
  const [ruleDraft, setRuleDraft] = useState("");
  const [ruleError, setRuleError] = useState<string | null>(null);
  const [ruleConflict, setRuleConflict] = useState(false);
  const [savingRule, setSavingRule] = useState(false);
  const [disposition, setDisposition] = useState<AutomationRunResult | null>(null);
  const [running, setRunning] = useState(false);
  const [togglingEnabled, setTogglingEnabled] = useState(false);
  // The persistent 412 review state for the enable/disable toggle — its own third
  // conflict flag alongside the name form's (conflictReview) and the rule's
  // (ruleConflict), so a toggle conflict surfaces the standard toast + re-read +
  // review banner rather than only a fading toast.
  const [enableConflict, setEnableConflict] = useState(false);

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
  }, [client]);

  useEffect(() => {
    void load();
  }, [load]);

  const reload = useCallback(async () => {
    setFieldErrors({});
    await load();
    setVersion((v) => v + 1);
  }, [load]);

  const selected = useMemo(
    () => (automations ?? []).find((a) => a.id === selectedId) ?? null,
    [automations, selectedId],
  );

  // Re-seed the rule editor whenever the selected automation (or its revision after
  // one of our own saves) changes: the editor mirrors the current server rule, and
  // a fresh selection starts clean. ruleConflict is intentionally NOT cleared here —
  // a 412 reload re-seeds the current values but the review banner must persist
  // until the operator moves on (onUiChange) or re-saves.
  const selId = selected?.id;
  const selRev = selected?.revision;
  useEffect(() => {
    if (!selected) return;
    setRuleDraft(ruleBodyJson(selected));
    setRuleError(null);
    setDisposition(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selId, selRev]);

  // The renderer owns the selection; moving to a different record retires any
  // captured field errors (keyed by bind path, no record identity) and the conflict
  // review states that belonged to the record just left.
  const onUiChange = useCallback((ui: Record<string, unknown>) => {
    const next = typeof ui.selected === "string" ? ui.selected : null;
    if (next === selectedIdRef.current) return;
    selectedIdRef.current = next;
    setSelectedId(next);
    setFieldErrors({});
    setConflictReview(false);
    setRuleConflict(false);
    setEnableConflict(false);
  }, []);

  const handler: ActionHandler = useMemo(
    () => ({
      // "New": create an automation from the document's starter itemDefault, placed
      // on the chosen scope node, then reload so the fresh row is selectable and its
      // rule is authored in the detail pane (the same idiom Screens/Schedules use).
      // An automation MUST have a scope_node the newAction itemDefault can't carry,
      // so with no candidate node there is nothing to place it on — say so plainly
      // rather than POST a body the server would reject. The create carries an
      // Idempotency-Key (the client owns that convention).
      create: async (_target, itemDefault) => {
        setConflictReview(false);
        setRuleConflict(false);
        setEnableConflict(false);
        const scope = activeScopeRef.current;
        if (!scope) {
          toast.error("Add a site before creating an automation — an automation is placed on a scope node.");
          return;
        }
        const rule = ruleUpdateFrom(itemDefault) ?? {};
        const body: AutomationCreate = {
          name: typeof itemDefault.name === "string" ? itemDefault.name : "New automation",
          scope_node: scope,
          enabled: true,
          mode: (rule.mode ?? "single") as AutomationCreate["mode"],
          triggers: (rule.triggers ?? []) as AutomationCreate["triggers"],
          actions: (rule.actions ?? []) as AutomationCreate["actions"],
          ...(rule.max !== undefined ? { max: rule.max } : {}),
          ...(rule.conditions !== undefined ? { conditions: rule.conditions } : {}),
        };
        try {
          const created = await client.automations.create(body);
          toast.success(`Added ${created.data.name}`);
          // The freshly created row becomes the selection (UIS-021): the detail
          // pane reopens on it so its rule is authored straight away, rather than
          // collapsing to the empty prompt.
          selectedIdRef.current = created.data.id;
          setSelectedId(created.data.id);
          await reload();
        } catch (err) {
          const message = compileMessageOf(err);
          if (message) {
            toast.error(`Couldn't create the automation: ${message}`);
          } else {
            reportProblem("Couldn't create the automation", err);
          }
        }
      },
      // "Save changes": persist the edited name under its If-Match (the standard
      // optimistic-concurrency flow); a 412 re-reads and surfaces the current state
      // for review, a 422's field errors land on the FormField.
      submit: async (_target, resource) => {
        const meta = idRev(resource);
        if (!meta) return;
        setFieldErrors({});
        setConflictReview(false);
        const r = resource as Automation;
        const patch: AutomationUpdate = { name: r.name };
        try {
          const outcome = await updateWithReview(client.automations, meta.id, patch, etagForRevision(meta.revision));
          if (outcome.status === "conflict") {
            toast.error("This automation changed elsewhere. Review the current values and try again.");
            setConflictReview(true);
          } else {
            toast.success("Saved changes");
          }
          await reload();
        } catch (err) {
          const fields = fieldErrorsOf(err);
          if (fields) {
            setFieldErrors(fields);
            toast.error("Couldn't save the automation — please fix the highlighted fields.");
          } else {
            reportProblem("Couldn't save the automation", err);
          }
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
    [client, reload],
  );

  // ── First-party affordances for the selected automation ────────────────────

  const runNow = useCallback(async () => {
    if (!selected) return;
    setRunning(true);
    setDisposition(null);
    try {
      const result = await client.automations.run(selected.id);
      setDisposition(result);
      const label = DISPOSITION[result.disposition]?.label ?? result.disposition;
      toast.success(`Run ${label.toLowerCase()}`);
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
        // The standard conflict UX, same as the name-save and rule-save flows: a
        // toast, the re-read current state (reload re-seeds the badge), AND a
        // persistent review banner — never a silent retry-overwrite. The selection
        // is preserved across the remount so the review state survives the reload.
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

  const saveRule = useCallback(async () => {
    if (!selected) return;
    setRuleError(null);
    setRuleConflict(false);
    let parsed: unknown;
    try {
      parsed = JSON.parse(ruleDraft);
    } catch (e) {
      setRuleError(`The rule isn't valid JSON: ${(e as Error).message}`);
      return;
    }
    const patch = ruleUpdateFrom(parsed);
    if (!patch) {
      setRuleError("The rule must be a JSON object with the rule's mode / triggers / conditions / actions.");
      return;
    }
    setSavingRule(true);
    try {
      const outcome = await updateWithReview(client.automations, selected.id, patch, etagForRevision(selected.revision));
      if (outcome.status === "conflict") {
        toast.error("This automation changed elsewhere. Review the current rule and try again.");
        setRuleConflict(true);
      } else {
        toast.success("Saved rule");
      }
      await reload();
    } catch (err) {
      const message = compileMessageOf(err);
      if (message) {
        setRuleError(message);
      } else {
        reportProblem("Couldn't save the rule", err);
      }
    } finally {
      setSavingRule(false);
    }
  }, [client, selected, ruleDraft, reload]);

  const editorClasses =
    "wv-touch min-h-[11rem] w-full min-w-0 rounded-input border border-border bg-[color:var(--wv-surface-2)] px-3 py-2 font-mono text-[13px] leading-relaxed text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring";

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Automations"
          description="Author the fleet's rules — when a screen turns on, run a playlist; on a schedule, power a display. Listed and edited through the same declarative page any extension renders through; the rule itself is authored as JSON."
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
            This automation was changed elsewhere while you were editing. The current values are shown below —
            review them, then save again to apply your change.
          </p>
        ) : null}

        <main className="min-w-0">
          {automations === null ? (
            <p className="text-sm text-muted-foreground">Loading automations…</p>
          ) : (
            <PageRenderer
              key={version}
              doc={automationsPageDoc}
              data={{ automations }}
              initialUi={{ selected: selectedId }}
              messages={messages}
              handler={handler}
              fieldErrors={fieldErrors}
              onUiChange={onUiChange}
            />
          )}
        </main>

        {/* grammar-gap: code editor. ui-schema/1's input catalog (text/number/
            duration/toggle/select/entity/time — UIS-070) has no widget that edits a
            free-form rules/1 rule as raw JSON, and the compiler is the authority on
            that body's shape (a 422 pins the exact offending rule member). The raw
            rule editor is one of the three sanctioned first-party gaps (alongside
            file upload and the live stream); Run and enable/disable ride alongside it
            as the selected automation's actions. Future path: a structured rule
            builder would be a ui-schema/1 surface once rules/1's authoring shape is
            specced as widgets. */}
        {selected ? (
          <section
            aria-label={`Rule and actions for ${selected.name}`}
            className="flex flex-col gap-4 rounded-card border border-border bg-card p-4 wv-elevation sm:p-5"
          >
            <div className="flex flex-wrap items-center justify-between gap-3">
              <h2 className="font-display text-[17px] font-semibold">Rule &amp; actions</h2>
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
                {disposition ? (
                  <span
                    data-testid="run-disposition"
                    data-status={DISPOSITION[disposition.disposition]?.status ?? "pending"}
                    className="inline-flex items-center"
                  >
                    <StatusBadge status={DISPOSITION[disposition.disposition]?.status ?? "pending"}>
                      {DISPOSITION[disposition.disposition]?.label ?? disposition.disposition}
                    </StatusBadge>
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

            {ruleConflict ? (
              <p
                role="status"
                className="rounded-card border border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] p-3 text-sm text-[color:var(--wv-warn)]"
              >
                This automation's rule was changed elsewhere while you were editing. The current rule is shown —
                review it, then save again to apply your change.
              </p>
            ) : null}

            <FormField
              label="Rule body (JSON)"
              help="The rules/1 rule — mode, triggers, conditions, and actions. Saved under the automation's revision; the compiler validates it on save."
              {...(ruleError ? { error: ruleError } : {})}
            >
              {(field) => (
                <textarea
                  {...field}
                  value={ruleDraft}
                  onChange={(e) => setRuleDraft(e.target.value)}
                  spellCheck={false}
                  rows={12}
                  className={editorClasses}
                />
              )}
            </FormField>

            <div className="flex justify-end">
              <Button icon={Save} className="wv-touch" disabled={savingRule} onClick={() => void saveRule()}>
                Save rule
              </Button>
            </div>
          </section>
        ) : null}
      </div>

      <Toaster />
    </div>
  );
}
