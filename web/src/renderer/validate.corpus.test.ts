import { describe, expect, it } from "vitest";
import { validatePage, isValidBindingPath } from "./validate";

// THE CORPUS IS THE ORACLE. These tests load the frozen ui-schema/1 conformance
// corpus directly from conformance/corpora/ui-schema-1/ — the single source of
// truth shared with the Go drivers — never a copy. A valid case MUST validate
// clean; an invalid case MUST be rejected with the contract's exact taxonomy
// code (UIS-200: a driver asserts on `code`, not on message text) at the
// offending field's own dotted/bracket path. A non-conformant document NEVER
// renders, so it must be caught here at validation.

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

// Vite's glob import reads every frozen JSON case at transform time — the same
// files the Go corpus driver reads, resolved straight out of the repo tree.
const modules = import.meta.glob<CorpusCase>(
  "../../../conformance/corpora/ui-schema-1/*.json",
  { eager: true, import: "default" },
);
const cases: CorpusCase[] = Object.keys(modules)
  .sort()
  .map((key) => modules[key]);

// A case whose `input` carries a `pageType` is a page-document validation case;
// a case whose `input` carries a bare `binding` (UIS-101) is a binding-resolution
// case exercised by the renderer's binding engine (Task 3), relevant here only in
// that its predicate-index path must be syntactically well-formed (UIS-100).
const pageCases = cases.filter((c) => typeof c.input.pageType === "string");
const bindingCases = cases.filter((c) => typeof c.input.binding === "string");

describe("ui-schema/1 corpus — validation oracle", () => {
  it("loads all nine frozen corpus cases (no copies)", () => {
    expect(cases.length).toBe(9);
    // Every case is accounted for by exactly one shape (page-document or binding).
    expect(pageCases.length + bindingCases.length).toBe(cases.length);
  });

  describe("page-document cases", () => {
    for (const c of pageCases) {
      it(`${c.case_id}`, () => {
        const result = validatePage(c.input);
        if (c.expected.valid) {
          // The four valid cases (list-detail, settings-form, dashboard, wizard)
          // plus the vocab-option-source case validate clean — no errors.
          if (!result.ok) {
            throw new Error(
              `${c.case_id} expected valid but got: ${JSON.stringify(result.errors)}`,
            );
          }
          expect(result.ok).toBe(true);
        } else {
          expect(result.ok).toBe(false);
          if (result.ok) return; // narrow
          const expectedErrors = c.expected.errors ?? [];
          expect(expectedErrors.length).toBeGreaterThan(0);
          // The rejection carries exactly the expected taxonomy code(s)…
          const gotCodes = result.errors.map((e) => e.code).sort();
          const wantCodes = expectedErrors.map((e) => e.code).sort();
          expect(gotCodes).toEqual(wantCodes);
          // …at exactly the offending field's own dotted/bracket path.
          for (const want of expectedErrors) {
            const match = result.errors.find((e) => e.code === want.code);
            expect(match, `no error with code ${want.code}`).toBeDefined();
            expect(match?.path).toBe(want.field);
          }
        }
      });
    }
  });

  describe("binding-resolution cases (grammar well-formedness)", () => {
    for (const c of bindingCases) {
      it(`${c.case_id} — predicate-index path is syntactically valid`, () => {
        // UIS-101's automations[id=$ui.selected] must NOT be rejected as a
        // malformed binding — the predicate-index form is part of the grammar.
        expect(isValidBindingPath(c.input.binding as string)).toBe(true);
      });
    }
  });

  it("exercises each rejection case (UIS-060, UIS-100, UIS-132-invalid)", () => {
    const rejections = pageCases.filter((c) => c.expected.valid === false).map((c) => c.case_id);
    expect(rejections).toContain("UIS-060-invalid-unknown-widget-rejected");
    expect(rejections).toContain("UIS-100-invalid-malformed-binding-rejected");
    expect(rejections).toContain("UIS-132-invalid-incomplete-vocab-labels-rejected");
    expect(rejections.length).toBe(3);
  });

  it("exercises each valid page type (list-detail, settings-form, dashboard, wizard)", () => {
    const valid = pageCases.filter((c) => c.expected.valid === true);
    const pageTypes = new Set(valid.map((c) => c.input.pageType as string));
    expect(pageTypes).toEqual(
      new Set(["list-detail", "settings-form", "dashboard", "wizard"]),
    );
  });
});
