// scripts/codegen/typecheck/smoke.ts
//
// Type-only smoke test over openapi-typescript's generated output
// (api/gen/ts/api.d.ts). This is not application code — nothing here runs;
// `tsc --noEmit` compiling this file is the check. It exists so a
// schema-breaking edit to api/openapi.yaml (a renamed/removed field, a
// loosened/tightened type, a dropped response code) fails a real compile
// instead of only "the generator didn't crash." Every shape referenced
// below is something a real consumer (the web UI, the CLI/MCP Go client's
// TS-side counterpart, a test) would actually import.
import type { components, operations } from "../../../api/gen/ts/api.js";

// --- components.schemas: the two fully-worked exemplar resources ---

const scopeNode: components["schemas"]["ScopeNode"] = {
  id: "01J8Z3K4N5P6Q7R8S9T0V1W2X3",
  kind: "site",
  parent_id: "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
  name: "Example Site",
  labels: [{ key: "env", value: "prod" }],
  revision: 1,
  created_at: "2026-07-15T00:00:00Z",
  updated_at: "2026-07-15T00:00:00Z",
};

const automation: components["schemas"]["Automation"] = {
  id: "01J8Z3K4N5P6Q7R8S9T0V1W2Z1",
  name: "Lobby screens on at open",
  scope_node: scopeNode.id,
  labels: [],
  enabled: true,
  mode: "single",
  max: null,
  triggers: [{ type: "state", entity_id: "01J8Z3K4N5P6Q7R8S9T0V1W2Z2", to: ["on"] }],
  conditions: [],
  actions: [{ type: "device_command", entity_id: "01J8Z3K4N5P6Q7R8S9T0V1W2Z2", command: "launch" }],
  revision: 1,
  created_at: "2026-07-15T00:00:00Z",
  updated_at: "2026-07-15T00:00:00Z",
};

// --- components.schemas: the shared conventions ---

const problem: components["schemas"]["Problem"] = {
  type: "about:blank",
  title: "Not Found",
  status: 404,
  code: "NOT_FOUND",
  trace_id: "01J8Z3K4N5P6Q7R8S9T0V1W2X4",
};

// A code outside the registry must fail to type-check — this line is
// intentionally commented out; uncommenting it should NOT compile:
// const badCode: components["schemas"]["ErrorCode"] = "NOT_A_REAL_CODE";

// --- operations: the paginated-list envelope on both exemplars ---

const scopeNodePage: operations["listScopeNodes"]["responses"][200]["content"]["application/json"] = {
  items: [scopeNode],
  cursor: null,
};

const automationPage: operations["listAutomations"]["responses"][200]["content"]["application/json"] = {
  items: [automation],
  cursor: "01J8Z3K4N5P6Q7R8S9T0V1W2ZZ",
};

// --- operations: ETag on create, Idempotency-Key on the run action ---

const createdEtag: operations["createScopeNode"]["responses"][201]["headers"]["ETag"] = '"1"';

const runHeaders: NonNullable<operations["runAutomation"]["parameters"]["header"]> = {
  "Idempotency-Key": "client-chosen-replay-key-1",
};

const runResult: operations["runAutomation"]["responses"][200]["content"]["application/json"] = {
  run_id: "01J8Z3K4N5P6Q7R8S9T0V1W2Z9",
  disposition: "ran",
};

// --- operations: the shared Problem responses a caller actually branches on ---

const conflict: operations["createScopeNode"]["responses"][409] = {
  headers: {},
  content: { "application/problem+json": problem },
};

export {
  scopeNode,
  automation,
  problem,
  scopeNodePage,
  automationPage,
  createdEtag,
  runHeaders,
  runResult,
  conflict,
};
