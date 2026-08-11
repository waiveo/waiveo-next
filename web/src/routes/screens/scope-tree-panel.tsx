import { useCallback, useMemo, useState } from "react";
import { Building2, FolderTree, MapPin, MonitorPlay, Plus, type LucideIcon } from "lucide-react";
import { Button, ConfirmModal, EmptyState, FormField, KitIcon, Modal, toast } from "@/components/kit";
import {
  ApiError,
  etagForRevision,
  type ScopeNode,
  type ScopeNodeCreate,
  type WaiveoApi,
} from "@/api";

/**
 * The scope tree — org → site → group → screen — as something an operator can
 * actually build, not only read.
 *
 * # The bootstrap this closes
 *
 * A screen must be attached under a site or a group (DAT-003), and the console
 * refuses to add one when no candidate parent exists — correctly, since the
 * server would reject the body. But nothing in the console could CREATE a site
 * or a group either, so a box whose seeded tree had been deleted was stuck: the
 * only path to a first screen ran through a node no surface could author. This
 * panel is that path.
 *
 * # Why creating a site needs three more fields than creating a group
 *
 * DAT-031 makes `tz`, `lat` and `long` MANDATORY and non-null on a site-kind
 * node — a site is where the platform's notion of local time and sun position
 * comes from, and a scheduling engine that resolves dayparts per instant cannot
 * have a site with no clock. So the site form collects all three and the group
 * form collects none, and that asymmetry is the contract's, not a UI choice.
 *
 * A group MAY override the same three, but DAT-032 makes the override
 * all-three-or-none ("they resolve together as one unit", DAT-033). Rather than
 * offer three fields that are only valid in two of their eight combinations,
 * this panel creates groups with no override — a group then inherits its
 * nearest site's geo, which is what an operator means by "a group inside a
 * site" every time.
 *
 * # What this panel does not do
 *
 * It creates sites and groups and deletes empty nodes. Renaming and re-parenting
 * are deliberately absent: the screens page below already edits a screen node's
 * own name and time zone, and re-parenting a populated node moves every schedule
 * resolution beneath it (DAT-051's cascade runs on parent_id) — a fleet-wide
 * consequence that deserves its own reviewed affordance rather than a drag
 * handle bolted onto a tree.
 */

/** The glyph and prose for each node kind — the tree's only visual key. */
const KIND: Record<string, { icon: LucideIcon; label: string }> = {
  org: { icon: Building2, label: "Organisation" },
  site: { icon: MapPin, label: "Site" },
  group: { icon: FolderTree, label: "Group" },
  screen: { icon: MonitorPlay, label: "Screen" },
};

/** DAT-003's parent-kind table, the half this panel authors against: which kinds
 * may carry a group. (A site is only ever carried by the org.) */
const GROUP_PARENT_KINDS = new Set(["site", "group"]);

function problemMessage(err: unknown): string {
  if (err instanceof ApiError) {
    const fieldMsg = Object.values(err.fieldErrors)[0];
    return fieldMsg ?? err.detail ?? err.code;
  }
  return "the service is unreachable.";
}

/** One node plus its children, ready to render. */
interface TreeNode {
  node: ScopeNode;
  children: TreeNode[];
}

/** Build the forest from a flat node list, children ordered by name so the tree
 * is stable across reloads (the API pages in id order, which is creation order —
 * fine for a cursor, meaningless to an operator reading a hierarchy).
 *
 * Roots are nodes whose parent is not in the set, not only nodes with a null
 * parent: a tree missing its org row (or one the caller filtered) must still
 * render every node it was given rather than silently dropping a subtree. */
function buildForest(nodes: ScopeNode[]): TreeNode[] {
  const byId = new Map(nodes.map((n) => [n.id, { node: n, children: [] as TreeNode[] }]));
  const roots: TreeNode[] = [];
  for (const entry of byId.values()) {
    const parentId = entry.node.parent_id;
    const parent = parentId ? byId.get(parentId) : undefined;
    if (parent) parent.children.push(entry);
    else roots.push(entry);
  }
  const sort = (list: TreeNode[]) => {
    list.sort((a, b) => a.node.name.localeCompare(b.node.name));
    for (const child of list) sort(child.children);
  };
  sort(roots);
  return roots;
}

/** Which create form, if any, is open — and where the new node will hang. */
type Draft =
  | { kind: "closed" }
  | { kind: "site"; parent: ScopeNode }
  | { kind: "group"; parent: ScopeNode };

export interface ScopeTreePanelProps {
  api: WaiveoApi;
  /** Every scope node the console has loaded — the panel renders the whole tree
   * rather than one kind, because "where does this screen go?" is a question
   * only the shape answers. */
  nodes: ScopeNode[];
  /** Re-read the tree after a create or delete. */
  onChanged: () => void | Promise<void>;
}

export function ScopeTreePanel({ api, nodes, onChanged }: ScopeTreePanelProps) {
  const [draft, setDraft] = useState<Draft>({ kind: "closed" });
  const [deleting, setDeleting] = useState<ScopeNode | null>(null);
  const [busy, setBusy] = useState(false);
  const [name, setName] = useState("");
  const [tz, setTz] = useState("");
  const [lat, setLat] = useState("");
  const [long, setLong] = useState("");

  const forest = useMemo(() => buildForest(nodes), [nodes]);
  const org = useMemo(() => nodes.find((n) => n.kind === "org") ?? null, [nodes]);

  const openDraft = useCallback((next: Draft) => {
    setName("");
    setTz("");
    setLat("");
    setLong("");
    setDraft(next);
  }, []);

  const create = useCallback(async () => {
    if (draft.kind === "closed" || busy) return;
    const trimmed = name.trim();
    if (trimmed === "") {
      toast.error(`Name the ${draft.kind} first.`);
      return;
    }
    let body: ScopeNodeCreate;
    if (draft.kind === "site") {
      // DAT-031: all three or the server refuses with SCOPE_NODE_GEO_REQUIRED.
      // Checked here so the operator sees which field is missing rather than a
      // 422 naming only the first one.
      const latitude = Number(lat);
      const longitude = Number(long);
      if (tz.trim() === "") {
        toast.error("A site needs a time zone — it is where local time comes from.");
        return;
      }
      if (lat.trim() === "" || long.trim() === "" || Number.isNaN(latitude) || Number.isNaN(longitude)) {
        toast.error("A site needs a latitude and a longitude — sunrise and sunset resolve from them.");
        return;
      }
      body = {
        kind: "site",
        name: trimmed,
        parent_id: draft.parent.id,
        tz: tz.trim(),
        lat: latitude,
        long: longitude,
      };
    } else {
      body = { kind: "group", name: trimmed, parent_id: draft.parent.id };
    }
    setBusy(true);
    try {
      await api.scopeNodes.create(body);
      toast.success(`Added ${trimmed}`);
      setDraft({ kind: "closed" });
      await onChanged();
    } catch (err) {
      toast.error(`Couldn't add the ${draft.kind}: ${problemMessage(err)}`);
    } finally {
      setBusy(false);
    }
  }, [api, busy, draft, lat, long, name, onChanged, tz]);

  const remove = useCallback(async () => {
    if (!deleting) return;
    try {
      await api.scopeNodes.remove(deleting.id, etagForRevision(deleting.revision));
      toast.success(`Deleted ${deleting.name}`);
      await onChanged();
    } catch (err) {
      // The server refuses a node that still has children (SCOPE_NODE_NOT_EMPTY)
      // or anything placed at it — quote its own reason, which names the blocker.
      toast.error(`Couldn't delete ${deleting.name}: ${problemMessage(err)}`);
    } finally {
      setDeleting(null);
    }
  }, [api, deleting, onChanged]);

  const renderNode = (entry: TreeNode, depth: number) => {
    const { node } = entry;
    const meta = KIND[node.kind] ?? { icon: FolderTree, label: node.kind };
    const canCarryGroup = GROUP_PARENT_KINDS.has(node.kind);
    return (
      <li key={node.id} className="flex flex-col gap-1">
        <div
          data-slot="scope-node"
          data-kind={node.kind}
          className="flex flex-wrap items-center gap-2 rounded-input px-2 py-1.5 hover:bg-accent"
          style={{ marginInlineStart: `${depth * 1.25}rem` }}
        >
          <KitIcon icon={meta.icon} decorative className="size-4 shrink-0 text-muted-foreground" />
          <span className="text-sm font-medium">{node.name}</span>
          <span className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">
            {meta.label}
          </span>
          <span className="ml-auto flex items-center gap-1">
            {node.kind === "org" ? (
              <Button
                size="sm"
                variant="ghost"
                icon={Plus}
                onClick={() => openDraft({ kind: "site", parent: node })}
              >
                Add site
              </Button>
            ) : null}
            {canCarryGroup ? (
              <Button
                size="sm"
                variant="ghost"
                icon={Plus}
                onClick={() => openDraft({ kind: "group", parent: node })}
              >
                Add group
              </Button>
            ) : null}
            {/* The org node is undeletable by contract (DAT-022 — deleting it
                would destroy the tree's root identity), so no button offers it. */}
            {node.kind === "org" ? null : (
              <Button
                size="sm"
                variant="ghost"
                aria-label={`Delete ${node.name}`}
                onClick={() => setDeleting(node)}
              >
                Delete
              </Button>
            )}
          </span>
        </div>
        {entry.children.length > 0 ? (
          <ul className="flex flex-col gap-1">
            {entry.children.map((child) => renderNode(child, depth + 1))}
          </ul>
        ) : null}
      </li>
    );
  };

  return (
    <section aria-label="Scope tree" className="flex flex-col gap-3">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold">Scope tree</h2>
          <p className="text-sm text-muted-foreground">
            Organisation, sites, groups and screens. A schedule authored at a node governs every
            screen beneath it, so this shape is what decides which display shows what.
          </p>
        </div>
        {org ? (
          <Button
            variant="outline"
            icon={Plus}
            onClick={() => openDraft({ kind: "site", parent: org })}
          >
            Add site
          </Button>
        ) : null}
      </div>

      {forest.length === 0 ? (
        <EmptyState
          title="No scope tree"
          description="This deployment has no organisation node, so there is nowhere to hang a site. The organisation node is created when the box is first claimed."
          icon={Building2}
        />
      ) : (
        <ul data-slot="scope-tree" className="flex flex-col gap-1 rounded-card border border-border bg-card p-2">
          {forest.map((entry) => renderNode(entry, 0))}
        </ul>
      )}

      <Modal
        title={draft.kind === "site" ? "Add a site" : "Add a group"}
        description={
          draft.kind === "site"
            ? "A site is a physical location. Its time zone and coordinates are what local time and sunrise/sunset resolve from, so all three are required."
            : "A group gathers screens inside a site. It inherits the site's time zone and coordinates."
        }
        open={draft.kind !== "closed"}
        onOpenChange={(open) => {
          if (!open) setDraft({ kind: "closed" });
        }}
        footer={
          <>
            <Button variant="ghost" onClick={() => setDraft({ kind: "closed" })}>
              Cancel
            </Button>
            <Button onClick={() => void create()} disabled={busy}>
              {busy ? "Adding…" : "Add"}
            </Button>
          </>
        }
      >
        {draft.kind === "closed" ? null : (
          <div className="flex flex-col gap-4">
            <p className="text-sm text-muted-foreground">Under {draft.parent.name}</p>
            <FormField label="Name" required>
              {(field) => (
                <input
                  {...field}
                  className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder={draft.kind === "site" ? "e.g. The Hanger" : "e.g. West Wing"}
                />
              )}
            </FormField>
            {draft.kind === "site" ? (
              <>
                <FormField label="Time zone" required help="An IANA name, e.g. America/New_York.">
                  {(field) => (
                    <input
                      {...field}
                      className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                      value={tz}
                      onChange={(e) => setTz(e.target.value)}
                      placeholder="America/New_York"
                    />
                  )}
                </FormField>
                <div className="flex gap-3">
                  <FormField label="Latitude" required className="flex-1">
                    {(field) => (
                      <input
                        {...field}
                        type="number"
                        step="any"
                        className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                        value={lat}
                        onChange={(e) => setLat(e.target.value)}
                      />
                    )}
                  </FormField>
                  <FormField label="Longitude" required className="flex-1">
                    {(field) => (
                      <input
                        {...field}
                        type="number"
                        step="any"
                        className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                        value={long}
                        onChange={(e) => setLong(e.target.value)}
                      />
                    )}
                  </FormField>
                </div>
              </>
            ) : null}
          </div>
        )}
      </Modal>

      <ConfirmModal
        title={deleting ? `Delete ${deleting.name}?` : "Delete node?"}
        description="Only an empty node can be deleted — the server refuses one that still carries children or has screens, schedules or devices placed at it."
        open={deleting !== null}
        onOpenChange={(open) => {
          if (!open) setDeleting(null);
        }}
        confirmLabel="Delete"
        destructive
        onConfirm={() => void remove()}
      />
    </section>
  );
}
