// api/1 — the DEVICE PLANE's client surface: the devices a relay has discovered,
// the entities those devices expose, the one command operation addressed at an
// entity, and the adopted-device rows that record what this deployment has
// actually taken responsibility for.
//
// It is a separate module from resources.ts for the same reason the server keeps
// two packages: these three families do not share one lifecycle. `/devices` and
// `/entities` are a READ MODEL a relay owns (internal/app/devices) — no create,
// no update, no delete, no `revision`, and therefore no ETag/If-Match to carry.
// `/adopted-devices` is an AUTHORED row this console writes (internal/app/store)
// and is an ordinary api/1 CRUD family. Modelling the read model with the full
// ResourceModule would hand a page four verbs the server does not serve.
//
// # Discovery is not adoption, and the UI must not blur them
//
// A device appearing in `/devices` means one thing only: a relay reported seeing
// it on its LAN (relay/1 REL-110/111). Nothing about it has been decided. What
// makes a device THIS deployment's is an adopted-device row keyed by REL-153's
// `(site, driver, native_id)` identity — poll cadence and per-entity
// enabled/hidden/display-name/category are decisions no discovery sweep can
// make.
//
// # Adoption is a SERVER operation, not a create body this console assembles
//
// The distinction above is real, but the console cannot act on it by writing an
// adoption record itself, and it must not pretend otherwise. An adoption record
// is keyed by `(site, driver, native_id)`, and `Device` publishes NEITHER half
// of that tuple: `device_id` is a one-way derivation of it on the server
// (internal/shared/deviceid) and cannot be run backwards in a browser, and
// `labels` is api/1 AUTHORED data that discovery never writes into (the server
// mints every discovered device with an empty label map). A console that tried
// to compose the create body would be composing it out of nothing.
//
// So adoption goes through `adopt` below — `POST /devices/{id}/adopt`, keyed by
// the one identifier the row does carry. The server already holds the identity
// tuple in its durable discovered-device mirror and builds the record from it,
// returning the device as it now reads. Correspondingly, `adopted` is a FLAG
// THE ROW CARRIES (`Device.adopted`), read straight off the device — not a join
// this console computes, which is a join it has no key to perform.
//
// `/adopted-devices` remains an ordinary CRUD family for REFINING that record
// afterwards (cadence, per-entity policy) and for releasing it. What it is not
// is the way a device gets adopted in the first place.
//
// # Why `deviceFacts` reads two places for the same fact
//
// `address`, `model` and `serial` ARE top-level members of Device. They are
// also, historically, the kind of fact a deployment surfaced through `labels`
// before the schema carried them. Two things follow, and `deviceFacts` exists
// because of both:
//
//   - A deployment on the widened schema serves the top-level member.
//   - `labels` is the one member of the row that IS open-ended, and it is the
//     additive-safe place a richer discovery report can surface a fact the
//     schema does not carry yet.
//
// So the reader prefers a top-level member and falls back to the same-named
// label, and returns null rather than "" for a fact nobody reported — a UI must
// be able to tell "no address" from "the empty string", because only the first
// is honest.
//
// It still reports `driver`/`nativeId`, which no schema member carries, because
// they are worth SHOWING an operator where a deployment surfaces them as
// labels. Nothing gates on them any more: adoption no longer needs them
// client-side.

import { ApiClient } from "./client";
import { crud } from "./crud";
import { paginate, type Page, type PageFetcher } from "./pagination";
import type { ListParams, ResourceModule } from "./resources";
import type { components } from "../../../api/gen/ts/api";

// ── Generated (contract-canonical) types ────────────────────────────────────

/** One physical device a relay has discovered behind it (read-only, REL-110). */
export type Device = components["schemas"]["Device"];
/** One addressable object a device exposes — the unit a command is addressed to. */
export type Entity = components["schemas"]["Entity"];
/** One device command to execute against an entity (REL-112). */
export type EntityCommandRequest = components["schemas"]["EntityCommandRequest"];
/** The relay's own answer: `ok`, plus a typed `error` when it is false. */
export type EntityCommandResult = components["schemas"]["EntityCommandResult"];

/** The authored adoption record — the row a `device_id` names (DAT-004a). */
export type AdoptedDevice = components["schemas"]["AdoptedDevice"];
export type AdoptedDeviceCreate = components["schemas"]["AdoptedDeviceCreate"];
export type AdoptedDeviceUpdate = components["schemas"]["AdoptedDeviceUpdate"];
/** Per-entity adoption POLICY (REL-063) — every member a decision, not a fact. */
export type AdoptedDeviceEntity = components["schemas"]["AdoptedDeviceEntity"];

// ── The read-only list surface ──────────────────────────────────────────────

/** A family this API only lists. Deliberately NOT a `ResourceModule`: there is
 * no get/create/update/remove to expose, and offering them would be four calls
 * that answer 405. */
export interface ListOnlyModule<T> {
  readonly path: string;
  list(params?: ListParams): Promise<Page<T>>;
  pages(params?: Omit<ListParams, "cursor">): AsyncGenerator<T, void, void>;
}

/** Build the list half of the api/1 CRUD conventions (selector + keyset cursor)
 * for a family that has no other half. Mirrors `crud`'s own list/pages exactly —
 * including omitting an empty cursor, which is not a keyset position and which
 * the server rejects (API-035). */
function listOnly<T>(client: ApiClient, path: string): ListOnlyModule<T> {
  const mod: ListOnlyModule<T> = {
    path,
    list(params = {}) {
      const query: Record<string, string | number | undefined> = {};
      if (params.selector) query.selector = params.selector;
      if (params.limit !== undefined) query.limit = params.limit;
      if (params.cursor) query.cursor = params.cursor;
      return client.list<T>(path, query);
    },
    pages(params: Omit<ListParams, "cursor"> = {}) {
      const fetchPage: PageFetcher<T> = (cursor) => mod.list({ ...params, cursor });
      return paginate(fetchPage);
    },
  };
  return mod;
}

// ── Devices: list + the one mutating operation ──────────────────────────────

export interface DevicesModule extends ListOnlyModule<Device> {
  /** Adopt ONE discovered device — put it under this platform's control by
   * creating the durable adoption record its relay is then sent in signed
   * desired state (relay/1 REL-063).
   *
   * Takes a device id and NOTHING else, and that is the whole point: the
   * identity tuple the record is keyed by is not on the row (see this module's
   * header), so the server derives it from its own discovered-device mirror.
   * There is no request body to get wrong, and no placement or cadence to ask
   * an operator for at this moment — the record is created with the device's
   * own reported entities enabled and primary, and that policy is refined
   * afterwards through `adoptedDevices`.
   *
   * Returns the device as it now reads, with `adopted` true — so a caller can
   * update one row from the answer rather than re-listing the fleet.
   *
   * Idempotent twice over: adopting an already-adopted device succeeds and
   * changes nothing, and the POST additionally carries an Idempotency-Key (the
   * ApiClient's `action` convention) so a retry-on-timeout replays the recorded
   * outcome. A double-click is not an error. */
  adopt(deviceId: string): Promise<Device>;

  /** Retire ONE device's stored `first_seen`, so the next report from its relay
   * plants a fresh one from the platform's own clock.
   *
   * The platform's only correction path for a device's age. `first_seen` is
   * planted once and never moves — which is what keeps a relay restart from
   * re-dating a whole network as new — so a WRONG value is permanent too, and
   * one population of wrong values exists by construction: a deployment
   * upgraded from a build predating the durable ledger inherited its ages from
   * a column written off the reporting relay's own unattested clock.
   * `first_seen_origin` is how a row says it is one of those.
   *
   * It can only make a device read YOUNGER, and it asserts no instant of its
   * own — there is deliberately no counterpart that SETS one. Naturally
   * idempotent: retiring a device with no stored age succeeds and changes
   * nothing. `last_seen` is untouched, because a device that reported a minute
   * ago must not start reading as never heard from.
   *
   * Returns the device as it now reads, with `first_seen` absent. */
  retireFirstSeen(deviceId: string): Promise<Device>;
}

function devicesModule(client: ApiClient): DevicesModule {
  const base = listOnly<Device>(client, "/devices");
  return {
    ...base,
    adopt(deviceId) {
      // No body: the operation declares none, and `action` omits Content-Type
      // entirely when given none, which is what a bodyless POST should send.
      return client.action<Device>(`/devices/${encodeURIComponent(deviceId)}/adopt`);
    },
    retireFirstSeen(deviceId) {
      return client.discardReturning<Device>(
        `/devices/${encodeURIComponent(deviceId)}/first-seen`,
      );
    },
  };
}

// ── Entities: list + the one mutating operation ─────────────────────────────

export interface EntitiesModule extends ListOnlyModule<Entity> {
  /** Dispatch ONE command to ONE already-resolved entity and return the relay's
   * own correlated answer (REL-112).
   *
   * Two things this signature is deliberate about. It takes an entity id and
   * not a selector, because relay/1 accepts neither a selector nor a
   * device-class filter — fanning a command across a set is the caller's
   * concern. And its result is a VALUE, not a thrown error: a `200` carrying
   * `ok:false` is a completed exchange whose command did not succeed (an
   * undeclared command, an unreachable TV), which is a different thing from the
   * request failing, and a caller must be able to report the relay's own code
   * rather than a generic failure. Only the request failing throws.
   *
   * The POST carries an Idempotency-Key (the ApiClient's `action` convention),
   * so a retry-on-timeout replays the retained result instead of firing the
   * command at the physical device twice — which for `power` or `launch` is the
   * difference between one action and two. */
  sendCommand(
    entityId: string,
    command: string,
    params?: Record<string, unknown>,
  ): Promise<EntityCommandResult>;
}

function entitiesModule(client: ApiClient): EntitiesModule {
  const base = listOnly<Entity>(client, "/entities");
  return {
    ...base,
    sendCommand(entityId, command, params) {
      // Built conditionally rather than with `params: params ?? undefined`: the
      // request schema is `additionalProperties:false` and an explicit
      // `"params": undefined` is not a member JSON.stringify drops in every
      // engine's spelling of the same intent — omitting the key is.
      const body: EntityCommandRequest = params === undefined ? { command } : { command, params };
      return client.action<EntityCommandResult>(
        `/entities/${encodeURIComponent(entityId)}/commands`,
        body,
      );
    },
  };
}

// ── The device plane, composed ──────────────────────────────────────────────

export interface DevicePlaneModules {
  /** Discovered devices (a relay owns this view) + the one operation that
   * decides something about one: `adopt`. */
  devices: DevicesModule;
  /** Entities + the entity-addressed command operation. */
  entities: EntitiesModule;
  /** The authored adoption records — a full api/1 CRUD family. */
  adoptedDevices: ResourceModule<AdoptedDevice, AdoptedDeviceCreate, AdoptedDeviceUpdate>;
}

export function createDevicePlaneModules(client: ApiClient): DevicePlaneModules {
  return {
    devices: devicesModule(client),
    entities: entitiesModule(client),
    adoptedDevices: crud<AdoptedDevice, AdoptedDeviceCreate, AdoptedDeviceUpdate>(
      client,
      "/adopted-devices",
    ),
  };
}

// ── Discovery facts a Device may or may not carry ───────────────────────────

/** What a discovered device reported about itself beyond its name, as far as
 * this deployment's `/devices` representation actually says.
 *
 * Every member is `null` when nothing reported it. `driver`/`nativeId` are the
 * two that gate adoption: without both there is no REL-153 identity tuple to
 * write an adoption record against, and the console must say so rather than
 * invent one. */
export interface DeviceFacts {
  /** The device's address on the relay's LAN (an IP or host), if reported. */
  address: string | null;
  /** The device's self-reported model, if reported. */
  model: string | null;
  /** Half of REL-153's identity tuple — the driver that speaks to it. */
  driver: string | null;
  /** The other half — the device's own id on its LAN (for a Roku, its SSDP USN). */
  nativeId: string | null;
}

/** Read one string fact off a device, preferring a top-level member over the
 * same-named label. Returns null for absent, non-string, or blank — a blank
 * string is a report of nothing, and rendering it as an address would be a lie
 * shaped like data. */
function fact(device: Device, key: string): string | null {
  const top = (device as unknown as Record<string, unknown>)[key];
  const raw = typeof top === "string" ? top : device.labels?.[key];
  if (typeof raw !== "string") return null;
  const trimmed = raw.trim();
  return trimmed === "" ? null : trimmed;
}

/** The discovery facts a Device carries, from wherever this deployment puts
 * them (see the module header for why there are two places). */
export function deviceFacts(device: Device): DeviceFacts {
  return {
    address: fact(device, "address"),
    model: fact(device, "model"),
    driver: fact(device, "driver"),
    nativeId: fact(device, "native_id"),
  };
}

/** The launchable channels a device's labels advertise, as `app.<channel-id>` →
 * display name, sorted by display name.
 *
 * api/1 publishes no channel inventory: the media-player class's `launch` takes
 * a channel id and the Entity representation carries only `state`, so there is
 * no contract member listing what a TV has installed. This reads the one
 * open-ended member that could carry it and renders nothing at all when it does
 * not — the remote's free-text channel field is the path that always works, and
 * this is the shortcut when a deployment has published the list. */
export function launchableApps(device: Device): { channel: string; name: string }[] {
  const apps: { channel: string; name: string }[] = [];
  for (const [key, value] of Object.entries(device.labels ?? {})) {
    if (!key.startsWith("app.")) continue;
    const channel = key.slice("app.".length);
    if (channel === "" || typeof value !== "string" || value.trim() === "") continue;
    apps.push({ channel, name: value.trim() });
  }
  return apps.sort((a, b) => a.name.localeCompare(b.name));
}
