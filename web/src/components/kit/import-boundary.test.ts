// @vitest-environment node
import { describe, expect, it } from "vitest";
import { ESLint } from "eslint";

/**
 * The kit-only import boundary is a STRUCTURAL guarantee, so it is proven by
 * running the real ESLint flat config — not by reading it. A page reaching into
 * the vendored `@/components/ui` base must fail `no-restricted-imports`; the same
 * import from inside `src/components/kit` (the sanctioned wrapping layer) must be
 * clean. If the rule or its kit exemption is ever removed, this test fails.
 *
 * The web gate runs with the working directory at `web/`, so ESLint's default
 * cwd is the project root and these repo-relative probe paths match the flat
 * config's `files` globs (no node builtins needed to compute them).
 */
const probe = 'import { Button } from "@/components/ui/button";\nexport const Probe = Button;\n';

async function restrictedImportCount(filePath: string): Promise<number> {
  const eslint = new ESLint();
  const [result] = await eslint.lintText(probe, { filePath });
  return result.messages.filter((m) => m.ruleId === "no-restricted-imports").length;
}

describe("kit-only import boundary (real ESLint config)", () => {
  it("forbids importing components/ui from a page/route", async () => {
    const count = await restrictedImportCount("src/routes/_boundary-probe.tsx");
    expect(count).toBeGreaterThan(0);
  });

  it("allows importing components/ui from inside the kit", async () => {
    const count = await restrictedImportCount("src/components/kit/_boundary-probe.tsx");
    expect(count).toBe(0);
  });
});
