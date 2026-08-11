// scripts/validate-error-codes.test.mjs — exercises validate-error-codes.mjs
// against disposable fixture trees built at test runtime. Fixtures are never
// committed: a deliberately-unimplemented code living under the real contracts/
// would itself trip the validator it is meant to test.
//
// These cases are the gate's own mutation evidence, kept executable. Two of them
// (removeTheEmitSite, codeOnlyInAComment) are the exact mutations the gate was
// verified against by hand; freezing them here means the gate cannot quietly
// stop catching them.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { spawnSync } from "node:child_process";

const VALIDATOR = join(import.meta.dirname, "validate-error-codes.mjs");

const CONTRACT = `# Example Contract

**Contract:** example/1
**Version:** 1.0
**Status:** draft

## Normative requirements

**[XXX-001]** The example MUST reject a bad thing (\`THING_INVALID\`, Error taxonomy).

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| \`THING_INVALID\` | The thing is not a thing. | no |
| \`THING_MISSING\` | There is no thing. | no |

## Conformance notes

- nothing here.
`;

// THING_INVALID is emitted; THING_MISSING is not, so it must be allowlisted.
const IMPL_GO = `package example

func Reject(kind string) string {
	if kind == "" {
		return "THING_INVALID"
	}
	return ""
}
`;

const ALLOWLIST = {
  groups: [
    {
      contract: "example-1.md",
      reason: "THING_MISSING has no emit site because the lookup path that would raise it is not built yet.",
      codes: ["THING_MISSING"],
    },
  ],
};

function makeFixture(build) {
  const root = mkdtempSync(join(tmpdir(), "validate-error-codes-"));
  const write = (relPath, content) => {
    const full = join(root, relPath);
    mkdirSync(dirname(full), { recursive: true });
    writeFileSync(full, typeof content === "string" ? content : JSON.stringify(content, null, 2));
  };
  build(write);
  return root;
}

function writeGoodTree(write, overrides = {}) {
  write("contracts/README.md", "# Contracts\n\nNo taxonomy here.\n");
  write("contracts/example-1.md", overrides.contract ?? CONTRACT);
  write("internal/example/example.go", overrides.impl ?? IMPL_GO);
  write("conformance/unimplemented-error-codes.json", overrides.allowlist ?? ALLOWLIST);
}

function run(cwd) {
  return spawnSync(process.execPath, [VALIDATOR], { cwd, encoding: "utf8" });
}

function withFixture(build, assertions) {
  const root = makeFixture(build);
  try {
    assertions(run(root));
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

test("a clean tree passes and reports the pair count", () => {
  withFixture(
    (w) => writeGoodTree(w),
    (r) => {
      assert.equal(r.status, 0, r.stderr);
      assert.match(r.stdout, /SUMMARY: validate-error-codes: OK \(2 published pair\(s\), 0 field-level pair\(s\), 0 emitted checked back, 1 allowlisted unimplemented\)/);
    }
  );
});

test("removing the emit site fails: an implemented code that leaves the implementation without entering the allowlist", () => {
  withFixture(
    (w) =>
      writeGoodTree(w, {
        impl: `package example

func Reject(kind string) string {
	if kind == "" {
		return "SOMETHING_ELSE"
	}
	return ""
}
`,
      }),
    (r) => {
      assert.equal(r.status, 1);
      assert.match(r.stderr, /THING_INVALID is published in the Error taxonomy but no implementation source emits it/);
      assert.match(r.stdout, /SUMMARY: validate-error-codes: FAILED — 1 issue/);
    }
  );
});

test("a code named only in a comment does not count as implemented", () => {
  withFixture(
    (w) =>
      writeGoodTree(w, {
        impl: `package example

// Reject one day returns THING_INVALID here.
func Reject(kind string) string { return "" }
`,
      }),
    (r) => {
      assert.equal(r.status, 1);
      assert.match(r.stderr, /THING_INVALID is published .* but no implementation source emits it/);
    }
  );
});

test("a code named only in a TypeScript /** */ doc comment does not count as implemented either", () => {
  // The `//` case above held for Go and silently did NOT hold for TypeScript,
  // whose doc comments are `/** … */` — and ui-schema/1's entire taxonomy is
  // implemented in TypeScript. Both JSDoc spellings slipped through: a BACKTICKED
  // mention matched the quoted-token scan and counted as a use outright, and a
  // BARE mention satisfied the "the constant is referenced elsewhere" rule.
  // Caught by mutation — deleting a renderer refusal's only emit site left this
  // gate green because two paragraphs of prose still named it.
  const jsdocOnly = (mention) => `/**
 * Reject one day returns ${mention} here.
 */
export function reject(kind: string): string {
  return kind === "" ? "SOMETHING_ELSE" : "";
}
`;
  for (const mention of ["THING_INVALID", "`THING_INVALID`"]) {
    withFixture(
      (w) => {
        writeGoodTree(w, { impl: "package example\n\nfunc Reject() string { return \"\" }\n" });
        w("web/src/example.ts", jsdocOnly(mention));
      },
      (r) => {
        assert.equal(r.status, 1, `a JSDoc mention (${mention}) is prose, not wiring`);
        assert.match(r.stderr, /THING_INVALID is published .* but no implementation source emits it/);
      }
    );
  }

  // …and the same file with a real emit site beside the prose passes, so the
  // block-comment stripping cannot be over-eager and blank out live code.
  withFixture(
    (w) => {
      writeGoodTree(w, { impl: "package example\n\nfunc Reject() string { return \"\" }\n" });
      w(
        "web/src/example.ts",
        `${jsdocOnly("`THING_INVALID`")}
export function reallyReject(): string {
  return "THING_INVALID";
}
`
      );
    },
    (r) => assert.equal(r.status, 0, r.stderr)
  );
});

test("a Go constant counts only when the identifier is referenced elsewhere", () => {
  const declOnly = `package example

const CodeThingInvalid = "THING_INVALID"
`;
  withFixture(
    (w) => writeGoodTree(w, { impl: declOnly }),
    (r) => {
      assert.equal(r.status, 1, "a constant nothing reads is a declaration, not an implementation");
      assert.match(r.stderr, /THING_INVALID is published/);
    }
  );
  withFixture(
    (w) =>
      writeGoodTree(w, {
        impl: `${declOnly}
func Reject() string { return CodeThingInvalid }
`,
      }),
    (r) => assert.equal(r.status, 0, r.stderr)
  );
});

test("a constant ENUMERATED in a taxonomy mirror is not a constant anything reads", () => {
  // The identifier spelling of the mirror rule. ui-schema/1 declares the two
  // codes its RENDERER raises (rather than its validator) as named constants and
  // lists them in ERROR_CODES — so before this, declaring a code and adding it to
  // that array satisfied the "referenced somewhere else" rule with no emit site
  // anywhere in the tree. Found by mutation on a real refusal.
  const mirrorOnly = `export const THING_INVALID = "THING_INVALID";

export const ERROR_CODES = [
  THING_INVALID,
  "THING_MISSING",
] as const;
`;
  withFixture(
    (w) => {
      writeGoodTree(w, { impl: "package example\n\nfunc Reject() string { return \"\" }\n" });
      w("web/src/codes.ts", mirrorOnly);
    },
    (r) => {
      assert.equal(r.status, 1, "an enumerated constant is a declaration, not an implementation");
      assert.match(r.stderr, /THING_INVALID is published .* but no implementation source emits it/);
    }
  );

  // One real read of the same constant, outside the mirror, and it counts.
  withFixture(
    (w) => {
      writeGoodTree(w, { impl: "package example\n\nfunc Reject() string { return \"\" }\n" });
      w("web/src/codes.ts", `${mirrorOnly}\nexport function reject(): string {\n  return THING_INVALID;\n}\n`);
    },
    (r) => assert.equal(r.status, 0, r.stderr)
  );
});

test("an array-literal element is a taxonomy mirror; a wrapped call argument is a use", () => {
  // Both spell the code on a line of its own. Only the second is an emit site.
  const mirror = `export const ERROR_CODES = [
  "THING_INVALID",
  "THING_MISSING",
] as const;
`;
  const use = `${mirror}
export function reject() {
  return fail(
    ctx,
    "THING_INVALID",
    "path",
  );
}
`;
  withFixture(
    (w) => {
      writeGoodTree(w);
      w("internal/example/example.go", "package example\n");
      w("web/src/renderer/schema.ts", mirror);
    },
    (r) => {
      assert.equal(r.status, 1, "a code listed in a mirror of the taxonomy is not implemented by that listing");
      assert.match(r.stderr, /THING_INVALID is published/);
    }
  );
  withFixture(
    (w) => {
      writeGoodTree(w);
      w("internal/example/example.go", "package example\n");
      w("web/src/renderer/validate.ts", use);
    },
    (r) => assert.equal(r.status, 0, r.stderr)
  );
});

test("test files, conformance drivers and generated files are not implementation sources", () => {
  for (const [path, body] of [
    ["internal/example/example_test.go", `package example\n\nvar want = "THING_INVALID"\n`],
    ["conformance/drivers/x/driver.go", `package x\n\nfunc code() string { return "THING_INVALID" }\n`],
    [
      "api/gen/go/api.gen.go",
      `// Code generated by oapi-codegen version v2.7.2 DO NOT EDIT.\npackage gen\n\nconst A = "THING_INVALID"\n\nvar B = A\n`,
    ],
  ]) {
    withFixture(
      (w) => {
        writeGoodTree(w);
        w("internal/example/example.go", "package example\n");
        w(path, body);
      },
      (r) => {
        assert.equal(r.status, 1, `${path} must not count as an implementation`);
        assert.match(r.stderr, /THING_INVALID is published/);
      }
    );
  }
});

test("an allowlist entry that has been overtaken by an implementation fails", () => {
  withFixture(
    (w) =>
      writeGoodTree(w, {
        impl: `package example

func Reject(kind string) (string, string) { return "THING_INVALID", "THING_MISSING" }
`,
      }),
    (r) => {
      assert.equal(r.status, 1);
      assert.match(r.stderr, /THING_MISSING IS implemented now .* delete it from/);
    }
  );
});

test("an allowlist entry for a code the contract no longer publishes fails", () => {
  withFixture(
    (w) =>
      writeGoodTree(w, {
        allowlist: {
          groups: [
            { contract: "example-1.md", reason: "A code that was renamed away and left behind here.", codes: ["THING_MISSING", "GONE_AWAY"] },
          ],
        },
      }),
    (r) => {
      assert.equal(r.status, 1);
      assert.match(r.stderr, /does not publish GONE_AWAY in its Error taxonomy/);
    }
  );
});

test("a placeholder reason fails, so the inventory cannot become 62 TODOs", () => {
  for (const reason of ["TODO", "TBD", "n/a", "-", "too short"]) {
    withFixture(
      (w) => writeGoodTree(w, { allowlist: { groups: [{ contract: "example-1.md", reason, codes: ["THING_MISSING"] }] } }),
      (r) => {
        assert.equal(r.status, 1, `reason ${JSON.stringify(reason)} must be rejected`);
        assert.match(r.stderr, /reason is a placeholder or too short/);
      }
    );
  }
});

test("the same pair listed twice fails", () => {
  withFixture(
    (w) =>
      writeGoodTree(w, {
        allowlist: {
          groups: [
            { contract: "example-1.md", reason: "The first group carrying this unbuilt lookup path.", codes: ["THING_MISSING"] },
            { contract: "example-1.md", reason: "A second group that duplicates the first one's entry.", codes: ["THING_MISSING"] },
          ],
        },
      }),
    (r) => {
      assert.equal(r.status, 1);
      assert.match(r.stderr, /lists THING_MISSING twice/);
    }
  );
});

test("a taxonomy row inside a fenced block is illustration, not publication", () => {
  withFixture(
    (w) =>
      writeGoodTree(w, {
        contract: `${CONTRACT}
Example of the shape:

\`\`\`
| code | meaning | retryable |
| \`NEVER_PUBLISHED\` | illustration only | no |
\`\`\`
`,
      }),
    (r) => {
      assert.equal(r.status, 0, r.stderr);
      assert.match(r.stdout, /2 published pair\(s\)/);
    }
  );
});

// The api-layer/OpenAPI check (#5). It asks a question the other four cannot: a
// code can be published AND implemented and still be absent from the schema, so
// no generated client can name it. That happened for five codes before this
// existed — three scope-node delete refusals and two pack ones — each of which
// passed every other check in this file.
const OPENAPI_WITH = `openapi: 3.1.0
components:
  schemas:
    ErrorCode:
      type: string
      enum:
        - THING_INVALID
`;
const OPENAPI_WITHOUT = `openapi: 3.1.0
components:
  schemas:
    ErrorCode:
      type: string
      enum:
        - SOMETHING_ELSE
`;
const API_LAYER_IMPL = `package api

func refuse() {
	writeProblem(w, r, 409, "THING_INVALID", "no")
}
`;

test("a code the api layer emits but the ErrorCode enum omits fails", () => {
  withFixture(
    (w) => {
      writeGoodTree(w, { impl: "package other\n" });
      w("internal/app/api/handler.go", API_LAYER_IMPL);
      w("api/openapi.yaml", OPENAPI_WITHOUT);
    },
    (r) => {
      assert.equal(r.status, 1, r.stdout);
      assert.match(r.stderr, /THING_INVALID is emitted by the api layer .* not a member of the ErrorCode enum/);
    }
  );
});

test("the same code present in the enum passes", () => {
  withFixture(
    (w) => {
      writeGoodTree(w, { impl: "package other\n" });
      w("internal/app/api/handler.go", API_LAYER_IMPL);
      w("api/openapi.yaml", OPENAPI_WITH);
    },
    (r) => {
      assert.equal(r.status, 0, r.stderr);
    }
  );
});

// A code emitted somewhere OTHER than the api layer rides a different binding and
// belongs to no enum here — the check must not drag it in.
test("a code emitted outside the api layer is not required to be in the enum", () => {
  withFixture(
    (w) => {
      writeGoodTree(w);
      w("api/openapi.yaml", OPENAPI_WITHOUT);
    },
    (r) => {
      assert.equal(r.status, 0, r.stderr);
    }
  );
});

// A check that cannot find what it inspects must SAY so, not pass quietly.
test("an unparseable ErrorCode enum fails rather than skipping", () => {
  withFixture(
    (w) => {
      writeGoodTree(w, { impl: "package other\n" });
      w("internal/app/api/handler.go", API_LAYER_IMPL);
      w("api/openapi.yaml", "openapi: 3.1.0\ncomponents: {}\n");
    },
    (r) => {
      assert.equal(r.status, 1, r.stdout);
      assert.match(r.stderr, /could not locate the ErrorCode enum/);
    }
  );
});

// ---------------------------------------------------------------------------
// The REVERSE direction (check #6): a code an implementation emits must be
// published somewhere. Every other case in this file walks published ->
// implemented, which is precisely the direction that cannot see an emitted code
// nobody published — the defect that let twenty-six per-field codes ship
// unpublished. These freeze the check that closes it.

// A per-field emission in the shape the tree actually uses.
const EMITS_UNPUBLISHED_GO = `package example

type Error struct {
	Field   string
	Code    string
	Message string
}

func Check(v string) []Error {
	if v == "" {
		return []Error{{Field: "thing", Code: "THING_UNPUBLISHED", Message: "thing is required"}}
	}
	return nil
}
`;

const CONTRACT_WITH_FIELD_REGISTER = CONTRACT.replace(
  "## Conformance notes",
  `## Field-level error register

| code | field-level meaning | retryable |
|---|---|---|
| \`THING_UNPUBLISHED\` | The thing's field is absent. | no |

## Conformance notes`
);

test("an emitted code published in NO contract fails, naming the site and both registers", () => {
  withFixture(
    (w) => {
      writeGoodTree(w);
      w("internal/example/fields.go", EMITS_UNPUBLISHED_GO);
    },
    (r) => {
      assert.equal(r.status, 1, "an emitted-but-unpublished code passed — the reverse walk is not running");
      assert.match(r.stderr, /THING_UNPUBLISHED is emitted but published in no contract/);
      assert.match(r.stderr, /internal\/example\/fields\.go:\d+/, "the failure must name the emit site");
    }
  );
});

test("publishing it in the Field-level error register satisfies the reverse walk", () => {
  withFixture(
    (w) => {
      writeGoodTree(w, { contract: CONTRACT_WITH_FIELD_REGISTER });
      w("internal/example/fields.go", EMITS_UNPUBLISHED_GO);
    },
    (r) => {
      assert.equal(r.status, 0, r.stderr);
      assert.match(r.stdout, /1 field-level pair\(s\)/);
      assert.match(r.stdout, /1 emitted code\(s\) checked back/);
    }
  );
});

test("the two registers are counted separately, so neither can stand in for the other", () => {
  // Originally this asserted that an allowlist entry for a field-published code
  // FAILED check #2. That stopped being the right assertion once the allowlist
  // was widened to cover both registers — a field-level code is unimplemented in
  // exactly the same way and earns an entry on the same terms.
  //
  // The property it was really pinning survives and is asserted here instead:
  // the sets stay disjoint in the gate's own accounting. A code moved from the
  // taxonomy to the field register leaves the top-level count and joins the
  // field one, so nothing that walks top-level codes — check #1's verdict
  // requirement, check #5's ErrorCode enum — can be satisfied by publishing a
  // per-field code instead.
  const contract = CONTRACT.replace("| `THING_MISSING` | There is no thing. | no |\n", "").replace(
    "## Conformance notes",
    `## Field-level error register

| code | field-level meaning | retryable |
|---|---|---|
| \`THING_MISSING\` | Published as a FIELD code only. | no |

## Conformance notes`
  );
  withFixture(
    (w) => writeGoodTree(w, { contract }),
    (r) => {
      assert.equal(r.status, 0, r.stderr);
      // One top-level code left (THING_INVALID); THING_MISSING is now field-level.
      assert.match(r.stdout, /SUMMARY: validate-error-codes: OK \(1 published pair\(s\), 1 field-level pair\(s\)/);
    }
  );
});

test("a two-argument call whose second argument is SCREAMING_SNAKE is not an emission", () => {
  // q.Set("algorithm", "SHA1") is a URL parameter, and matched the reverse
  // scan until it required the message argument every real emission carries.
  const notAnEmission = `package example

import "net/url"

func Link() string {
	q := url.Values{}
	q.Set("algorithm", "SHA1")
	return q.Encode()
}
`;
  withFixture(
    (w) => {
      writeGoodTree(w);
      w("internal/example/link.go", notAnEmission);
    },
    (r) => {
      assert.equal(r.status, 0, r.stderr);
      assert.doesNotMatch(r.stderr, /SHA1/, "a URL query parameter was read as an emitted error code");
    }
  );
});

// ---------------------------------------------------------------------------
// The FORWARD walk over the field-level register (check #7), and the third
// emission shape it caught.
//
// #6 gave the gate an emitted -> published direction but only for the codes its
// patterns recognised, and nothing asked the field register for a verdict the
// way check #1 asks the Error taxonomy. A per-field code could be published,
// never wired, carry no recorded reason, and pass.

const CONTRACT_FIELD_ONLY = CONTRACT.replace(
  "## Conformance notes",
  `## Field-level error register

| code | field-level meaning | retryable |
|---|---|---|
| \`THING_FIELD_UNWIRED\` | Published; nothing raises it. | no |

## Conformance notes`
);

test("a field-level code with no emitter fails, exactly as a top-level one does", () => {
  withFixture(
    (w) => writeGoodTree(w, { contract: CONTRACT_FIELD_ONLY }),
    (r) => {
      assert.equal(r.status, 1, "a published field-level code with nothing emitting it passed");
      assert.match(r.stderr, /THING_FIELD_UNWIRED is published in the Field-level error register but no implementation source emits it/);
    }
  );
});

test("allowlisting a field-level code satisfies the forward walk", () => {
  withFixture(
    (w) =>
      writeGoodTree(w, {
        contract: CONTRACT_FIELD_ONLY,
        allowlist: {
          groups: [
            ...ALLOWLIST.groups,
            {
              contract: "example-1.md",
              reason:
                "THING_FIELD_UNWIRED is published ahead of the surface that raises it; the validator that would emit it is not built yet, and the entry goes away with that work.",
              codes: ["THING_FIELD_UNWIRED"],
            },
          ],
        },
      }),
    (r) => {
      assert.equal(r.status, 0, r.stderr);
      assert.match(r.stdout, /1 field-level pair\(s\)/);
    }
  );
});

test("an allowlist entry naming a code in NEITHER register still fails", () => {
  // The widened check #2 accepts a code published in either register. It must
  // not have widened into accepting one published in neither, which would let a
  // renamed code keep a stale entry alive with a plausible reason attached.
  withFixture(
    (w) =>
      writeGoodTree(w, {
        allowlist: {
          groups: [
            {
              contract: "example-1.md",
              reason:
                "THING_RENAMED_AWAY was published once and no longer is; this entry should not survive the code it describes.",
              codes: ["THING_RENAMED_AWAY"],
            },
          ],
        },
      }),
    (r) => {
      assert.equal(r.status, 1, "an allowlist entry for a code in neither register was accepted");
      assert.match(r.stderr, /does not publish THING_RENAMED_AWAY in its Error taxonomy or its Field-level error register/);
    }
  );
});

test("a code passed as a call's FIRST argument is recognised as an emission", () => {
  // The shape the pack surface uses: artifactErr("CODE", "message", ...). It was
  // invisible to the scan until check #7 surfaced a published-but-apparently-
  // unemitted code that was in fact emitted twenty-nine times over.
  const codeFirst = `package example

type ArtifactError struct {
	Code    string
	Message string
}

func artifactErr(code, message string) *ArtifactError {
	return &ArtifactError{Code: code, Message: message}
}

func Read(ok bool) *ArtifactError {
	if !ok {
		return artifactErr("THING_UNPUBLISHED", "the thing could not be read")
	}
	return nil
}
`;
  withFixture(
    (w) => {
      writeGoodTree(w);
      w("internal/example/reader.go", codeFirst);
    },
    (r) => {
      assert.equal(r.status, 1, "a code passed as a call's first argument was not seen as an emission");
      assert.match(r.stderr, /THING_UNPUBLISHED is emitted but published in no contract/);
    }
  );
});
