/**
 * Realistic signage-platform fixture content for the gallery — screens,
 * schedule dayparts and automations. Every identifier is a fixture ULID and
 * every address is in 192.0.2.0/24 (TEST-NET-1, RFC 5737), so nothing here is
 * real infrastructure and no secret ever appears. The vocabulary is the
 * product's own (dusk, daybreak, daypart, on air, publish) per the brand voice.
 */

import type { Status } from "@/components/kit";

export interface ScreenRow {
  id: string;
  name: string;
  site: string;
  address: string;
  /** Devices governed by this screen. */
  devices: number;
  /** The daypart currently on air (bound to a gradient deck). */
  daypart: string;
  latencyMs: number;
  status: Status;
  /** The broadcast-callout label (rendered UPPERCASE by StatusBadge). */
  statusLabel: string;
}

export const screens: ScreenRow[] = [
  {
    id: "01JQ7ZG9K4RH8V0M3TN2Q6D5S1",
    name: "The Hangar TV",
    site: "Riverside Roastery",
    address: "192.0.2.11",
    devices: 3,
    daypart: "Golden Hour",
    latencyMs: 38,
    status: "ok",
    statusLabel: "On air",
  },
  {
    id: "01JQ7ZG9K4RH8V0M3TN2Q6D5S2",
    name: "Cafe Board",
    site: "Riverside Roastery",
    address: "192.0.2.24",
    devices: 1,
    daypart: "Midday",
    latencyMs: 51,
    status: "ok",
    statusLabel: "On air",
  },
  {
    id: "01JQ7ZG9K4RH8V0M3TN2Q6D5S3",
    name: "Lobby Wall",
    site: "Depot Nine",
    address: "192.0.2.37",
    devices: 2,
    daypart: "Sunrise",
    latencyMs: 240,
    status: "warn",
    statusLabel: "Degraded",
  },
  {
    id: "01JQ7ZG9K4RH8V0M3TN2Q6D5S4",
    name: "Patio Screen",
    site: "Depot Nine",
    address: "192.0.2.52",
    devices: 1,
    daypart: "Nightfall",
    latencyMs: 0,
    status: "off",
    statusLabel: "Offline",
  },
  {
    id: "01JQ7ZG9K4RH8V0M3TN2Q6D5S5",
    name: "Drive-Thru Menu",
    site: "Riverside Roastery",
    address: "192.0.2.68",
    devices: 4,
    daypart: "Midday",
    latencyMs: 74,
    status: "pending",
    statusLabel: "Syncing",
  },
];

export interface DaypartRow {
  /** The gradient deck this daypart tags. */
  cssVar: string;
  name: string;
  window: string;
  program: string;
}

// The schedule dayparts, each tagged with a gradient deck — the schedule grid
// renders as the sky turning across the day (brand/HORIZON.md §4).
export const dayparts: DaypartRow[] = [
  { cssVar: "--grad-deck-1", name: "Sunrise", window: "06:00 – 10:00", program: "Morning open" },
  { cssVar: "--grad-deck-2", name: "Midday", window: "10:00 – 16:00", program: "Lunch & promo" },
  { cssVar: "--grad-deck-3", name: "Golden Hour", window: "16:00 – 20:00", program: "Dinner rush" },
  { cssVar: "--grad-deck-4", name: "Nightfall", window: "20:00 – 00:00", program: "After close" },
];

export interface AutomationRow {
  id: string;
  name: string;
  trigger: string;
  lastRun: string;
  status: Status;
  statusLabel: string;
}

export const automations: AutomationRow[] = [
  {
    id: "01JQ7ZG9K4RH8V0M3TN2Q6DA01",
    name: "Screens on at open",
    trigger: "Sunrise daypart begins",
    lastRun: "5m ago",
    status: "ok",
    statusLabel: "Ran",
  },
  {
    id: "01JQ7ZG9K4RH8V0M3TN2Q6DA02",
    name: "Dim the patio at nightfall",
    trigger: "Nightfall daypart begins",
    lastRun: "Scheduled",
    status: "pending",
    statusLabel: "Scheduled",
  },
  {
    id: "01JQ7ZG9K4RH8V0M3TN2Q6DA03",
    name: "Alert when a screen drops",
    trigger: "Any screen goes offline",
    lastRun: "2m ago",
    status: "warn",
    statusLabel: "Fired",
  },
];
