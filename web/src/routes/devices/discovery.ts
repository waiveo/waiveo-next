// The Devices page's ONE judgement, extracted from the page so it can be tested
// without a DOM: given what `/devices` returned and what `/system-health` said
// about connected relays, what state is discovery actually in?
//
// # The defect this file exists to prevent
//
// An operator opening Devices and seeing an empty table cannot tell which of
// four completely different situations they are in:
//
//   1. No relay is connected, so NOTHING is discovering anything. Go and look at
//      the relay.
//   2. A relay is connected and has swept its LAN and there is genuinely nothing
//      on it. Go and look at the network — wrong subnet, SSDP not crossing.
//   3. Everything on the LAN was found and is already adopted. Nothing to do;
//      this is the healthy steady state and reads identically to (2) if the only
//      thing on screen is "no candidates".
//   4. The console cannot read relay health at all (it is owner-only), so it
//      genuinely does not know whether (1) or (2) is the case.
//
// Those four send an operator to four different places, and a page that renders
// one empty table for all of them sends them nowhere. So this module returns a
// discriminated `kind` per situation and the page renders each one differently —
// including (4), which is the one a page is most tempted to quietly collapse
// into (2) and thereby state something it does not know.
//
// # Why relay connectivity is the substrate
//
// Discovery is not a thing this console runs. Each relay sweeps its OWN LAN and
// pushes its full current view upward (`relay/1` REL-110/111), and the app peer
// holds a per-relay view keyed by the connection's authenticated identity
// (internal/app/devices). A relay that is not connected is therefore not
// reporting, and `SystemHealth.relays` lists exactly the CONNECTED ones — its
// own schema says a relay that is not connected does not appear. So "is
// discovery running" is answerable, and its answer is "is any relay connected".
//
// `/system-health` is owner-only, which is why every function here takes the
// relay list as `null`-able rather than assuming it can be read.

import type { Device, RelayHealth } from "@/api";

/** Why relay health could not be read — the two cases a page must word
 * differently. `forbidden` is not a fault, it is this principal's scope. */
export type BlindReason = "forbidden" | "unreachable";

export interface DiscoveryInput {
  /** The discovered devices, or `null` while the first read is in flight. */
  devices: Device[] | null;
  /** The `/devices` read's own failure, if it failed. */
  devicesError: string | null;
  /** The CONNECTED relays, or `null` when health could not be read. */
  relays: RelayHealth[] | null;
  /** Why `relays` is null. Null when `relays` is present. */
  blind: BlindReason | null;
}

/** The seven situations, kept apart because they have seven different remedies. */
export type DiscoveryKind =
  /** The first read has not answered yet. */
  | "loading"
  /** `/devices` itself failed — nothing below is known. */
  | "unreadable"
  /** Health is readable and NO relay is connected: nothing is discovering. */
  | "no-relay"
  /** Nothing found, and this console cannot tell whether anything is looking. */
  | "blind"
  /** A relay is connected and has reported nothing on its LAN. */
  | "searching"
  /** Everything found is already adopted — the healthy steady state. */
  | "all-adopted"
  /** Something found is waiting for an adoption decision. */
  | "candidates";

export interface Discovery {
  kind: DiscoveryKind;
  /** The headline an operator reads first. */
  headline: string;
  /** What it means and what to do about it. */
  detail: string;
  /** What this reading could be wrong about, when it could be wrong. Null when
   * the reading is complete — a caveat rendered unconditionally is noise, and a
   * caveat omitted when it applies is the page overstating what it knows. */
  caveat: string | null;
  found: number;
  adopted: number;
  /** Found and NOT adopted — the number of decisions waiting. */
  pending: number;
  /** Connected relays, or `null` when health could not be read. Never 0 for
   * "unknown": those are different facts and 0 is a claim. */
  relayCount: number | null;
}

/** The sentence naming why health is unreadable, for a caveat or a headline. */
function blindSentence(reason: BlindReason): string {
  return reason === "forbidden"
    ? "Only the workspace owner can read relay health, so this console cannot confirm a relay is connected."
    : "Relay health could not be read, so this console cannot confirm a relay is connected.";
}

/**
 * Classify the device plane's discovery state.
 *
 * Ordering matters and is deliberate: a failed `/devices` read beats everything
 * (nothing below it is known), and "no relay connected" beats "found nothing"
 * because it EXPLAINS it. Where devices WERE found, relay health stops mattering
 * for the headline — a device on the page was reported by some relay, so
 * discovery demonstrably ran, whatever health does or does not say.
 */
export function describeDiscovery(input: DiscoveryInput): Discovery {
  const { devices, devicesError, relays, blind } = input;
  const relayCount = relays === null ? null : relays.length;
  const found = devices?.length ?? 0;
  const adopted = (devices ?? []).filter((d) => d.adopted).length;
  const pending = found - adopted;
  const base = { found, adopted, pending, relayCount };

  if (devicesError !== null) {
    return {
      ...base,
      kind: "unreadable",
      headline: "The device plane could not be read",
      detail: devicesError,
      caveat: null,
    };
  }
  if (devices === null) {
    return {
      ...base,
      kind: "loading",
      headline: "Reading what the relays have found…",
      detail: "The console is fetching the relays' current view of their networks.",
      caveat: null,
    };
  }

  if (found === 0) {
    if (relayCount === 0) {
      return {
        ...base,
        kind: "no-relay",
        headline: "Discovery is not running — no relay is connected",
        detail:
          "A relay sweeps its own network and reports what it finds; this console only reads that report. With no relay connected nothing is looking, so an empty list here says nothing about what is on the network. Start or reconnect a relay, then refresh.",
        caveat: null,
      };
    }
    if (relayCount === null) {
      return {
        ...base,
        kind: "blind",
        headline: "Nothing found — and it is not known whether anything is looking",
        detail: `${blind === null ? "Relay health is unavailable." : blindSentence(blind)} An empty list is therefore not evidence that the networks are empty: it reads the same whether a relay swept and found nothing or no relay is running at all.`,
        caveat: "Ask an owner to check relay health, or read it on the System page.",
      };
    }
    return {
      ...base,
      kind: "searching",
      headline: `Discovery is running on ${relayCount} relay${relayCount === 1 ? "" : "s"} — nothing found yet`,
      detail:
        "A connected relay has reported its full view and that view is empty. The devices are not on a network this relay can see, or their discovery protocol is not crossing to it — a relay only ever sees its own LAN.",
      caveat: null,
    };
  }

  const relayCaveat =
    relayCount === null
      ? `${blind === null ? "Relay health is unavailable." : blindSentence(blind)} The devices below were reported by some relay, so discovery has run — but whether it is running NOW is not known here.`
      : null;

  if (pending === 0) {
    return {
      ...base,
      kind: "all-adopted",
      headline: `Everything found is adopted — ${found} device${found === 1 ? "" : "s"}, no decisions waiting`,
      detail:
        "Every device the relays reported already has an adoption record, so there is nothing to adopt. This is the steady state, not an empty result.",
      caveat: relayCaveat,
    };
  }
  return {
    ...base,
    kind: "candidates",
    headline: `${pending} device${pending === 1 ? "" : "s"} found and waiting for a decision`,
    detail:
      "A discovered device is one a relay reported seeing. Nothing has been decided about it: it is not polled, its state is not tracked, and no command can be delivered to it until it is adopted.",
    caveat: relayCaveat,
  };
}

/** The reasons a device an operator EXPECTS is not on the list, as the code
 * behind this page actually produces them. Rendered as a standing explainer
 * rather than only inside an empty state, because "I can see 3 of my 4 TVs" is
 * the shape the question is usually asked in — and an empty-state-only answer is
 * invisible exactly then.
 *
 * Every entry below is a real path, not general advice:
 *   - connectivity: `SystemHealth.relays` documents that a relay which is not
 *     connected does not appear; only a connected relay's view is held.
 *   - own LAN: discovery is per-relay SSDP/mDNS on that relay's own network
 *     (cmd/waiveo-relay's watches) — neither protocol routes between subnets.
 *   - ignored: intake carries an operator-ignored candidate in the report and
 *     deliberately does NOT materialize a row for it
 *     (internal/app/devices/intake.go, REL-110).
 *   - incumbency: a device another relay is already the incumbent for is dropped
 *     from this relay's view before the merge (REL-153a/b).
 *   - whole-report refusal: one malformed candidate refuses the ENTIRE report
 *     and the previous view stands (REL-111) — so a device can be missing
 *     because a DIFFERENT device's candidate was malformed.
 */
export interface MissingReason {
  title: string;
  detail: string;
}

export const MISSING_DEVICE_REASONS: MissingReason[] = [
  {
    title: "Its relay is not connected",
    detail:
      "Only a connected relay's view is held. A relay that drops takes its whole reported view with it — the devices it found stop being listed even though they are still on the network.",
  },
  {
    title: "It is not on a relay's own network",
    detail:
      "Each relay discovers over SSDP and mDNS on the LAN it sits on, and neither protocol crosses subnets. A device on another VLAN is invisible to it no matter how reachable it is by IP.",
  },
  {
    title: "It was ignored",
    detail:
      "A candidate an operator suppressed is still carried in the relay's report and is deliberately not listed here — otherwise the suppression would be cosmetic.",
  },
  {
    title: "Another relay already owns it",
    detail:
      "Two relays on one LAN can both see a device. Only the incumbent reports it, so it appears under one relay and not the other. This is the guard against two relays fighting over one TV.",
  },
  {
    title: "Its relay's whole report was refused",
    detail:
      "A report is applied whole or not at all: one malformed candidate refuses the entire report and the previous view stands. A device can therefore be missing — or stale — because a different device's report was bad.",
  },
];
