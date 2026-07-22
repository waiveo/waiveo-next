import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
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
// An accounting test asserts all nine cases are driven (none pending — the Go
// drivers' driven/pending discipline), and a teeth block proves the oracle bites:
// a mutated expectation must fail.

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
};

// ── The valid page-document render oracle ───────────────────────────────────
// One fixture per valid case: the page-level data root Bindings resolve against
// (UIS-005), optional host slot content, an optional action handler, and the
// structural assertion the contract-declared page must satisfy once painted.
interface RenderFixture {
  data?: Record<string, unknown>;
  slots?: Record<string, ReactNode>;
  handler?: ActionHandler;
  assert: () => void;
}

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
  it("drives every one of the nine frozen corpus cases (no copies, none pending)", () => {
    expect(cases.length).toBe(9);
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
        "UIS-100-invalid-malformed-binding-rejected",
        "UIS-101-valid-predicate-index-binding",
        "UIS-132-invalid-incomplete-vocab-labels-rejected",
        "UIS-132-valid-vocab-option-source",
      ]),
    );
    // The four page types are each covered by a valid case.
    expect(new Set(validPageCases.map((c) => c.input.pageType as string))).toEqual(
      new Set(["list-detail", "settings-form", "dashboard", "wizard"]),
    );
  });

  describe("valid page documents validate clean and render their expected structure", () => {
    for (const c of validPageCases) {
      it(`${c.case_id}`, () => {
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
        fx.assert();
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
