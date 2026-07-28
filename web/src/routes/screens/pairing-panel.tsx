import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Button,
  ConfirmModal,
  DataTable,
  EmptyState,
  FormField,
  Modal,
  toast,
  type ColumnDef,
} from "@/components/kit";
import {
  ApiError,
  collectPages,
  etagForRevision,
  type PairingCodeResult,
  type Screen,
  type ScopeNode,
  type WaiveoApi,
} from "@/api";

/**
 * The operator half of screen pairing: the screen identity rows (the row a
 * `screen_id` names — data-model/1 DAT-004a, served by /api/v1/screens) and
 * the pairing-code flow that turns a physical display into one of them.
 *
 * The flow this panel drives end to end:
 *
 *   1. "Pair a new screen" — name the screen, choose the site/group it lives
 *      under, and the panel creates the identity row.
 *   2. The panel immediately asks the server for a pairing code: the server
 *      mints a one-time grant BOUND to the new row and delivers it to the
 *      site's relay inside the next signed desired-state generation; the code
 *      shown here encodes the relay's dial address, the grant's selector, and
 *      the relay's certificate commitment — everything the screen needs.
 *   3. The operator reads the code into the screen's own pairing setup. The
 *      screen redeems it AT THE RELAY and receives this row's id as its
 *      identity — which is what joins the physical display to everything
 *      scheduled against the row.
 *
 * What this panel deliberately does NOT show: a "paired" tick. Redemption
 *  happens at the relay, and the relay's redemption report upstream is not
 * built yet — so the panel reports what the app actually knows (the code, its
 * expiry) and never a confirmation it has no evidence for.
 */

/** How the panel's pairing modal is currently occupied. */
type PairDialog =
  | { kind: "closed" }
  | { kind: "form" }
  | { kind: "code"; screenName: string; result: PairingCodeResult };

export interface PairingPanelProps {
  api: WaiveoApi;
  /** The site/group nodes a new screen row may be placed under — the same
   * candidates the route's node manager offers (a screen row may be placed at
   * a node of any kind; sites and groups are the ones an operator recognizes
   * as "where the screen is"). */
  parents: ScopeNode[];
  /** Every known scope node, for rendering a row's placement by name. */
  nodeNames: Map<string, string>;
}

/** Format an epoch-ms instant as a local wall-clock time for the expiry line. */
function localTime(ms: number): string {
  return new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function problemMessage(err: unknown): string {
  if (err instanceof ApiError) return err.detail ?? err.code;
  return "the service is unreachable.";
}

export function PairingPanel({ api, parents, nodeNames }: PairingPanelProps) {
  const [rows, setRows] = useState<Screen[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [dialog, setDialog] = useState<PairDialog>({ kind: "closed" });
  const [name, setName] = useState("");
  const [parentId, setParentId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [removing, setRemoving] = useState<Screen | null>(null);

  const activeParent = parentId ?? parents[0]?.id ?? null;

  const load = useCallback(async () => {
    try {
      const list = await collectPages<Screen>((cursor) => api.screens.list({ cursor }));
      setRows(list);
      setLoadError(null);
    } catch (err) {
      setRows([]);
      setLoadError(problemMessage(err));
    }
  }, [api]);

  useEffect(() => {
    void load();
  }, [load]);

  // Create the identity row, then immediately issue its pairing code — the
  // two-step the server intentionally keeps separate (a row is a resource; a
  // code is a credential issuance against it). If the create lands but the
  // code issuance fails, the row exists and the per-row "Pairing code" button
  // is the retry path — say so rather than pretending nothing happened.
  const createAndPair = useCallback(async () => {
    if (busy) return;
    const trimmed = name.trim();
    if (!trimmed) {
      toast.error("Name the screen first.");
      return;
    }
    if (!activeParent) {
      toast.error("Add a site before pairing a screen — a screen must live under a site or group.");
      return;
    }
    setBusy(true);
    try {
      const created = await api.screens.create({ name: trimmed, scope_node: activeParent });
      try {
        const result = await api.screens.issuePairingCode(created.data.id);
        setDialog({ kind: "code", screenName: created.data.name, result });
      } catch (err) {
        setDialog({ kind: "closed" });
        toast.error(
          `${created.data.name} was created, but no pairing code could be issued: ${problemMessage(err)} — use its Pairing code button to retry.`,
        );
      }
      setName("");
      await load();
    } catch (err) {
      toast.error(`Couldn't create the screen: ${problemMessage(err)}`);
    } finally {
      setBusy(false);
    }
  }, [api, activeParent, busy, load, name]);

  const issueFor = useCallback(
    async (row: Screen) => {
      try {
        const result = await api.screens.issuePairingCode(row.id);
        setDialog({ kind: "code", screenName: row.name, result });
      } catch (err) {
        toast.error(`Couldn't issue a pairing code: ${problemMessage(err)}`);
      }
    },
    [api],
  );

  const removeRow = useCallback(async () => {
    if (!removing) return;
    try {
      await api.screens.remove(removing.id, etagForRevision(removing.revision));
      toast.success(`Removed ${removing.name}`);
      await load();
    } catch (err) {
      toast.error(`Couldn't remove the screen: ${problemMessage(err)}`);
    } finally {
      setRemoving(null);
    }
  }, [api, load, removing]);

  const columns = useMemo<ColumnDef<Screen>[]>(
    () => [
      { accessorKey: "name", header: "Name" },
      {
        id: "placement",
        header: "Placed under",
        cell: ({ row }) => nodeNames.get(row.original.scope_node) ?? row.original.scope_node,
      },
    ],
    [nodeNames],
  );

  return (
    <section aria-label="Displays" className="flex flex-col gap-3">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold">Displays</h2>
          <p className="text-sm text-muted-foreground">
            The screens content is scheduled against. Pair a physical display by entering its
            one-time code in the screen's own setup.
          </p>
        </div>
        <Button onClick={() => setDialog({ kind: "form" })}>Pair a new screen</Button>
      </div>

      {loadError ? (
        <p role="alert" className="text-sm text-[color:var(--wv-err)]">
          Couldn't load displays — {loadError}
        </p>
      ) : null}

      <DataTable<Screen>
        columns={columns}
        data={rows ?? []}
        label="Displays"
        loading={rows === null}
        emptyState={
          <EmptyState
            title="No displays yet"
            description="Pair a new screen to give a physical display an identity content can be scheduled against."
          />
        }
        rowActions={(row) => (
          <div className="flex justify-end gap-2">
            <Button size="sm" variant="outline" onClick={() => void issueFor(row)}>
              Pairing code
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setRemoving(row)}>
              Remove
            </Button>
          </div>
        )}
      />

      {/* Step 1: name + placement. */}
      <Modal
        title="Pair a new screen"
        description="Create the screen, then read its one-time pairing code into the display's setup."
        open={dialog.kind === "form"}
        onOpenChange={(open) => {
          if (!open) setDialog({ kind: "closed" });
        }}
        footer={
          <>
            <Button variant="ghost" onClick={() => setDialog({ kind: "closed" })}>
              Cancel
            </Button>
            <Button onClick={() => void createAndPair()} disabled={busy}>
              {busy ? "Creating…" : "Create & get code"}
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-4">
          <FormField label="Screen name">
            {(field) => (
              <input
                {...field}
                className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g. Lobby TV"
              />
            )}
          </FormField>
          <FormField label="Placed under">
            {(field) => (
              <select
                {...field}
                className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                value={activeParent ?? ""}
                onChange={(e) => setParentId(e.target.value)}
              >
                {parents.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
            )}
          </FormField>
        </div>
      </Modal>

      {/* Step 2: the code itself (or the honest reason there is none). */}
      <Modal
        title={dialog.kind === "code" ? `Pairing code — ${dialog.screenName}` : "Pairing code"}
        open={dialog.kind === "code"}
        onOpenChange={(open) => {
          if (!open) setDialog({ kind: "closed" });
        }}
        footer={
          <Button onClick={() => setDialog({ kind: "closed" })}>Done</Button>
        }
      >
        {dialog.kind === "code" ? (
          dialog.result.pairing_code ? (
            <div className="flex flex-col gap-3">
              <p className="text-sm text-muted-foreground">
                On the display, open the screen's pairing setup and enter:
              </p>
              <p
                data-testid="pairing-code"
                className="select-all break-all rounded-card border border-border bg-muted/30 p-4 text-center font-mono text-lg tracking-wider"
              >
                {dialog.result.pairing_code}
              </p>
              <p className="text-sm text-muted-foreground">
                The code is one-time and expires at {localTime(dialog.result.expires_at)}. It was
                also delivered to this site's relay; the screen confirms on its own display once
                pairing completes — this console is not yet told when a code is redeemed.
              </p>
            </div>
          ) : (
            <div className="flex flex-col gap-3">
              <p className="text-sm">
                The pairing grant was created and will reach the site's relay, but no code could
                be shown: {dialog.result.code_unavailable_reason ?? "no reason reported."}
              </p>
              <p className="text-sm text-muted-foreground">
                Once a relay is connected, use the screen's Pairing code button to issue a fresh
                code.
              </p>
            </div>
          )
        ) : null}
      </Modal>

      <ConfirmModal
        title={removing ? `Remove ${removing.name}?` : "Remove screen?"}
        description="The screen row and anything scheduled directly against it stop being delivered. A display already paired with this identity loses it."
        open={removing !== null}
        onOpenChange={(open) => {
          if (!open) setRemoving(null);
        }}
        confirmLabel="Remove"
        destructive
        onConfirm={() => void removeRow()}
      />
    </section>
  );
}
