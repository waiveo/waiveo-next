import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { PageRenderer, isCreateDraftUi, type ActionHandler } from "@/renderer";
import { PageHeader, Toaster, toast } from "@/components/kit";
import {
  ApiError,
  collectPages,
  createApi,
  etagForRevision,
  updateWithReview,
  type FieldErrors,
  type ScopeNode,
  type Variable,
  type VariableCreate,
  type VariableUpdate,
  type WaiveoApi,
} from "@/api";
import variablesPageDoc from "./page.uis.json";

/**
 * The Variables route — the operator surface over `data-model/1`'s Variables
 * (DAT-130–138): the shared state a `rules/1` `variable` condition reads
 * (RUL-150) and a `variable_write` action writes (RUL-220).
 *
 * Authored as a ui-schema/1 `list-detail` document rendered through the SAME
 * PageRenderer an extension page goes through (parity-loop Step 1.5). This file
 * is only the host seam: it feeds the rows in as the page's bound data and wires
 * the closed action verbs onto the typed api/1 client.
 *
 * # The one thing the document cannot express, and how it is worked around
 *
 * A variable's `value` is a POLYMORPHIC SCALAR — string, number, or boolean
 * (DAT-132) — and the closed widget catalog has no widget that binds one. Its
 * three input widgets each bind one type (`text-input` a string, `number-input` a
 * number, `toggle` a boolean), and `switch` discriminates on a BOUND SIBLING
 * FIELD, never on the runtime type of the value it is about to render.
 *
 * grammar-gap: the renderer needs either (a) a `scalar-input` widget kind that
 * binds a string|number|boolean and carries its own type affordance, or (b) a
 * `switch` discriminant that can name a value's runtime JSON type. Recorded per
 * Step 1.5 rather than added — web/src/renderer/** is another track's.
 *
 * The workaround keeps the DOCUMENT honest and puts the cost in the host: the
 * rows handed to the renderer are PROJECTED (toViewRow) into an explicit
 * `value_kind` plus one field per type, which the document's `select` +
 * `switch` then bind ordinarily; the reverse projection (valueFromView)
 * reassembles the single wire scalar on the way out. Nothing about the page's
 * grammar is bent — it is a plain discriminated form over fields that exist.
 */

const messages: Record<string, string> = {
  "msg:variables.col.name": "Name",
  "msg:variables.col.value": "Value",
  "msg:variables.col.kind": "Type",
  "msg:variables.col.scope": "Scope",
  "msg:variables.detail.title": "Variable",
  "msg:variables.detail.empty": "Select a variable to edit it, or add a new one.",
  "msg:variables.detail.name": "Name",
  "msg:variables.detail.namePlaceholder": "lowercase_with_underscores",
  "msg:variables.detail.scope": "Scope node",
  "msg:variables.detail.kind": "Type",
  "msg:variables.kind.string": "Text",
  "msg:variables.kind.number": "Number",
  "msg:variables.kind.boolean": "True / false",
  "msg:variables.detail.valueText": "Value",
  "msg:variables.detail.valueNumber": "Value",
  "msg:variables.detail.valueBoolean": "Value",
  "msg:variables.detail.on": "True",
  "msg:variables.detail.off": "False",
  "msg:variables.detail.save": "Save changes",
  "msg:variables.detail.delete": "Delete variable",
  // A variable is SHARED RULE STATE: a rules/1 condition reads it (RUL-150) and an
  // action writes it (RUL-220), so deleting one can silently stop an automation
  // that depends on it. One misclick, no undo — hence the gate.
  //
  // This was ungated until now, and the reason is worth keeping: this page was
  // authored BEFORE a confirm existed on an ActionRef. The track recorded that
  // honestly; UIS-165 landed two merges later in ui-schema/1 1.1 and nothing
  // re-read the note (HV-21).
  "msg:variables.delete.confirm.title": "Delete this variable?",
  "msg:variables.delete.confirm.body":
    "Any automation that reads this variable in a condition, or writes it in an action, will stop finding it. Those rules keep running — the condition simply stops matching — so nothing will report an error. Deleting it does not delete them.",
  "msg:variables.delete.confirm.ok": "Delete it",
  "msg:variables.delete.confirm.cancel": "Keep it",
};

/** The three value types DAT-132 admits. */
type ValueKind = "string" | "number" | "boolean";

/** One variable projected into the fields the document binds. See the file's
 * own grammar-gap note for why the projection exists. */
type VariableViewRow = Variable & {
  value_kind: ValueKind;
  value_text: string;
  value_number: number | null;
  value_boolean: boolean;
  /** The value rendered for the LIST column, as a string. A table cell binds one
   * path, and the three typed fields would need three columns. */
  value_display: string;
  /** The placement's human name, so the list column reads "Demo Site" rather
   * than a ULID. Falls back to the id when the node is not readable. */
  scope_name: string;
};

/** Which of DAT-132's three scalars a stored value is. Anything else — which the
 * server refuses to store — is shown as text so an unexpected row is still
 * editable rather than blank. */
function kindOf(value: unknown): ValueKind {
  if (typeof value === "number") return "number";
  if (typeof value === "boolean") return "boolean";
  return "string";
}

/** Render a scalar for the list column. */
function displayOf(value: unknown): string {
  if (typeof value === "boolean") return value ? "true" : "false";
  if (value === null || value === undefined) return "";
  return String(value);
}

function toViewRow(v: Variable, nodeNames: Map<string, string>): VariableViewRow {
  const kind = kindOf(v.value);
  return {
    ...v,
    value_kind: kind,
    value_text: kind === "string" ? String(v.value ?? "") : "",
    value_number: kind === "number" ? (v.value as number) : null,
    value_boolean: kind === "boolean" ? (v.value as boolean) : false,
    value_display: displayOf(v.value),
    scope_name: nodeNames.get(v.scope_node) ?? v.scope_node,
  };
}

/** The inverse projection: the single wire scalar this edited row means.
 *
 * A cleared `number-input` binds `null` (UIS-072), and `null` is NOT a settable
 * variable value (DAT-133) — so it is reported as unsendable here rather than
 * sent and refused at the server with a message about a member the operator
 * never typed. */
function valueFromView(row: VariableViewRow): { value: Variable["value"] } | { error: string } {
  switch (row.value_kind) {
    case "number":
      if (typeof row.value_number !== "number" || Number.isNaN(row.value_number)) {
        return { error: "Enter a number — a variable cannot be left unset. To unset one, delete it." };
      }
      return { value: row.value_number };
    case "boolean":
      return { value: Boolean(row.value_boolean) };
    default:
      return { value: String(row.value_text ?? "") };
  }
}

function fieldErrorsOf(err: unknown): FieldErrors | null {
  if (err instanceof ApiError && err.status === 422 && Object.keys(err.fieldErrors).length > 0) {
    return err.fieldErrors;
  }
  return null;
}

function reportProblem(context: string, err: unknown): void {
  if (err instanceof ApiError) {
    const fieldMsg = Object.values(err.fieldErrors)[0];
    toast.error(`${context}: ${fieldMsg ?? err.detail ?? err.code}`);
  } else {
    toast.error(`${context}: the service is unreachable.`);
  }
}

function idRev(resource: unknown): { id: string; revision: number } | null {
  if (resource && typeof resource === "object") {
    const r = resource as Partial<Variable>;
    if (typeof r.id === "string" && typeof r.revision === "number") {
      return { id: r.id, revision: r.revision };
    }
  }
  return null;
}

export default function VariablesRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [variables, setVariables] = useState<Variable[] | null>(null);
  const [nodes, setNodes] = useState<ScopeNode[]>([]);
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({});
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selectedIdRef = useRef<string | null>(null);
  const draftOpenRef = useRef(false);
  const [conflictReview, setConflictReview] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [version, setVersion] = useState(0);

  // The FALLBACK placement for a draft saved without a choice. A variable MUST
  // carry a placement (DAT-006/DAT-130), so there has to be one.
  //
  // This used to be the only placement a console-created variable ever got,
  // because the comment here said `newAction`'s grammar "cannot carry a chosen
  // one". That reading was wrong and cost the page its primary job: the grammar's
  // itemDefault is indeed a literal-only seed (UIS-108/109), but the seam is not
  // handed the seed — on a draft save the renderer passes `scope.root`, the record
  // the detail form has been editing (actions.tsx:203). The choice was in the
  // parameter all along.
  const nodesRef = useRef<ScopeNode[]>([]);
  nodesRef.current = nodes;

  const load = useCallback(async () => {
    try {
      const [rows, nodeRows] = await Promise.all([
        collectPages<Variable>((cursor) => client.variables.list({ cursor })),
        collectPages<ScopeNode>((cursor) => client.scopeNodes.list({ cursor })),
      ]);
      setVariables(rows);
      setNodes(nodeRows);
      setLoadError(null);
    } catch (err) {
      setVariables([]);
      setNodes([]);
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

  const onUiChange = useCallback((ui: Record<string, unknown>) => {
    const next = typeof ui.selected === "string" ? ui.selected : null;
    const draftOpen = isCreateDraftUi(ui);
    const enteringDraft = draftOpen && !draftOpenRef.current;
    draftOpenRef.current = draftOpen;
    if (next === selectedIdRef.current && !enteringDraft) return;
    selectedIdRef.current = next;
    setSelectedId(next);
    setFieldErrors({});
    setConflictReview(false);
  }, []);

  const handler: ActionHandler = useMemo(
    () => ({
      create: async (_target, itemDefault) => {
        setFieldErrors({});
        setConflictReview(false);
        // The DRAFT, not the itemDefault. UIS-108/109 requires itemDefault to be a
        // literal-only seed, so it is what the form STARTED as and never what the
        // operator made it. Building the body from it discarded every edit
        // silently: the form offers scope, type and value, and a create posted a
        // default-scoped empty string regardless. The scope selection vanishing
        // also made DAT-134/135 overrides — the same name at a node and its
        // ancestor — unreachable from the console, and a name collision on the
        // substituted default then reported DAT-131 "already exists at this scope
        // node" about a placement the operator never chose.
        // `itemDefault` IS the live draft on a draft save — the renderer hands the
        // seam `scope.root`, the record the detail form has been editing
        // (actions.tsx:203). The previous code read only `.name` from it and
        // hardcoded the rest, on the strength of a comment in this file claiming
        // the parameter was the page's static seed. It is the seed only at the
        // moment the draft is CREATED; by Save it carries every edit.
        const draftRow = itemDefault as unknown as VariableViewRow;
        const chosenScope =
          typeof draftRow.scope_node === "string" && draftRow.scope_node !== ""
            ? draftRow.scope_node
            : undefined;
        // The first readable node stays the FALLBACK, not the answer: a variable
        // MUST carry a placement (DAT-006/DAT-130), so a draft dismissed without a
        // choice still has somewhere to land.
        const placement = chosenScope ?? nodesRef.current[0]?.id;
        if (!placement) {
          toast.error("Add a scope node before adding a variable — a variable is placed at one.");
          return;
        }
        // Projected through the SAME function submit uses, so a create and an edit
        // cannot disagree about what "boolean false" or "the number 0" means.
        const projected = valueFromView(draftRow);
        if ("error" in projected) {
          setFieldErrors({ value_number: projected.error });
          toast.error("Couldn't add the variable — please fix the highlighted field.");
          return;
        }
        const body: VariableCreate = {
          name: typeof draftRow.name === "string" && draftRow.name !== "" ? draftRow.name : "new_variable",
          value: projected.value,
          scope_node: placement,
        };
        try {
          const created = await client.variables.create(body);
          toast.success(`Added ${created.data.name}`);
          selectedIdRef.current = created.data.id;
          setSelectedId(created.data.id);
          await reload();
        } catch (err) {
          reportProblem("Couldn't add the variable", err);
        }
      },
      submit: async (_target, resource) => {
        const meta = idRev(resource);
        if (!meta) return;
        setFieldErrors({});
        setConflictReview(false);
        const row = resource as VariableViewRow;
        const projected = valueFromView(row);
        if ("error" in projected) {
          setFieldErrors({ value_number: projected.error });
          toast.error("Couldn't save the variable — please fix the highlighted field.");
          return;
        }
        const patch: VariableUpdate = {
          name: row.name,
          value: projected.value,
          scope_node: row.scope_node,
        };
        try {
          const outcome = await updateWithReview(
            client.variables,
            meta.id,
            patch,
            etagForRevision(meta.revision),
          );
          if (outcome.status === "conflict") {
            toast.error("This variable changed elsewhere. Review the current values and try again.");
            setConflictReview(true);
          } else {
            toast.success("Saved changes");
          }
          await reload();
        } catch (err) {
          const fields = fieldErrorsOf(err);
          if (fields) {
            setFieldErrors(fields);
            toast.error("Couldn't save the variable — please fix the highlighted fields.");
          } else {
            reportProblem("Couldn't save the variable", err);
          }
        }
      },
      remove: async (_target, resource) => {
        const meta = idRev(resource);
        if (!meta) return;
        setConflictReview(false);
        try {
          await client.variables.remove(meta.id, etagForRevision(meta.revision));
          toast.success("Deleted variable");
          await reload();
        } catch (err) {
          reportProblem("Couldn't delete this variable", err);
        }
      },
    }),
    [client, reload],
  );

  const nodeNames = useMemo(
    () => new Map(nodes.map((n) => [n.id, n.name] as [string, string])),
    [nodes],
  );
  const viewRows = useMemo(
    () => (variables ?? []).map((v) => toViewRow(v, nodeNames)),
    [variables, nodeNames],
  );

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Variables"
          description="Shared values your automations read and write. A rule can test a variable in a condition and set it in an action; a value set at a site is visible to every group and screen beneath it, unless one of them overrides it."
        />
        {/* DAT-138, said where an operator is standing when they would get it
            wrong. A variable is rule-readable, published on every change, and
            returned to anyone who can see its placement — so the prohibition
            belongs on the surface, not only in the contract. */}
        <p className="rounded-card border border-border bg-surface-2 p-4 text-sm text-muted-foreground">
          Don't put passwords, API keys or tokens here. A variable is readable by every automation, is
          published in full on the activity feed each time it changes, and is visible to anyone who can see
          the scope node it sits at.
        </p>
        {loadError ? (
          <p role="alert" className="text-sm text-[color:var(--wv-err)]">
            Couldn't load variables — {loadError}
          </p>
        ) : null}
        {conflictReview ? (
          <p
            role="status"
            className="rounded-card border border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] p-4 text-sm text-[color:var(--wv-warn)]"
          >
            This variable was changed elsewhere while you were editing. The current values are shown below —
            review them, then save again to apply your change.
          </p>
        ) : null}
        <main className="min-w-0">
          {variables === null ? (
            <p className="text-sm text-muted-foreground">Loading variables…</p>
          ) : (
            <PageRenderer
              key={version}
              doc={variablesPageDoc}
              data={{ variables: viewRows, nodes }}
              initialUi={{ selected: selectedId }}
              messages={messages}
              handler={handler}
              fieldErrors={fieldErrors}
              onUiChange={onUiChange}
            />
          )}
        </main>
      </div>
      <Toaster />
    </div>
  );
}
