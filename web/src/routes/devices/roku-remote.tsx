import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
} from "react";
import {
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  ChevronUp,
  FastForward,
  House,
  Info,
  Pause,
  Play,
  Power,
  PowerOff,
  Rewind,
  Undo2,
  Volume1,
  Volume2,
  VolumeX,
  type LucideIcon,
} from "lucide-react";
import { Button, FormField, StatusBadge, toast } from "@/components/kit";
import { ApiError, type Entity, type EntityCommandResult, type WaiveoApi } from "@/api";
import type { CommandDispatch, CommandOutcome } from "./command-outcome";

/**
 * The virtual remote — a physical Roku handset rendered as a panel, dispatching
 * real `device.command` frames at one already-resolved entity.
 *
 * # Every button here is a command the device class actually declares
 *
 * The built-in `media-player` class (device-class-registry/1 REG-066,
 * internal/deviceclass/builtin.go) declares exactly four commands, and this
 * remote is built out of those four and nothing else:
 *
 *   launch(channel)         → the channel launcher at the foot of the panel
 *   home()                  → the Home key (a first-class command, not a keypress)
 *   keypress(key)           → the D-pad, Back, transport, and volume keys
 *   power(state: on|off)    → the two power buttons
 *
 * That constraint is the point. A relay REFUSES a command its target's class
 * does not declare, without attempting it against the physical device
 * (relay/1 REL-113, `COMMAND_UNRESOLVED`) — so a remote with a "Sleep" button
 * would be a button that can only ever fail. The `key` values are Roku ECP's own
 * key names, which is what the driver behind `keypress` speaks.
 *
 * # ok:false is a result, not a failure
 *
 * `POST /entities/{id}/commands` returns 200 with `{ok:false, error}` when the
 * exchange completed but the command did not take — an undeclared command, a TV
 * the relay could not reach. That is a different thing from the request failing,
 * and the panel reports the relay's OWN code and message for it rather than a
 * generic "something went wrong", because those two codes are the whole
 * diagnosis: `COMMAND_UNRESOLVED` means the vocabulary is wrong,
 * `COMMAND_TARGET_UNREACHABLE` means the TV is off the network.
 *
 * # Why one command at a time
 *
 * A press is disabled while another press is in flight. A relay serializes
 * commands per DEVICE (REL-115), so firing six D-pad presses at once does not
 * make them arrive faster — it makes them arrive in an order nobody chose, which
 * on a menu is the difference between landing where you aimed and landing three
 * rows past it. Holding the panel to one outstanding press keeps the on-screen
 * cursor and the operator's fingers in agreement.
 */

/** The Roku ECP key names this remote's `keypress` buttons send. Kept as a union
 * so a typo is a compile error rather than a `COMMAND_UNRESOLVED` discovered on
 * a TV. */
type EcpKey =
  | "Up"
  | "Down"
  | "Left"
  | "Right"
  | "Select"
  | "Back"
  | "Info"
  | "Play"
  | "Pause"
  | "Rev"
  | "Fwd"
  | "VolumeUp"
  | "VolumeDown"
  | "VolumeMute";

/** One dispatchable press: what the operator sees, and what goes on the wire. */
interface Press {
  /** The accessible name — a remote's buttons are glyphs, so this IS the label. */
  label: string;
  /** The class command and its params (REG-066). */
  command: string;
  params?: Record<string, unknown>;
}

/** A press rendered as a glyph button. OK and Launch carry text instead, so the
 * icon lives on this narrower type rather than being a required field two
 * presses would have to invent a value for. */
interface IconPress extends Press {
  icon: LucideIcon;
}

function keyPress(label: string, icon: LucideIcon, key: EcpKey): IconPress {
  return { label, icon, command: "keypress", params: { key } };
}

const DPAD_UP = keyPress("Up", ChevronUp, "Up");
const DPAD_LEFT = keyPress("Left", ChevronLeft, "Left");
const DPAD_RIGHT = keyPress("Right", ChevronRight, "Right");
const DPAD_DOWN = keyPress("Down", ChevronDown, "Down");
const DPAD_OK: Press = { label: "OK", command: "keypress", params: { key: "Select" satisfies EcpKey } };
const BACK = keyPress("Back", Undo2, "Back");
/** Home is the class's own `home` command, not a keypress — REG-066 declares it
 * separately, and using the declared command is what keeps a driver free to
 * implement "go home" as something other than an ECP keypress. */
const HOME: IconPress = { label: "Home", icon: House, command: "home" };
/** The `*` key on a physical handset. Legacy's remote had it beside Back and
 * Home, and it is a plain ECP key the adapter passes straight through — the
 * relay's `keypress` path URL-escapes whatever key it is given rather than
 * whitelisting a set (internal/relay/ecp/ecp.go). */
const INFO = keyPress("Info", Info, "Info");

/** Play and Pause are SEPARATE keys, as legacy's remote had them. Roku's `Play`
 * toggles on most surfaces but not all — a paused screensaver-adjacent surface
 * can eat it — and an operator who means "pause" should be able to say so
 * rather than pressing a toggle and reading the screen to find out what it did. */
const TRANSPORT: IconPress[] = [
  keyPress("Rewind", Rewind, "Rev"),
  keyPress("Play", Play, "Play"),
  keyPress("Pause", Pause, "Pause"),
  keyPress("Fast forward", FastForward, "Fwd"),
];

const VOLUME: IconPress[] = [
  keyPress("Volume down", Volume1, "VolumeDown"),
  keyPress("Mute", VolumeX, "VolumeMute"),
  keyPress("Volume up", Volume2, "VolumeUp"),
];

const POWER_ON: IconPress = { label: "Power on", icon: Power, command: "power", params: { state: "on" } };
const POWER_OFF: IconPress = { label: "Power off", icon: PowerOff, command: "power", params: { state: "off" } };

/** Keyboard → press, for the keys a handset has and a keyboard shares. Typing
 * into the channel field is excluded below, so these never steal a character.
 *
 * Enter is mapped, but only when focus is NOT on a button: a focused button
 * already activates on Enter, and mapping it unconditionally would send two
 * commands for one keystroke. */
const KEYBOARD: Record<string, Press> = {
  ArrowUp: DPAD_UP,
  ArrowDown: DPAD_DOWN,
  ArrowLeft: DPAD_LEFT,
  ArrowRight: DPAD_RIGHT,
  Enter: DPAD_OK,
  Backspace: BACK,
};

export interface RokuRemoteProps {
  api: WaiveoApi;
  /** The entity the commands are addressed to — already resolved, because
   * relay/1 accepts no selector (REL-112). */
  entity: Entity;
  /** Launch shortcuts this deployment advertised for the entity's device, if
   * any. api/1 publishes no channel inventory, so this is usually empty and the
   * free-text channel field is the path that always works. */
  apps?: { channel: string; name: string }[];
  /** Called once per completed dispatch with what was asked and what came back.
   *
   * Optional, and the panel is fully usable without it — its own live region
   * still reports the last press. It exists so a HOST PAGE can keep the whole
   * sequence on screen: driving a device is "power on, home, launch, four D-pad
   * presses", and the question afterwards is which of those actually landed.
   * The panel does not own that history because the panel is also opened as a
   * transient dialog, where a history would vanish with the dialog. */
  onDispatch?: ((dispatch: CommandDispatch) => void) | undefined;
}

/** The last dispatch's outcome, for the panel's live region. */
type Outcome =
  | { kind: "idle" }
  | { kind: "sent"; label: string }
  | { kind: "refused"; label: string; code: string; message: string };

export function RokuRemote({ api, entity, apps = [], onDispatch }: RokuRemoteProps) {
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<Outcome>({ kind: "idle" });
  const [channel, setChannel] = useState("");
  // The in-flight guard is read inside the (stable) dispatch callback, so it is
  // a ref as well as state: two arrow keys pressed inside one React batch would
  // both see the pre-render `busy === false` and both fire.
  const inFlight = useRef(false);
  // The dispatch sequence, for the record's stable key. A ref rather than state
  // because bumping it must not re-render, and two presses inside one batch
  // must not both read the same value.
  const seq = useRef(0);
  // Held in a ref so `send` stays stable across a parent that passes a fresh
  // closure every render — the keyboard handler and every button depend on it,
  // and a `send` that changed identity on each parent render would rebuild the
  // whole pad on every logged command.
  //
  // Updated in an EFFECT, not during render: a ref mutated during render is a
  // side effect in a phase React is free to discard and re-run. Nothing is lost
  // by the delay — every dispatch here originates from a click or a keystroke,
  // which cannot happen before the commit that ran this effect.
  const onDispatchRef = useRef(onDispatch);
  useEffect(() => {
    onDispatchRef.current = onDispatch;
  }, [onDispatch]);

  const send = useCallback(
    async (press: Press) => {
      if (inFlight.current) return;
      inFlight.current = true;
      setBusy(true);
      // Reported for EVERY completion, success included: a log that only records
      // failures cannot answer "did the launch land", which is the question an
      // operator actually has.
      const report = (result: CommandOutcome) => {
        seq.current += 1;
        onDispatchRef.current?.({
          seq: seq.current,
          at: Date.now(),
          label: press.label,
          command: press.command,
          params: press.params,
          outcome: result,
        });
      };
      try {
        const result: EntityCommandResult = await api.entities.sendCommand(
          entity.id,
          press.command,
          press.params,
        );
        if (result.ok) {
          setOutcome({ kind: "sent", label: press.label });
          report({ kind: "ok" });
        } else {
          const code = result.error?.code ?? "INTERNAL";
          const message = result.error?.message ?? "the relay reported no reason.";
          setOutcome({ kind: "refused", label: press.label, code, message });
          report({ kind: "refused", code, message });
          toast.error(`${press.label} was not applied — ${code}: ${message}`);
        }
      } catch (err) {
        const detail = err instanceof ApiError ? (err.detail ?? err.code) : "the service is unreachable.";
        setOutcome({ kind: "refused", label: press.label, code: "REQUEST_FAILED", message: detail });
        report({ kind: "failed", detail });
        toast.error(`Couldn't send ${press.label}: ${detail}`);
      } finally {
        inFlight.current = false;
        setBusy(false);
      }
    },
    [api, entity.id],
  );

  const launch = useCallback(
    (id: string) => {
      const trimmed = id.trim();
      if (trimmed === "") {
        toast.error("Enter a channel id to launch.");
        return;
      }
      void send({ label: `Launch ${trimmed}`, command: "launch", params: { channel: trimmed } });
    },
    [send],
  );

  // Keyboard control. Bound to the panel (not the window) so a remote that is
  // not focused never eats the arrow keys of the page around it, and skipped
  // entirely while focus is in the channel field — where ArrowLeft means "move
  // the caret", not "press left on the TV".
  const onKeyDown = useCallback(
    (event: ReactKeyboardEvent<HTMLDivElement>) => {
      const target = event.target as HTMLElement;
      if (target.tagName === "INPUT" || target.tagName === "TEXTAREA") return;
      const press = KEYBOARD[event.key];
      if (!press) return;
      if (event.key === "Enter" && target.tagName === "BUTTON") return;
      event.preventDefault();
      void send(press);
    },
    [send],
  );

  const status = useMemo(() => {
    switch (outcome.kind) {
      case "idle":
        return "Ready.";
      case "sent":
        return `${outcome.label} sent.`;
      case "refused":
        return `${outcome.label} was not applied — ${outcome.code}: ${outcome.message}`;
    }
  }, [outcome]);

  const padButton = (press: IconPress) => (
    <Button
      key={press.label}
      variant="outline"
      size="icon"
      icon={press.icon}
      aria-label={press.label}
      disabled={busy}
      onClick={() => void send(press)}
    />
  );

  return (
    <div
      data-slot="roku-remote"
      role="group"
      aria-label={`Remote — ${entity.name}`}
      // Focusable in its own right, so the pad can be driven from the keyboard
      // without first tabbing onto one of its buttons — a handset is held, not
      // tabbed through. Key events from the buttons inside bubble to the same
      // handler, so focus anywhere in the panel drives it.
      tabIndex={0}
      onKeyDown={onKeyDown}
      className="flex w-full max-w-xs flex-col gap-4 rounded-card border border-border bg-card p-4"
    >
      <div className="flex items-center justify-between gap-2">
        <span className="text-sm font-medium">{entity.name}</span>
        {entity.state ? (
          <StatusBadge status={entity.state === "off" ? "off" : "ok"}>{entity.state}</StatusBadge>
        ) : (
          <StatusBadge status="pending">no state</StatusBadge>
        )}
      </div>

      {/* Power — two explicit buttons, because the class's `power` takes a
          state and a toggle would have to guess the current one. */}
      <div className="flex justify-between gap-2">
        {padButton(POWER_ON)}
        {padButton(POWER_OFF)}
      </div>

      {/* The D-pad, laid out as it sits under a thumb. */}
      <div className="grid grid-cols-3 justify-items-center gap-2">
        <div />
        {padButton(DPAD_UP)}
        <div />
        {padButton(DPAD_LEFT)}
        <Button
          variant="secondary"
          size="sm"
          disabled={busy}
          onClick={() => void send(DPAD_OK)}
          className="rounded-pill"
        >
          OK
        </Button>
        {padButton(DPAD_RIGHT)}
        <div />
        {padButton(DPAD_DOWN)}
        <div />
      </div>

      <div className="flex justify-center gap-2">
        {padButton(BACK)}
        {padButton(HOME)}
        {padButton(INFO)}
      </div>

      <div className="flex justify-center gap-2">{TRANSPORT.map(padButton)}</div>
      <div className="flex justify-center gap-2">{VOLUME.map(padButton)}</div>

      {/* Channel launch. The free-text field is always available; the buttons
          above it appear only for a deployment that advertised an inventory. */}
      {apps.length > 0 ? (
        <div className="flex flex-wrap gap-2" data-slot="remote-apps">
          {apps.map((app) => (
            <Button
              key={app.channel}
              variant="ghost"
              size="sm"
              disabled={busy}
              onClick={() => launch(app.channel)}
            >
              {app.name}
            </Button>
          ))}
        </div>
      ) : null}

      <div className="flex items-end gap-2">
        <FormField
          label="Channel id"
          help="api/1 carries no channel inventory — launch by the channel's own id."
          className="flex-1"
        >
          {(field) => (
            <input
              {...field}
              className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
              value={channel}
              onChange={(e) => setChannel(e.target.value)}
              placeholder="e.g. 12"
            />
          )}
        </FormField>
        <Button variant="outline" disabled={busy} onClick={() => launch(channel)}>
          Launch
        </Button>
      </div>

      <p role="status" aria-live="polite" className="min-h-[1.25rem] text-xs text-muted-foreground">
        {status}
      </p>
      <p className="text-xs text-muted-foreground">
        Arrow keys, Enter and Backspace drive the pad while this panel has focus.
      </p>
    </div>
  );
}
