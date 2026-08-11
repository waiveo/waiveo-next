// What happened to a command an operator pressed — the record, and the sentence
// that turns a relay error code into something to go and do.
//
// # Why this is a module and not two lines inside a button handler
//
// The defect this codebase keeps shipping is the button that silently does
// nothing. `POST /entities/{id}/commands` is honest about it — a `200` with
// `ok:false` and a typed `relay/1` code is a completed exchange whose command
// did not take — but a UI that renders that as "failed" throws away the entire
// diagnosis, and the three codes are three unrelated problems:
//
//   COMMAND_UNRESOLVED         the command never reached the device at all.
//   COMMAND_TARGET_UNREACHABLE the relay tried; the device did not answer.
//   INTERNAL                   the relay itself failed.
//
// The first is the one that matters most and reads the least like an error. The
// relay refuses it BEFORE touching the device (relay/1 REL-113) and logs
// `NOT DISPATCHED (COMMAND_UNRESOLVED)` on its own stderr — which, in the
// deployment shape this platform actually ships, is the waiveo-relay systemd
// unit's journal and NOT the feeder's `/platform-logs` ring that the console's
// System page reads (cmd/waiveo-relay installs no platformlog tee). So the
// synchronous `error.code` on this response is the console's ONLY signal that a
// command was refused, and it has to carry the whole explanation.
//
// # Why a log rather than a toast
//
// A toast is gone before the operator has looked back at the TV. Driving a
// device is a sequence — power on, home, launch, four D-pad presses — and the
// question afterwards is "which of those actually landed". A running list
// answers it; a series of dismissed toasts does not.

/** What the console asked for and what came back. `at` is wall-clock ms, which
 * is what an operator correlates against the thing they watched happen. */
export interface CommandDispatch {
  /** Monotonic within one page mount — a stable React key that two commands
   * issued in the same millisecond cannot collide on. */
  seq: number;
  at: number;
  /** The button's own accessible name ("Power on"), not the wire command. */
  label: string;
  /** The device-class command actually sent (device-class-registry/1 REG-066). */
  command: string;
  /** Its params as sent, or undefined when the command declares none. */
  params?: Record<string, unknown> | undefined;
  outcome: CommandOutcome;
}

export type CommandOutcome =
  /** The relay answered `ok:true`: it dispatched and the device accepted. */
  | { kind: "ok" }
  /** The relay answered `ok:false` with its own typed code — the exchange
   * completed and the command did not take. */
  | { kind: "refused"; code: string; message: string }
  /** The REQUEST failed: a Problem, or the service unreachable. A different
   * thing from a refusal, and never collapsed into one — a refusal means the
   * relay was reached and had an opinion. */
  | { kind: "failed"; detail: string };

/** The wire form of a dispatch, for the log's second column: `launch
 * channel=dev`, `keypress key=Up`, `home`. Params are rendered because "which
 * key did I press" is the question a bare `keypress` cannot answer.
 *
 * Values are stringified, never JSON — a param value here is a channel id or a
 * key name, and quoting them would put punctuation in front of an operator
 * scanning a column. */
export function commandSummary(command: string, params?: Record<string, unknown>): string {
  const entries = Object.entries(params ?? {});
  if (entries.length === 0) return command;
  return `${command} ${entries.map(([k, v]) => `${k}=${String(v)}`).join(" ")}`;
}

/** The operator-facing diagnosis for a relay error code: what it means, and
 * what to check. Returns null for a code this console has no specific reading
 * of — the relay's own `message` is then the whole answer, which is better than
 * a generic sentence that pretends to know more.
 *
 * These are grounded in the relay's actual refusal paths
 * (internal/relay/devicetargets, internal/relay/deviceplane/command.go), not in
 * general advice: an entity is controllable if and only if it is adopted AND
 * enabled AND the relay can currently locate an address for it. */
export function diagnoseCommandError(code: string): string | null {
  switch (code) {
    case "COMMAND_UNRESOLVED":
      return (
        "The relay refused this before touching the device — it could not resolve the entity to " +
        "something it may drive. An entity is drivable only when its device is adopted, that " +
        "entity is enabled on the adoption record, and the relay currently holds an address for " +
        "it. Check those three; the relay also writes this as a NOT DISPATCHED line on its own " +
        "journal (journalctl -u waiveo-relay), which is not the log the System page shows."
      );
    case "COMMAND_TARGET_UNREACHABLE":
      return (
        "The relay tried and the device did not answer. It is powered fully off, off the network, " +
        "or its address has changed since discovery last saw it — the relay dials the address it " +
        "was found at."
      );
    case "INTERNAL":
      return "The relay itself failed while handling this. Its own log has the reason; this console is only told that it failed.";
    default:
      return null;
  }
}
