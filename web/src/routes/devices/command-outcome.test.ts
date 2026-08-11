import { describe, it, expect } from "vitest";
import { commandSummary, diagnoseCommandError } from "./command-outcome";

describe("commandSummary", () => {
  it("renders a paramless command as its bare name", () => {
    expect(commandSummary("home")).toBe("home");
    expect(commandSummary("home", {})).toBe("home");
  });

  it("renders the params, because 'keypress' alone cannot say which key", () => {
    expect(commandSummary("keypress", { key: "Up" })).toBe("keypress key=Up");
    expect(commandSummary("power", { state: "on" })).toBe("power state=on");
    expect(commandSummary("launch", { channel: "12" })).toBe("launch channel=12");
  });
});

describe("diagnoseCommandError", () => {
  // The three relay codes are three unrelated problems, and the whole point of
  // the log is that an operator can tell them apart WITHOUT reading the source.
  it("says COMMAND_UNRESOLVED never reached the device, and names all three preconditions", () => {
    const d = diagnoseCommandError("COMMAND_UNRESOLVED")!;
    expect(d).toMatch(/before touching the device/);
    expect(d).toMatch(/adopted/);
    expect(d).toMatch(/enabled/);
    expect(d).toMatch(/address/);
  });

  it("points at the relay's own journal for the NOT DISPATCHED line, not the System page", () => {
    // The relay is a separate binary and installs no platformlog tee, so its
    // dispatch line is NOT in the feeder's /platform-logs ring that the System
    // page reads. Sending an operator there would waste the trip.
    const d = diagnoseCommandError("COMMAND_UNRESOLVED")!;
    expect(d).toMatch(/NOT DISPATCHED/);
    expect(d).toMatch(/journalctl -u waiveo-relay/);
    expect(d).toMatch(/not the log the System page shows/);
  });

  it("says COMMAND_TARGET_UNREACHABLE means the relay DID try", () => {
    const d = diagnoseCommandError("COMMAND_TARGET_UNREACHABLE")!;
    expect(d).toMatch(/tried and the device did not answer/);
    expect(d).not.toMatch(/adopted/);
  });

  it("keeps INTERNAL as a relay-side failure rather than an operator action", () => {
    expect(diagnoseCommandError("INTERNAL")).toMatch(/relay itself failed/);
  });

  it("returns null rather than inventing a reading for a code it does not know", () => {
    // A generic sentence would pretend to a diagnosis; null lets the caller show
    // the relay's own message, which is then the whole truth available.
    expect(diagnoseCommandError("SOMETHING_NEW")).toBeNull();
  });
});
