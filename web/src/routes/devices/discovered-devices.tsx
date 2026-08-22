import { useMemo } from "react";
import { PageRenderer, type ActionHandler } from "@/renderer";
import { deviceFacts, type Device, type Entity } from "@/api";
import discoveredDevicesDoc from "./discovered-devices.uis.json";
import { formatAge } from "@/lib/format-age";
import { describePort, portsSearchText, portsState } from "./open-ports";

/**
 * The discovered-devices inventory — Discovery spec §7's "All devices" table.
 *
 * # Authored as `ui-schema/1`, not as React
 *
 * `discovered-devices.uis.json` is the whole table: eleven content columns, the
 * three enum filters, the narrowed search, the classified empty state and the
 * per-row decisions. This file is the data half — the projection each column
 * binds to, and the seams the document's verbs land on — exactly as
 * `adopted-devices.tsx` is for its own panel.
 *
 * It took four iterations to become authorable, and each gap was found by
 * attempting it rather than by reading the grammar: per-row controls and widget
 * cells (UIS-071a), column search/filters/alignment (UIS-071b), a classified
 * empty state and a loading state (UIS-071c), per-row badge tone (UIS-078) and
 * explanatory hover text (UIS-079).
 *
 * # The projection exists because a `cell` is a BINDING, not a function
 *
 * Every string a column shows is computed HERE and bound there. That is not a
 * workaround — it is the boundary the format draws, and it is the reason this
 * page becomes portable into a pack: the document carries layout and copy, and
 * the host carries the one thing a pack cannot ship, which is code.
 *
 * The projection is deliberately TOTAL — every row gets every field, including
 * the ones a given row will not display. A field computed only sometimes is a
 * binding that resolves to nothing on the rows that omitted it, and an absent
 * binding renders as empty rather than as an error, which is how a column goes
 * quietly blank for a subset of a fleet.
 *
 * # The renderer limit this hit, and where it was fixed
 *
 * The store used to seed ONCE at mount and never re-sync, so this route — which
 * fetches after first paint — held the empty seed forever and had to REMOUNT the
 * renderer to escape it. That cost the table's search, filters, sort and `$ui`
 * selection every time a read resolved.
 *
 * Fixed in the renderer rather than worked around here: a new `initialResource`
 * identity now re-seeds the store unless the page has edited it. This file
 * carries no remount key as a result — which is the point, since three hosts had
 * been carrying one.
 *
 * # The one renderer gap still open, and what it cost
 *
 * G-R6, the one renderer gap still open: `ui-schema/1` has no
 * layout-neutral container. The
 * only children-bearing widget is `section`, which emits a landmark with a
 * heading affordance and `gap-4` — wrong inside a table cell. It forced two
 * choices here, both recorded rather than worked around silently:
 *
 *  1. **Hardware is ONE line** (`Proxmox · bc:24:11:1a:2b:3c`) where the React
 *     cell stacked the vendor over a monospaced MAC. Information preserved,
 *     presentation lost.
 *  2. **The three decisions are three COLUMNS**, not one action cell, because
 *     three buttons cannot share a cell without a container. Each carries its
 *     own `visibleIf`, so a row still offers exactly the decisions it should —
 *     an adopted row offers none, and only an inherited age offers Retire.
 */

/** The `msg:` catalog the document's references resolve against. Copy lives here
 * so the document stays layout; a pack shipping this page supplies its own. */
export const DISCOVERED_MESSAGES: Record<string, string> = {
  "msg:dev.col.name": "Device",
  "msg:dev.col.address": "Address",
  "msg:dev.col.hardware": "Hardware",
  "msg:dev.col.model": "Model",
  "msg:dev.col.class": "Class",
  "msg:dev.col.relay": "Reported by",
  "msg:dev.col.entities": "Entities",
  "msg:dev.col.ports": "Open ports",
  "msg:dev.col.firstSeen": "First seen",
  "msg:dev.col.lastSeen": "Last seen",
  "msg:dev.col.status": "Status",
  "msg:dev.col.retire": "",
  "msg:dev.col.ignore": "",
  "msg:dev.col.adopt": "",
  "msg:dev.filter.class": "Device class",
  "msg:dev.filter.relay": "Reported by",
  "msg:dev.filter.decision": "Decision",
  "msg:dev.act.retire": "Retire age",
  "msg:dev.act.ignore": "Ignore",
  "msg:dev.act.unignore": "Un-ignore",
  "msg:dev.act.adopt": "Adopt",
  "msg:dev.search.placeholder": "Name, address, vendor, MAC, model, class or port",
  "msg:dev.detail.empty": "",
  // The three-state port column (api/1: absent is not empty).
  "msg:dev.ports.unscanned": "Not scanned",
  "msg:dev.ports.unscannedHelp":
    "Nothing has scanned this device, so nothing is known about its ports. This is not the same as " +
    "having no ports open — only a scan can assert that. Start one from the extension that owns scanning.",
  "msg:dev.ports.none": "None open",
  "msg:dev.ports.noneHelp": "A scan looked and found nothing listening. This is a result, not an absence of one.",
  "msg:dev.ports.openHelp":
    "What typically answers on these ports. Evidence, never a classification — an unlisted port shows " +
    "its number and no claim.",
  "msg:dev.firstSeen.inheritedHelp":
    "Inherited, not observed. This age was copied from an older build's column, where it had been written " +
    "from the reporting relay's own unattested clock — so it is an estimate rather than an instant this " +
    "deployment watched, it is not comparable with an observed one, and it can overstate the device's age " +
    "as well as understate it. Retire it to have the next report plant a real one.",
  // The empty state IS the classification: four problems that once wore one sentence.
  "msg:dev.empty.noRelay":
    "No relay is connected, so nothing is discovering. An empty list here says nothing about your network.",
  "msg:dev.empty.blind": "This console could not read relay health, so it cannot say whether anything is discovering.",
  "msg:dev.empty.unreadable": "The device list could not be read.",
  "msg:dev.empty.searching": "A relay is connected and has reported nothing yet.",
};

/** Whether a device's `first_seen` is an instant this deployment OBSERVED.
 *
 * Only `planted` is. `adopted` was copied from a pre-ledger column at an upgrade,
 * written off the reporting relay's own unattested clock; `unrecorded` is a row
 * from a build that did not record which of the two it was, so it cannot be shown
 * not to be the second. Both get the same caution. */
export function isObservedAge(device: Device): boolean {
  return device.first_seen_origin === "planted";
}

function age(atMs: number | undefined): string {
  if (atMs === undefined || atMs <= 0) return "—";
  return formatAge(Math.max(0, Date.now() - atMs));
}

/** One row as the document binds it. Every field is present on every row — see
 * the module header on why the projection is total. */
export interface DiscoveredRow extends Record<string, unknown> {
  id: string;
  name: string;
  address_display: string;
  hardware_display: string;
  hardware_search: string;
  model_display: string;
  device_class: string;
  relay_id: string;
  entity_count: number;
  ports_state: "unscanned" | "none" | "open";
  ports_display: string;
  ports_search: string;
  first_seen_kind: "observed" | "inherited";
  first_seen_display: string;
  first_seen_sort: number;
  last_seen_display: string;
  status: string;
  status_tone: string;
  ignored: boolean;
  can_decide: boolean;
  can_retire: boolean;
}

export function toDiscoveredRow(device: Device, entities: Entity[]): DiscoveredRow {
  const ports = portsState(device.open_ports);
  // Through deviceFacts, not off the member: an address the deployment reported
  // as a LABEL is still the address an operator needs to find the box.
  const facts = deviceFacts(device);
  const observed = isObservedAge(device);
  const firstAge = age(device.first_seen);
  return {
    id: device.id,
    name: device.name,
    address_display: facts.address ?? "—",
    // ONE line — see the module header's G-R6 note.
    hardware_display: [device.vendor, device.mac].filter(Boolean).join(" · ") || "—",
    hardware_search: [device.vendor, device.mac].filter(Boolean).join(" ") || "—",
    model_display: facts.model ?? "—",
    device_class: device.device_class,
    relay_id: device.relay_id,
    entity_count: entities.length,
    ports_state: ports.kind,
    ports_display: ports.kind === "open" ? ports.ports.map(describePort).join(", ") : "",
    ports_search: portsSearchText(device.open_ports),
    // An age with no origin is not marked inherited: an absent origin belongs to
    // an absent age, and tilde-marking an em dash would invent a doubt.
    first_seen_kind: observed || firstAge === "—" ? "observed" : "inherited",
    first_seen_display: observed || firstAge === "—" ? firstAge : `~${firstAge}`,
    first_seen_sort: device.first_seen ?? 0,
    last_seen_display: age(device.last_seen),
    // Adoption SUPERSEDES ignoring: a device this deployment actively polls is
    // not "set aside", whatever the other flag says.
    status: device.adopted ? "Adopted" : device.ignored ? "Ignored" : "Discovered",
    status_tone: device.adopted ? "positive" : device.ignored ? "warning" : "neutral",
    ignored: device.ignored,
    // Every undecided row carries both decisions (spec §7) and an ignored row
    // carries the reversal; an adopted row carries neither.
    can_decide: !device.adopted,
    // Retire is offered only where the age is INHERITED — on an observed instant
    // it would discard a fact this deployment actually watched.
    can_retire: device.first_seen !== undefined && !observed,
  };
}

export interface DiscoveredDevicesProps {
  rows: DiscoveredRow[];
  loading: boolean;
  /** Which emptiness this is, when there are no rows — the document switches on
   * it. `searching` is the default arm. */
  emptyKind: string;
  onSelect: (id: string | null) => void;
  onAdopt: (id: string) => void;
  onIgnore: (id: string) => void;
  onUnignore: (id: string) => void;
  onRetireAge: (id: string) => void;
}

export function DiscoveredDevices({
  rows,
  loading,
  emptyKind,
  onSelect,
  onAdopt,
  onIgnore,
  onUnignore,
  onRetireAge,
}: DiscoveredDevicesProps) {
  const data = useMemo(
    () => ({ devices: rows, loading, empty_kind: emptyKind }),
    [rows, loading, emptyKind],
  );

  const handler: ActionHandler = useMemo(
    () => ({
      callAction: (name, params) => {
        const id = String((params as { device_id?: unknown } | undefined)?.device_id ?? "");
        if (!id) return;
        if (name === "adopt-device") onAdopt(id);
        else if (name === "ignore-device") onIgnore(id);
        else if (name === "unignore-device") onUnignore(id);
        else if (name === "retire-first-seen") onRetireAge(id);
      },
    }),
    [onAdopt, onIgnore, onUnignore, onRetireAge],
  );

  return (
    <PageRenderer
      doc={discoveredDevicesDoc as unknown as Record<string, unknown>}
      data={data}
      messages={DISCOVERED_MESSAGES}
      handler={handler}
      onUiChange={(ui) => onSelect((ui.selected as string | undefined) ?? null)}
    />
  );
}
