// Can this entity actually be driven — and if not, exactly which precondition
// is missing?
//
// # Why a page needs this at all
//
// The relay's rule is one sentence (internal/relay/devicetargets' package doc):
// an entity is controllable IF AND ONLY IF the app peer adopted it AND enabled
// it AND the relay can currently locate it. It is a safety property, not a
// convenience — every media player a relay can see but nobody adopted resolves
// to nothing, which is what stops two controllers fighting over one TV.
//
// The cost of that property is that a command against an entity failing any of
// the three comes back as one undifferentiated `COMMAND_UNRESOLVED`, AFTER the
// operator has pressed a button and watched the TV not move. Every one of the
// three is knowable from data api/1 already publishes, so the page states them
// BEFORE the press instead of leaving the operator to infer them from a code.
//
// # The join this is built on, and why it is sound
//
// `AdoptedDevice` and `Device` share no member — the adoption record is keyed by
// `(site, driver, native_id)` and the discovered row publishes neither half. But
// `AdoptedDevice.entities[].entity_id` and `Entity.id` ARE the same identifier:
// both are derived from the identity tuple plus the relay's own addressing key
// (`internal/shared/deviceid`, called from `internal/app/devices/intake.go` for
// the discovered row and from `internal/app/store/discovereddevices.go` for the
// adoption record), and their being the same is precisely what lets the relay
// recognize the id the app peer later addresses a command to. So this file joins
// on ENTITY id, never on device id, and never on the tuple.
//
// That join is also what makes `driver` readable at all: api/1's `Device` has no
// driver member, so `roku-ecp` is only knowable for a device whose adoption
// record can be found.

import type { AdoptedDevice, AdoptedDeviceEntity, Device, Entity } from "@/api";

export type ReadinessCode =
  /** No adoption record: the relay resolves the entity (it discovered it) and
   * then refuses the command for having no target it may drive. */
  | "not-adopted"
  /** Adopted, but this entity is not in the record — a record's entity list is
   * a snapshot taken at adoption, so an entity the device began reporting
   * afterwards is not in the inventory the relay was sent. */
  | "entity-not-in-record"
  /** In the record and switched off. A disabled entity is filtered out of the
   * relay's drivable set entirely, rather than being present-and-refused. */
  | "entity-disabled"
  /** No address on the discovered row: nothing to dial. */
  | "no-address";

export interface ReadinessProblem {
  code: ReadinessCode;
  title: string;
  detail: string;
}

export interface Readiness {
  /** True only when NOTHING is missing. A page must not offer a control that
   * can only ever answer COMMAND_UNRESOLVED. */
  controllable: boolean;
  problems: ReadinessProblem[];
  /** The adoption record carrying this entity, when one was found. */
  record: AdoptedDevice | null;
  /** This entity's own authored policy on that record. */
  policy: AdoptedDeviceEntity | null;
}

/** Find the adoption record that carries this entity, by the one identifier the
 * two families genuinely share. Returns both the record and the entity's own
 * policy row on it, so a caller never has to re-scan the array. */
export function findAdoption(
  entityId: string,
  records: AdoptedDevice[],
): { record: AdoptedDevice; policy: AdoptedDeviceEntity } | null {
  for (const record of records) {
    for (const policy of record.entities) {
      if (policy.entity_id === entityId) return { record, policy };
    }
  }
  return null;
}

/**
 * Assess one entity against the relay's three preconditions.
 *
 * `records` may legitimately be EMPTY for a caller that could not read
 * `/adopted-devices` — in that case an adopted device reports
 * `entity-not-in-record`, which is the honest answer: the console cannot see the
 * record, so it cannot say the entity is in it. It is worded as a thing to check
 * rather than as a fault.
 */
export function assessReadiness(
  device: Device,
  entity: Entity,
  records: AdoptedDevice[],
): Readiness {
  const problems: ReadinessProblem[] = [];
  const found = findAdoption(entity.id, records);

  if (!device.adopted) {
    problems.push({
      code: "not-adopted",
      title: "Not adopted",
      detail:
        "The relay has seen this device but nothing has been decided about it, so it holds no target it may drive. Every command will be refused COMMAND_UNRESOLVED. Adopt it on the Devices page.",
    });
  } else if (found === null) {
    problems.push({
      code: "entity-not-in-record",
      title: "This entity is not on the adoption record",
      detail:
        "The device is adopted, but no adoption record visible here lists this entity. A record's entity list is fixed when the device is adopted, so an entity the device only began reporting afterwards is absent from the inventory its relay was sent — and the relay refuses commands to it.",
    });
  } else if (!found.policy.enabled) {
    problems.push({
      code: "entity-disabled",
      title: "Disabled on the adoption record",
      detail:
        "This entity is recorded as one not to act on, so its relay drops it from the drivable set entirely. Enable it on the adopted-device record before commanding it.",
    });
  }

  if (device.address === undefined || device.address.trim() === "") {
    problems.push({
      code: "no-address",
      title: "No address was reported",
      detail:
        "Discovery found this device but no dialable address for it, so the relay has nowhere to send a command. Unless the deployment pins one out of band on the relay itself, commands will be refused.",
    });
  }

  return {
    controllable: problems.length === 0,
    problems,
    record: found?.record ?? null,
    policy: found?.policy ?? null,
  };
}
