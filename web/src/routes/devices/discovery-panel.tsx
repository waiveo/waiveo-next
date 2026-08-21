import { useState } from "react";
import { Link } from "react-router";
import { ChevronDown, ChevronRight, Cpu, EyeOff, MonitorPlay, Network, Radio } from "lucide-react";
import { StatCard, StatusBadge, type Status } from "@/components/kit";
import { formatAge } from "@/lib/format-age";
import type { DiscoveryScanStatus, RelayHealth } from "@/api";
import {
  MISSING_DEVICE_REASONS,
  type Discovery,
} from "./discovery";
import type { ScanOwner } from "./scan-owner";
import type { Recognizer } from "./recognizers";

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
 * Nothing here can START a sweep, and the panel says so — but it now also says
 * WHO CAN. The earlier wording ("there is no scan to start from here") was true
 * about this page and false about the deployment: scanning is owned by an
 * extension, `waiveo/discovery` declares a `scan-now` action gated on
 * `discovery.scan`, and invoking it drives a real ARP sweep and port scan. An
 * operator reading the old sentence concluded the platform could not scan on
 * demand, while the one operational control in Discovery sat three clicks away
 * under "Extensions" with nothing pointing at it.
 *
 * The absence of a Scan BUTTON here is still the architecture — core does not
 * perform pack work — but the absence of a POINTER was just a gap. Relays
 * discover; this console reads; an extension decides when to look.
 */

/** What a relay's scan engine is doing, as one short phrase.
 *
 * Four answers, and the first two are the ones that matter. A relay that is
 * CONNECTED but has never reported a scan is not the same as one that swept a
 * minute ago, and until this was rendered they looked identical on this page —
 * which is precisely the failure the paragraph above used to describe and
 * attribute to a missing contract member.
 *
 * `undefined` means this relay is connected and has reported no scan state at
 * all; `null` for the whole map means the read failed and nothing is known,
 * which is the caller's distinction to make, not this function's. */
function describeScan(status: DiscoveryScanStatus | undefined): string {
  if (status === undefined) return "no scan reported";
  if (status.state === "scanning") {
    return status.reason === "operator" ? "scanning now (operator)" : "scanning now (scheduled)";
  }
  if (!status.finished_at) return "idle";
  return `last swept ${formatAge(Math.max(0, Date.now() - status.finished_at))} ago`;
}

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
  /** Each relay's scan-engine state by relay id, or `null` when the scan-status
   * read failed. Null is not an empty map: empty would say every connected relay
   * has reported no scan, and a failed read says this console does not know. */
  scanByRelay: Map<string, DiscoveryScanStatus> | null;
  /** The installed extensions that teach device RECOGNITION, or `null` when the
   * pack registry could not be read. Empty is a real and consequential state —
   * every host found, none recognised — and only distinguishable from a failed
   * read by keeping the two apart. */
  recognizers: Recognizer[] | null;
  /** The installed extensions that can start a scan, or `null` when the pack
   * registry could not be read. Empty and null are different claims: empty says
   * this deployment has nothing that scans on demand, null says this console
   * does not know — and only the first is the page's to assert. */
  scanOwners: ScanOwner[] | null;
}

export function DiscoveryPanel({
  discovery,
  relays,
  devicesByRelay,
  entityCount,
  scanByRelay,
  recognizers,
  scanOwners,
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
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
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
        {/* Set-aside is a COUNT that is always rendered, including at zero.
            A tile that appears only when non-zero would make the decision
            discoverable only after it had already been made — and the reason
            this count is on the page at all is that an ignore must never be a
            hidden trash can (discovery spec §7). Zero is also the answer to
            "have I set anything aside and forgotten about it?", which is the
            question the tile exists to keep answerable. */}
        <StatCard
          label="Set aside"
          value={String(discovery.ignored)}
          icon={EyeOff}
          hint={
            discovery.ignored > 0
              ? "Still discovered, still reported"
              : "Nothing has been ignored"
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
          {/* "When did it last sweep" is the obvious next question, and this
              console could not answer it — not because api/1 withheld the
              answer, but because nothing here read it. `/discovery/scan-status`
              publishes each relay's engine state, the scan it last ran, and
              when that scan started and finished. The paragraph that used to
              stand here explained the absence as a contract gap; the contract
              had closed it and the console had not noticed.
          */}

          {/* This paragraph used to say "No sweep timestamp is published", and
              named the consequence precisely: a relay that stopped sweeping
              without disconnecting would be indistinguishable from one whose
              network is empty. api/1 DOES publish it — `/discovery/scan-status`
              carries each relay's engine state, the scan it last ran and when
              that scan started and finished — and nothing in this console read
              it. The gap the paragraph described was real; the reason it gave
              had stopped being true. */}
          <p data-slot="sweep-time-gap" className="text-xs text-muted-foreground">
            Each relay sweeps its own network on its own schedule, and reports what its scan
            engine is doing. A connected relay that had quietly stopped sweeping would otherwise
            be indistinguishable from one whose network is empty — the state below is what tells
            them apart.
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
                  <span data-slot="relay-scan-state" className="text-muted-foreground">
                    {/* `scanByRelay === null` is checked EXPLICITLY rather than
                        ridden through with `?.`, which would hand describeScan
                        an undefined and render "no scan reported" — collapsing a
                        read this console was refused into a claim about the
                        relay. That is the same absent-vs-empty collapse this
                        panel already distinguishes for relay health, and the
                        optional chain reintroduced it silently. */}
                    {scanByRelay === null
                      ? "scan state unavailable"
                      : describeScan(scanByRelay.get(relay.relay_id))}
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
        <strong className="font-medium text-foreground">Ignore</strong> is the other decision, and
        it is deliberately much smaller: it sets a device aside as something this deployment does
        not care about, and reaches no relay at all. The device is still discovered, still reported
        and still counted — it is marked, not removed, and un-ignoring it on the row puts it back.
        Nothing stops looking for a device because it was ignored.{" "}
        <strong className="font-medium text-foreground">Refresh</strong> re-reads what the relays
        have already reported; it starts nothing.
      </p>

      {/* (6) WHY EVERYTHING READS "UNCLASSIFIED". Discovery enumerates the
          network pattern-blind and knows what none of it is; recognising a KIND
          of device is an extension's contribution, and core declares no pattern
          of its own on purpose. So a deployment with no such pack finds
          everything and classifies almost nothing — which is the dev box today,
          fifty of sixty-three rows reading `unclassified` with the page offering
          no reason. Unexplained, that reads as classification being broken. It
          is absent by configuration, and one install from working. */}
      <div
        data-slot="recognition"
        className="flex flex-col gap-1 rounded-card border border-border bg-card p-4 text-sm"
      >
        <h3 className="font-medium text-foreground">Recognising what a device is</h3>
        {recognizers === null ? (
          <p className="text-muted-foreground">
            Whether anything installed can recognise a device kind could not be read here — the
            extension registry did not answer, which is not the same as nothing being installed.
          </p>
        ) : recognizers.length === 0 ? (
          <p className="text-muted-foreground">
            Nothing installed recognises a device kind, so a device is found but not identified.
            Discovery enumerates the network without knowing what anything is; an extension
            supplies the patterns that say which host is a Roku, a printer, a speaker. Until one is
            installed, a row reading <code className="font-mono text-xs">unclassified</code> is the
            correct answer rather than a failure.
          </p>
        ) : (
          <ul className="flex flex-col gap-1">
            {recognizers.map((r) => (
              <li key={r.id} data-slot="recognizer">
                <span className="font-medium text-foreground">{r.displayName}</span>{" "}
                <span className="text-muted-foreground">
                  {r.disabled
                    ? `declares ${r.classes.join(", ")} — but it is disabled, so it recognises nothing today.`
                    : `recognises ${r.classes.join(", ")}.`}
                </span>
              </li>
            ))}
          </ul>
        )}
      </div>

      {/* (6) WHERE A SCAN COMES FROM. This page cannot start one — core does not
          perform an extension's work — but it used to leave that as "there is no
          scan to start", which reads as "this platform cannot scan on demand".
          It can: an extension owning `discovery.scan` asks core, core asks each
          relay, and the relay runs its ARP sweep and port scan. Saying so, with
          the way there, is the difference between an architecture explained and
          a capability hidden. */}
      <div
        data-slot="scan-owner"
        className="flex flex-col gap-1 rounded-card border border-border bg-card p-4 text-sm"
      >
        <h3 className="font-medium text-foreground">Starting a scan</h3>
        {scanOwners === null ? (
          <p className="text-muted-foreground">
            Passive discovery runs continuously and needs nothing started. Whether any installed
            extension can also scan on demand could not be read here — the extension registry did
            not answer, which is not the same as nothing being installed.
          </p>
        ) : scanOwners.length === 0 ? (
          <p className="text-muted-foreground">
            Passive discovery runs continuously and needs nothing started. Nothing installed can
            scan on demand: active scanning is owned by an extension holding{" "}
            <code className="font-mono text-xs">discovery.scan</code>, and this deployment has none.
          </p>
        ) : (
          <>
            <p className="text-muted-foreground">
              Passive discovery runs continuously. An on-demand sweep is an extension&apos;s
              decision, not this page&apos;s — it asks core, core asks every relay, and each relay
              scans the subnets its policy allows.
            </p>
            <ul className="flex flex-col gap-1">
              {scanOwners.map((owner) => (
                <li key={owner.id} data-slot="scan-owner-entry">
                  {owner.href !== null ? (
                    <Link
                      to={owner.href}
                      className="font-medium text-foreground underline underline-offset-4 outline-none hover:opacity-80 focus-visible:ring-2 focus-visible:ring-ring"
                    >
                      {owner.displayName}
                    </Link>
                  ) : (
                    <span className="font-medium text-foreground">{owner.displayName}</span>
                  )}{" "}
                  <span className="text-muted-foreground">
                    {owner.disabled
                      ? "— can scan, but it is disabled, so its pages are withdrawn. Enable it on the Extensions page."
                      : owner.href === null
                        ? "— can scan, but declares no page this console can open."
                        : "— can scan this deployment's networks now."}
                  </span>
                </li>
              ))}
            </ul>
          </>
        )}
      </div>
    </section>
  );
}
