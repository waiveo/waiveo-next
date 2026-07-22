/**
 * A type-level test (checked by `tsc --noEmit`, not run by vitest): it proves the
 * api/1 optimistic-concurrency contract is enforced by the COMPILER, not merely
 * documented. `update` and `remove` require an ETag argument, so a mutation
 * without a prior read — the read is what yields the ETag — cannot type-check.
 * There is no unconditional-overwrite overload (API-022). If that requirement
 * were ever loosened, the `@ts-expect-error` directives below would become unused
 * and `tsc` would fail here — so this file is the guarantee.
 */
import { createApi } from "./resources";

const api = createApi({ baseUrl: "http://192.0.2.10/api/v1" });
const id = "01J8Z3K4N5P6Q7R8S9T0V1W2X3";

// Happy path: an update carries the ETag captured from a prior read.
export async function updatesWithEtag() {
  const read = await api.scopeNodes.get(id);
  await api.scopeNodes.update(id, { name: "Renamed" }, read.etag);
  await api.scopeNodes.remove(id, read.etag);
}

// Contract: an update WITHOUT an ETag must NOT type-check.
export function updateWithoutEtagIsRejected() {
  // @ts-expect-error — update requires an If-Match ETag; no unconditional-overwrite path exists.
  return api.scopeNodes.update(id, { name: "Renamed" });
}

// Contract: a delete WITHOUT an ETag must NOT type-check.
export function removeWithoutEtagIsRejected() {
  // @ts-expect-error — remove requires an If-Match ETag; no unconditional-overwrite path exists.
  return api.scopeNodes.remove(id);
}
