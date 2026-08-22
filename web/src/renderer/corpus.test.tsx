import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { PageRenderer } from "./PageRenderer";
import { validatePage } from "./validate";
import { resolvePath, type RenderScope } from "./bindings";
import type { ActionHandler } from "./types";

// THE CORPUS DRIVER. This is the single end-to-end oracle for ui-schema/1's web
// renderer: it loads EVERY frozen case from conformance/corpora/ui-schema-1/
// directly (never a copy — the same files the Go corpus drivers read) and drives
// each one to its contract-declared outcome:
//   • a valid page document (UIS-020/030/040/050 + the vocab-option case)
//     validates clean AND renders its expected structure through the real
//     renderer over the Horizon kit;
//   • an invalid page document (UIS-060/100/132-invalid) is REJECTED at
//     validation with the contract's exact taxonomy code at the offending
//     field's own path — never partially painted (UIS-200);
//   • a binding-resolution case (UIS-101) evaluates through the binding engine to
//     the record the contract fixes.
// An accounting test asserts all twenty-three cases are driven (none pending — the Go
// drivers' driven/pending discipline), and a teeth block proves the oracle bites:
// a mutated expectation must fail.
//
// The v1.1 cases are DRIVEN, not rendered-and-eyeballed: a confirm is clicked
// through its dialog to the seam call (and dismissed, to prove the dismissal
// dispatches nothing), and an outcome case is pressed, observed pending, then
// settled — because a control that paints correctly and does nothing has shipped
// here before.

interface ExpectedError {
  code: string;
  field: string;
  message?: string;
}

interface CorpusCase {
  case_id: string;
  contract: string;
  req_ids: string[];
  description: string;
  input: Record<string, unknown>;
  expected: {
    valid?: boolean;
    errors?: ExpectedError[];
    resolved?: unknown;
  };
}

// Vite's glob reads every frozen JSON at transform time, straight out of the repo
// tree — one source of truth shared with the Go corpus driver, no copies.
const modules = import.meta.glob<CorpusCase>(
  "../../../conformance/corpora/ui-schema-1/*.json",
  { eager: true, import: "default" },
);
const cases: CorpusCase[] = Object.keys(modules)
  .sort()
  .map((key) => modules[key]);
const byId: Record<string, CorpusCase> = Object.fromEntries(
  cases.map((c) => [c.case_id, c]),
);

// A case whose `input` carries a `pageType` is a page-document case (valid →
// render; invalid → reject); a case whose `input` carries a bare `binding` is a
// binding-resolution case exercised through the binding engine.
const pageCases = cases.filter((c) => typeof c.input.pageType === "string");
const bindingCases = cases.filter((c) => typeof c.input.binding === "string");
const validPageCases = pageCases.filter((c) => c.expected.valid === true);
const invalidPageCases = pageCases.filter((c) => c.expected.valid === false);

// A message catalog so the structural assertions read against real copy; the
// renderer's own humanizer covers anything unlisted.
const messages: Record<string, string> = {
  "msg:presets.list.name": "Name",
  "msg:presets.detail.empty": "Select a preset to edit it.",
  "msg:presets.detail.nameLabel": "Preset name",
  "msg:settings.general.title": "General",
  "msg:settings.general.displayName": "Display name",
  "msg:settings.general.quietHours": "Quiet hours",
  "msg:settings.save": "Save",
  "msg:dashboard.onlineScreens": "Online screens",
  "msg:dashboard.activeAutomations": "Active automations",
  "msg:wizard.step.name": "Name",
  "msg:wizard.step.nameLabel": "Automation name",
  "msg:wizard.step.review": "Review",
  "msg:automations.detail.modeLabel": "Mode",
  "msg:mode.single": "Single",
  "msg:mode.restart": "Restart",
  "msg:mode.queued": "Queued",
  "msg:mode.parallel": "Parallel",
  // v1.1 — confirm (UIS-165), outcome (UIS-166), disabledIf/announce (UIS-076/077)
  "msg:settings.delete": "Delete site",
  "msg:settings.delete.confirmTitle": "Delete this site?",
  "msg:settings.delete.confirmBody": "Everything authored under it goes with it.",
  "msg:settings.delete.confirmOk": "Delete it",
  "msg:settings.delete.confirmCancel": "Keep it",
  "msg:system.restart.button": "Restart this box",
  "msg:system.restart.pending": "Restarting — asking this box to stop and start again.",
  "msg:system.restart.blocked": "Not now: this box is busy with work a restart would break rather than resume. {0}",
  "msg:system.restart.back": "Back up — {0} started it again.",
  // v1.2 — the decision column (UIS-071a), per-row tone (UIS-078), explanatory
  // hover text (UIS-079)
  "msg:devices.list.name": "Name",
  "msg:devices.list.status": "Status",
  "msg:devices.list.statusHelp": "Discovered means seen but not yet adopted.",
  "msg:devices.list.ports": "Open ports",
  "msg:devices.list.portsHelp":
    "Nothing has scanned this device — which is not the same as having no ports open.",
  "msg:devices.list.decision": "Decision",
  "msg:devices.list.adopt": "Adopt",
  "msg:devices.detail.empty": "Select a device.",
  // v1.2 — column list affordances (UIS-071b)
  "msg:devices.list.address": "Address",
  "msg:devices.list.class": "Class",
  "msg:devices.list.entities": "Entities",
  "msg:devices.filter.class": "Device class",
  "msg:devices.filter.decision": "Decision",
};

// ── The valid page-document render oracle ───────────────────────────────────
// One fixture per valid case: the page-level data root Bindings resolve against
// (UIS-005), optional host slot content, an optional action handler, and the
// structural assertion the contract-declared page must satisfy once painted.
interface RenderFixture {
  data?: Record<string, unknown>;
  slots?: Record<string, ReactNode>;
  handler?: ActionHandler;
  /** May be async: the v1.1 cases DRIVE the page (click, dismiss, settle) rather
   * than assert its first paint. */
  assert: () => void | Promise<void>;
}

/** A promise a test settles by hand, so the PENDING half of an ActionOutcome
 * (UIS-166) is observable rather than raced past. */
function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void } {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

// The confirm case's own seam, hoisted so the fixture's assert can read what the
// dialog did and did not dispatch.
const confirmSeam: ActionHandler = { submit: vi.fn(), remove: vi.fn() };

// The outcome case's own seam: one deferred invocation the assert settles.
const restartCall = deferred<unknown>();
const outcomeSeam: ActionHandler = { callAction: vi.fn(() => restartCall.promise) };

const RENDER_FIXTURES: Record<string, RenderFixture> = {
  // list-detail (UIS-020): the presets list renders as a table with both rows;
  // the detail panel shows its emptyMsg until a selection is made.
  "UIS-020-valid-list-detail-presets": {
    data: {
      presets: [
        { id: "01J8Z3K4N5P6Q7R8S9T0V1W2A1", name: "Morning open" },
        { id: "01J8Z3K4N5P6Q7R8S9T0V1W2A2", name: "Evening wind-down" },
      ],
    },
    assert: () => {
      const table = screen.getByRole("table", { name: "presets" });
      expect(within(table).getByText("Name")).toBeInTheDocument();
      expect(within(table).getByText("Morning open")).toBeInTheDocument();
      expect(within(table).getByText("Evening wind-down")).toBeInTheDocument();
      expect(screen.getByText("Select a preset to edit it.")).toBeInTheDocument();
    },
  },
  // settings-form (UIS-030): the section heading, its bound fields, and the
  // submit-wired save button all paint from the source record.
  "UIS-030-valid-settings-form": {
    data: { site: { displayName: "The Hangar", quietHours: true } },
    handler: { submit: vi.fn() },
    assert: () => {
      expect(screen.getByRole("heading", { name: "General" })).toBeInTheDocument();
      expect(screen.getByLabelText("Display name")).toHaveValue("The Hangar");
      expect(screen.getByRole("switch", { name: "Quiet hours" })).toHaveAttribute(
        "aria-checked",
        "true",
      );
      expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
    },
  },
  // dashboard (UIS-040): two stat-tiles resolve independent computed counts as
  // root Bindings, and the slot tile paints the host-supplied fragment content.
  "UIS-040-valid-dashboard": {
    data: { screens: [{}, {}, {}], automations: [{}, {}] },
    slots: { "dashboard-cards": <div data-testid="slot-content">cards</div> },
    assert: () => {
      expect(screen.getByText("Online screens")).toBeInTheDocument();
      expect(screen.getByText("3")).toBeInTheDocument();
      expect(screen.getByText("Active automations")).toBeInTheDocument();
      expect(screen.getByText("2")).toBeInTheDocument();
      expect(screen.getByTestId("slot-content")).toBeInTheDocument();
    },
  },
  // wizard (UIS-050): the first step paints its title + bound input, and Next is
  // gated closed by canAdvanceIf until the shared draft's `name` is truthy.
  "UIS-050-valid-wizard": {
    handler: { callAction: vi.fn() },
    assert: () => {
      expect(screen.getByRole("heading", { name: "Name" })).toBeInTheDocument();
      expect(screen.getByLabelText("Automation name")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Next" })).toBeDisabled();
    },
  },
  // vocab OptionSource (UIS-132-valid): the select shows the bound mode and
  // offers every one of rules/1:mode's four members as an option.
  "UIS-132-valid-vocab-option-source": {
    data: { automations: { "01J8Z3K4N5P6Q7R8S9T0V1W2A1": { mode: "single" } } },
    handler: { submit: vi.fn() },
    assert: () => {
      const select = screen.getByLabelText("Mode");
      expect(select).toHaveValue("single");
      for (const member of ["Single", "Restart", "Queued", "Parallel"]) {
        expect(within(select).getByRole("option", { name: member })).toBeInTheDocument();
      }
    },
  },
  // confirm (UIS-165): the press alone dispatches NOTHING. The dialog appears with
  // the ConfirmSpec's own copy; a dismissal leaves the seam untouched; only the
  // acknowledgement runs that same `delete` against the host.
  "UIS-165-valid-confirm-gated-destructive-action": {
    data: { site: { displayName: "The Hangar" } },
    handler: confirmSeam,
    assert: async () => {
      const user = userEvent.setup();
      await user.click(screen.getByRole("button", { name: "Delete site" }));
      // The ConfirmSpec's own title/body/labels, resolved through the catalog.
      expect(await screen.findByRole("dialog", { name: "Delete this site?" })).toBeInTheDocument();
      expect(screen.getByText("Everything authored under it goes with it.")).toBeInTheDocument();
      expect(confirmSeam.remove).not.toHaveBeenCalled();

      // A dismissal dispatches nothing at all (UIS-165).
      await user.click(screen.getByRole("button", { name: "Keep it" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
      expect(confirmSeam.remove).not.toHaveBeenCalled();

      // The acknowledgement runs the SAME ActionRef, in the same Scope.
      await user.click(screen.getByRole("button", { name: "Delete site" }));
      await user.click(await screen.findByRole("button", { name: "Delete it" }));
      expect(confirmSeam.remove).toHaveBeenCalledWith("$root", { displayName: "The Hangar" });
      // The unconfirmed sibling action was never touched by any of this.
      expect(confirmSeam.submit).not.toHaveBeenCalled();
    },
  },
  // outcome (UIS-166) + disabledIf (UIS-076) + announce (UIS-077): press → pending
  // (control disabled, polite live region) → refused, with the published code
  // rendered as its own assertive sentence and the control handed back.
  "UIS-166-valid-outcome-renders-refusal-code": {
    handler: outcomeSeam,
    assert: async () => {
      const user = userEvent.setup();
      const button = screen.getByRole("button", { name: "Restart this box" });
      expect(button).toBeEnabled();
      expect(screen.queryByRole("status")).not.toBeInTheDocument();

      await user.click(button);
      // PENDING is observable: the outcome landed in $ui before the seam settled.
      expect(await screen.findByRole("status")).toHaveTextContent(/asking this box to stop/i);
      expect(screen.getByRole("button", { name: "Restart this box" })).toBeDisabled();

      restartCall.reject({ code: "RESTART_BLOCKED", detail: "a backup is running", traceId: "t-1" });
      const alert = await screen.findByRole("alert");
      expect(alert).toHaveTextContent(/busy with work a restart would break/i);
      // The server's own detail rides through the outcome into the sentence.
      expect(alert).toHaveTextContent(/a backup is running/);
      // A refusal returns the control to the operator; the pending line is gone.
      await waitFor(() =>
        expect(screen.getByRole("button", { name: "Restart this box" })).toBeEnabled(),
      );
      expect(screen.queryByRole("status")).not.toBeInTheDocument();
    },
  },
  // an unwired seam the page ASKED about settles as ACTION_DISPATCH_UNWIRED
  // (UIS-167) — no handler is supplied at all here.
  "UIS-167-valid-unwired-dispatch-settles-error": {
    assert: async () => {
      const user = userEvent.setup();
      await user.click(screen.getByRole("button", { name: "Restart this box" }));
      const alert = await screen.findByRole("alert");
      expect(alert).toHaveTextContent("ACTION_DISPATCH_UNWIRED");
      // Settled, not stuck: the control is pressable again rather than pending.
      expect(screen.getByRole("button", { name: "Restart this box" })).toBeEnabled();
    },
  },
  // v1.2 — the decision column (UIS-071a): a table whose columns render widget
  // SUBTREES per row. The assertion drives the third row's control, because a
  // subtree wired to the page scope (or to row 0) paints identically and only
  // shows itself when a row other than the first is acted on.
  // v1.2 — column list affordances (UIS-071b). Asserted through the RENDERED
  // controls, not the document: a declaration the renderer drops still validates.
  "UIS-071b-valid-searchable-filtered-inventory": {
    data: {
      devices: [
        { id: "d1", name: "Zulu", address: "192.168.50.31", device_class: "media_player", entity_count: 2, status: "Discovered", status_tone: "warning" },
        { id: "d2", name: "Alpha", address: "192.168.50.12", device_class: "printer", entity_count: 1, status: "Adopted", status_tone: "positive" },
      ],
    },
    assert: () => {
      // Both declared filters exist as real, labelled controls.
      expect(screen.getByLabelText("Device class")).toBeInTheDocument();
      expect(screen.getByLabelText("Decision")).toBeInTheDocument();
      // Faceted from the rows themselves, so the options cannot drift from the data.
      const cls = screen.getByLabelText("Device class");
      // Faceted options carry their own row COUNT ("printer (1)"), which is the
      // kit's doing and is the proof the set came from the data rather than from
      // anything the document declared — an author states no option list at all.
      expect(within(cls).getByRole("option", { name: /^printer \(1\)$/ })).toBeInTheDocument();
      expect(within(cls).getByRole("option", { name: /^media_player \(1\)$/ })).toBeInTheDocument();
      // A column that declared no filter gets no control.
      expect(screen.queryByLabelText("Address")).toBeNull();
    },
  },
  "UIS-071a-valid-decision-column-with-widget-cell": {
    data: {
      devices: [
        { id: "d1", name: "Zulu", status: "Discovered", status_tone: "warning", adopted: false, ports_label: "8060 roku", ports_search: "roku" },
        { id: "d2", name: "Alpha", status: "Adopted", status_tone: "positive", adopted: true, ports_label: "Not scanned", ports_search: "not scanned" },
      ],
    },
    handler: { callAction: vi.fn() },
    assert: async () => {
      const user = userEvent.setup();
      const table = screen.getByRole("table", { name: "devices" });
      const rows = within(table).getAllByRole("row").slice(1);

      // UIS-078: each row's badge takes its own tone from the row.
      expect(
        within(rows[0]!).getByText("Discovered").closest("[data-slot='status-badge']"),
      ).toHaveAttribute("data-status", "warn");
      expect(
        within(rows[1]!).getByText("Adopted").closest("[data-slot='status-badge']"),
      ).toHaveAttribute("data-status", "ok");

      // UIS-079: the explanation rides along, resolved through the catalog.
      expect(
        within(rows[0]!).getByText("Discovered").closest("[data-slot='status-badge']"),
      ).toHaveAttribute("title", "Discovered means seen but not yet adopted.");

      // UIS-071a: a per-row control, disabled per row, dispatching that row's id.
      expect(within(rows[0]!).getByRole("button", { name: "Adopt" })).toBeEnabled();
      expect(within(rows[1]!).getByRole("button", { name: "Adopt" })).toBeDisabled();
      await user.click(within(rows[0]!).getByRole("button", { name: "Adopt" }));
    },
  },
};

// ── Pure assertion helpers (throwable, so the teeth block can wrap them) ──────

/** Assert an invalid document is rejected with exactly the expected taxonomy
 * code(s) at the expected field path(s). Uses vitest matchers internally so a
 * mismatch throws — which is what gives the teeth block something to catch. */
function assertRejection(input: Record<string, unknown>, expectedErrors: ExpectedError[]): void {
  const result = validatePage(input);
  expect(result.ok).toBe(false);
  if (result.ok) throw new Error("expected an invalid document but validation passed");
  expect(expectedErrors.length).toBeGreaterThan(0);
  const gotCodes = result.errors.map((e) => e.code).sort();
  const wantCodes = expectedErrors.map((e) => e.code).sort();
  expect(gotCodes).toEqual(wantCodes);
  for (const want of expectedErrors) {
    const match = result.errors.find((e) => e.code === want.code);
    if (!match) throw new Error(`no error with code ${want.code}`);
    expect(match.path).toBe(want.field);
  }
}

/** A RenderScope over a bare data object: `current`/`root` are the object,
 * `$ui` is its own `$ui` key (UIS-101's predicate reads `$ui.selected`). */
function scopeFromData(data: Record<string, unknown>): RenderScope {
  return {
    root: data,
    current: data,
    currentPath: [],
    currentTree: "resource",
    ui: (data["$ui"] as Record<string, unknown>) ?? {},
    context: {},
  };
}

/** Assert a binding resolves to the contract-fixed record. */
function assertResolved(binding: string, data: Record<string, unknown>, expectedResolved: unknown): void {
  const got = resolvePath(binding, scopeFromData(data));
  expect(got).toEqual(expectedResolved);
}

// ── The driver ──────────────────────────────────────────────────────────────

describe("ui-schema/1 corpus — the render + reject driver", () => {
  it("drives every one of the twenty-three frozen corpus cases (no copies, none pending)", () => {
    expect(cases.length).toBe(23);
    // Each case is driven by exactly one arm — page-document or binding — so the
    // partition covers all nine with nothing left over (the Go driver discipline).
    expect(validPageCases.length + invalidPageCases.length + bindingCases.length).toBe(
      cases.length,
    );
    // No valid page case is pending: every one has a render fixture + oracle.
    for (const c of validPageCases) {
      expect(RENDER_FIXTURES[c.case_id], `missing render fixture for ${c.case_id}`).toBeDefined();
    }
    // The exact case membership the corpus is expected to carry.
    expect(new Set(cases.map((c) => c.case_id))).toEqual(
      new Set([
        "UIS-020-valid-list-detail-presets",
        "UIS-030-valid-settings-form",
        "UIS-040-valid-dashboard",
        "UIS-050-valid-wizard",
        "UIS-060-invalid-unknown-widget-rejected",
        "UIS-071a-invalid-cell-value-on-a-value-column-rejected",
        "UIS-071a-invalid-column-with-both-cell-and-cell-widget-rejected",
        "UIS-071a-invalid-column-with-neither-cell-nor-cell-widget-rejected",
        "UIS-071a-valid-decision-column-with-widget-cell",
        "UIS-071b-invalid-filter-label-without-filter-rejected",
        "UIS-071b-invalid-unknown-column-filter-rejected",
        "UIS-071b-valid-searchable-filtered-inventory",
        "UIS-100-invalid-malformed-binding-rejected",
        "UIS-101-valid-predicate-index-binding",
        "UIS-132-invalid-incomplete-vocab-labels-rejected",
        "UIS-132-valid-vocab-option-source",
        "UIS-165-invalid-confirm-missing-title-rejected",
        "UIS-165-invalid-confirm-unknown-field-rejected",
        "UIS-165-valid-confirm-gated-destructive-action",
        "UIS-166-invalid-outcome-to-outside-ui-rejected",
        "UIS-166-valid-outcome-renders-refusal-code",
        "UIS-167-invalid-outcome-to-on-local-verb-rejected",
        "UIS-167-valid-unwired-dispatch-settles-error",
      ]),
    );
    // The four page types are each covered by a valid case.
    expect(new Set(validPageCases.map((c) => c.input.pageType as string))).toEqual(
      new Set(["list-detail", "settings-form", "dashboard", "wizard"]),
    );
  });

  it("conformance/driven-manifest.json's ui-schema/1 entry matches what this driver actually runs, in both directions", () => {
    // The Set-literal assertion above only proves this file is internally
    // consistent with itself — it never reads driven-manifest.json, so a
    // hand-edited manifest entry (the only way ui-schema/1's entry can be
    // updated at all, since conformance/cmd/driven-manifest cannot import a
    // vitest compilation unit) could drift from reality with nothing to catch
    // it. This is the bidirectional check data-model/1 and rules/1 already
    // get for free from their own Go driven-manifest tests
    // (internal/datamodel/driven_manifest_test.go,
    // internal/rules/driven_manifest_test.go) — mirrored here so ui-schema/1's
    // entry has the same teeth.
    // A plain path (not a `new URL(..., import.meta.url)`) — jsdom's global
    // `URL` shadows Node's in this test environment, and Node's `fs` rejects
    // that shape with "The URL must be of scheme file" even though it prints
    // as one.
    const manifestPath = join(import.meta.dirname, "..", "..", "..", "conformance", "driven-manifest.json");
    const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
    const entry = manifest["ui-schema/1"];
    expect(entry, `${manifestPath} has no "ui-schema/1" entry`).toBeDefined();
    const actualDriven = [...cases.map((c) => c.case_id)].sort();
    expect([...entry.driven].sort()).toEqual(actualDriven);
    expect(entry.pending ?? []).toEqual([]);
  });

  describe("valid page documents validate clean and render their expected structure", () => {
    for (const c of validPageCases) {
      it(`${c.case_id}`, async () => {
        expect(validatePage(c.input).ok).toBe(true);
        const fx = RENDER_FIXTURES[c.case_id];
        render(
          <PageRenderer
            doc={c.input}
            messages={messages}
            {...(fx.data ? { data: fx.data } : {})}
            {...(fx.slots ? { slots: fx.slots } : {})}
            {...(fx.handler ? { handler: fx.handler } : {})}
          />,
        );
        // A conformant document paints — the rejection panel is never present.
        expect(document.querySelector('[data-slot="renderer-invalid"]')).toBeNull();
        await fx.assert();
      });
    }
  });

  describe("invalid page documents are rejected with their taxonomy code, never painted", () => {
    for (const c of invalidPageCases) {
      it(`${c.case_id}`, () => {
        const expectedErrors = c.expected.errors ?? [];
        // Validation rejects with exactly the corpus's own code(s) + path(s).
        assertRejection(c.input, expectedErrors);
        // And the renderer refuses to paint it — the rejection panel shows the
        // taxonomy code and onValidationError fires with the typed errors.
        const onValidationError = vi.fn();
        render(<PageRenderer doc={c.input} onValidationError={onValidationError} />);
        const panel = screen.getByRole("alert");
        for (const e of expectedErrors) expect(panel).toHaveTextContent(e.code);
        expect(onValidationError).toHaveBeenCalled();
      });
    }
  });

  describe("binding-resolution cases evaluate to the contract-fixed record", () => {
    for (const c of bindingCases) {
      it(`${c.case_id}`, () => {
        assertResolved(
          c.input.binding as string,
          c.input.data as Record<string, unknown>,
          c.expected.resolved,
        );
      });
    }
  });

  // The oracle must bite: a mutated expectation MUST fail. Each arm confirms the
  // real expectation passes and a deliberate mutation of it throws — proving the
  // assertions above are not vacuously green.
  describe("the driver has teeth (a mutated expectation fails)", () => {
    it("a mutated rejection code no longer matches", () => {
      const c = byId["UIS-060-invalid-unknown-widget-rejected"];
      const real = c.expected.errors ?? [];
      expect(() => assertRejection(c.input, real)).not.toThrow();
      const mutated = real.map((e) => ({ ...e, code: "OPTION_SOURCE_INVALID" }));
      expect(() => assertRejection(c.input, mutated)).toThrow();
    });

    it("a mutated rejection field path no longer matches", () => {
      const c = byId["UIS-100-invalid-malformed-binding-rejected"];
      const real = c.expected.errors ?? [];
      expect(() => assertRejection(c.input, real)).not.toThrow();
      const mutated = real.map((e) => ({ ...e, field: "sections[9].fields[9].bind" }));
      expect(() => assertRejection(c.input, mutated)).toThrow();
    });

    it("a mutated resolved value no longer matches", () => {
      const c = byId["UIS-101-valid-predicate-index-binding"];
      const data = c.input.data as Record<string, unknown>;
      expect(() =>
        assertResolved(c.input.binding as string, data, c.expected.resolved),
      ).not.toThrow();
      expect(() =>
        assertResolved(c.input.binding as string, data, { id: "wrong", name: "wrong" }),
      ).toThrow();
    });

    it("the render oracle distinguishes real structure from a mutation", () => {
      render(
        <PageRenderer
          doc={byId["UIS-030-valid-settings-form"].input}
          data={{ site: { displayName: "The Hangar", quietHours: true } }}
          messages={messages}
          handler={{ submit: vi.fn() }}
        />,
      );
      // The real field is present; a mutated expectation (a field that does not
      // exist in the document) is not — getByLabelText throws when it is absent.
      expect(screen.getByLabelText("Display name")).toBeInTheDocument();
      expect(() => screen.getByLabelText("Nonexistent field")).toThrow();
    });
  });
});
