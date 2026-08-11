import { useState } from "react";
import { ChevronDown, ChevronRight, Cpu, MonitorPlay, Network, Radio } from "lucide-react";
import { StatCard, StatusBadge, type Status } from "@/components/kit";
import type { RelayHealth } from "@/api";
import {
  MISSING_DEVICE_REASONS,
  type Discovery,
} from "./discovery";

/**
 * The Discovery panel — the top of the Devices page, and the whole answer to
 * "how does a device get from being on the network to being controllable?"
 *
 * It renders five things in the order an operator needs them:
 *
 *   1. WHAT STATE DISCOVERY IS IN, as one headline sentence with a status chip.
 *      Seven distinct states (see ./discovery), never one empty table for all of
 *      them. This is the panel's reason to exist.
 *   2. THE COUNTS, so the headline can be checked against numbers.
 *   3. WHO IS REPORTING — the connected relays, by id and dial address, with how
 *      many devices each one is currently accounting for. "Which relay found
 *      this" is the first question after "why is this missing".
 *   4. WHY A DEVICE MIGHT BE MISSING — the five real paths, collapsed by default
 *      so it is available without shouting.
 *   5. WHAT ADOPT ACTUALLY DOES, stated before the operator presses it rather
 *      than only inside the confirm dialog.
 *
 * Nothing here can START a sweep, and the panel says so in as many words: the
 * absence of a Scan button is otherwise read as a missing feature rather than as
 * the architecture. Relays discover; this console reads.
 */

/** Discovery state → the kit's status vocabulary.
 *
 * `all-adopted` is `ok` and `searching` is `pending`, and the difference is the
 * point: the first is a finished, healthy state and the second is an open
 * question. `blind` is `warn` rather than `error` — the console not being able
 * to read health is not evidence that anything is broken, and spending the error
 * colour on an uncertainty is how a status chip stops being believed. */
const DISCOVERY_STATUS: Record<Discovery["kind"], Status> = {
  loading: "pending",
  unreadable: "error",
  "no-relay": "error",
  blind: "warn",
  searching: "pending",
  "all-adopted": "ok",
  candidates: "warn",
};

const DISCOVERY_LABEL: Record<Discovery["kind"], string> = {
  loading: "Reading",
  unreadable: "Unreadable",
  "no-relay": "Not running",
  blind: "Unknown",
  searching: "Searching",
  "all-adopted": "All adopted",
  candidates: "Decisions waiting",
};

export interface DiscoveryPanelProps {
  discovery: Discovery;
  /** The connected relays, or `null` when health could not be read. */
  relays: RelayHealth[] | null;
  /** How many devices each relay is currently accounting for, by relay id. */
  devicesByRelay: Map<string, number>;
  /** How many entities the deployment's devices expose in total. */
  entityCount: number;
}

export function DiscoveryPanel({
  discovery,
  relays,
  devicesByRelay,
  entityCount,
}: DiscoveryPanelProps) {
  const [showMissing, setShowMissing] = useState(false);

  return (
    <section aria-label="Discovery status" className="flex flex-col gap-4">
      {/* (1) The state, as a sentence. `role="status"` so a screen reader is
          told when a refresh changes it — the headline IS the page's news. */}
      <div
        data-slot="discovery-state"
        data-kind={discovery.kind}
        role="status"
        className="flex flex-col gap-2 rounded-card border border-border bg-card p-4"
      >
        <div className="flex flex-wrap items-center gap-3">
          <StatusBadge status={DISCOVERY_STATUS[discovery.kind]}>
            {DISCOVERY_LABEL[discovery.kind]}
          </StatusBadge>
          <p className="font-display text-[17px] font-semibold">{discovery.headline}</p>
        </div>
        <p className="text-sm text-muted-foreground">{discovery.detail}</p>
        {discovery.caveat ? (
          <p data-slot="discovery-caveat" className="text-sm text-[color:var(--wv-warn)]">
            {discovery.caveat}
          </p>
        ) : null}
      </div>

      {/* (2) The counts. */}
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard
          label="Discovered"
          value={String(discovery.found)}
          icon={Radio}
          hint="Reported by a relay"
        />
        <StatCard
          label="Adopted"
          value={String(discovery.adopted)}
          icon={MonitorPlay}
          hint={
            discovery.pending > 0
              ? `${discovery.pending} awaiting a decision`
              : "Nothing awaiting a decision"
          }
        />
        <StatCard
          label="Relays reporting"
          value={discovery.relayCount === null ? "—" : String(discovery.relayCount)}
          icon={Network}
          hint={discovery.relayCount === null ? "Health not readable" : "Connected now"}
        />
        <StatCard
          label="Entities"
          value={String(entityCount)}
          icon={Cpu}
          hint="Commands are addressed here"
        />
      </div>

      {/* (3) Who is reporting. Rendered only when health was readable — an empty
          table would otherwise read as "no relays", which is a claim this
          console is not entitled to make when it was refused the answer. */}
      {relays !== null ? (
        <div className="flex flex-col gap-2">
          <h3 className="text-sm font-semibold">Which relays are reporting</h3>
          {/* Said rather than omitted. "When did it last sweep" is the obvious
              next question and this console cannot answer it: api/1 publishes no
              per-relay sweep timestamp, only the current connected set. Leaving
              the field out reads as an oversight; saying so tells an operator
              what the page's silence means and what IS load-bearing instead. */}
          <p data-slot="sweep-time-gap" className="text-xs text-muted-foreground">
            No sweep timestamp is published — api/1 reports which relays are connected now, not
            when each last swept. A connected relay is continuously reporting its full current
            view, so its presence here is the freshness signal; a relay that stopped sweeping
            without disconnecting would be indistinguishable from one whose network is empty.
          </p>
          {relays.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No relay is connected. Nothing is discovering, and nothing can be commanded — every
              command is delivered over a relay's own connection.
            </p>
          ) : (
            <ul className="flex flex-col gap-2">
              {relays.map((relay) => (
                <li
                  key={relay.relay_id}
                  data-slot="discovery-relay"
                  className="flex flex-wrap items-center justify-between gap-2 rounded-input border border-border px-3 py-2 text-sm"
                >
                  <span className="font-mono text-xs break-all">{relay.relay_id}</span>
                  <span className="text-muted-foreground">{relay.address}</span>
                  <span className="text-muted-foreground">
                    {devicesByRelay.get(relay.relay_id) ?? 0} device
                    {(devicesByRelay.get(relay.relay_id) ?? 0) === 1 ? "" : "s"}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>
      ) : null}

      {/* (4) Why a device might be missing. */}
      <div className="flex flex-col gap-2">
        <button
          type="button"
          aria-expanded={showMissing}
          onClick={() => setShowMissing((v) => !v)}
          className="wv-touch flex w-fit items-center gap-2 rounded-input px-2 text-sm font-medium text-muted-foreground outline-none transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
        >
          {showMissing ? (
            <ChevronDown className="size-4 shrink-0" aria-hidden="true" />
          ) : (
            <ChevronRight className="size-4 shrink-0" aria-hidden="true" />
          )}
          Why is a device I expected not listed?
        </button>
        {showMissing ? (
          <dl
            data-slot="missing-reasons"
            className="grid gap-3 rounded-card border border-border bg-card p-4 text-sm sm:grid-cols-2"
          >
            {MISSING_DEVICE_REASONS.map((reason) => (
              <div key={reason.title} className="flex flex-col gap-1">
                <dt className="font-medium">{reason.title}</dt>
                <dd className="text-muted-foreground">{reason.detail}</dd>
              </div>
            ))}
          </dl>
        ) : null}
      </div>

      {/* (5) The two sentences the whole page turns on. */}
      <p className="text-sm text-muted-foreground">
        <strong className="font-medium text-foreground">Adopt</strong> is the decision to make a
        discovered device this deployment&apos;s: it writes a durable adoption record, ships it to
        the device&apos;s relay to poll, and is what makes commands to that device&apos;s entities
        resolve at all. Until then a device is only a sighting.{" "}
        <strong className="font-medium text-foreground">Refresh</strong> re-reads what the relays
        have already reported — each relay sweeps its own network on its own schedule, so there is
        no scan to start from here.
      </p>
    </section>
  );
}
