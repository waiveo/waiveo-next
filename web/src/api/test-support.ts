// Shared fixtures + helpers for the api-client tests (msw, dev-only).
//
// Tests address an ABSOLUTE base in the documentation-only TEST-NET-1 range
// (RFC 5737, 192.0.2.0/24): msw intercepts the request before any socket opens,
// so nothing leaves the process, and undici's fetch (which requires an absolute
// URL) has a parseable host — while the SHIPPED client still defaults to the
// same-origin relative `/api/v1`. Identifiers are fixture ULIDs; no real hosts,
// no secrets.

import { HttpResponse } from "msw";
import type { Problem } from "./client";

export const TEST_ORIGIN = "http://192.0.2.10";
export const TEST_BASE = `${TEST_ORIGIN}/api/v1`;

export const TRACE_ID = "01J8Z3K4N5P6Q7R8S9T0V1W2X4";

// Fixture ULIDs (Crockford base32, sortable) — never real identifiers.
export const ULID_ROOT = "01J8Z0ROOT0000000000000000";
export const ULID_A = "01J8Z3K4N5P6Q7R8S9T0V1W2X3";
export const ULID_B = "01J8Z3K4N5P6Q7R8S9T0V1W2X5";
export const ULID_C = "01J8Z3K4N5P6Q7R8S9T0V1W2X7";

/** The quoted-decimal ETag api/1 derives from a revision (API-020). */
export const etag = (revision: number): string => `"${revision}"`;

/** A JSON success response carrying the api/1 headers a real one would (ETag +
 * Trace-Id). */
export function ok(
  body: Parameters<typeof HttpResponse.json>[0],
  { status = 200, revision }: { status?: number; revision?: number } = {},
) {
  const headers: Record<string, string> = { "Trace-Id": TRACE_ID };
  if (revision !== undefined) headers.ETag = etag(revision);
  return HttpResponse.json(body, { status, headers });
}

/** An RFC 9457 problem+json body with the api/1 `code`/`trace_id` extensions. */
export function problem(
  status: number,
  code: Problem["code"],
  detail: string,
  extra: Partial<Problem> = {},
) {
  const body: Problem = {
    type: "about:blank",
    title: code,
    status,
    detail,
    code,
    trace_id: TRACE_ID,
    ...extra,
  };
  return HttpResponse.json(body, {
    status,
    headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID },
  });
}

/** A fixture scope-node record (the fully-worked resource in the OpenAPI).
 *
 * `labels` is a key→value MAP and `created_at`/`updated_at` are epoch
 * MILLISECONDS, because that is what the server returns — the same shapes the
 * hand-written client types in `./resources` already declare for every other
 * family. A fixture that claimed the array-of-{key,value} / RFC-3339 shapes
 * would let a component pass its tests against a wire format the server has
 * never produced. */
export function scopeNode(over: Record<string, unknown> = {}) {
  return {
    id: ULID_A,
    kind: "screen",
    parent_id: ULID_ROOT,
    name: "Lobby display",
    labels: {},
    revision: 1,
    created_at: 1_753_142_400_000,
    updated_at: 1_753_142_400_000,
    ...over,
  };
}

// ── Declarative-pack fixtures (manifest/1 + ui-schema/1) ─────────────────────

/** The fixture pack id (`publisher/name`, MAN-001) — lowercase, slash-bearing. */
export const PACK_ID = "acme/menu-board";

/** A fixture pack manifest (the console-relevant subset): two pages (a list-detail
 * + a settings-form) and one `menu_items` collection. displayName + titles are
 * `msg:` references resolved against the locale-catalog fixture below. */
export function packManifest(over: Record<string, unknown> = {}) {
  return {
    id: PACK_ID,
    version: "1.0.0",
    displayName: "msg:pack.displayName",
    ui: {
      pages: [
        { path: "menu-items", pageType: "list-detail", titleMsg: "msg:page.menuItems.title" },
        { path: "settings", pageType: "settings-form", titleMsg: "msg:page.settings.title" },
      ],
    },
    dataModel: {
      version: 1,
      collections: [
        {
          name: "menu_items",
          fields: [
            { name: "name", type: "string", role: "title", searchable: true },
            { name: "section", type: "string" },
            { name: "price", type: "number" },
          ],
        },
      ],
    },
    ...over,
  };
}

/** A fixture installed-pack envelope (the registry's list/get shape). */
export function pack(over: Record<string, unknown> = {}) {
  return {
    id: PACK_ID,
    revision: 1,
    version: "1.0.0",
    data_model_version: 1,
    created_at: 1_753_000_000,
    updated_at: 1_753_000_000,
    manifest: packManifest(),
    ...over,
  };
}

/** A fixture pack-data row (a `menu_items` menu item): the declared fields
 * flattened together with the universal entity envelope (MAN-051). */
export function packRow(over: Record<string, unknown> = {}) {
  return {
    entity_id: ULID_A,
    revision: 1,
    lifecycle_state: "published",
    scope_node: ULID_ROOT,
    labels: [],
    template_ref: null,
    params: null,
    name: "Cortado",
    section: "Coffee",
    price: 4.5,
    created_at: 1_753_000_000,
    updated_at: 1_753_000_000,
    ...over,
  };
}

/** The fixture pack's default-locale (en) catalog — BARE keys (no `msg:` prefix),
 * as a pack ships them on disk (MAN-110). */
export const PACK_EN_CATALOG: Record<string, string> = {
  "pack.displayName": "Menu Board",
  "page.menuItems.title": "Menu Items",
  "page.settings.title": "Settings",
  "col.name": "Name",
  "col.section": "Section",
  "detail.title": "Menu item",
  "detail.empty": "Select an item to edit it, or add a new one.",
  "detail.name": "Item name",
  "detail.section": "Section",
  "detail.save": "Save changes",
  "detail.delete": "Delete item",
};

/** A fixture automation resource, matching the live feeder's wire shape (the
 * management-API envelope around a rules/1 Rule: name/scope_node/labels/enabled/
 * mode/max + the rule vocabulary triggers/conditions/actions + the api/1
 * baseline). The compiler's edge/app `execution_class` is a server-internal
 * column, NOT a wire field (openapi Automation is additionalProperties:false), so
 * it is deliberately absent here — the console badges the wire-available `mode`
 * and `enabled` state instead. */
export function automation(over: Record<string, unknown> = {}) {
  return {
    id: ULID_A,
    name: "Open the doors",
    scope_node: ULID_ROOT,
    labels: {},
    enabled: true,
    mode: "single",
    max: null,
    triggers: [{ type: "state", entity_id: ULID_B, to: ["on"] }],
    conditions: [],
    actions: [{ type: "device_command", entity_id: ULID_B, command: "launch", params: { channel: "dev" } }],
    revision: 1,
    created_at: 1_753_142_400_000,
    updated_at: 1_753_142_400_000,
    ...over,
  };
}

// ── Cast + content-origin fixtures (the Studio's two data sources) ───────────

/** A fixture cast: one slide carrying the three most load-bearing layer kinds —
 * a background rect, a title, and a live clock. Deliberately NOT a bare
 * single-layer stub: the Studio's z-order, selection and geometry behaviour are
 * only meaningfully exercised against a real stack. */
export function cast(over: Record<string, unknown> = {}) {
  return {
    id: ULID_A,
    scope_node: ULID_ROOT,
    name: "Lobby loop",
    slides: [
      {
        id: "slide-1",
        duration_ms: 10_000,
        layers: [
          { kind: "rect", x: 0, y: 0, w: 1920, h: 1080, color: "#101020" },
          { kind: "text", x: 120, y: 200, w: 900, h: 160, text: "Welcome", font_px: 96, color: "#ffffff" },
          { kind: "clock", x: 1400, y: 80, w: 420, h: 120, text: "3:04 PM", font_px: 72, color: "#f368c4" },
        ],
      },
    ],
    labels: {},
    revision: 1,
    created_at: 1_753_142_400_000,
    updated_at: 1_753_142_400_000,
    ...over,
  };
}

/** A fixture content-origin listing row (GET /api/v1/content's `content[]`). */
export function contentAsset(over: Record<string, unknown> = {}) {
  return {
    asset_ref: "sha256:aa11bb22cc33",
    url: "/content/aa11bb22cc33",
    size_bytes: 24_576,
    stored_at: 1_753_142_400_000,
    ...over,
  };
}
