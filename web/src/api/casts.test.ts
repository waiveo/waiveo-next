// @vitest-environment node
//
// The cast family's client surface + the content-origin listing the Studio's
// image picker resolves against.
//
// The casts module is built by the same `crud()` factory as every other family,
// so what is worth proving here is not that CRUD works (resources.test.ts owns
// that) but that THIS family is wired to the conventions rather than around
// them: a create carries an Idempotency-Key, a save carries the If-Match
// precondition, and a 412 runs the standard re-read/review flow. Plus the two
// things that are genuinely this module's own: the `/casts` path it names, and
// the slide validator that mirrors the wire's.

import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { createApi, updateWithReview } from "./resources";
import { LAYER_KINDS, validateSlide, validateCastSlides, type CastSlide } from "./casts";
import { TEST_BASE, ULID_A, ULID_ROOT, cast, contentAsset, etag, ok, problem } from "./test-support";

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

function api() {
  return createApi({ baseUrl: TEST_BASE });
}

describe("casts module", () => {
  it("lists casts off /casts", async () => {
    server.use(
      http.get(`${TEST_BASE}/casts`, () => HttpResponse.json({ items: [cast()], cursor: null })),
    );
    const page = await api().casts.list();
    expect(page.items).toHaveLength(1);
    expect(page.items[0]?.name).toBe("Lobby loop");
    expect(page.items[0]?.slides[0]?.layers).toHaveLength(3);
  });

  it("creates a cast carrying an Idempotency-Key (a retried POST cannot double-create)", async () => {
    let key: string | null = null;
    let body: unknown = null;
    server.use(
      http.post(`${TEST_BASE}/casts`, async ({ request }) => {
        key = request.headers.get("Idempotency-Key");
        body = await request.json();
        return ok(cast({ revision: 1 }), { status: 201, revision: 1 });
      }),
    );
    const created = await api().casts.create({
      scope_node: ULID_ROOT,
      name: "Lobby loop",
      slides: [{ id: "slide-1", layers: [{ kind: "rect", x: 0, y: 0, w: 1920, h: 1080, color: "#101020" }] }],
    });
    expect(key).toBeTruthy();
    expect(body).toMatchObject({ scope_node: ULID_ROOT, name: "Lobby loop" });
    expect(created.etag).toBe(etag(1));
  });

  it("saves slides under the If-Match derived from the read revision", async () => {
    // A holder object rather than bare `let`s: the assignments happen inside a
    // handler callback, which TypeScript's control-flow analysis cannot see, so
    // a `let` initialised to null narrows to `never` at the assertions.
    const seen: { ifMatch?: string | null; body?: { slides?: CastSlide[] } } = {};
    server.use(
      http.patch(`${TEST_BASE}/casts/${ULID_A}`, async ({ request }) => {
        seen.ifMatch = request.headers.get("If-Match");
        seen.body = (await request.json()) as { slides?: CastSlide[] };
        return ok(cast({ revision: 2 }), { revision: 2 });
      }),
    );
    const slides: CastSlide[] = [
      { id: "slide-1", layers: [{ kind: "text", x: 10, y: 10, w: 400, h: 80, text: "Hi" }] },
    ];
    const saved = await api().casts.update(ULID_A, { slides }, etag(1));
    expect(seen.ifMatch).toBe(etag(1));
    expect(seen.body?.slides?.[0]?.layers[0]?.text).toBe("Hi");
    expect(saved.data.revision).toBe(2);
  });

  it("on a concurrent edit re-reads and hands back the current cast for review", async () => {
    let patches = 0;
    server.use(
      http.patch(`${TEST_BASE}/casts/${ULID_A}`, () => {
        patches++;
        return problem(412, "REVISION_CONFLICT", "The resource was modified concurrently.", {
          current_revision: 9,
        });
      }),
      http.get(`${TEST_BASE}/casts/${ULID_A}`, () =>
        ok(cast({ revision: 9, name: "Renamed elsewhere" }), { revision: 9 }),
      ),
    );
    const outcome = await updateWithReview(api().casts, ULID_A, { name: "Mine" }, etag(1));
    expect(outcome.status).toBe("conflict");
    if (outcome.status === "conflict") {
      expect(outcome.current.data.name).toBe("Renamed elsewhere");
      expect(outcome.conflict.currentRevision).toBe(9);
    }
    // The whole point: exactly ONE attempt, no silent overwrite retry.
    expect(patches).toBe(1);
  });

  it("deletes under an If-Match", async () => {
    let ifMatch: string | null = null;
    server.use(
      http.delete(`${TEST_BASE}/casts/${ULID_A}`, ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        return new HttpResponse(null, { status: 204 });
      }),
    );
    await api().casts.remove(ULID_A, etag(3));
    expect(ifMatch).toBe(etag(3));
  });
});

describe("content-origin listing", () => {
  it("unwraps the origin's `{content: []}` directory shape", async () => {
    server.use(
      http.get(`${TEST_BASE}/content`, () =>
        HttpResponse.json({
          content: [contentAsset(), contentAsset({ asset_ref: "sha256:dd44", url: "/content/dd44" })],
        }),
      ),
    );
    const assets = await api().content.list();
    expect(assets.map((a) => a.asset_ref)).toEqual(["sha256:aa11bb22cc33", "sha256:dd44"]);
    expect(assets[0]?.size_bytes).toBe(24_576);
  });

  it("reads an origin with nothing in it as an empty library, not a crash", async () => {
    server.use(http.get(`${TEST_BASE}/content`, () => HttpResponse.json({})));
    await expect(api().content.list()).resolves.toEqual([]);
  });
});

describe("validateSlide — the console-side mirror of wire.ValidateSlideLayers", () => {
  const geo = { x: 10, y: 10, w: 200, h: 100 };

  it("accepts each of the four v1 kinds with its required fields", () => {
    expect(validateSlide({ id: "s", layers: [{ kind: "text", ...geo, text: "hi" }] })).toEqual([]);
    expect(validateSlide({ id: "s", layers: [{ kind: "rect", ...geo, color: "#00FF00" }] })).toEqual([]);
    expect(
      validateSlide({ id: "s", layers: [{ kind: "image", ...geo, asset_ref: "sha256:aa", url: "/content/aa" }] }),
    ).toEqual([]);
    expect(validateSlide({ id: "s", layers: [{ kind: "clock", ...geo, text: "15:04" }] })).toEqual([]);
  });

  it("refuses an empty stack as a whole-slide problem (index null)", () => {
    const problems = validateSlide({ id: "s", layers: [] });
    expect(problems).toHaveLength(1);
    expect(problems[0]?.index).toBeNull();
  });

  it("names the offending layer's index so the editor can point at it", () => {
    const problems = validateSlide({
      id: "s",
      layers: [
        { kind: "rect", ...geo, color: "#000000" },
        { kind: "text", ...geo, text: "" },
      ],
    });
    expect(problems).toHaveLength(1);
    expect(problems[0]?.index).toBe(1);
    expect(problems[0]?.message).toMatch(/text is required/i);
  });

  it("refuses geometry that runs off the far edge of the 1920x1080 canvas", () => {
    const problems = validateSlide({
      id: "s",
      layers: [{ kind: "rect", x: 1800, y: 1000, w: 200, h: 100, color: "#000000" }],
    });
    expect(problems.map((p) => p.message).join(" ")).toMatch(/past the 1920×1080 canvas/);
  });

  it("accepts geometry flush to the far edge (the boundary is inclusive)", () => {
    expect(
      validateSlide({ id: "s", layers: [{ kind: "rect", x: 1720, y: 980, w: 200, h: 100, color: "#fff000" }] }),
    ).toEqual([]);
  });

  it("refuses a colour a renderer could not parse", () => {
    const problems = validateSlide({
      id: "s",
      layers: [{ kind: "rect", ...geo, color: "rebeccapurple" }],
    });
    expect(problems.map((p) => p.message).join(" ")).toMatch(/not a #RRGGBB/);
  });

  it("refuses an image layer with no bytes behind it", () => {
    const problems = validateSlide({ id: "s", layers: [{ kind: "image", ...geo }] });
    expect(problems[0]?.message).toMatch(/media library/i);
  });

  // `video` is the ninth kind, and the console shipped believing there were
  // eight. The consequence was not a missing feature but a BROKEN editor: this
  // mirror reported "Unknown layer kind" for a layer the server had just stored,
  // and the Studio's save gate reads exactly this list of problems — so a cast
  // that already carried a video layer could never be saved again, for a reason
  // no server would agree with. Both directions are pinned below.
  it("accepts a video layer, which the server accepts too", () => {
    expect(
      validateSlide({ id: "s", layers: [{ kind: "video", ...geo, asset_ref: "sha256:bb", url: "/content/bb" }] }),
    ).toEqual([]);
  });

  it("never reports a server-valid kind as unknown", () => {
    // The whole closed set, each with the fields its kind requires, must pass
    // the unknown-kind branch. Driven off LAYER_KINDS so a tenth kind added to
    // the wire and forgotten here fails HERE rather than on an operator's save.
    for (const kind of LAYER_KINDS) {
      const problems = validateSlide({
        id: "s",
        layers: [{ kind, ...geo, text: "{temp} x", color: "#00FF00", asset_ref: "sha256:aa", url: "/content/aa", entity_id: ULID_A, target_ms: 1 }],
      });
      expect(problems.map((p) => p.message).join(" "), `${kind} must not be reported as an unknown kind`).not.toMatch(
        /unknown layer kind/i,
      );
    }
  });

  it("mirrors the rasterizer's own rules for a derive layer", () => {
    // Each of these is a refusal the SERVER makes, so a console that let it
    // through would answer the operator with a 422 they cannot read. And each is
    // reachable in the ordinary course of authoring: turning a gradient into a
    // solid, giving a border a width, clearing a QR's payload.
    const derive = (spec: unknown) =>
      validateSlide({ id: "s", layers: [{ kind: "derive", ...geo, derive: spec } as never] }).map((p) => p.message);

    expect(derive({ kind: "qr", data: "https://waiveo.local/pair" })).toEqual([]);
    expect(derive({ kind: "text", text: "hi" })).toEqual([]);
    expect(derive({ kind: "rect", fill: { kind: "solid", from: "#FFFFFF" } })).toEqual([]);

    expect(derive({ kind: "qr" }).join(" ")).toMatch(/link or text to encode/i);
    expect(derive({ kind: "text" }).join(" ")).toMatch(/text is required/i);
    expect(derive({ kind: "rect" }).join(" ")).toMatch(/needs a fill/i);
    expect(derive({ kind: "hologram" }).join(" ")).toMatch(/unknown rasterized kind/i);
    expect(derive({ kind: "rect", fill: { kind: "linear", from: "#FFFFFF" } }).join(" ")).toMatch(/second colour/i);
    expect(derive({ kind: "rect", fill: { kind: "solid", from: "#FFFFFF", to: "#000000" } }).join(" ")).toMatch(
      /solid fill has no second colour/i,
    );
    expect(
      derive({ kind: "text", text: "hi", border: { width: 2 } }).join(" "),
    ).toMatch(/border with a width needs a colour/i);
    expect(derive({ kind: "text", text: "hi", color: "red" }).join(" ")).toMatch(/not a #RRGGBB/i);

    // TYPOGRAPHY is text-only and the server REFUSES it elsewhere rather than
    // ignoring it: the renderer writes the size and family into the text rule
    // only, and embeds the uploaded face for a text run only. A mirror that
    // stayed silent here would let the console send a body it knows will 422,
    // losing every other edit in the document with it.
    expect(derive({ kind: "text", text: "hi", font_px: 40, font_family: "Oswald",
      font_asset_ref: `sha256:${"a".repeat(64)}` })).toEqual([]);
    expect(derive({ kind: "qr", data: "x", font_px: 40 }).join(" ")).toMatch(/font size only applies/i);
    expect(derive({ kind: "qr", data: "x", font_family: "Oswald" }).join(" ")).toMatch(/font family only applies/i);
    expect(derive({ kind: "qr", data: "x", font_asset_ref: `sha256:${"a".repeat(64)}` }).join(" ")).toMatch(
      /custom font file only applies/i,
    );
    expect(derive({ kind: "rect", fill: { kind: "solid", from: "#FFFFFF" },
      font_asset_ref: `sha256:${"a".repeat(64)}` }).join(" ")).toMatch(/custom font file only applies/i);
    // …and a rect has no foreground for a colour to paint: its picture is its
    // fill, border and shadow.
    expect(derive({ kind: "rect", fill: { kind: "solid", from: "#FFFFFF" }, color: "#112233" }).join(" ")).toMatch(
      /no foreground colour/i,
    );

    // A layer with no spec at all, and a spec on a kind that does not take one —
    // the mirror direction, which the server also refuses.
    expect(validateSlide({ id: "s", layers: [{ kind: "derive", ...geo }] }).map((p) => p.message).join(" ")).toMatch(
      /needs something to draw/i,
    );
    expect(
      validateSlide({
        id: "s",
        layers: [{ kind: "text", ...geo, text: "hi", derive: { kind: "rect", fill: { kind: "solid", from: "#FFFFFF" } } } as never],
      })
        .map((p) => p.message)
        .join(" "),
    ).toMatch(/only a rasterized layer/i);
  });

  it("accepts a derive layer that has not been rendered yet", () => {
    // PENDING is the normal state of a freshly authored rasterized layer — the
    // off-appliance tool has not run. A mirror that demanded an asset_ref would
    // make it impossible to SAVE the thing the tool is supposed to find.
    expect(
      validateSlide({
        id: "s",
        layers: [{ kind: "derive", ...geo, derive: { kind: "qr", data: "x" } } as never],
      }),
    ).toEqual([]);
  });

  it("does not demand the DERIVED url of a content layer the way the serving gate does", () => {
    // `url` is minted by the producer at projection time
    // (wire.ValidateAuthoredSlideLayers accepts a content layer without one), so
    // an authored layer that names only its asset_ref is valid. A mirror
    // stricter than the server is not caution — it holds the save on a cast the
    // server would have taken.
    for (const kind of ["image", "video"] as const) {
      expect(validateSlide({ id: "s", layers: [{ kind, ...geo, asset_ref: "sha256:aa" }] })).toEqual([]);
    }
  });

  it("still refuses a video layer with no bytes behind it", () => {
    const problems = validateSlide({ id: "s", layers: [{ kind: "video", ...geo }] });
    expect(problems[0]?.message).toMatch(/pick a video/i);
  });

  it("indexes a whole cast's problems by slide so the filmstrip can badge them", () => {
    const bySlide = validateCastSlides([
      { id: "a", layers: [{ kind: "rect", ...geo, color: "#123456" }] },
      { id: "b", layers: [] },
    ]);
    expect(bySlide.has(0)).toBe(false);
    expect(bySlide.get(1)?.[0]?.index).toBeNull();
  });
});
