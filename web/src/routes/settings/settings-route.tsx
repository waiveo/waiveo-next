import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router";
import { Button, PageHeader } from "@/components/kit";
import { PageRenderer, type ActionHandler } from "@/renderer";
import {
  ApiError,
  collectPages,
  createApi,
  etagForRevision,
  updateWithReview,
  type FieldErrors,
  type ScopeNode,
  type ScopeNodeUpdate,
  type WaiveoApi,
} from "@/api";
import settingsPageDoc from "./page.uis.json";
import { timeZoneOptions } from "./timezones";

/**
 * The Settings route ("/settings") — the box's own configuration.
 *
 * # What is here, and why it is one section rather than fifteen
 *
 * The parity diff ranks "no settings surface" first of fifteen gaps, and lists
 * what it cascades into: timezone, log level, log retention, session lifetime,
 * reboot, and a weather provider key. Six candidates. Reading each of them
 * against this codebase, FIVE turn out to have nothing to write to:
 *
 *   • **Log level** — there is no `log/slog` in this tree and no level concept
 *     of any kind. Every `log.Printf` goes unconditionally to stderr and the
 *     ring buffer (cmd/waiveo-feeder/main.go:472), and `/platform-logs`'s
 *     `level` is DERIVED from the line text after the fact by a documented
 *     heuristic (internal/app/platformlog/classify.go). A "log level" control
 *     would have no destination.
 *   • **Log retention** — the buffer's capacity is a compile-time constant
 *     (`platformLogCapacity = 4000`, cmd/waiveo-feeder/main.go:456) allocated
 *     once in the constructor; `Buffer` exposes Write and Read and no resize.
 *   • **Session lifetime** — browser sessions have no expiry to configure. The
 *     `sessions` table has no `expires_at` (internal/app/auth/store.go:62), the
 *     lookup filters on `revoked_at IS NULL` only, and the cookie carries no
 *     Max-Age (internal/app/auth/middleware.go:296).
 *   • **Restart** — this one is real and it ALREADY HAS A HOME: the System
 *     page's restart panel. Building a second one here is the duplication this
 *     page exists to avoid, so Settings links to it instead.
 *   • **Weather provider key** — the weather layer resolves through Open-Meteo
 *     specifically BECAUSE it needs no key (internal/slidelive/openmeteo.go:16),
 *     so that "a box that ships to a customer site must be able to draw the
 *     weather widget on the day it is plugged in". There is no provider to
 *     select and no credential to hold.
 *
 * The sixth is real, and it is what this page is: **a site's time zone and
 * coordinates**. It is the only operator-settable platform configuration in
 * this system that anything actually reads, and it is read constantly —
 * `EffectiveGeo` (internal/datamodel/scopetree.go:390) resolves it for every
 * daypart boundary, every `sun` trigger's solar computation, and the
 * coordinates the relay fetches weather against (cmd/waiveo-relay/sitegeo.go).
 *
 * # Whose time zone was missing
 *
 * Not the box's — this platform deliberately has no box time zone. DAT-034
 * forbids resolution from ever substituting the evaluating machine's locale,
 * and DAT-032 forbids the org node (the workspace's own identity row) from
 * carrying a tz at all. Local time comes from a SITE, and from nowhere else.
 *
 * The gap was that a site's tz was **write-once**. The scope-tree panel on the
 * old /screens page created a site with tz/lat/long (DAT-031 makes all three
 * mandatory) and then had no edit path, so a site's clock could be set exactly
 * once, at creation, and never corrected. A box installed in the wrong zone
 * stayed there.
 *
 * That is also why editing lives HERE rather than beside the tree: the console's
 * established division is that a tree panel authors the SHAPE (create a node,
 * delete an empty one) and a separate surface edits a node's own fields. That
 * matters more since the 2026-08-19 strip, not less — /screens and its scope
 * tree left core with the slidecast ship target, so this page is now the ONLY
 * console surface that writes a scope node's location or clock at all. Creating
 * one is api/1 or CLI work until a pack brings the tree panel back.
 *
 * # The log-timestamp cascade, answered
 *
 * The diff attributes "/system renders log timestamps in browser-local time
 * with no label" to the missing timezone setting. It is not caused by one and
 * is not fixed by one: those lines are the FEEDER PROCESS's log, and a
 * workspace may hold several sites, so there is no site whose zone is the right
 * one to render them in. The operator reading them is in the browser, so the
 * browser's zone is the correct choice — what was wrong was that it was
 * unlabelled. That is fixed on the System page itself, by naming the zone, and
 * needs no stored setting.
 */

/** The message catalog the document's `msg:` references resolve against. */
const messages: Record<string, string> = {
  "msg:settings.col.name": "Site",
  "msg:settings.col.tz": "Time zone",
  "msg:settings.col.coords": "Coordinates",
  "msg:settings.detail.empty": "Select a site to edit where it is and what local time it keeps.",
  "msg:settings.detail.title": "Location and local time",
  "msg:settings.detail.lead":
    "This site's time zone and coordinates decide when its dayparts change over, when its sunrise and sunset rules fire, and which forecast its weather layers draw. They resolve as one unit, so all three are saved together.",
  "msg:settings.detail.name": "Site name",
  "msg:settings.detail.tz": "Time zone",
  "msg:settings.detail.tzPlaceholder": "Choose an IANA time zone",
  "msg:settings.detail.lat": "Latitude",
  "msg:settings.detail.long": "Longitude",
  "msg:settings.detail.save": "Save this site",
  "msg:settings.detail.saving": "Saving — this site's schedules will resolve against the new values.",
  "msg:settings.detail.saved": "Saved. This site now keeps local time in {0}.",
};

/** Only site-kind nodes. A group or a screen MAY carry a geo override, but a
 * group's override is not consulted for its descendants — DAT-033 walks to the
 * nearest SITE-kind ancestor specifically — so a group override changes only
 * rows placed at the group itself, and a screen's own override is already
 * editable on the Screens page. Offering either here would be a second home for
 * one and a near-inert control for the other. */
const SITE_SELECTOR = "kind=site";

/** A refusal this page raises itself, shaped so the renderer's duck-typed
 * `problemOf` reads it exactly as it reads an ApiError: `code`, `detail`,
 * `traceId`. Used for the pre-flight completeness check, whose whole point is to
 * refuse locally rather than send a body the server would reject. */
class SaveRefused extends Error {
  readonly code: string;
  readonly detail: string;
  readonly traceId: string | null;

  constructor(code: string, detail: string, traceId: string | null = null) {
    super(detail);
    this.name = "SaveRefused";
    this.code = code;
    this.detail = detail;
    this.traceId = traceId;
  }
}

/** A site as the renderer's editable store holds it while the form is open:
 * every field is whatever the operator has typed, which is why none of them can
 * be assumed present. */
interface SiteDraft {
  id?: unknown;
  revision?: unknown;
  name?: unknown;
  tz?: unknown;
  lat?: unknown;
  long?: unknown;
}

function asDraft(resource: unknown): SiteDraft | null {
  if (resource === null || typeof resource !== "object") return null;
  const r = resource as SiteDraft;
  return typeof r.id === "string" && typeof r.revision === "number" ? r : null;
}

/** Render a coordinate pair for the list column. Sites always carry both
 * (DAT-031), but the column reads from the live editable store — where a
 * half-cleared number-input is a real, visible state — so a missing half says
 * so rather than printing `null`. */
export function formatCoords(lat: unknown, long: unknown): string {
  const ok = (v: unknown): v is number => typeof v === "number" && Number.isFinite(v);
  if (!ok(lat) || !ok(long)) return "—";
  return `${lat.toFixed(4)}, ${long.toFixed(4)}`;
}

/**
 * DAT-031's completeness rule, checked before the request rather than after it.
 *
 * A site MUST carry non-null tz, lat AND long. The form can reach an incomplete
 * state honestly — clearing a `number-input` writes `null` by contract
 * (UIS-072) — and PATCHing `lat: null` would be refused by the schema with a
 * message about a type, not about a site needing a location. So the page says
 * what is actually wrong, on the field that is wrong.
 *
 * Exported for its own test: this is the guard that decides whether a save can
 * strip a site of the clock every one of its schedules resolves against.
 */
export function geoProblems(draft: SiteDraft): FieldErrors {
  const errors: FieldErrors = {};
  if (typeof draft.name !== "string" || draft.name.trim() === "") {
    errors.name = "A site needs a name.";
  }
  if (typeof draft.tz !== "string" || draft.tz.trim() === "") {
    errors.tz =
      "A site needs a time zone — it is where local time comes from, and nothing substitutes for it.";
  }
  if (typeof draft.lat !== "number" || !Number.isFinite(draft.lat)) {
    errors.lat = "A site needs a latitude — sunrise and sunset rules are computed from it.";
  }
  if (typeof draft.long !== "number" || !Number.isFinite(draft.long)) {
    errors.long = "A site needs a longitude — sunrise and sunset rules are computed from it.";
  }
  return errors;
}

export default function SettingsRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);

  const [sites, setSites] = useState<ScopeNode[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selectedIdRef = useRef<string | null>(null);
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({});
  // A refusal that has no field to pin itself to, kept on screen (never a
  // toast) with its code and trace id — the console's standing rule that every
  // failure has a surface and a reason.
  const [saveError, setSaveError] = useState<{ code: string; detail: string; traceId: string | null } | null>(
    null,
  );
  // The 412 reconciliation state: what the server currently holds for the record
  // the operator is editing. Kept beside the form rather than swapped INTO it,
  // so a conflict never silently discards what they typed — they read both and
  // decide.
  const [conflict, setConflict] = useState<{ id: string; current: ScopeNode } | null>(null);
  // Bumped to remount the renderer against freshly fetched data. The store seeds
  // ONCE from `data` (state.tsx's useState initializer), so a re-seed is a
  // remount and nothing less.
  const [version, setVersion] = useState(0);

  /**
   * The revision to condition the NEXT save on, per site.
   *
   * A successful save deliberately does not remount: the operator is looking at
   * the values they just saved and a remount would flash the form. But the
   * renderer's store still holds the revision the page LOADED, and the server
   * has moved on by one. Without this the second consecutive save on one site
   * would send a stale If-Match and be refused 412 — a conflict with nobody.
   *
   * A CONFLICT deliberately does not update it. Adopting the server's revision
   * after a 412 would arm the next Save to overwrite the change made elsewhere,
   * which is precisely the unconditional overwrite API-022 forbids. Reload is
   * the only way forward from a conflict, and it clears this map.
   */
  const revisions = useRef(new Map<string, number>());

  const load = useCallback(async () => {
    try {
      const rows = await collectPages<ScopeNode>((cursor) =>
        client.scopeNodes.list({ selector: SITE_SELECTOR, cursor }),
      );
      setSites(rows);
      setLoadError(null);
    } catch (err) {
      setSites(null);
      setLoadError(err instanceof ApiError ? (err.detail ?? err.code) : "The service is unreachable.");
    }
  }, [client]);

  useEffect(() => {
    void load();
  }, [load]);

  /** Re-read from the server and re-seed the form from what came back. Every
   * piece of per-record state the old seed justified goes with it. */
  const reload = useCallback(async () => {
    revisions.current = new Map();
    setFieldErrors({});
    setSaveError(null);
    setConflict(null);
    await load();
    setVersion((v) => v + 1);
  }, [load]);

  // The renderer owns the selection; this is how the host learns it moved. Every
  // piece of state below is keyed to ONE record and would otherwise render
  // against the next one: field errors are keyed by bind path with no record
  // identity, and a conflict or a refusal belongs to the record it happened on.
  const onUiChange = useCallback((ui: Record<string, unknown>) => {
    const next = typeof ui.selected === "string" ? ui.selected : null;
    if (next === selectedIdRef.current) return;
    selectedIdRef.current = next;
    setSelectedId(next);
    setFieldErrors({});
    setSaveError(null);
    setConflict(null);
  }, []);

  const handler: ActionHandler = useMemo(
    () => ({
      submit: async (_target, resource, report) => {
        const draft = asDraft(resource);
        if (!draft) {
          throw new SaveRefused("INTERNAL", "This form is not bound to a saved site.");
        }
        const id = draft.id as string;
        // Name the record this invocation is about BEFORE anything can fail, so
        // the document's pending and success lines can check that the outcome
        // they are rendering belongs to the site currently on screen (UIS-166's
        // interim-progress channel; the outcome slot itself is a static path).
        report?.({ result: { id } });

        setSaveError(null);
        setConflict(null);

        const problems = geoProblems(draft);
        if (Object.keys(problems).length > 0) {
          setFieldErrors(problems);
          const detail = Object.values(problems)[0];
          setSaveError({ code: "VALIDATION_FAILED", detail, traceId: null });
          throw new SaveRefused("VALIDATION_FAILED", detail);
        }
        setFieldErrors({});

        // tz, lat and long always travel together: DAT-033 resolves them as one
        // unit, and a partial patch would leave a site describing a place it is
        // not in.
        const patch: ScopeNodeUpdate = {
          name: (draft.name as string).trim(),
          tz: (draft.tz as string).trim(),
          lat: draft.lat as number,
          long: draft.long as number,
        };
        const revision = revisions.current.get(id) ?? (draft.revision as number);

        try {
          const outcome = await updateWithReview(
            client.scopeNodes,
            id,
            patch,
            etagForRevision(revision),
          );
          if (outcome.status === "conflict") {
            setConflict({ id, current: outcome.current.data });
            const detail =
              "This site was changed elsewhere while you were editing it. Your changes were not saved.";
            setSaveError({ code: "REVISION_CONFLICT", detail, traceId: outcome.conflict.traceId });
            throw new SaveRefused("REVISION_CONFLICT", detail, outcome.conflict.traceId);
          }
          const saved = outcome.resource.data;
          revisions.current.set(id, saved.revision);
          setSites((prev) => (prev ? prev.map((s) => (s.id === id ? saved : s)) : prev));
          return saved;
        } catch (err) {
          if (err instanceof SaveRefused) throw err;
          if (err instanceof ApiError) {
            if (Object.keys(err.fieldErrors).length > 0) setFieldErrors(err.fieldErrors);
            const detail = Object.values(err.fieldErrors)[0] ?? err.detail ?? err.code;
            setSaveError({ code: err.code, detail, traceId: err.traceId });
            throw err;
          }
          const detail = "The service is unreachable.";
          setSaveError({ code: "UNREACHABLE", detail, traceId: null });
          throw new SaveRefused("UNREACHABLE", detail);
        }
      },
    }),
    [client],
  );

  // The zone list offered by the select, and the derived display column. Built
  // from the loaded records so a site already on a zone this runtime does not
  // enumerate keeps it as an option — see timezones.ts for why that matters.
  const data = useMemo(() => {
    const rows = sites ?? [];
    return {
      sites: rows.map((s) => ({ ...s, coords: formatCoords(s.lat, s.long) })),
      timezones: timeZoneOptions(rows.map((s) => s.tz)).map((id) => ({ id })),
    };
  }, [sites]);

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        variant="hero"
        title="Settings"
        description="Where this deployment is and what local time it keeps. Everything a site's schedules, sunrise rules and weather layers resolve against comes from here."
      />

      {loadError ? (
        <p role="alert" className="text-sm text-[color:var(--wv-err)]">
          Couldn't load sites — {loadError}
        </p>
      ) : null}

      {sites !== null && sites.length === 0 ? (
        <p className="rounded-card border border-border bg-card p-4 text-sm text-muted-foreground">
          This workspace has no sites yet, and this console cannot create one: a site needs a place
          in the hierarchy, and the scope-tree panel that authored that shape left core with the
          Screens page. Until an extension brings one back, create a site over api/1 or the CLI.
          Once one exists, its location and local time are edited here.
        </p>
      ) : null}

      {conflict ? (
        <div
          role="alert"
          className="flex flex-col gap-3 rounded-card border border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] p-4 text-sm text-[color:var(--wv-warn)]"
        >
          <p>
            This site was changed elsewhere while you were editing it, so nothing was saved. The box
            currently holds: <strong>{conflict.current.name}</strong> · {conflict.current.tz ?? "no time zone"}{" "}
            · {formatCoords(conflict.current.lat, conflict.current.long)}.
          </p>
          <p>
            Your edits are still in the form below. Reload to work from the box's values instead —
            that discards what you typed.
          </p>
          <div>
            <Button variant="secondary" className="wv-touch" onClick={() => void reload()}>
              Reload this site
            </Button>
          </div>
        </div>
      ) : null}

      {saveError && !conflict ? (
        <p
          role="alert"
          className="rounded-card border border-[color:var(--wv-err)] bg-[color:var(--wv-err-bg)] p-4 text-sm text-[color:var(--wv-err)]"
        >
          Couldn't save this site — {saveError.detail}{" "}
          <span className="font-mono text-xs">
            ({saveError.code}
            {saveError.traceId ? ` · trace ${saveError.traceId}` : ""})
          </span>
        </p>
      ) : null}

      <main className="min-w-0">
        {sites === null && loadError === null ? (
          <p className="text-sm text-muted-foreground">Loading sites…</p>
        ) : (
          <PageRenderer
            key={version}
            doc={settingsPageDoc}
            data={data}
            initialUi={{ selected: selectedId }}
            messages={messages}
            handler={handler}
            fieldErrors={fieldErrors}
            onUiChange={onUiChange}
          />
        )}
      </main>

      {/* Signposting, not controls. An operator who opens Settings looking for a
          restart button should be told where it is, not offered a second one. */}
      <section
        aria-labelledby="settings-elsewhere"
        className="flex flex-col gap-2 rounded-card border border-border bg-card p-5 text-sm text-muted-foreground"
      >
        <h2 id="settings-elsewhere" className="text-base font-semibold text-foreground">
          Configured elsewhere
        </h2>
        <p>
          Restarting the application server lives on{" "}
          <Link className="underline underline-offset-2" to="/system">
            System
          </Link>
          , beside the health and log readings an operator is usually looking at when they decide they
          need one.
        </p>
        <p>
          Two settings that used to be linked from here have no console home at present: the values
          automation rules read and write (they left with the Variables page), and a screen's own
          time-zone override (it left with the Screens page). Both are still api/1 resources; the
          surfaces return with the packs that own them.
        </p>
      </section>
    </div>
  );
}
