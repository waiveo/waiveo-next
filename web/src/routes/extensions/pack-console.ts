// The Extensions console's pure logic: how a refusal is read, what an installed
// pack's provenance says about what may be done to it, and which of a pack's
// declared pages an operator can actually reach.
//
// It is a separate module from the route for one reason: every rule below is a
// place this console could quietly LIE — offer an update the box will refuse,
// present a removal as available when the deployment forbids it, or list a page
// as a destination the nav has silently dropped. A pure function can be driven
// against the exact refusal the server emits; a JSX branch cannot.

import { ApiError, encodePackPagePath, type Pack, type PackInstallRecord, type PackUpdateResult, type ValidationFieldError } from "@/api";

// ── Refusals ─────────────────────────────────────────────────────────────────

/** marketplace/1 MKT-093b(i): an uninstall of a required pack was refused. */
export const REQUIRED_PACK_FLOOR = "REQUIRED_PACK_FLOOR";

/** A refusal, read out of an api/1 Problem WITHOUT interpretation.
 *
 * `codes` is a LIST, and the reason is drift, not indecision. api/1's error-code
 * registry now admits `REQUIRED_PACK_FLOOR` and `MARKETPLACE_REF_INVALID` as
 * top-level codes (openapi `ErrorCode`), while the pack handlers today emit them
 * INSIDE `errors[]` beneath a top-level `VALIDATION_FAILED` — because every other
 * pack refusal (the artifact engine's `PACK_ARTIFACT_*`, the manifest engine's
 * per-field taxonomy) has no registry-valid top-level home and rides there. A
 * console that read only one of those two positions would stop recognising the
 * refusal the day the server moved it, and would do so SILENTLY: the operator
 * would see a generic "validation failed" where a required-pack refusal belongs.
 * So both positions are collected and `hasCode` asks about the union. */
export interface Refusal {
  /** Every machine-readable code the Problem carried: the top-level `code` plus
   * each `errors[].code`, in that order. */
  codes: string[];
  /** The server's own human sentence. Never composed here — a refusal an operator
   * acts on must quote the box, not this page's guess at what the box meant. */
  detail: string;
  /** The per-field violations verbatim (API-013), for the ones that carry them:
   * a manifest refusal names every offending manifest member at once. */
  fields: ValidationFieldError[];
  /** The trace id to quote when asking why. */
  traceId: string | null;
  /** HTTP status, or 0 when the request never reached the box. */
  status: number;
}

/** Read any thrown value as a Refusal. A non-ApiError (a dropped connection, a
 * DNS failure) becomes an explicit unreachable-box refusal rather than being
 * reported as a validation problem it is not. */
export function describeRefusal(err: unknown): Refusal {
  if (err instanceof ApiError) {
    const fields = Array.isArray(err.problem?.errors) ? err.problem.errors : [];
    const codes = [err.code, ...fields.map((f) => f.code)].filter((c): c is string => !!c);
    return {
      codes,
      detail: err.detail ?? err.title ?? err.code,
      fields,
      traceId: err.traceId,
      status: err.status,
    };
  }
  return {
    codes: [],
    detail: "The box could not be reached, so nothing was attempted.",
    fields: [],
    traceId: null,
    status: 0,
  };
}

/** Does this refusal carry `code`, in either position? */
export function hasCode(refusal: Refusal, code: string): boolean {
  return refusal.codes.includes(code);
}

// ── Provenance: what the install record permits ──────────────────────────────

/** What an installed pack's newest install record says about it (MKT-094/094a).
 *
 * `autoTracked` is the load-bearing one. An update check re-resolves the pack
 * through the trust channel PINNED on that record; an install that pinned no
 * channel — the direct, out-of-band artifact path — has no `(pack, channel)` pair
 * to re-resolve and the host MUST NOT default one, because supplying a channel
 * would be choosing the provenance tier on the operator's behalf. So the box
 * refuses that update check, and this console must not offer it: an enabled
 * button whose only outcome is a refusal is the surface accepting work it cannot
 * perform. */
export interface PackProvenance {
  newest: PackInstallRecord | null;
  /** The pinned trust channel, or null for a direct install. */
  channel: string | null;
  /** Whether `POST /packs/{id}/update` can do anything but refuse. */
  autoTracked: boolean;
  /** Why an update check is unavailable, when it is. Empty when it is available. */
  reason: string;
}

/** Read provenance off a pack's install-record history (oldest first — the LAST
 * entry is the current pin, MKT-094/094b). */
export function packProvenance(records: PackInstallRecord[]): PackProvenance {
  const newest = records.length > 0 ? records[records.length - 1] : null;
  if (!newest) {
    return {
      newest: null,
      channel: null,
      autoTracked: false,
      reason:
        "This box holds no install record for this pack, so nothing names the channel it would be re-resolved through.",
    };
  }
  const channel = newest.trust_channel;
  if (!channel) {
    return {
      newest,
      channel: null,
      autoTracked: false,
      reason:
        "Installed directly from an artifact, with no trust channel pinned — so it is not auto-tracked and the box has nothing to check it against. Re-install it from a marketplace reference to track a channel.",
    };
  }
  return { newest, channel, autoTracked: true, reason: "" };
}

// ── Reachability: which declared pages an operator can actually open ─────────

/** One of a pack's declared pages, resolved to the console route that opens it. */
export interface ReachablePage {
  path: string;
  href: string;
  titleMsg: string;
  pageType: string;
}

/** What this box can actually show of an installed pack. */
export interface PackHealth {
  pages: ReachablePage[];
  /** Declared page paths that CANNOT be confined to the pack's own
   * `/p/{publisher}/{name}/` namespace — a `..`, a `.`, or an empty segment.
   *
   * The Extensions nav drops these silently (use-installed-packs), which is the
   * right thing for a nav landmark and the wrong thing for a management console:
   * the operator installed a pack that declares N pages and can reach fewer, with
   * nothing anywhere saying so. Naming them here is the whole difference between
   * a page that is missing and a page that is missing FOR A REASON. */
  unreachablePages: string[];
  collections: string[];
}

/** Resolve an installed pack's page + collection surface. Pure: it reads the
 * manifest the registry already returned, and issues nothing. */
export function packHealth(pack: Pack): PackHealth {
  const pages: ReachablePage[] = [];
  const unreachablePages: string[] = [];
  for (const page of pack.manifest.ui?.pages ?? []) {
    const safe = encodePackPagePath(page.path);
    if (safe === null) {
      unreachablePages.push(page.path);
      continue;
    }
    pages.push({
      path: page.path,
      href: `/p/${pack.id}/${safe}`,
      titleMsg: page.titleMsg,
      pageType: page.pageType,
    });
  }
  const collections = (pack.manifest.dataModel?.collections ?? []).map((c) => c.name);
  return { pages, unreachablePages, collections };
}

// ── Outcomes ─────────────────────────────────────────────────────────────────

/** Say what an update check DID, in the operator's terms — including the case
 * where it deliberately did nothing.
 *
 * `reverted` is not a failure and must not read as one: the box moved BACKWARD on
 * purpose because the version it was running was withdrawn by its publisher. That
 * is the only circumstance a channel's version may decrease (MKT-050's sole
 * exception), and an operator who sees a lower number with no explanation will
 * reasonably think the update broke. */
export function describeUpdate(result: PackUpdateResult): string {
  switch (result.action) {
    case "unchanged":
      return `Already current at ${result.to_version} — the channel still points at the installed version, so nothing was written.`;
    case "updated":
      return `Updated ${result.from_version} → ${result.to_version}. It is live now; the box was not restarted.`;
    case "reverted":
      return `Reverted ${result.from_version} → ${result.to_version}. The version that was running has been withdrawn by its publisher, so the box went back to the most recent version it had successfully run.`;
  }
}

/** Say what an install DID, given what was installed before it.
 *
 * The endpoint answers 201 for a fresh install and 200 for one that replaced a
 * pack in place, but the SUMMARY body is identical either way and the client
 * surfaces the body, not the status — so this derives the distinction from the
 * list the console already holds rather than claiming one it cannot see. */
export function describeInstall(
  id: string,
  version: string,
  previous: { id: string; version: string }[],
): string {
  const prior = previous.find((p) => p.id === id);
  if (!prior) return `Installed ${id} ${version}. Its pages are live now — the box was not restarted.`;
  if (prior.version === version) return `Re-installed ${id} ${version} over the copy already on this box.`;
  return `Updated ${id} ${prior.version} → ${version} in place. It is live now — the box was not restarted.`;
}
