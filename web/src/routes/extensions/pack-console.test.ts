import { describe, it, expect } from "vitest";
import { ApiError, type Pack, type PackInstallRecord, type Problem } from "@/api";
import { pack, packManifest } from "@/api/test-support";
import {
  describeInstall,
  describeRefusal,
  describeUpdate,
  hasCode,
  packHealth,
  packProvenance,
  REQUIRED_PACK_FLOOR,
} from "./pack-console";

// The Extensions console's decisions, driven directly: which refusal this is,
// what the install record permits, and which declared pages an operator can
// actually open. Each of these is a place the page could quietly mislead — offer
// an update the box refuses, present a removal the deployment forbids as
// available, or list a page the nav has dropped — so each is asserted against the
// exact shape the server emits rather than through the rendered page.

function apiError(status: number, over: Partial<Problem>): ApiError {
  const problem: Problem = {
    type: "about:blank",
    title: "Validation Failed",
    status,
    code: "VALIDATION_FAILED",
    trace_id: "01J8Z3K4N5P6Q7R8S9T0V1W2X4",
    ...over,
  };
  return new ApiError(status, problem, problem.trace_id ?? null);
}

function record(over: Partial<PackInstallRecord> = {}): PackInstallRecord {
  return {
    id: "01J8Z3K4N5P6Q7R8S9T0V1W2X3",
    pack_id: "acme/menu-board",
    resolved_version: "1.0.0",
    trust_channel: "community",
    source: "https://registry.example/index.json",
    stale_source: false,
    content_digest: "sha256:aa11",
    key_id: "key-1",
    artifact_digest: "sha256:bb22",
    installed_at: 1_753_000_000_000,
    ...over,
  };
}

describe("describeRefusal", () => {
  it("finds a marketplace code carried INSIDE errors[] beneath a generic top-level code", () => {
    // This is the shape the pack handlers actually emit today: the api/1 top-level
    // registry is closed, so REQUIRED_PACK_FLOOR rides in errors[] under
    // VALIDATION_FAILED. A console reading only `problem.code` would render a
    // required-pack refusal as an anonymous validation failure.
    const refusal = describeRefusal(
      apiError(422, {
        detail:
          "acme/menu-board is a required pack on this deployment (floor version 1.0.0) and cannot be uninstalled.",
        errors: [
          { field: "pack", code: REQUIRED_PACK_FLOOR, message: "required pack floor 1.0.0" },
        ],
      }),
    );
    expect(hasCode(refusal, REQUIRED_PACK_FLOOR)).toBe(true);
    expect(refusal.codes).toEqual(["VALIDATION_FAILED", REQUIRED_PACK_FLOOR]);
    expect(refusal.detail).toContain("floor version 1.0.0");
    expect(refusal.fields).toHaveLength(1);
  });

  it("ALSO finds the same code at the top level, where the openapi registry admits it", () => {
    // openapi's ErrorCode enum carries REQUIRED_PACK_FLOOR and
    // MARKETPLACE_REF_INVALID as top-level codes. If the server ever moves the
    // code up there, the console must keep recognising it — silently losing the
    // refusal on a server refactor is the failure this covers.
    const refusal = describeRefusal(
      apiError(422, { code: REQUIRED_PACK_FLOOR, detail: "required here" }),
    );
    expect(hasCode(refusal, REQUIRED_PACK_FLOOR)).toBe(true);
  });

  it("keeps every per-field manifest violation, not just the first", () => {
    const refusal = describeRefusal(
      apiError(422, {
        detail: "The pack manifest failed validation.",
        errors: [
          { field: "capabilities[0]", code: "UNKNOWN_CAPABILITY", message: "unknown capability" },
          { field: "dataModel.version", code: "DATAMODEL_VERSION_REGRESSION", message: "regressed" },
        ],
      }),
    );
    expect(refusal.fields.map((f) => f.code)).toEqual([
      "UNKNOWN_CAPABILITY",
      "DATAMODEL_VERSION_REGRESSION",
    ]);
  });

  it("reports an unreachable box as exactly that, never as a validation problem", () => {
    const refusal = describeRefusal(new TypeError("Failed to fetch"));
    expect(refusal.status).toBe(0);
    expect(refusal.codes).toEqual([]);
    expect(refusal.detail).toMatch(/could not be reached/i);
  });
});

describe("packProvenance", () => {
  it("reads the NEWEST record — the last one, since the history is oldest-first", () => {
    const p = packProvenance([
      record({ id: "a", resolved_version: "1.0.0", trust_channel: null }),
      record({ id: "b", resolved_version: "1.1.0", trust_channel: "verified" }),
    ]);
    expect(p.newest?.resolved_version).toBe("1.1.0");
    expect(p.channel).toBe("verified");
    expect(p.autoTracked).toBe(true);
  });

  it("refuses to call a DIRECT install auto-tracked, and says why", () => {
    // MKT-094a: no channel pinned means nothing to re-resolve, and the host must
    // not default one. An update check on this pack can only be refused, so the
    // console must not offer it.
    const p = packProvenance([record({ trust_channel: null })]);
    expect(p.autoTracked).toBe(false);
    expect(p.channel).toBeNull();
    expect(p.reason).toMatch(/no trust channel pinned/i);
  });

  it("treats an empty history as not auto-trackable rather than as a channelled install", () => {
    const p = packProvenance([]);
    expect(p.autoTracked).toBe(false);
    expect(p.newest).toBeNull();
    expect(p.reason).toMatch(/no install record/i);
  });
});

describe("packHealth", () => {
  it("routes each declared page under the pack's own /p/{pack}/ namespace", () => {
    const health = packHealth(pack() as unknown as Pack);
    expect(health.pages.map((p) => p.href)).toEqual([
      "/p/acme/menu-board/menu-items",
      "/p/acme/menu-board/settings",
    ]);
    expect(health.collections).toEqual(["menu_items"]);
    expect(health.unreachablePages).toEqual([]);
  });

  it("NAMES a page whose declared path escapes the pack's namespace instead of dropping it", () => {
    // The nav drops these silently, which is right for a nav and wrong for a
    // management console: the operator installed a pack declaring three pages and
    // can reach one, with nothing anywhere saying so.
    const p = pack({
      manifest: packManifest({
        ui: {
          pages: [
            { path: "../../design", pageType: "list-detail", titleMsg: "msg:a" },
            { path: "menu-items", pageType: "list-detail", titleMsg: "msg:b" },
            { path: "%2e%2e/%2e%2e/scope-nodes", pageType: "settings-form", titleMsg: "msg:c" },
          ],
        },
      }),
    }) as unknown as Pack;
    const health = packHealth(p);
    expect(health.unreachablePages).toEqual(["../../design"]);
    // A literal `%2e%2e` is not a traversal — it is confined by re-encoding, so it
    // stays REACHABLE and inert rather than being reported as broken.
    expect(health.pages.map((pg) => pg.href)).toEqual([
      "/p/acme/menu-board/menu-items",
      "/p/acme/menu-board/%252e%252e/%252e%252e/scope-nodes",
    ]);
  });
});

describe("describeUpdate", () => {
  it("says nothing was written when the channel still points at the installed version", () => {
    const msg = describeUpdate({
      action: "unchanged",
      id: "acme/menu-board",
      from_version: "1.0.0",
      to_version: "1.0.0",
    });
    expect(msg).toMatch(/already current at 1\.0\.0/i);
    expect(msg).toMatch(/nothing was written/i);
  });

  it("names both versions on an applied update, and says it is live without a restart", () => {
    const msg = describeUpdate({
      action: "updated",
      id: "acme/menu-board",
      from_version: "1.0.0",
      to_version: "1.1.0",
    });
    expect(msg).toContain("1.0.0 → 1.1.0");
    expect(msg).toMatch(/not restarted/i);
  });

  it("explains a REVERT as a publisher withdrawal, so a lower number does not read as a failure", () => {
    const msg = describeUpdate({
      action: "reverted",
      id: "acme/menu-board",
      from_version: "1.2.0",
      to_version: "1.1.0",
    });
    expect(msg).toContain("1.2.0 → 1.1.0");
    expect(msg).toMatch(/withdrawn by its publisher/i);
  });
});

describe("describeInstall", () => {
  it("distinguishes a fresh install from one that replaced a pack in place", () => {
    expect(describeInstall("acme/menu-board", "1.0.0", [])).toMatch(/^Installed acme\/menu-board/);
    expect(
      describeInstall("acme/menu-board", "1.1.0", [{ id: "acme/menu-board", version: "1.0.0" }]),
    ).toContain("1.0.0 → 1.1.0");
    expect(
      describeInstall("acme/menu-board", "1.0.0", [{ id: "acme/menu-board", version: "1.0.0" }]),
    ).toMatch(/re-installed/i);
  });
});
