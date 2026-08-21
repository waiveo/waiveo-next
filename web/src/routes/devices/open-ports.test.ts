import { describe, it, expect } from "vitest";
import { describePort, describePorts, portsState, portsSearchText, PORT_MEANINGS } from "./open-ports";

// The three answers `open_ports` carries. Five of six layers under this console
// used to collapse the middle one into the first — "a scan looked and found
// nothing open" reported as "nobody has looked" — and the console is the last
// place that collapse could be reintroduced, in the one place an operator would
// actually meet it.

describe("portsState — three answers, kept apart", () => {
  it("reads an ABSENT member as unscanned", () => {
    expect(portsState(undefined)).toEqual({ kind: "unscanned" });
  });

  it("reads an EMPTY list as a scan that found nothing — not as unscanned", () => {
    // The distinction the whole stack was fixed to carry. If this ever returns
    // `unscanned`, the console is lying about a scan that ran.
    expect(portsState([])).toEqual({ kind: "none" });
  });

  it("reads a list as findings", () => {
    expect(portsState([22, 8060])).toEqual({ kind: "open", ports: [22, 8060] });
  });
});

describe("describePort — evidence, never identification", () => {
  it("names what typically answers, inline", () => {
    expect(describePort(8060)).toBe("8060 roku");
    expect(describePort(9100)).toBe("9100 printer");
  });

  it("renders a port it knows nothing about as a bare number, inventing nothing", () => {
    // The drift guard. This map mirrors internal/relay/portscan.Ports; if that
    // list gains a port before this map does, the column must degrade to a
    // number rather than guess a meaning.
    expect(describePort(4711)).toBe("4711");
  });

  it("covers exactly the ports the relay probes", () => {
    // Pinned so a port added to the Go list without a meaning here is a visible
    // decision rather than a silent bare number nobody notices.
    expect(Object.keys(PORT_MEANINGS).map(Number).sort((a, b) => a - b)).toEqual([
      22, 80, 443, 445, 631, 1400, 5353, 8060, 9100, 32400,
    ]);
  });

  it("spells the unknown port out in the tooltip rather than leaving a gap", () => {
    expect(describePorts([4711])).toMatch(/nothing known about this port/);
    expect(describePorts([8060])).toMatch(/Roku ECP/);
  });
});

describe("portsSearchText — an operator types what they are looking for", () => {
  it("matches a printer by the word, not by the port number", () => {
    // The column's operator value: most of a real fleet is `unclassified`
    // because it announces nothing, and this is the only signal that says what
    // a host does.
    expect(portsSearchText([9100])).toContain("printer");
    expect(portsSearchText([631])).toContain("printer");
  });

  it("makes both decided states searchable too", () => {
    // A fleet is worth narrowing to "what has nobody looked at" and to "what
    // came back clean"; a search that only matched ports could express neither.
    expect(portsSearchText(undefined)).toBe("not scanned");
    expect(portsSearchText([])).toBe("none open");
  });
});
