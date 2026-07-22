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

/** A fixture scope-node record (the fully-worked resource in the OpenAPI). */
export function scopeNode(over: Record<string, unknown> = {}) {
  return {
    id: ULID_A,
    kind: "screen",
    parent_id: ULID_ROOT,
    name: "Lobby display",
    labels: [],
    revision: 1,
    created_at: "2026-07-22T00:00:00Z",
    updated_at: "2026-07-22T00:00:00Z",
    ...over,
  };
}

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
    labels: [],
    enabled: true,
    mode: "single",
    max: null,
    triggers: [{ type: "state", entity_id: ULID_B, to: ["on"] }],
    conditions: [],
    actions: [{ type: "device_command", entity_id: ULID_B, command: "launch", params: { channel: "dev" } }],
    revision: 1,
    created_at: "2026-07-22T00:00:00Z",
    updated_at: "2026-07-22T00:00:00Z",
    ...over,
  };
}
