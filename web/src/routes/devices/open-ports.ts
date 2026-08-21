// What an active scan found listening, and what that is worth to an operator.
//
// api/1 calls the port list "the cheapest honest evidence of what a device DOES,
// for the many hosts that announce nothing over mDNS or SSDP" — which is the
// whole reason it is on this page. In a fleet of sixty-odd rows where most
// devices are `unclassified` because they announce nothing, a host answering
// 9100 is a printer and a host answering 32400 is a media server, and that is
// the only signal saying so.
//
// EVIDENCE, NOT IDENTIFICATION. An open port says something answered there, not
// what the device IS — so every label below is phrased as what typically answers
// on that port, and nothing here ever sets a device's class. The platform's own
// class comes from discovery's generic classifier and from extension patterns;
// this is a hint an operator reads, deliberately kept out of the machinery.

/** The curated set Discovery probes, with the meaning each carries.
 *
 * Mirrors `internal/relay/portscan.Ports` — the same ten ports, in the same
 * order, with the meanings that list already documents inline. It is duplicated
 * rather than served because it is a small closed set that changes at the speed
 * of someone editing that Go slice, and the cost of drift is bounded by design:
 * an unlisted port renders as a bare number, which is exactly what this table
 * showed for every port before. Drift degrades, it does not lie. */
export const PORT_MEANINGS: Record<number, string> = {
  22: "SSH — a computer, a NAS, an appliance",
  80: "HTTP — a web UI",
  443: "HTTPS — a web UI, secured",
  445: "SMB — file storage",
  631: "IPP — a printer",
  1400: "Sonos — a speaker",
  5353: "mDNS — announces itself",
  8060: "Roku ECP — a Roku this platform can drive",
  9100: "JetDirect — a raw-print printer",
  32400: "Plex — a media server",
};

/** The short word for a port, for the inline hint. The long form above is the
 * tooltip; this is what fits in a table cell. */
const PORT_SHORT: Record<number, string> = {
  22: "ssh",
  80: "http",
  443: "https",
  445: "smb",
  631: "printer",
  1400: "sonos",
  5353: "mdns",
  8060: "roku",
  9100: "printer",
  32400: "plex",
};

/** How a port reads: `8060 roku`, or a bare `4711` for one nothing is known
 * about. Never invents a meaning — an unlisted port gets its number and no
 * claim. */
export function describePort(port: number): string {
  const short = PORT_SHORT[port];
  return short === undefined ? String(port) : `${port} ${short}`;
}

/** The tooltip for a whole list. */
export function describePorts(ports: number[]): string {
  return ports.map((p) => PORT_MEANINGS[p] ?? `${p} — nothing known about this port`).join("\n");
}

/** The three answers a device's `open_ports` can carry, named.
 *
 * `unscanned` and `none` are DIFFERENT and the whole point of this module is
 * that the table draws them differently. api/1: "absent means nobody has looked,
 * empty would mean a scan looked and found nothing open, and only a scan can
 * assert the second." Rendering both as an em dash would put the console back
 * where the rest of the stack was until the layer below this one was fixed —
 * showing a scan's finding as an absence of one. */
export type PortsState =
  | { kind: "unscanned" }
  | { kind: "none" }
  | { kind: "open"; ports: number[] };

export function portsState(openPorts: number[] | undefined): PortsState {
  if (openPorts === undefined) return { kind: "unscanned" };
  if (openPorts.length === 0) return { kind: "none" };
  return { kind: "open", ports: openPorts };
}

/** The text a search matches on, so an operator can type "printer" and find the
 * host answering 9100 without knowing the number.
 *
 * The two decided states get words too — "not scanned" and "none open" are both
 * things worth narrowing a fleet to, and a filter that could only find ports
 * would make the absence of them unsearchable. */
export function portsSearchText(openPorts: number[] | undefined): string {
  const state = portsState(openPorts);
  switch (state.kind) {
    case "unscanned":
      return "not scanned";
    case "none":
      return "none open";
    case "open":
      return state.ports.map(describePort).join(" ");
  }
}
