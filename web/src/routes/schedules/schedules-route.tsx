import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Plus, Pencil } from "lucide-react";
import { PageRenderer, isCreateDraftUi, type ActionHandler } from "@/renderer";
import {
  Button,
  DataTable,
  FormField,
  Modal,
  PageHeader,
  StatusBadge,
  Toaster,
  toast,
  type ColumnDef,
  type Status,
} from "@/components/kit";
import {
  ApiError,
  collectPages,
  createApi,
  etagForRevision,
  normalizePlaylistItem,
  updateWithReview,
  type Cast,
  type Daypart,
  type DaypartCreate,
  type DaypartUpdate,
  type FieldErrors,
  type Playlist,
  type PlaylistCreate,
  type PlaylistItem,
  type PlaylistUpdate,
  type Schedule,
  type ScheduleCreate,
  type ScheduleUpdate,
  type ScopeNode,
  type WaiveoApi,
} from "@/api";
import scheduleDoc from "./schedule.uis.json";
import daypartDoc from "./daypart.uis.json";
import playlistDoc from "./playlist.uis.json";

/**
 * The Schedules route — author a screen's day: a schedule of DAYPARTS (a window
 * of the week set to a display state and a playlist) plus the PLAYLISTS those
 * dayparts play. The two collections a schedule is expressed over — schedules and
 * playlists — are authored as DOGFOODED ui-schema/1 list-detail documents
 * (schedule.uis.json / playlist.uis.json), and the daypart editor itself is a
 * DOGFOODED settings-form (daypart.uis.json): each is validated and rendered
 * through the SAME PageRenderer an extension page takes, and this file is only the
 * host seam wiring the closed action verbs onto the typed api/1 client with the
 * conventions the client owns — creates carry an Idempotency-Key, edits/deletes
 * carry the If-Match derived from the record's revision, a 412 REVISION_CONFLICT
 * re-reads and surfaces the current state for review (never a silent overwrite),
 * and a 422 VALIDATION_FAILED's per-field errors land on the offending FormField.
 *
 * The daypart editor is where the optimistic-concurrency + validation UX is
 * proven end to end: a concurrent edit (412) re-reads and opens the editor on the
 * current values with a review banner; a within-schedule overlap (422 DAT-073,
 * which the server reports against the collection field `dayparts`) is remapped
 * onto the window so the operator sees it on the field they adjust to fix it.
 */

// The message catalog the three documents' `msg:` references resolve against.
const messages: Record<string, string> = {
  // schedule.uis.json
  "msg:sched.col.name": "Name",
  "msg:sched.detail.title": "Schedule",
  "msg:sched.detail.empty": "Select a schedule to edit it, or add a new one.",
  "msg:sched.detail.name": "Schedule name",
  "msg:sched.detail.save": "Save schedule",
  "msg:sched.detail.delete": "Delete schedule",
  // daypart.uis.json
  "msg:dp.section.window": "When it runs",
  "msg:dp.section.display": "What it shows",
  "msg:dp.name": "Daypart name",
  "msg:dp.namePh": "e.g. Morning open",
  "msg:dp.days": "Active days",
  "msg:dp.mon": "Monday",
  "msg:dp.tue": "Tuesday",
  "msg:dp.wed": "Wednesday",
  "msg:dp.thu": "Thursday",
  "msg:dp.fri": "Friday",
  "msg:dp.sat": "Saturday",
  "msg:dp.sun": "Sunday",
  "msg:dp.start": "Starts",
  "msg:dp.end": "Ends",
  "msg:dp.power": "Display",
  "msg:dp.power.on": "On (play a playlist)",
  "msg:dp.power.blank": "Blank screen",
  "msg:dp.power.off": "Power off",
  "msg:dp.playlist": "Playlist",
  "msg:dp.playlistPh": "Choose a playlist",
  "msg:dp.save": "Save daypart",
  // playlist.uis.json
  "msg:pl.col.name": "Name",
  "msg:pl.col.count": "Items",
  "msg:pl.detail.title": "Playlist",
  "msg:pl.detail.empty": "Select a playlist to edit it, or add a new one.",
  "msg:pl.name": "Playlist name",
  "msg:pl.items.empty": "No items yet — add the content this playlist plays.",
  "msg:pl.item.source": "Plays",
  "msg:pl.item.source.asset": "An uploaded file",
  "msg:pl.item.source.cast": "A cast (slides you authored)",
  "msg:pl.item.source.playable": "Content from a pack",
  "msg:pl.item.ref": "Content reference",
  "msg:pl.item.refPh": "sha256:…",
  // The content origin is content-addressed and holds no MIME type, so nothing
  // downstream can tell an MP4 from a PNG by looking at it — this is where an
  // operator says which it is, and it is the difference between a video that
  // plays and a black rectangle held for the item's dwell time.
  "msg:pl.item.kind": "File type",
  "msg:pl.item.kind.image": "Image (held on screen)",
  "msg:pl.item.kind.video": "Video (plays)",
  "msg:pl.item.cast": "Cast",
  "msg:pl.item.castPh": "Choose a cast",
  "msg:pl.item.castNote":
    "Every slide of the cast plays here, in the order the Studio shows them. Editing the cast changes every playlist that references it.",
  "msg:pl.item.pack": "Pack id",
  "msg:pl.item.content": "Content id",
  "msg:pl.item.dur": "Duration",
  "msg:pl.item.remove": "Remove item",
  "msg:pl.item.add": "Add a file",
  "msg:pl.item.addCast": "Add a cast",
  "msg:pl.save": "Save playlist",
  "msg:pl.delete": "Delete playlist",
};

const WEEKDAY_LABELS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

/** The display-state → StatusBadge tone for a daypart's display_power (DAT-074:
 * on|blank|off). Never a green wash on anything but a live/ok — `on` is the
 * live/content state, so it earns the ok lane; blank/off read as off. */
const POWER_STATUS: Record<string, { status: Status; label: string }> = {
  on: { status: "ok", label: "On" },
  blank: { status: "off", label: "Blank" },
  off: { status: "off", label: "Off" },
};

/** A 422 VALIDATION_FAILED's per-field errors, if this failure is one — the map a
 * form drops onto its FormField error slots. `null` for any other error. */
function fieldErrorsOf(err: unknown): FieldErrors | null {
  if (err instanceof ApiError && err.status === 422 && Object.keys(err.fieldErrors).length > 0) {
    return err.fieldErrors;
  }
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
    const r = resource as { id?: unknown; revision?: unknown };
    if (typeof r.id === "string" && typeof r.revision === "number") {
      return { id: r.id, revision: r.revision };
    }
  }
  return null;
}

function trimClock(t: string | undefined): string {
  if (!t) return "";
  // "HH:MM:SS" → "HH:MM" for a compact window read; keep as-is if already short.
  return /^\d{2}:\d{2}:\d{2}$/.test(t) ? t.slice(0, 5) : t;
}

function formatDays(days: number[] | undefined): string {
  if (!days || days.length === 0) return "—";
  // Present in week order (Mon-first), then Sunday, matching the editor's own order.
  const order = [1, 2, 3, 4, 5, 6, 0];
  const present = order.filter((d) => days.includes(d));
  return present.map((d) => WEEKDAY_LABELS[d]).join(", ");
}

// ─────────────────────────────────────────────────────────────────────────────
// The route: header + the schedules section + the playlists section.
// ─────────────────────────────────────────────────────────────────────────────

export default function SchedulesRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-12 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Schedules"
          description="Program each screen's week — dayparts that set the display and the playlist, over the playlists they draw from. Authored through the same declarative pages any extension renders through."
        />
        <SchedulesSection client={client} />
        <PlaylistsSection client={client} />
      </div>
      <Toaster />
    </div>
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// Schedules: the schedule list-detail (dogfood) + the daypart panel + editor.
// ─────────────────────────────────────────────────────────────────────────────

interface DaypartEditor {
  mode: "create" | "edit";
  draft: Record<string, unknown>;
  scheduleId: string;
  scopeNode: string;
  id?: string;
  etag?: string;
}

/** The server reports a within-schedule overlap (DAT-073) against the daypart
 * COLLECTION — field `dayparts` (internal/datamodel ValidateNoOverlap) — but the
 * operator resolves an overlap by narrowing THIS daypart's window, so the message
 * is surfaced on the window's start (`start_time`), the field they change to fix
 * it. Every other server field name maps to its own bind path unchanged. */
const DAYPART_FIELD_REMAP: Record<string, string> = { dayparts: "start_time" };

function remapDaypartFields(fields: FieldErrors): FieldErrors {
  const out: FieldErrors = {};
  for (const [field, message] of Object.entries(fields)) {
    out[DAYPART_FIELD_REMAP[field] ?? field] = message;
  }
  return out;
}

function SchedulesSection({ client }: { client: WaiveoApi }) {
  const [schedules, setSchedules] = useState<Schedule[] | null>(null);
  const [playlists, setPlaylists] = useState<Playlist[]>([]);
  const [dayparts, setDayparts] = useState<Daypart[]>([]);
  const [scopeNodes, setScopeNodes] = useState<ScopeNode[]>([]);
  const [targetScopeId, setTargetScopeId] = useState<string | null>(null);
  const [selectedScheduleId, setSelectedScheduleId] = useState<string | null>(null);
  const [scheduleFieldErrors, setScheduleFieldErrors] = useState<FieldErrors>({});
  const [scheduleConflict, setScheduleConflict] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [version, setVersion] = useState(0);

  // The daypart editor (a modal over the dogfooded settings-form) + its own
  // out-of-band state the renderer cannot see (server field errors, a conflict
  // review banner), each scoped to the daypart in the editor.
  const [editor, setEditor] = useState<DaypartEditor | null>(null);
  const [daypartFieldErrors, setDaypartFieldErrors] = useState<FieldErrors>({});
  const [daypartConflict, setDaypartConflict] = useState(false);
  const [editorVersion, setEditorVersion] = useState(0);

  const selectedIdRef = useRef<string | null>(null);
  // Whether a create draft was open on the last `$ui` we saw, so a fresh draft
  // opening (New→Cancel→New, which keeps `$ui.selected` null) is detectable below.
  const draftOpenRef = useRef(false);
  const activeScope = targetScopeId ?? scopeNodes[0]?.id ?? null;
  const activeScopeRef = useRef<string | null>(activeScope);
  activeScopeRef.current = activeScope;

  const load = useCallback(async () => {
    try {
      const [scheduleRows, playlistRows, daypartRows, scopeRows] = await Promise.all([
        collectPages<Schedule>((cursor) => client.schedules.list({ cursor })),
        collectPages<Playlist>((cursor) => client.playlists.list({ cursor })),
        collectPages<Daypart>((cursor) => client.dayparts.list({ cursor })),
        collectPages<ScopeNode>((cursor) => client.scopeNodes.list({ cursor })),
      ]);
      setSchedules(scheduleRows);
      setPlaylists(playlistRows);
      setDayparts(daypartRows);
      setScopeNodes(scopeRows);
      setLoadError(null);
    } catch (err) {
      setSchedules([]);
      setLoadError(err instanceof ApiError ? (err.detail ?? err.code) : "The service is unreachable.");
    }
  }, [client]);

  useEffect(() => {
    void load();
  }, [load]);

  const reload = useCallback(async () => {
    setScheduleFieldErrors({});
    await load();
    setVersion((v) => v + 1);
  }, [load]);

  // Reload only the dayparts (after a daypart mutation) so the panel refreshes
  // without remounting the schedule list-detail (which would drop the selection).
  const reloadDayparts = useCallback(async () => {
    try {
      const rows = await collectPages<Daypart>((cursor) => client.dayparts.list({ cursor }));
      setDayparts(rows);
    } catch {
      // A daypart reload failure leaves the last-known panel; the mutation toast
      // already reported the real error.
    }
  }, [client]);

  // Entering a FRESH create draft (UIS-021) resets the same out-of-band state — a
  // prior failed attempt's field error must not leak onto the new blank draft, which
  // keeps `$ui.selected` null across its whole New→Cancel→New lifecycle.
  const onScheduleUiChange = useCallback((ui: Record<string, unknown>) => {
    const next = typeof ui.selected === "string" ? ui.selected : null;
    const draftOpen = isCreateDraftUi(ui);
    const enteringDraft = draftOpen && !draftOpenRef.current;
    draftOpenRef.current = draftOpen;
    if (next === selectedIdRef.current && !enteringDraft) return;
    selectedIdRef.current = next;
    setSelectedScheduleId(next);
    setScheduleFieldErrors({});
    setScheduleConflict(false);
  }, []);

  const scheduleHandler: ActionHandler = useMemo(
    () => ({
      // A schedule MUST have a scope_node (the node it programs), which the
      // list-detail newAction cannot carry — so with no candidate node there is
      // nothing to attach it to; say so rather than POST a body the server rejects.
      create: async (_target, itemDefault) => {
        setScheduleFieldErrors({});
        setScheduleConflict(false);
        const scope = activeScopeRef.current;
        if (!scope) {
          toast.error("Add a site or screen before creating a schedule — a schedule programs a scope node.");
          return;
        }
        const body: ScheduleCreate = {
          scope_node: scope,
          name: typeof itemDefault.name === "string" ? itemDefault.name : "New schedule",
        };
        try {
          const created = await client.schedules.create(body);
          toast.success(`Added ${created.data.name}`);
          // The fresh row becomes the selection (UIS-021) so its detail opens.
          selectedIdRef.current = created.data.id;
          setSelectedScheduleId(created.data.id);
          await reload();
        } catch (err) {
          reportProblem("Couldn't add the schedule", err);
        }
      },
      submit: async (_target, resource) => {
        const meta = idRev(resource);
        if (!meta) return;
        setScheduleFieldErrors({});
        setScheduleConflict(false);
        const r = resource as Schedule;
        const patch: ScheduleUpdate = { name: r.name };
        try {
          const outcome = await updateWithReview(client.schedules, meta.id, patch, etagForRevision(meta.revision));
          if (outcome.status === "conflict") {
            toast.error("This schedule changed elsewhere. Review the current values and try again.");
            setScheduleConflict(true);
          } else {
            toast.success("Saved schedule");
          }
          await reload();
        } catch (err) {
          const fields = fieldErrorsOf(err);
          if (fields) {
            setScheduleFieldErrors(fields);
            toast.error("Couldn't save the schedule — please fix the highlighted fields.");
          } else {
            reportProblem("Couldn't save the schedule", err);
          }
        }
      },
      remove: async (_target, resource) => {
        const meta = idRev(resource);
        if (!meta) return;
        setScheduleConflict(false);
        try {
          await client.schedules.remove(meta.id, etagForRevision(meta.revision));
          toast.success("Deleted schedule");
          if (selectedIdRef.current === meta.id) {
            selectedIdRef.current = null;
            setSelectedScheduleId(null);
          }
          await reload();
        } catch (err) {
          reportProblem("Couldn't delete the schedule", err);
        }
      },
    }),
    [client, reload],
  );

  const openCreateDaypart = useCallback(() => {
    const schedule = (schedules ?? []).find((s) => s.id === selectedScheduleId);
    if (!schedule) return;
    setDaypartFieldErrors({});
    setDaypartConflict(false);
    setEditor({
      mode: "create",
      scheduleId: schedule.id,
      scopeNode: schedule.scope_node,
      draft: {
        schedule_id: schedule.id,
        scope_node: schedule.scope_node,
        name: "",
        days_of_week: [1, 2, 3, 4, 5],
        start_time: "09:00:00",
        end_time: "17:00:00",
        display_power: "on",
        playlist_id: playlists[0]?.id ?? "",
      },
    });
    setEditorVersion((v) => v + 1);
  }, [schedules, selectedScheduleId, playlists]);

  const openEditDaypart = useCallback((dp: Daypart) => {
    setDaypartFieldErrors({});
    setDaypartConflict(false);
    setEditor({
      mode: "edit",
      scheduleId: dp.schedule_id,
      scopeNode: dp.scope_node,
      id: dp.id,
      etag: etagForRevision(dp.revision),
      draft: { ...dp },
    });
    setEditorVersion((v) => v + 1);
  }, []);

  const closeEditor = useCallback(() => {
    setEditor(null);
    setDaypartFieldErrors({});
    setDaypartConflict(false);
  }, []);

  const daypartHandler: ActionHandler = useMemo(
    () => ({
      submit: async (_target, resource) => {
        if (!editor) return;
        const dp = resource as Daypart;
        setDaypartFieldErrors({});
        setDaypartConflict(false);
        const days = Array.isArray(dp.days_of_week) ? dp.days_of_week : [];
        const wantsPlaylist = dp.display_power === "on" && typeof dp.playlist_id === "string" && dp.playlist_id !== "";
        if (editor.mode === "create") {
          const body: DaypartCreate = {
            schedule_id: editor.scheduleId,
            scope_node: editor.scopeNode,
            days_of_week: days,
            start_time: String(dp.start_time ?? ""),
            end_time: String(dp.end_time ?? ""),
            display_power: String(dp.display_power ?? "on"),
            ...(wantsPlaylist ? { playlist_id: dp.playlist_id as string } : {}),
            ...(dp.name ? { name: String(dp.name) } : {}),
          };
          try {
            await client.dayparts.create(body);
            toast.success("Added daypart");
            closeEditor();
            await reloadDayparts();
          } catch (err) {
            const fields = fieldErrorsOf(err);
            if (fields) {
              setDaypartFieldErrors(remapDaypartFields(fields));
              toast.error("Couldn't save the daypart — please fix the highlighted fields.");
            } else {
              reportProblem("Couldn't add the daypart", err);
            }
          }
          return;
        }
        const patch: DaypartUpdate = {
          days_of_week: days,
          start_time: String(dp.start_time ?? ""),
          end_time: String(dp.end_time ?? ""),
          display_power: String(dp.display_power ?? "on"),
          ...(wantsPlaylist ? { playlist_id: dp.playlist_id as string } : {}),
          ...(dp.name !== undefined ? { name: String(dp.name) } : {}),
        };
        try {
          const outcome = await updateWithReview(client.dayparts, editor.id ?? "", patch, editor.etag ?? "");
          if (outcome.status === "conflict") {
            // The standard conflict UX: a toast, the re-read current values seeding
            // the editor (remounted via editorVersion), AND a persistent review
            // banner — the modal stays open on the current values, never a silent
            // overwrite (exactly one write was attempted).
            const current = outcome.current.data;
            toast.error("This daypart changed elsewhere. Review the current values and try again.");
            setEditor({
              mode: "edit",
              scheduleId: current.schedule_id,
              scopeNode: current.scope_node,
              id: current.id,
              etag: outcome.current.etag,
              draft: { ...current },
            });
            setDaypartConflict(true);
            setEditorVersion((v) => v + 1);
            await reloadDayparts();
          } else {
            toast.success("Saved daypart");
            closeEditor();
            await reloadDayparts();
          }
        } catch (err) {
          const fields = fieldErrorsOf(err);
          if (fields) {
            setDaypartFieldErrors(remapDaypartFields(fields));
            toast.error("Couldn't save the daypart — please fix the highlighted fields.");
          } else {
            reportProblem("Couldn't save the daypart", err);
          }
        }
      },
    }),
    [client, editor, closeEditor, reloadDayparts],
  );

  const scheduleDayparts = useMemo(
    () => dayparts.filter((d) => d.schedule_id === selectedScheduleId),
    [dayparts, selectedScheduleId],
  );
  const playlistNameOf = useCallback(
    (id: string | undefined) => (id ? (playlists.find((p) => p.id === id)?.name ?? id) : "—"),
    [playlists],
  );

  const daypartColumns: ColumnDef<Daypart>[] = useMemo(
    () => [
      { id: "name", header: "Daypart", accessorFn: (d) => d.name || "(unnamed)" },
      { id: "days", header: "Days", accessorFn: (d) => formatDays(d.days_of_week) },
      {
        id: "window",
        header: "Window",
        accessorFn: (d) => `${trimClock(d.start_time)}–${trimClock(d.end_time)}`,
      },
      {
        id: "display",
        header: "Display",
        cell: ({ row }) => {
          const meta = POWER_STATUS[row.original.display_power] ?? { status: "off" as Status, label: row.original.display_power };
          return <StatusBadge status={meta.status}>{meta.label}</StatusBadge>;
        },
      },
      { id: "playlist", header: "Playlist", accessorFn: (d) => (d.display_power === "on" ? playlistNameOf(d.playlist_id) : "—") },
    ],
    [playlistNameOf],
  );

  const selectedSchedule = (schedules ?? []).find((s) => s.id === selectedScheduleId) ?? null;

  return (
    <section className="flex flex-col gap-6">
      <div className="flex flex-col gap-1">
        <h2 className="font-display text-[19px] font-semibold tracking-[-0.01em]">Schedules</h2>
        <p className="max-w-2xl text-sm text-muted-foreground">
          Each schedule programs a scope node. Pick one to edit it and manage its dayparts.
        </p>
      </div>

      {loadError ? (
        <p role="alert" className="text-sm text-[color:var(--wv-err)]">
          Couldn't load schedules — {loadError}
        </p>
      ) : null}

      {/* grammar-gap: a schedule's create needs a scope_node the list-detail
          newAction cannot carry (like a screen's parent), and a schedule's
          scope_node is fixed after create — so when the org spans more than one
          node the operator must pick where a new schedule lands. A first-party
          target picker until the grammar grows a create-form with a placement
          selector. */}
      {scopeNodes.length > 1 ? (
        <div className="max-w-sm">
          <FormField label="Add new schedules on">
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

      {scheduleConflict ? (
        <p
          role="status"
          className="rounded-card border border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] p-4 text-sm text-[color:var(--wv-warn)]"
        >
          This schedule was changed elsewhere while you were editing. The current values are shown — review
          them, then save again to apply your change.
        </p>
      ) : null}

      <div className="min-w-0">
        {schedules === null ? (
          <p className="text-sm text-muted-foreground">Loading schedules…</p>
        ) : (
          <PageRenderer
            key={version}
            doc={scheduleDoc}
            data={{ schedules }}
            initialUi={{ selected: selectedScheduleId }}
            messages={messages}
            handler={scheduleHandler}
            fieldErrors={scheduleFieldErrors}
            onUiChange={onScheduleUiChange}
          />
        )}
      </div>

      {/* grammar-gap: dayparts are a SUB-COLLECTION of the selected schedule, each
          a separate api/1 resource edited under its own If-Match, and created with
          the parent schedule's scope_node. A ui-schema list-detail is a top-level
          master-detail over one collection — it cannot express "the selected
          parent's children, edited in a modal with per-child concurrency" — so the
          daypart PANEL (list + add/edit orchestration) is a first-party host seam.
          The daypart EDITOR it opens is itself a dogfooded settings-form. */}
      {selectedSchedule ? (
        <section className="flex flex-col gap-3 rounded-card border border-border bg-card p-4 wv-elevation sm:p-5">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h3 className="font-display text-[15px] font-semibold">Dayparts · {selectedSchedule.name}</h3>
            <Button variant="secondary" size="sm" icon={Plus} className="wv-touch" onClick={openCreateDaypart}>
              Add daypart
            </Button>
          </div>
          <DataTable
            label={`Dayparts of ${selectedSchedule.name}`}
            columns={daypartColumns}
            data={scheduleDayparts}
            rowActions={(dp) => (
              <Button
                variant="ghost"
                size="sm"
                icon={Pencil}
                className="wv-touch"
                aria-label={`Edit daypart ${dp.name || "(unnamed)"}`}
                onClick={() => openEditDaypart(dp)}
              >
                Edit
              </Button>
            )}
          />
        </section>
      ) : null}

      {editor ? (
        <Modal
          open
          onOpenChange={(o) => {
            if (!o) closeEditor();
          }}
          size="lg"
          title={editor.mode === "create" ? "Add daypart" : "Edit daypart"}
          description="Set when this daypart runs and what the screen shows."
        >
          {daypartConflict ? (
            <p
              role="status"
              className="mb-4 rounded-card border border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] p-3 text-sm text-[color:var(--wv-warn)]"
            >
              This daypart was changed elsewhere while you were editing. The current values are shown — review
              them, then save again to apply your change.
            </p>
          ) : null}
          <PageRenderer
            key={editorVersion}
            doc={daypartDoc}
            data={{ daypart: editor.draft, playlists }}
            messages={messages}
            handler={daypartHandler}
            fieldErrors={daypartFieldErrors}
          />
        </Modal>
      ) : null}
    </section>
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// Playlists: the playlist list-detail (dogfood) — name + items management.
// ─────────────────────────────────────────────────────────────────────────────

function PlaylistsSection({ client }: { client: WaiveoApi }) {
  const [playlists, setPlaylists] = useState<Playlist[] | null>(null);
  // The cast library, fed to the document as a `$context` collection so a
  // playlist item can REFERENCE one. Without this the Studio is a dead end: an
  // operator can author a cast and never put it on a screen, because a screen's
  // program is resolved from dayparts → playlist → items and `cast` is the only
  // item source that names one.
  const [casts, setCasts] = useState<Cast[]>([]);
  const [scopeNodes, setScopeNodes] = useState<ScopeNode[]>([]);
  const [targetScopeId, setTargetScopeId] = useState<string | null>(null);
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({});
  const [conflict, setConflict] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [version, setVersion] = useState(0);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selectedIdRef = useRef<string | null>(null);
  // Whether a create draft was open on the last `$ui` we saw, so a fresh draft
  // opening (New→Cancel→New, which keeps `$ui.selected` null) is detectable below.
  const draftOpenRef = useRef(false);

  const activeScope = targetScopeId ?? scopeNodes[0]?.id ?? null;
  const activeScopeRef = useRef<string | null>(activeScope);
  activeScopeRef.current = activeScope;

  const load = useCallback(async () => {
    try {
      const [rows, scopeRows, castRows] = await Promise.all([
        collectPages<Playlist>((cursor) => client.playlists.list({ cursor })),
        collectPages<ScopeNode>((cursor) => client.scopeNodes.list({ cursor })),
        collectPages<Cast>((cursor) => client.casts.list({ cursor })),
      ]);
      setPlaylists(rows);
      setScopeNodes(scopeRows);
      // TEMPLATES ARE NOT SCHEDULABLE. The server refuses a playlist item that
      // names one (422 CAST_TEMPLATE_NOT_SCHEDULABLE), so offering it in the
      // picker is a control that can only ever fail — the operator picks "House
      // style", saves, and is told no by a rule the dropdown never mentioned. A
      // template is a starting point for authoring, and the casts library
      // already splits it out of the list of things you can schedule; this is
      // the same split on the surface that actually schedules.
      setCasts(castRows.filter((c) => c.template !== true));
      setLoadError(null);
    } catch (err) {
      setPlaylists([]);
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

  // Entering a FRESH create draft (UIS-021) resets the same out-of-band state — a
  // prior failed attempt's field error must not leak onto the new blank draft, which
  // keeps `$ui.selected` null across its whole New→Cancel→New lifecycle.
  const onUiChange = useCallback((ui: Record<string, unknown>) => {
    const next = typeof ui.selected === "string" ? ui.selected : null;
    const draftOpen = isCreateDraftUi(ui);
    const enteringDraft = draftOpen && !draftOpenRef.current;
    draftOpenRef.current = draftOpen;
    if (next === selectedIdRef.current && !enteringDraft) return;
    selectedIdRef.current = next;
    setSelectedId(next);
    setFieldErrors({});
    setConflict(false);
  }, []);

  const handler: ActionHandler = useMemo(
    () => ({
      create: async (_target, itemDefault) => {
        setFieldErrors({});
        setConflict(false);
        const scope = activeScopeRef.current;
        if (!scope) {
          toast.error("Add a site before creating a playlist — a playlist is placed on a scope node.");
          return;
        }
        const body: PlaylistCreate = {
          scope_node: scope,
          name: typeof itemDefault.name === "string" ? itemDefault.name : "New playlist",
          items: [],
        };
        try {
          const created = await client.playlists.create(body);
          toast.success(`Added ${created.data.name}`);
          // The fresh row becomes the selection (UIS-021) so its detail opens.
          selectedIdRef.current = created.data.id;
          setSelectedId(created.data.id);
          await reload();
        } catch (err) {
          reportProblem("Couldn't add the playlist", err);
        }
      },
      submit: async (_target, resource) => {
        const meta = idRev(resource);
        if (!meta) return;
        setFieldErrors({});
        setConflict(false);
        const r = resource as Playlist;
        // Normalised per item: switching an item's source in the editor leaves
        // the old source's members on the object (an asset flipped to a cast
        // still carries `asset_ref: ""`), and the server validates an item
        // against the source it declares.
        const items = (Array.isArray(r.items) ? r.items : []).map((item) =>
          normalizePlaylistItem(item as PlaylistItem),
        );
        const patch: PlaylistUpdate = { name: r.name, items };
        try {
          const outcome = await updateWithReview(client.playlists, meta.id, patch, etagForRevision(meta.revision));
          if (outcome.status === "conflict") {
            toast.error("This playlist changed elsewhere. Review the current values and try again.");
            setConflict(true);
          } else {
            toast.success("Saved playlist");
          }
          await reload();
        } catch (err) {
          const fields = fieldErrorsOf(err);
          if (fields) {
            setFieldErrors(fields);
            toast.error("Couldn't save the playlist — please fix the highlighted fields.");
          } else {
            reportProblem("Couldn't save the playlist", err);
          }
        }
      },
      remove: async (_target, resource) => {
        const meta = idRev(resource);
        if (!meta) return;
        setConflict(false);
        try {
          await client.playlists.remove(meta.id, etagForRevision(meta.revision));
          toast.success("Deleted playlist");
          await reload();
        } catch (err) {
          reportProblem("Couldn't delete the playlist", err);
        }
      },
    }),
    [client, reload],
  );

  return (
    <section className="flex flex-col gap-6">
      <div className="flex flex-col gap-1">
        <h2 className="font-display text-[19px] font-semibold tracking-[-0.01em]">Playlists</h2>
        <p className="max-w-2xl text-sm text-muted-foreground">
          The content a daypart plays. Build a playlist here, then point a daypart's “On” state at it.
        </p>
      </div>

      {loadError ? (
        <p role="alert" className="text-sm text-[color:var(--wv-err)]">
          Couldn't load playlists — {loadError}
        </p>
      ) : null}

      {/* grammar-gap: a playlist's create needs a scope_node the list-detail
          newAction cannot carry — same placement picker as schedules above. */}
      {scopeNodes.length > 1 ? (
        <div className="max-w-sm">
          <FormField label="Add new playlists on">
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

      {conflict ? (
        <p
          role="status"
          className="rounded-card border border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] p-4 text-sm text-[color:var(--wv-warn)]"
        >
          This playlist was changed elsewhere while you were editing. The current values are shown — review
          them, then save again to apply your change.
        </p>
      ) : null}

      <div className="min-w-0">
        {playlists === null ? (
          <p className="text-sm text-muted-foreground">Loading playlists…</p>
        ) : (
          <PageRenderer
            key={version}
            doc={playlistDoc}
            data={{ playlists, casts }}
            initialUi={{ selected: selectedId }}
            messages={messages}
            handler={handler}
            fieldErrors={fieldErrors}
            onUiChange={onUiChange}
          />
        )}
      </div>
    </section>
  );
}
