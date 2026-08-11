// The IANA time-zone names the Settings page offers for a site's `tz`.
//
// # Why this is a closed list and not a text box
//
// A site's `tz` is not decoration. `EffectiveGeo` (internal/datamodel/
// scopetree.go) resolves it for every daypart, every `sun` trigger and the
// coordinates the relay fetches weather against, and DAT-034 forbids the
// platform from ever substituting box-local state when it cannot resolve one.
// The write path does NOT check that the name is loadable — validateGeo only
// counts that all three of tz/lat/long are present (DAT-031) — so a typo is
// accepted by the server and surfaces much later as `EFFECTIVE_GEO_UNRESOLVED`
// at resolution time, which reads to an operator as "that screen went dark",
// not as "I mistyped a time zone three weeks ago".
//
// Both surfaces that could already write a `tz` — the scope-tree panel's create
// form and the screens detail form — take it as free text, so that typo is
// reachable today. This page does not add a third way to make it: the control is
// a select over names the runtime itself enumerates.
//
// # The stored value is always an option
//
// `timeZoneOptions` unions in whatever the records already hold. Without that,
// a site carrying a name this runtime does not enumerate would render a select
// with NOTHING selected — and the first save would silently rewrite the site's
// time zone to whatever the operator happened to pick, or to the placeholder.
// A settings control that quietly changes a value it was only supposed to
// display is worse than no control, so the stored value is preserved as an
// option even when it is one this browser has never heard of.

/** `Intl.supportedValuesOf`, read defensively.
 *
 * Typed locally rather than relying on the `es2022.intl` lib slice, and wrapped
 * in a feature test plus a try, because this is the one input to the page that
 * comes from the host environment rather than from the box: an older engine
 * omits the function entirely, and a locked-down one can throw. Either way the
 * page must still render a usable select rather than an empty one. */
type SupportedValuesOf = (key: "timeZone") => string[];

function runtimeZones(): string[] {
  const intl = Intl as unknown as { supportedValuesOf?: SupportedValuesOf };
  if (typeof intl.supportedValuesOf !== "function") return [];
  try {
    const zones = intl.supportedValuesOf("timeZone");
    return Array.isArray(zones) ? zones : [];
  } catch {
    return [];
  }
}

/** The list used when the runtime enumerates nothing.
 *
 * Deliberately short and deliberately NOT a mirror of the tz database: a stale
 * hand-copied IANA list is a slow-motion lie (zones are added and renamed every
 * year), so this is a legibly partial set of common zones whose only job is to
 * keep the control usable on an engine that cannot enumerate. The stored value
 * is unioned in on top of it, so a deployment already on a zone that is missing
 * here keeps its own. */
export const FALLBACK_ZONES: readonly string[] = [
  "UTC",
  "America/Anchorage",
  "America/Chicago",
  "America/Denver",
  "America/Halifax",
  "America/Los_Angeles",
  "America/Mexico_City",
  "America/New_York",
  "America/Phoenix",
  "America/Sao_Paulo",
  "America/Toronto",
  "Asia/Dubai",
  "Asia/Hong_Kong",
  "Asia/Kolkata",
  "Asia/Shanghai",
  "Asia/Singapore",
  "Asia/Tokyo",
  "Australia/Melbourne",
  "Australia/Perth",
  "Australia/Sydney",
  "Europe/Amsterdam",
  "Europe/Berlin",
  "Europe/Dublin",
  "Europe/Lisbon",
  "Europe/London",
  "Europe/Madrid",
  "Europe/Paris",
  "Europe/Rome",
  "Europe/Stockholm",
  "Europe/Warsaw",
  "Pacific/Auckland",
  "Pacific/Honolulu",
];

/**
 * Every zone name the select should offer: what the runtime enumerates (or the
 * fallback set when it enumerates nothing), plus `UTC`, plus every name already
 * stored on a record — sorted, de-duplicated.
 *
 * `stored` is tolerant of nulls and blanks because it is fed straight from the
 * loaded records: a site is REQUIRED to carry a tz (DAT-031) but a group or a
 * screen may carry none, and a list built by filtering at the call site is a
 * list that drifts from this guard.
 */
export function timeZoneOptions(stored: readonly (string | null | undefined)[] = []): string[] {
  const zones = new Set<string>(runtimeZones());
  if (zones.size === 0) for (const z of FALLBACK_ZONES) zones.add(z);
  // UTC is the one name that must always be offerable: some engines omit it from
  // the enumeration (it is not a geographic zone), and it is the answer for a
  // deployment that genuinely does not want local time.
  zones.add("UTC");
  for (const z of stored) {
    if (typeof z === "string" && z.trim() !== "") zones.add(z.trim());
  }
  return [...zones].sort((a, b) => a.localeCompare(b));
}
