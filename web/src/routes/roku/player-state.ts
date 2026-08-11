// What a media player is actually DOING right now, read off the entity.
//
// # Why `state` alone is not enough, and why this file exists
//
// `Entity.state` is one word from the class's closed vocabulary — on, playing,
// paused, buffering, idle, off, standby, unavailable. It answers "is it awake".
// It cannot answer the question an operator standing in front of a wrong-looking
// screen actually has, which is "what is on it": a TV showing somebody's Netflix
// and a TV showing the platform's own player are both `on`.
//
// `device-class-registry/1` REG-064 declares six attributes for exactly this,
// and the api/1 Entity carries them as a string map. This module names them,
// says what each one means, and — the part that matters — distinguishes an
// attribute the driver did NOT report from one it reported as empty. Rendering
// "—" for both would let an operator conclude a screensaver is not running when
// the truth is that nothing has said either way.
//
// # Values are strings, including the booleans
//
// The wire type is `map[string]string` deliberately: this is display detail
// crossing a trust boundary from a relay, and a bounded string map is checkable
// at intake in a way an arbitrary JSON value is not. So `is_screensaver` arrives
// as "true"/"false" and this module does not coerce it — it renders what the
// driver said. A page that parsed it into a boolean would have to invent an
// answer for "yes", "1", or "" and would then be reporting its own guess.

import type { Entity } from "@/api";

/** One declared attribute of the built-in `media-player` class (REG-064), with
 * the sentence that says why an operator should care. */
export interface PlayerAttribute {
  name: string;
  label: string;
  help: string;
  /** REG-064's change-emission class. `cosmetic` is shown but marked, because a
   * change to it alone does not fire an any-attribute-changed automation and an
   * operator writing rules needs to know which ones do. */
  emission: "significant" | "cosmetic";
}

/** Exactly REG-064's six, in the order they answer questions: what is on the
 * screen, then how it is identified, then what kind of surface it is, then the
 * power detail behind `state`. */
export const MEDIA_PLAYER_ATTRIBUTES: PlayerAttribute[] = [
  {
    name: "active_app",
    label: "Active app",
    help: "What is foregrounded, as the device labels it — or the powered-on baseline surface when nothing is.",
    emission: "significant",
  },
  {
    name: "active_app_id",
    label: "Active app id",
    help: "The driver-assigned id of that app. This is what an automation compares against to tell the platform's own player apart from anything else.",
    emission: "significant",
  },
  {
    name: "app_type",
    label: "Surface type",
    help: "app, screensaver, home or menu — how the driver classifies what is foregrounded.",
    emission: "significant",
  },
  {
    name: "is_screensaver",
    label: "Screensaver",
    help: "Whether the idle/attract surface has taken the foreground. A screen showing a screensaver is on and is not showing content.",
    emission: "significant",
  },
  {
    name: "power_mode",
    label: "Power mode",
    help: "The device's own raw power sub-mode, before it is classified into state — the difference between a real power-on and a low-power mode polling can still see.",
    emission: "significant",
  },
  {
    name: "app_version",
    label: "App version",
    help: "The foregrounded app's own version, where the driver can read one. Cosmetic: a change to it alone fires no attribute-changed automation.",
    emission: "cosmetic",
  },
];

export interface PlayerFact extends PlayerAttribute {
  /** What the driver reported, or null when it reported nothing for this
   * attribute. Null is NOT the empty string: "the driver said nothing" and "the
   * driver said blank" are different, and only the first justifies "not
   * reported". */
  value: string | null;
}

/** The six declared attributes, in declaration order, each with what this entity
 * reported for it. Always six entries — an absent attribute is a fact worth
 * showing, because "no state at all" is what an unpolled or unreachable device
 * looks like and hiding the row hides that. */
export function playerFacts(entity: Entity): PlayerFact[] {
  const attributes = entity.attributes ?? {};
  return MEDIA_PLAYER_ATTRIBUTES.map((attribute) => {
    const raw = attributes[attribute.name];
    return { ...attribute, value: typeof raw === "string" ? raw : null };
  });
}

/** Anything the driver reported that REG-064 does not declare, sorted by name.
 *
 * Shown rather than dropped: the map is open on the wire, a driver may report
 * more than the class declares, and silently discarding it would mean the one
 * fact that explains a failure is the one the console chose not to render.
 * Displayed apart from the six so it is never mistaken for declared vocabulary
 * an automation can match on. */
export function extraAttributes(entity: Entity): { name: string; value: string }[] {
  const declared = new Set(MEDIA_PLAYER_ATTRIBUTES.map((a) => a.name));
  return Object.entries(entity.attributes ?? {})
    .filter(([name]) => !declared.has(name))
    .map(([name, value]) => ({ name, value }))
    .sort((a, b) => a.name.localeCompare(b.name));
}

/** The one-line answer to "what is this screen showing", built from `state` and
 * the attributes together.
 *
 * It never guesses. A device that has reported no state at all says so, because
 * "not reported" and "off" are the difference between a relay that has not
 * polled yet and a TV somebody switched off — and only the second is something
 * to go and look at. */
export function nowShowing(entity: Entity): string {
  const attributes = entity.attributes ?? {};
  const state = entity.state;
  if (state === undefined || state === "") {
    return "No state reported yet — the relay has not polled this device, or could not.";
  }
  if (state === "unavailable") {
    return "The relay could not reach this device at all, so nothing about the screen is known.";
  }
  if (state === "off" || state === "standby") {
    const mode = attributes.power_mode;
    return mode ? `Not powered on (the device reports power mode "${mode}").` : "Not powered on.";
  }
  if (attributes.is_screensaver === "true") {
    return "Powered on, showing its screensaver — no content is on the screen.";
  }
  const app = attributes.active_app;
  if (app !== undefined && app.trim() !== "") {
    const id = attributes.active_app_id;
    return id ? `Powered on, showing ${app} (${id}).` : `Powered on, showing ${app}.`;
  }
  return `Powered on (${state}). The driver did not report what is foregrounded.`;
}
