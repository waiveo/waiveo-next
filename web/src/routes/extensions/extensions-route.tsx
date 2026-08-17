import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router";
import {
  FileText,
  History,
  PackagePlus,
  Power,
  PowerOff,
  RefreshCw,
  Store,
  Trash2,
  TriangleAlert,
  Upload,
} from "lucide-react";
import {
  Button,
  ConfirmModal,
  EmptyState,
  FormField,
  KitIcon,
  PageHeader,
  StatCard,
  StatusBadge,
  Toaster,
  toast,
} from "@/components/kit";
import {
  collectPages,
  createApi,
  TRUST_CHANNELS,
  type CatalogEntry,
  type CatalogSource,
  type Pack,
  type PackInstallRecord,
  type TrustChannel,
  type WaiveoApi,
} from "@/api";
import { loadPackCatalog, resolveTitle } from "@/routes/packs/catalog";
import { notifyPacksChanged } from "@/routes/packs/use-installed-packs";
import { useOptionalSession } from "@/auth/session-gate";
import { can } from "@/auth/can";
import { resolvePackIcon } from "@/shell/pack-icon";
import {
  describeInstall,
  describeRefusal,
  describeUpdate,
  describeAvailability,
  type AvailabilityNote,
  hasCode,
  packHealth,
  packProvenance,
  REQUIRED_PACK_FLOOR,
  type PackHealth,
  type PackProvenance,
  type Refusal,
} from "./pack-console";

/**
 * The Extensions route ("/extensions") — the pack lifecycle, in one place.
 *
 * Until this page the platform's defining capability was unreachable: the feeder
 * has served install, list, read, install-history, update-check and uninstall
 * since Wave 0, contracts/marketplace-1.md carries 117 requirements about them,
 * and the console had no route, no nav entry and no consumer for any of it. An
 * operator could see an installed pack's PAGES in the rail and had no way to
 * learn what was installed, put something new on the box, or take it off.
 *
 * ── What this page is careful about ─────────────────────────────────────────
 *
 * IT NEVER OFFERS WORK THE BOX WILL REFUSE. An update check re-resolves a pack
 * through the trust channel pinned on its newest install record; a pack installed
 * directly from an artifact pinned no channel, so the box refuses to check it
 * rather than defaulting one (MKT-094a — supplying a default would be choosing
 * the pack's provenance tier on the operator's behalf). This page reads that off
 * the record and disables the button WITH the reason, instead of shipping a
 * control whose only outcome is a refusal.
 *
 * IT NOW KNOWS WHICH PACKS ARE REQUIRED, AND IT USED NOT TO. A required pack
 * cannot be uninstalled (MKT-093b(i)), and this file used to say the roster was
 * deployment configuration no api/1 route published — so the console marked a
 * pack Required only AFTER an operator tried to remove it and read the refusal.
 * Discovery by attempted destruction. The pack's own representation now carries
 * `required` and `required_floor`, so the badge is there before the attempt.
 *
 * The learned-from-refusal path is KEPT alongside it, not replaced. A box built
 * before the member existed sends neither, and `undefined` is not `false` — it
 * means "this build does not report it", which must not render as "safe to
 * remove". Against such a box the old behaviour is still the only thing there
 * is, and it still refuses LOUDLY: the box's own code, floor version and
 * sentence rendered in full. The legacy console solved this by hiding the Remove
 * button with no explanation at all, which is the "silently uninstallable"
 * failure inverted — an operator who cannot remove a pack and is told nothing.
 *
 * One floor value is not a policy: `unresolved-roster` (packs.UnresolvedFloor)
 * means the host could not READ its roster, so it reports every pack required at
 * a floor no version satisfies and refuses every mutation. Rendering that as an
 * ordinary Required badge would present a broken deployment as a deliberate one,
 * so it gets its own words.
 *
 * IT SAYS "LIVE" ONLY WHERE LIVE IS TRUE. A pack is DATA: a manifest, page
 * documents, message catalogs and declared collections. Installing one writes
 * them in a single transaction; nothing is compiled, nothing is executed, no
 * service restarts. That claim is made good in this console too — a successful
 * install/update/uninstall fires PACKS_CHANGED_EVENT and the shell's Extensions
 * nav re-resolves, so the pack's pages appear in the rail immediately. A console
 * that needed a reload to show its own work would have required a restart from
 * where the operator sits, whatever the box did.
 *
 * EVERY FAILURE IS SHOWN WITH ITS REASON. A refused install renders the box's
 * detail, its machine code, EVERY per-field violation the manifest engine
 * returned, and the trace id — never a generic "install failed". A pack whose
 * declared page path cannot be confined to its own namespace is reported as
 * unreachable rather than silently dropped (which is what the nav does, correctly,
 * and what a management console must not).
 *
 * ── grammar-gap: byte upload ────────────────────────────────────────────────
 *
 * The file picker below is hand-built React over the kit for the same reason
 * /upload's is: ui-schema/1 has no input widget that takes BYTES from a file
 * picker (UIS-070's catalog is text/number/duration/toggle/select/entity/time).
 * Everything downstream — the api/1 POST, the Problem handling, the kit surface —
 * stays inside the shared conventions.
 */

/** The zip an operator installs is produced by `make example-pack` for the in-repo
 * example, and by a publisher's own build otherwise. Named in the empty state so a
 * fresh box has an actual next step rather than an encouraging sentence. */
const EXAMPLE_PACK_COMMAND = "make example-pack   # writes .dev/menu-board.pack.zip";

/** One installed pack, resolved for display: the registry envelope, its own
 * locale-resolved title, what of it is reachable, and where it came from. */
interface PackCard {
  pack: Pack;
  title: string;
  /** False when the pack's `msg:` display name did not resolve against its own
   * catalog — the operator-visible symptom of a missing/incomplete en catalog
   * (MAN-110/111), reported rather than papered over. */
  titleResolved: boolean;
  messages: Record<string, string>;
  health: PackHealth;
  provenance: PackProvenance;
  records: PackInstallRecord[];
  /** Why the install history could not be read, when it could not. */
  recordsError: string | null;
  /** What an update check WOULD do (MKT-095), read without running one.
   *
   * `null` while it is still being fetched, or when the pack is not
   * auto-tracked and there is no channel to ask. A FAILED read is `null` too,
   * and `availabilityError` says why: a source that cannot be reached is not
   * evidence that nothing is waiting, and rendering "Up to date" from a failed
   * lookup would be the console telling a comfortable lie. */
  availability: AvailabilityNote | null;
  availabilityError: string | null;
}

/** A per-pack or per-form outcome: what happened, in the box's own words. */
type Outcome =
  | { kind: "ok"; message: string }
  | { kind: "refused"; refusal: Refusal }
  | { kind: "busy"; message: string };

function OutcomeBanner({ outcome }: { outcome: Outcome | undefined }) {
  if (!outcome) return null;
  if (outcome.kind === "busy") {
    return (
      <p className="text-sm text-muted-foreground" role="status">
        {outcome.message}
      </p>
    );
  }
  if (outcome.kind === "ok") {
    return (
      <p className="text-sm text-[color:var(--wv-ok)]" role="status">
        {outcome.message}
      </p>
    );
  }
  const { refusal } = outcome;
  return (
    <div role="alert" className="flex flex-col gap-1 text-sm text-[color:var(--wv-err)]">
      <p>{refusal.detail}</p>
      {refusal.fields.length > 0 ? (
        <ul className="flex flex-col gap-0.5 pl-4">
          {refusal.fields.map((f, i) => (
            <li key={`${f.field}-${f.code}-${i}`} className="list-disc font-mono text-[11px]">
              {f.field}: {f.code} — {f.message}
            </li>
          ))}
        </ul>
      ) : null}
      <p className="font-mono text-[11px] text-muted-foreground">
        {refusal.codes.length > 0 ? refusal.codes.join(" · ") : `HTTP ${refusal.status || "—"}`}
        {refusal.traceId ? ` · trace ${refusal.traceId}` : ""}
      </p>
    </div>
  );
}

/** The install history — the only answer this box has to "which key vouched for
 * the bytes that are running" (the signature envelope is dropped at install and
 * never persisted, MKT-009a/094a). */
function HistoryPanel({
  records,
  error,
  onGoBack,
}: {
  records: PackInstallRecord[];
  error: string | null;
  /** Reinstall an earlier version this box actually ran. Absent for a record
   * with no trust channel — a direct upload has no reference to resolve. */
  onGoBack: (record: PackInstallRecord) => void;
}) {
  if (error) {
    return (
      <p className="text-sm text-[color:var(--wv-err)]" role="alert">
        The install history could not be read: {error}
      </p>
    );
  }
  if (records.length === 0) {
    return <p className="text-sm text-muted-foreground">No install records.</p>;
  }
  // Newest first for reading; the API serves oldest-first (the last entry is the pin).
  const newestFirst = [...records].reverse();
  return (
    <ol className="flex flex-col gap-3">
      {newestFirst.map((rec, i) => (
        <li key={rec.id} className="flex flex-col gap-1 border-l-2 border-border pl-3">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium">{rec.resolved_version}</span>
            {i === 0 ? <StatusBadge status="ok">Current</StatusBadge> : null}
            {rec.trust_channel ? (
              <StatusBadge status="pending">{rec.trust_channel}</StatusBadge>
            ) : (
              <StatusBadge status="off">Direct</StatusBadge>
            )}
            {rec.stale_source ? <StatusBadge status="warn">Stale source</StatusBadge> : null}
            {/* The escape from a bad version, made reachable.
                MKT-050 refuses a channel pointer that moves backward, and its
                only exception fires when the running version has been YANKED —
                which never happens to a version that is merely broken for this
                deployment. An explicit version pin is deliberately exempt
                (MKT-060a(c)), so this is the operator's way back, and until now
                it existed only for someone willing to hand-type a reference.
                Offered on versions this box ITSELF ran, which is the set worth
                trusting: each one is a version that installed and verified
                here. A direct upload has no reference to resolve, so it gets no
                button rather than a broken one. */}
            {i > 0 && rec.trust_channel ? (
              <Button
                variant="ghost"
                icon={History}
                onClick={() => onGoBack(rec)}
                aria-label={`Go back to version ${rec.resolved_version}`}
              >
                Go back to this version
              </Button>
            ) : null}
          </div>
          <p className="text-xs text-muted-foreground">
            {new Date(rec.installed_at).toLocaleString()} · source{" "}
            <span className="font-mono">{rec.source}</span>
          </p>
          <p className="font-mono text-[11px] break-all text-muted-foreground">
            key {rec.key_id} · digest {rec.content_digest}
            {rec.artifact_digest ? ` · entry ${rec.artifact_digest}` : ""}
          </p>
        </li>
      ))}
    </ol>
  );
}

/** The floor an UNRESOLVED roster reports for every pack (marketplace/1
 * MKT-093a; `packs.UnresolvedFloor` server-side). Not a version and deliberately
 * unparseable as one, so it can never satisfy a floor comparison — which is how
 * a host that could not read its roster refuses every pack mutation instead of
 * silently lifting every restriction.
 *
 * Named here rather than compared inline so the one place this console decides
 * "broken, not chosen" is greppable from the constant. */
const UNRESOLVED_ROSTER_FLOOR = "unresolved-roster";

export default function ExtensionsRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);

  const [cards, setCards] = useState<PackCard[]>([]);
  const [loading, setLoading] = useState(true);
  const [listError, setListError] = useState<string | null>(null);
  const [catalog, setCatalog] = useState<{ sources: CatalogSource[]; error: string | null }>({
    sources: [],
    error: null,
  });
  const [catalogLoading, setCatalogLoading] = useState(false);
  // Every pack LIFECYCLE route is gated server-side on admin at the workspace
  // root (packLifecycleWritable). Mirrored here so an operator is not offered
  // an install they cannot complete — the threshold is read off the server's
  // own rule rather than guessed, because a console that gates at a DIFFERENT
  // level than the server either hides work someone may do or keeps offering
  // work they may not.
  //
  // Reading is untouched: the list, the health, the history and the catalog are
  // all visible to anyone signed in, and only the verbs that change the
  // deployment are withheld.
  const session = useOptionalSession();
  const mayChange = can(session?.session.role, "admin");
  // `session === null` is the shell mounted OUTSIDE a SessionGate (the design
  // route, this page's own tests). There is no role to check there, and
  // disabling everything would make the page untestable and the design route
  // misleading — so the gate applies only where a session actually exists.
  const gated = session !== null && !mayChange;

  // Required status LEARNED from the box's own refusal, and remembered for the
  // session so the operator is not invited to try again into the same wall.
  //
  // Retained now that the representation reports it, for the reason the header
  // gives: an older box sends neither member, and this is the only signal there.
  const [requiredPacks, setRequiredPacks] = useState<Set<string>>(new Set());

  const [outcomes, setOutcomes] = useState<Record<string, Outcome>>({});
  const [openHistory, setOpenHistory] = useState<Set<string>>(new Set());
  const [confirmingRemoval, setConfirmingRemoval] = useState<string | null>(null);

  // Install-from-file
  const fileRef = useRef<HTMLInputElement>(null);
  const [file, setFile] = useState<File | null>(null);
  // Install-from-reference (MKT-060a). The trust channel starts EMPTY: it is
  // required and is never defaulted, here as on the box.
  const [refPackId, setRefPackId] = useState("");
  const [refChannel, setRefChannel] = useState<TrustChannel | "">("");
  const [refSource, setRefSource] = useState("");
  const [refVersion, setRefVersion] = useState("");
  const [busy, setBusy] = useState(false);

  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  const setOutcome = useCallback((key: string, outcome: Outcome | null) => {
    setOutcomes((prev) => {
      const next = { ...prev };
      if (outcome === null) delete next[key];
      else next[key] = outcome;
      return next;
    });
  }, []);

  /** Load every installed pack, with its locale-resolved title and its install
   * history. Two extra reads per pack (the en catalog the nav already needs, and
   * the record history the update affordance depends on) — a box holds a handful
   * of packs, and both answers are load-bearing rather than decorative. */
  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const packs = await collectPages<Pack>((cursor) => client.packs.list({ cursor }));
      const built = await Promise.all(
        packs.map(async (pack): Promise<PackCard> => {
          const messages = await loadPackCatalog(client.packs, pack.id);
          let records: PackInstallRecord[] = [];
          let recordsError: string | null = null;
          try {
            records = await collectPages<PackInstallRecord>((cursor) =>
              client.packs.installs(pack.id, { cursor }),
            );
          } catch (err: unknown) {
            recordsError = describeRefusal(err).detail;
          }
          const provenance = packProvenance(records);
          // Only auto-tracked packs have a channel to ask about. A direct
          // install has nothing to resolve through (MKT-094a) and the endpoint
          // refuses it, so asking would spend a request to be told what the
          // records already say.
          let availability: AvailabilityNote | null = null;
          let availabilityError: string | null = null;
          if (provenance.autoTracked) {
            try {
              availability = describeAvailability(await client.packs.updateAvailability(pack.id));
            } catch (err: unknown) {
              availabilityError = describeRefusal(err).detail;
            }
          }
          return {
            pack,
            title: resolveTitle(messages, pack.manifest.displayName),
            titleResolved: typeof messages[pack.manifest.displayName] === "string",
            messages,
            health: packHealth(pack),
            provenance,
            records,
            recordsError,
            availability,
            availabilityError,
          };
        }),
      );
      if (!alive.current) return;
      setCards(built);
      setListError(null);
    } catch (err: unknown) {
      if (!alive.current) return;
      setListError(describeRefusal(err).detail);
    } finally {
      if (alive.current) setLoading(false);
    }
  }, [client]);

  useEffect(() => {
    void refresh();
    // One request, whatever the source count — the box fetches each index
    // server-side — and its own state, so a slow registry never delays the
    // installed list.
    void loadCatalog();
  }, [refresh]);

  /** The identity+version of what is installed right now — what an install's
   * outcome sentence is worded against. */
  const installedNow = useMemo(
    () => cards.map((c) => ({ id: c.pack.id, version: c.pack.version })),
    [cards],
  );

  const installFile = useCallback(async () => {
    if (!file) return;
    const previous = installedNow;
    setBusy(true);
    setOutcome("install-file", { kind: "busy", message: `Installing ${file.name}…` });
    try {
      // Read the artifact into bytes rather than handing the File to fetch as a
      // stream. A pack artifact is capped at 8 MiB server-side, so buffering it is
      // cheap — and the request then carries a definite body with a known length
      // instead of depending on the runtime's Blob-streaming support, which is
      // exactly where a "successful" upload can silently send the wrong thing.
      const bytes = await file.arrayBuffer();
      const result = await client.packs.install(bytes);
      if (!alive.current) return;
      setOutcome("install-file", {
        kind: "ok",
        message: describeInstall(result.id, result.version, previous),
      });
      setFile(null);
      if (fileRef.current) fileRef.current.value = "";
      await refresh();
      notifyPacksChanged();
      toast.success(`${result.id} ${result.version} installed`);
    } catch (err: unknown) {
      if (alive.current) setOutcome("install-file", { kind: "refused", refusal: describeRefusal(err) });
    } finally {
      if (alive.current) setBusy(false);
    }
  }, [client, file, installedNow, refresh, setOutcome]);

  const installRef = useCallback(async () => {
    if (refChannel === "") return;
    const previous = installedNow;
    setBusy(true);
    setOutcome("install-ref", { kind: "busy", message: `Resolving ${refPackId}…` });
    try {
      const result = await client.packs.installRef({
        pack_id: refPackId.trim(),
        trust_channel: refChannel,
        ...(refSource.trim() ? { source: refSource.trim() } : {}),
        ...(refVersion.trim() ? { version: refVersion.trim() } : {}),
      });
      if (!alive.current) return;
      setOutcome("install-ref", {
        kind: "ok",
        message: describeInstall(result.id, result.version, previous),
      });
      await refresh();
      notifyPacksChanged();
      toast.success(`${result.id} ${result.version} installed`);
    } catch (err: unknown) {
      if (alive.current) setOutcome("install-ref", { kind: "refused", refusal: describeRefusal(err) });
    } finally {
      if (alive.current) setBusy(false);
    }
  }, [client, installedNow, refChannel, refPackId, refSource, refVersion, refresh, setOutcome]);

  /** Load what the configured sources offer (MKT-096). Kept OUT of `refresh`
   * so a slow or unreachable registry never delays the installed list, which is
   * the answer an operator needs most and the one this box can always give. */
  const loadCatalog = useCallback(async () => {
    setCatalogLoading(true);
    try {
      const sources = await client.packs.catalog();
      if (alive.current) setCatalog({ sources, error: null });
    } catch (err: unknown) {
      if (alive.current) setCatalog({ sources: [], error: describeRefusal(err).detail });
    } finally {
      if (alive.current) setCatalogLoading(false);
    }
  }, [client]);

  /** Install one browsed entry, by the exact reference it names. The listing is
   * untrusted (see CatalogEntry) — what makes acting on it safe is that this
   * goes through the same resolve-and-verify path a typed reference does. */
  const installFromCatalog = useCallback(
    async (entry: CatalogEntry) => {
      const key = `catalog:${entry.source}:${entry.id}:${entry.version}`;
      // The channel arrives as a bare string from an index this box does not
      // verify, so it is CHECKED against the channels this client knows rather
      // than cast into the union. An entry naming a channel we do not
      // implement is one we cannot install, and saying so beats sending a
      // value we could see was wrong and letting the server reject it.
      const channel = TRUST_CHANNELS.find((c) => c === entry.trust_channel);
      if (!channel) {
        setOutcome(key, {
          kind: "ok",
          message: `${entry.id} names trust channel "${entry.trust_channel}", which this console does not implement. It cannot be installed from here.`,
        });
        return;
      }
      setOutcome(key, { kind: "busy", message: `Resolving ${entry.id} ${entry.version}…` });
      try {
        const installed = await collectPages<Pack>((cursor) => client.packs.list({ cursor }));
        const result = await client.packs.installRef({
          pack_id: entry.id,
          trust_channel: channel,
          source: entry.source,
          version: entry.version,
        });
        if (!alive.current) return;
        setOutcome(key, {
          kind: "ok",
          message: describeInstall(result.id, result.version, installed),
        });
        await refresh();
        notifyPacksChanged();
      } catch (err: unknown) {
        if (alive.current) setOutcome(key, { kind: "refused", refusal: describeRefusal(err) });
      }
    },
    [client, refresh, setOutcome],
  );

  /** Turn a pack off, or back on (MKT-097). The reversible alternative to
   * uninstalling a pack that is misbehaving — which is the only other thing an
   * operator can do to one, and it destroys their authored rows. */
  const setEnabled = useCallback(
    async (id: string, enabled: boolean) => {
      setOutcome(id, { kind: "busy", message: enabled ? "Enabling…" : "Disabling…" });
      try {
        await client.packs.setEnabled(id, enabled);
        if (!alive.current) return;
        setOutcome(id, {
          kind: "ok",
          message: enabled
            ? "Enabled. Its pages are being served again."
            : "Disabled. Its pages are withdrawn; its data, install history and the extension itself are untouched.",
        });
        await refresh();
        // The rail lists only enabled packs, so it has to re-resolve.
        notifyPacksChanged();
      } catch (err: unknown) {
        if (alive.current) setOutcome(id, { kind: "refused", refusal: describeRefusal(err) });
      }
    },
    [client, refresh, setOutcome],
  );

  /** Reinstall an earlier version this box itself ran (MKT-060a(c)).
   *
   * The channel and source come off the RECORD, not from the current pin: the
   * question is "put back the thing that worked", and the thing that worked was
   * resolved through the channel and source recorded at the time. */
  const goBackToVersion = useCallback(
    async (id: string, rec: PackInstallRecord) => {
      setOutcome(id, { kind: "busy", message: `Resolving ${rec.resolved_version}…` });
      try {
        const installed = await collectPages<Pack>((cursor) => client.packs.list({ cursor }));
        const channel = TRUST_CHANNELS.find((c) => c === rec.trust_channel);
        if (!channel) {
          setOutcome(id, {
            kind: "ok",
            message: `That install was recorded on trust channel "${rec.trust_channel}", which this console does not implement.`,
          });
          return;
        }
        const result = await client.packs.installRef({
          pack_id: id,
          trust_channel: channel,
          source: rec.source,
          version: rec.resolved_version,
        });
        if (!alive.current) return;
        setOutcome(id, { kind: "ok", message: describeInstall(result.id, result.version, installed) });
        await refresh();
        notifyPacksChanged();
      } catch (err: unknown) {
        if (alive.current) setOutcome(id, { kind: "refused", refusal: describeRefusal(err) });
      }
    },
    [client, refresh, setOutcome],
  );

  const checkUpdate = useCallback(
    async (id: string) => {
      setOutcome(id, { kind: "busy", message: "Checking the pinned channel…" });
      try {
        const result = await client.packs.update(id);
        if (!alive.current) return;
        setOutcome(id, { kind: "ok", message: describeUpdate(result) });
        if (result.action !== "unchanged") {
          await refresh();
          notifyPacksChanged();
        }
      } catch (err: unknown) {
        if (alive.current) setOutcome(id, { kind: "refused", refusal: describeRefusal(err) });
      }
    },
    [client, refresh, setOutcome],
  );

  const uninstall = useCallback(
    async (id: string) => {
      setConfirmingRemoval(null);
      setOutcome(id, { kind: "busy", message: "Removing…" });
      try {
        // Re-read for a FRESH ETag: the If-Match must be the revision this delete
        // is conditioned on, not one captured when the page loaded (API-022 — no
        // unconditional overwrite, and no stale-precondition delete either).
        const read = await client.packs.get(id);
        await client.packs.remove(id, read.etag);
        if (!alive.current) return;
        // Reported at PAGE level, not on the card: the card is about to stop
        // existing, and a success message that disappears with the thing it
        // describes is a success nobody was told about.
        setOutcome(id, null);
        setOutcome("page", {
          kind: "ok",
          message: `${id} removed — its pages, its rows and its install records went with it, in one transaction.`,
        });
        await refresh();
        notifyPacksChanged();
        toast.success(`${id} removed`);
      } catch (err: unknown) {
        if (!alive.current) return;
        const refusal = describeRefusal(err);
        if (hasCode(refusal, REQUIRED_PACK_FLOOR)) {
          // Learned, not guessed: no route publishes the roster, so the refusal IS
          // the discovery. Remember it so the control stops inviting a retry.
          setRequiredPacks((prev) => new Set(prev).add(id));
        }
        setOutcome(id, { kind: "refused", refusal });
      }
    },
    [client, refresh, setOutcome],
  );

  const toggleHistory = useCallback((id: string) => {
    setOpenHistory((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const autoTracked = cards.filter((c) => c.provenance.autoTracked).length;
  // What is already on this box, so a catalog row can say so rather than
  // offering an install that would be a no-op or a downgrade.
  const installedVersions = new Map(cards.map((c) => [c.pack.id, c.pack.version]));
  const updatesWaiting = cards.filter((c) => c.availability?.waiting).length;
  // Counted apart from updatesWaiting on purpose — see describeAvailability:
  // a withdrawn running version is a fault, not an upgrade, and the two must
  // never be summed into one number.
  const withdrawn = cards.filter((c) => c.availability?.tone === "warn").length;
  const refReady = refPackId.trim() !== "" && refChannel !== "" && !busy;

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Extensions"
          description="Everything this box can do beyond the core console — installed, updated and removed while it keeps running."
          actions={
            <Button
              variant="secondary"
              icon={RefreshCw}
              onClick={() => void refresh()}
              aria-label="Reload the installed extensions"
            >
              Reload
            </Button>
          }
        />

        <section
          aria-labelledby="live-heading"
          className="flex flex-col gap-2 rounded-card border border-border bg-card p-5 wv-elevation"
        >
          <h2 id="live-heading" className="text-lg font-semibold">
            Installing happens live
          </h2>
          <p className="text-sm text-muted-foreground">
            An extension is <strong>sealed and signed</strong>: a manifest, its pages, its
            message catalogs and the data collections it declares. Installing one writes those files
            and its collections in a single transaction, and the pack's pages appear in the
            navigation immediately. There is no image rebuild and no reboot in that path. Removing
            one takes its pages, its authored rows and its install records away in one transaction
            too.
          </p>
          <p className="text-sm text-muted-foreground">
            Every pack installable today is <strong>declarative</strong> — data this box validates
            and renders. Extensions that carry code install through this same signed pipeline; some
            of those will need a quick restart of the application server to take effect, never a
            reboot of the box.
          </p>
        </section>

        {gated ? (
          <p className="rounded-input border border-border px-3 py-2 text-sm">
            <strong>You can look, but not change.</strong>{" "}
            <span className="text-muted-foreground">
              Installing, updating, disabling and removing extensions need the <strong>admin</strong>{" "}
              role; you are signed in as <strong>{session?.session.role}</strong>. Everything below
              is readable.
            </span>
          </p>
        ) : null}

        <div className="flex flex-wrap gap-4">
          <StatCard label="Installed" value={String(cards.length)} hint="extensions on this box" />
          <StatCard
            label="Auto-tracked"
            value={String(autoTracked)}
            hint="pinned to a trust channel"
          />
          <StatCard
            label="Direct installs"
            value={String(cards.length - autoTracked)}
            hint="no channel — not auto-tracked"
          />
          <StatCard
            label="Updates waiting"
            value={String(updatesWaiting)}
            hint={
              updatesWaiting === 0
                ? "every tracked extension is current"
                : "a newer version is published; nothing applied"
            }
          />
          {withdrawn > 0 ? (
            <StatCard
              label="Withdrawn"
              value={String(withdrawn)}
              hint="running a version its publisher pulled"
            />
          ) : null}
        </div>

        <section aria-labelledby="add-heading" className="flex flex-col gap-3">
          <h2 id="add-heading" className="text-lg font-semibold">
            Add an extension
          </h2>
          <div className="grid gap-4 lg:grid-cols-2">
            {/* ── From an artifact ─────────────────────────────────────────── */}
            <div className="flex flex-col gap-3 rounded-card border border-border bg-card p-5 wv-elevation">
              <div className="flex items-center gap-2">
                <KitIcon icon={Upload} decorative className="size-4 text-muted-foreground" />
                <h3 className="font-medium">From an extension file</h3>
              </div>
              <p className="text-sm text-muted-foreground">
                A signed <span className="font-mono text-xs">.pack.zip</span>. The box verifies its
                signature envelope against this deployment's trust anchors and puts the manifest
                through the same gate a marketplace install goes through — an unsigned or
                mis-signed artifact is refused, and nothing lands.
              </p>
              <FormField label="Extension artifact" help="A signed zip built by the extension's publisher.">
                {(control) => (
                  <input
                    {...control}
                    ref={fileRef}
                    type="file"
                    accept=".zip,application/zip"
                    className="text-sm file:mr-3 file:rounded-input file:border file:border-border file:bg-[color:var(--wv-surface-2)] file:px-3 file:py-1.5 file:text-sm"
                    onChange={(e) => setFile(e.target.files?.[0] ?? null)}
                  />
                )}
              </FormField>
              <div>
                <Button
                  icon={PackagePlus}
                  disabled={!file || busy || gated}
                  onClick={() => void installFile()}
                  aria-label="Install the chosen extension file"
                >
                  Install extension
                </Button>
              </div>
              <OutcomeBanner outcome={outcomes["install-file"]} />
            </div>

            {/* ── From a marketplace reference ─────────────────────────────── */}
            <div className="flex flex-col gap-3 rounded-card border border-border bg-card p-5 wv-elevation">
              <div className="flex items-center gap-2">
                <KitIcon icon={Store} decorative className="size-4 text-muted-foreground" />
                <h3 className="font-medium">From the marketplace</h3>
              </div>
              <p className="text-sm text-muted-foreground">
                Name a pack and this box resolves it through its configured registry sources, then
                verifies and installs it through the identical pipeline a file upload takes.{" "}
                <strong>There is no catalogue to browse here</strong> — this api exposes resolution,
                not discovery — so the extension id is typed rather than picked. Nothing publishes which
                registry sources this box trusts either; if it has none configured, it will say so
                when you ask it to resolve one.
              </p>
              <div className="grid gap-3 sm:grid-cols-2">
                <FormField label="Extension id" help="Publisher-qualified, e.g. waiveo/menu-board.">
                  {(control) => (
                    <input
                      {...control}
                      type="text"
                      autoComplete="off"
                      placeholder="publisher/name"
                      className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
                      value={refPackId}
                      onChange={(e) => setRefPackId(e.target.value)}
                    />
                  )}
                </FormField>
                <FormField
                  label="Trust channel"
                  help="Required. Nothing chooses this for you: the channel is the level of review the install is accepted under."
                >
                  {(control) => (
                    <select
                      {...control}
                      className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
                      value={refChannel}
                      onChange={(e) => setRefChannel(e.target.value as TrustChannel | "")}
                    >
                      <option value="">Choose a channel…</option>
                      {TRUST_CHANNELS.map((c) => (
                        <option key={c} value={c}>
                          {c}
                        </option>
                      ))}
                    </select>
                  )}
                </FormField>
                <FormField label="Source (optional)" help="Blank walks every configured source in order.">
                  {(control) => (
                    <input
                      {...control}
                      type="text"
                      autoComplete="off"
                      className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
                      value={refSource}
                      onChange={(e) => setRefSource(e.target.value)}
                    />
                  )}
                </FormField>
                <FormField
                  label="Version (optional)"
                  help="Blank follows the channel; a version pins that exact release."
                >
                  {(control) => (
                    <input
                      {...control}
                      type="text"
                      autoComplete="off"
                      placeholder="1.0.0"
                      className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
                      value={refVersion}
                      onChange={(e) => setRefVersion(e.target.value)}
                    />
                  )}
                </FormField>
              </div>
              <div>
                <Button
                  icon={Store}
                  disabled={!refReady || gated}
                  onClick={() => void installRef()}
                  aria-label="Resolve and install this marketplace reference"
                >
                  Resolve and install
                </Button>
              </div>
              <OutcomeBanner outcome={outcomes["install-ref"]} />
            </div>
          </div>
        </section>

        <section aria-labelledby="catalog-heading" className="flex flex-col gap-3">
          <h2 id="catalog-heading" className="text-lg font-semibold">
            Browse the catalog
          </h2>
          <p className="text-sm text-muted-foreground">
            What the configured registry sources say they offer. A listing is not a guarantee:
            this box has not verified anything about an artifact at the point it is listed. What
            it verifies is the install — the same resolution and signature checks a typed
            reference goes through, which is why picking from here is no riskier than typing it.
          </p>

          {catalog.error ? (
            <p className="text-sm text-[color:var(--wv-err)]" role="alert">
              The catalog could not be read: {catalog.error}
            </p>
          ) : catalogLoading ? (
            <p className="text-sm text-muted-foreground" role="status">
              Reading the registry sources…
            </p>
          ) : catalog.sources.length === 0 ? (
            <EmptyState
              title="No registry sources configured"
              description="This deployment resolves packs from uploaded files only. A source list is host-provisioned."
            />
          ) : (
            <div className="flex flex-col gap-4">
              {catalog.sources.map((src) => (
                <div key={src.source} className="rounded-input border border-border p-3">
                  <div className="flex flex-wrap items-baseline gap-2">
                    <h3 className="font-medium">{src.source}</h3>
                    <span className="text-xs text-muted-foreground">{src.trust_channel}</span>
                  </div>
                  {src.unavailable ? (
                    /* Reported, never omitted: an empty catalog and an
                       unreachable registry are different facts. */
                    <p className="mt-2 text-sm text-[color:var(--wv-err)]" role="alert">
                      This source could not be read: {src.unavailable}
                    </p>
                  ) : src.entries.length === 0 ? (
                    <p className="mt-2 text-sm text-muted-foreground">
                      This source answered and offers nothing.
                    </p>
                  ) : (
                    <ul className="mt-2 flex flex-col gap-2">
                      {src.entries.map((entry) => {
                        const key = `catalog:${entry.source}:${entry.id}:${entry.version}`;
                        const installed = installedVersions.get(entry.id);
                        return (
                          <li key={key} className="flex flex-col gap-1">
                            <div className="flex flex-wrap items-center gap-2">
                              <span className="font-mono text-sm">{entry.id}</span>
                              <span className="text-sm text-muted-foreground">{entry.version}</span>
                              {entry.status !== "active" ? (
                                <span className="text-xs text-muted-foreground">
                                  {entry.status}
                                </span>
                              ) : null}
                              {installed === entry.version ? (
                                <span className="text-xs text-muted-foreground">installed</span>
                              ) : installed ? (
                                <span className="text-xs text-muted-foreground">
                                  installed: {installed}
                                </span>
                              ) : null}
                              <Button
                                variant="secondary"
                                icon={Store}
                                disabled={installed === entry.version || gated}
                                onClick={() => void installFromCatalog(entry)}
                                aria-label={`Install ${entry.id} ${entry.version} from ${entry.source}`}
                              >
                                {installed ? "Switch to this version" : "Install"}
                              </Button>
                            </div>
                            <OutcomeBanner outcome={outcomes[key]} />
                          </li>
                        );
                      })}
                    </ul>
                  )}
                </div>
              ))}
            </div>
          )}
        </section>

        <section aria-labelledby="installed-heading" className="flex flex-col gap-3">
          <h2 id="installed-heading" className="text-lg font-semibold">
            Installed
          </h2>
          <p className="text-sm text-muted-foreground">
            This deployment does not publish which packs it treats as required, so none is marked
            up front. A pack the deployment cannot run without refuses removal, with its floor
            version, at the moment you ask.
          </p>

          <OutcomeBanner outcome={outcomes["page"]} />

          {listError ? (
            <p className="text-sm text-[color:var(--wv-err)]" role="alert">
              The installed extensions could not be read: {listError}
            </p>
          ) : loading ? (
            <p className="text-sm text-muted-foreground" role="status">
              Loading installed extensions…
            </p>
          ) : cards.length === 0 ? (
            <EmptyState
              title="No extensions installed"
              description="Install a signed extension file above, or resolve one from a configured registry source. The in-repo example builds with the command below."
              action={
                <code className="rounded-input bg-[color:var(--wv-surface-2)] px-3 py-1.5 font-mono text-xs">
                  {EXAMPLE_PACK_COMMAND}
                </code>
              }
            />
          ) : (
            <ul className="flex flex-col gap-4">
              {cards.map((card) => {
                const id = card.pack.id;
                // Reported by the box, OR learned from a refusal against a box
                // that does not report it. The union is the load-bearing part:
                // an absent member is not `false`, it is "this build does not
                // say", and the learned set is the only thing that can speak for
                // that box. (`=== true` reads as intent here and is nothing more
                // — for `boolean | undefined` it is exactly `Boolean(...)`. A
                // mutation proved that, after this comment had claimed it was a
                // guard.)
                const required = card.pack.required === true || requiredPacks.has(id);
                // The floor that says the roster itself is broken, not a policy.
                const rosterUnresolved = card.pack.required_floor === UNRESOLVED_ROSTER_FLOOR;
                const historyOpen = openHistory.has(id);
                return (
                  <li
                    key={id}
                    data-slot="pack-card"
                    data-pack-id={id}
                    className="flex flex-col gap-3 rounded-card border border-border bg-card p-5 wv-elevation"
                  >
                    <div className="flex flex-wrap items-start justify-between gap-3">
                      <div className="flex min-w-0 items-start gap-3">
                        <KitIcon
                          icon={resolvePackIcon(card.pack.manifest.icon)}
                          decorative
                          className="mt-0.5 size-5 shrink-0 text-muted-foreground"
                        />
                        <div className="flex min-w-0 flex-col gap-1">
                          <h3 className="font-display text-[17px] font-semibold">{card.title}</h3>
                          <p className="font-mono text-xs break-all text-muted-foreground">{id}</p>
                        </div>
                      </div>
                      <div className="flex flex-wrap items-center gap-2">
                        <StatusBadge status="ok">v{card.pack.version}</StatusBadge>
                        {!card.pack.enabled ? <StatusBadge status="off">Disabled</StatusBadge> : null}
                        {rosterUnresolved ? (
                          <StatusBadge status="error">Roster unreadable</StatusBadge>
                        ) : required ? (
                          <StatusBadge status="warn">
                            {card.pack.required_floor
                              ? `Required · floor v${card.pack.required_floor}`
                              : "Required"}
                          </StatusBadge>
                        ) : null}
                        {card.provenance.channel ? (
                          <StatusBadge status="pending">{card.provenance.channel}</StatusBadge>
                        ) : (
                          <StatusBadge status="off">Direct install</StatusBadge>
                        )}
                      </div>
                    </div>

                    {!card.pack.enabled ? (
                      // Said in full, not left to a badge. "Disabled" alone does
                      // not tell an operator whether their data survived, and
                      // that is the single thing they need to know before
                      // trusting the control enough to use it instead of
                      // uninstalling.
                      <p className="rounded-input border border-border px-3 py-2 text-sm">
                        <strong>This extension is turned off.</strong>{" "}
                        <span className="text-muted-foreground">
                          Its pages are not served and it does not appear in the navigation. Its
                          data, its install history and the extension itself are untouched —
                          enabling it puts everything back.
                        </span>
                      </p>
                    ) : null}

                    {card.pack.manifest.description ? (
                      <p className="text-sm text-muted-foreground">
                        {card.pack.manifest.description}
                      </p>
                    ) : null}

                    {/* Health — only what is actually wrong, each with its reason. */}
                    {!card.titleResolved ? (
                      <p className="flex items-start gap-2 text-sm text-[color:var(--wv-warn)]">
                        <KitIcon icon={TriangleAlert} decorative className="mt-0.5 size-4 shrink-0" />
                        <span>
                          This pack's display strings did not resolve: its default (en) message
                          catalog is missing or does not carry{" "}
                          <span className="font-mono text-xs">{card.pack.manifest.displayName}</span>
                          . The names below are derived from their references.
                        </span>
                      </p>
                    ) : null}
                    {card.health.unreachablePages.length > 0 ? (
                      <p className="flex items-start gap-2 text-sm text-[color:var(--wv-err)]">
                        <KitIcon icon={TriangleAlert} decorative className="mt-0.5 size-4 shrink-0" />
                        <span>
                          {card.health.unreachablePages.length} declared page
                          {card.health.unreachablePages.length === 1 ? " is" : "s are"} unreachable
                          and {card.health.unreachablePages.length === 1 ? "does" : "do"} not appear
                          in the navigation — the path escapes this pack's own namespace:{" "}
                          <span className="font-mono text-xs break-all">
                            {card.health.unreachablePages.join(", ")}
                          </span>
                        </span>
                      </p>
                    ) : null}

                    <dl className="grid gap-3 text-sm sm:grid-cols-2">
                      <div className="flex flex-col gap-1">
                        <dt className="text-xs uppercase tracking-wide text-muted-foreground">
                          Pages
                        </dt>
                        <dd>
                          {card.health.pages.length === 0 ? (
                            <span className="text-muted-foreground">
                              None — this pack contributes no console pages.
                            </span>
                          ) : (
                            <ul className="flex flex-wrap gap-2">
                              {card.health.pages.map((pg) => (
                                <li key={pg.path}>
                                  <Link
                                    to={pg.href}
                                    className="inline-flex items-center gap-1 rounded-input px-2 py-0.5 text-sm underline outline-none focus-visible:ring-2 focus-visible:ring-ring"
                                  >
                                    <KitIcon icon={FileText} decorative className="size-3.5" />
                                    {resolveTitle(card.messages, pg.titleMsg)}
                                  </Link>
                                </li>
                              ))}
                            </ul>
                          )}
                        </dd>
                      </div>
                      <div className="flex flex-col gap-1">
                        <dt className="text-xs uppercase tracking-wide text-muted-foreground">
                          Data collections
                        </dt>
                        <dd className="text-muted-foreground">
                          {card.health.collections.length === 0
                            ? "None."
                            : card.health.collections.join(", ")}
                        </dd>
                      </div>
                      <div className="flex flex-col gap-1">
                        <dt className="text-xs uppercase tracking-wide text-muted-foreground">
                          Provenance
                        </dt>
                        <dd className="text-muted-foreground">
                          {card.provenance.newest ? (
                            <>
                              from <span className="font-mono text-xs">{card.provenance.newest.source}</span>
                              , verified under key{" "}
                              <span className="font-mono text-xs break-all">
                                {card.provenance.newest.key_id}
                              </span>
                            </>
                          ) : (
                            (card.recordsError ?? "No install record.")
                          )}
                        </dd>
                      </div>
                      <div className="flex flex-col gap-1">
                        <dt className="text-xs uppercase tracking-wide text-muted-foreground">
                          Installed
                        </dt>
                        <dd className="text-muted-foreground">
                          {new Date(card.pack.updated_at).toLocaleString()}
                        </dd>
                      </div>
                    </dl>

                    {card.availability ? (
                      <p
                        className={
                          card.availability.tone === "warn"
                            ? "rounded-input border border-[color:var(--wv-err)] px-3 py-2 text-sm"
                            : card.availability.tone === "info"
                              ? "rounded-input border border-border px-3 py-2 text-sm"
                              : "text-sm text-muted-foreground"
                        }
                      >
                        <strong>{card.availability.label}</strong>{" "}
                        <span className="text-muted-foreground">{card.availability.detail}</span>
                      </p>
                    ) : card.availabilityError ? (
                      // Never rendered as "Up to date": a lookup that failed is
                      // not evidence that nothing is waiting.
                      <p className="text-sm text-muted-foreground">
                        Could not read the channel for {id}: {card.availabilityError}
                      </p>
                    ) : null}

                    <div className="flex flex-wrap items-center gap-2">
                      <Button
                        variant="secondary"
                        icon={card.pack.enabled ? PowerOff : Power}
                        disabled={gated}
                        onClick={() => void setEnabled(id, !card.pack.enabled)}
                        aria-label={
                          card.pack.enabled
                            ? `Disable ${id}, keeping its data`
                            : `Enable ${id}`
                        }
                      >
                        {card.pack.enabled ? "Disable" : "Enable"}
                      </Button>
                      <Button
                        variant={card.availability?.waiting ? "default" : "secondary"}
                        icon={RefreshCw}
                        disabled={!card.provenance.autoTracked || gated}
                        onClick={() => void checkUpdate(id)}
                        aria-label={
                          card.availability?.waiting
                            ? `Update ${id} to ${card.availability.label.replace(/^.*— /, "")}`
                            : `Check ${id} for updates`
                        }
                      >
                        {card.availability?.waiting
                          ? "Update now"
                          : card.availability?.tone === "warn"
                            ? "Roll back now"
                            : "Check for updates"}
                      </Button>
                      <Button
                        variant="ghost"
                        icon={History}
                        aria-expanded={historyOpen}
                        onClick={() => toggleHistory(id)}
                        aria-label={`${historyOpen ? "Hide" : "Show"} the install history for ${id}`}
                      >
                        Install history
                      </Button>
                      <Button
                        variant="destructive"
                        icon={Trash2}
                        disabled={required || gated}
                        onClick={() => setConfirmingRemoval(id)}
                        aria-label={`Uninstall ${id}`}
                      >
                        Uninstall
                      </Button>
                    </div>

                    {!card.provenance.autoTracked ? (
                      <p className="text-xs text-muted-foreground">
                        Updates unavailable. {card.provenance.reason}
                      </p>
                    ) : null}
                    {required ? (
                      <p className="text-xs text-[color:var(--wv-warn)]">
                        This box refused to remove {id}: it is required here. Removal stays
                        unavailable until the deployment's required-pack roster changes.
                      </p>
                    ) : null}

                    <OutcomeBanner outcome={outcomes[id]} />

                    {historyOpen ? (
                      <div className="flex flex-col gap-2 border-t border-border pt-3">
                        <h4 className="text-sm font-medium">Install history</h4>
                        <p className="text-xs text-muted-foreground">
                          Every version this box has applied, and the key that vouched for its
                          bytes. The signature envelope itself is discarded at install, so this is
                          the only record of it.
                        </p>
                        <HistoryPanel
                          records={card.records}
                          error={card.recordsError}
                          onGoBack={(rec) => void goBackToVersion(id, rec)}
                        />
                      </div>
                    ) : null}
                  </li>
                );
              })}
            </ul>
          )}
        </section>
      </div>

      {/* One page-level confirm, the console's single dialog idiom. A removal is
          irreversible on this box and names exactly what leaves with the pack. */}
      <ConfirmModal
        open={confirmingRemoval !== null}
        onOpenChange={(open) => {
          if (!open) setConfirmingRemoval(null);
        }}
        title={confirmingRemoval ? `Uninstall ${confirmingRemoval}?` : "Uninstall extension?"}
        description="Its bundle, every row authored into its collections, and its install history are removed together, in one transaction. Nothing on this box can bring them back."
        confirmLabel="Uninstall permanently"
        cancelLabel="Keep it"
        destructive
        onConfirm={() => {
          if (confirmingRemoval) void uninstall(confirmingRemoval);
        }}
      />

      <Toaster />
    </div>
  );
}
