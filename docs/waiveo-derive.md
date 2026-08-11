# waiveo-derive — the off-appliance rasterizer

A signage slide can want five things no player node can draw:

- a **gradient** (linear or radial)
- a **drop shadow**
- a **rounded or stroked border**
- **custom typography** — a font the device does not ship
- a **QR code**

`waiveo-derive` renders those to a PNG, uploads it to the content origin, and
writes the resulting `asset_ref` back onto the layer. From then on the layer
reaches the screen through the **existing image path** — fetched,
content-address-verified and cached by the player exactly like any other image.
**No player change is involved in this feature at all.**

## It does not run on the appliance

This is the constraint the whole design is built around, not an implementation
detail. The feeder and the relay are deliberately Go-only, and the box this
platform replaces already hosts the legacy stack — a second headless Chromium on
a Pi is a non-starter. So:

- the renderer is a **separate binary** an operator or a dev box runs out of band;
- the appliance's entire share of the loop is **one read-only listing**,
  `GET /api/v1/derive/pending`;
- results come back through operations that already existed —
  `POST /api/v1/content` for the bytes, `PATCH /api/v1/casts/{id}` for the
  reference — so there is no second content path to keep in step with the first.

`cmd/waiveo-feeder/offappliance_test.go` asserts this from the real import graph:
neither shipped binary may reach `internal/derive`, and `cmd/waiveo-derive` must.

## Using it

```sh
# One spec to a PNG — no server involved. The fastest way to iterate on a design.
waiveo-derive render -spec card.json -w 1200 -h 320 -out card.png

# What is outstanding on a box.
waiveo-derive pending -api https://192.168.50.12:7420 -insecure-tls

# Render everything outstanding, upload it, write the references back.
waiveo-derive sync -api https://192.168.50.12:7420 -insecure-tls
```

`sync` is **one pass, not a daemon**: run it after authoring, or from a CI step.

The credential is read from the dev key file (`make dev-key`) or from
`-token-file`, never from a flag — an argument is visible in `ps` and lands in
shell history.

## Authoring

In the Studio: **Add rasterized** → QR code / Styled text / Styled panel. The
layer lands on the canvas marked `NEEDS RENDER` and stays marked until the tool
has run. Over the API it is an ordinary `derive` slide layer:

```json
{
  "kind": "derive", "x": 1400, "y": 120, "w": 400, "h": 400,
  "derive": {
    "kind": "qr", "data": "https://waiveo.local/pair/ABCD-1234", "ec_level": "M",
    "color": "#111827",
    "fill": { "kind": "solid", "from": "#FFFFFF" },
    "border": { "width": 6, "color": "#7C3AED", "radius": 24 },
    "shadow": { "dy": 8, "blur": 24, "color": "#000000", "opacity_pct": 45 }
  }
}
```

The spec vocabulary is **closed and declarative** — there is deliberately no way
to send the renderer markup, because it drives a real browser on somebody's
machine over content that may have been authored by somebody else.

## Pending, stale, current

`derived_from` records the digest of the spec **and the geometry** the current
raster was produced from. Geometry is in the digest because the PNG is rendered
at exactly the layer's pixel size — resizing a layer changes the picture as
surely as changing its text does, and a digest over the spec alone would leave a
resized layer reading "current" while the panel showed a stretched image.

| state | meaning | what a screen shows |
|---|---|---|
| `pending` | no raster has ever been produced | the layer is omitted; **the rest of the slide still draws** |
| `stale` | the spec or the geometry changed since the raster | **the previous picture keeps playing** until the tool catches up |
| `current` | the raster matches | the raster |

Serving a stale raster rather than dropping it is deliberate: an edit nobody has
rendered yet must never blank a screen.

## The envelope rule: a shadow is drawn INSIDE the layer

A CSS box-shadow paints outside the element that casts it. Legacy captured the
element's own rectangle and clipped every shadow — the "blur clip-expansion" item
still open on that codebase's ledger.

Here the output must be exactly the authored `w`×`h`, because it becomes an
`image` layer of that geometry and anything else is silently rescaled on the
panel. So the **authored layer box is the shadow's bounding box** and the styled
element is inset inside it by the shadow's own extent per side (`ShadowInset`).
A layer with no room left for its own shadow is refused with a message saying so,
rather than rendering a zero-size box that captures as a blank PNG.

## Determinism

Same spec + same geometry → **byte-identical PNG**, which is what makes
content-addressing dedupe and what makes a second `sync` pass over a rendered
workspace cost nothing. It is held up by:

- a pure-Go, deterministic QR encoder (`internal/derive/qr`, golden-tested
  against an independent implementation across all forty versions);
- a page builder that is a pure function of the job, with integer-only
  arithmetic for anything that reaches the CSS text;
- Chromium launch flags that pin the colour profile, LCD/subpixel text, font
  hinting and compositor staging;
- re-encoding the capture through Go's PNG encoder, so the browser's own encoder
  settings and metadata cannot reach the digest.

Determinism holds for **one Chromium build**. A different Chromium major may
antialias differently; pin one with `WAIVEO_DERIVE_CHROMIUM` if byte-stability
across machines matters.

## The guards, and why each one exists

All four are transplanted from legacy's `render-worker.js`, where they were paid
for in production. Every one of them fails as a **hang or a silent wrong
picture**, never as an error.

| guard | where | the failure it prevents |
|---|---|---|
| per-job wall-clock timeout + **process-tree kill** | `browser.go` | a wedged Chromium does not exit when its parent gives up; `Process.Kill` reaches only the launcher, so renderer/GPU/zygote children survive, hold the profile dir, and accumulate until the host is out of memory |
| **two-rAF idle-frame force** (+ `document.fonts.ready`) | `browser.go` | a loaded, idle headless page may never commit another compositor frame, and `captureScreenshot` then blocks for its **full** timeout waiting for one — legacy's #1154, where the immediate retry succeeded in ~35 ms |
| **per-element circuit breaker**, 60 s doubling to a 10 min ceiling | `runner.go` | one layer that cannot render is retried every pass forever and starves the layers that can |
| **bounded concurrency clamp** (default 2, legacy's pool size) | `runner.go` | each concurrent job is a whole browser; unbounded is how a render host is taken down by its own queue |

Plus one legacy could not afford: a **blank-capture recapture**. A fully
transparent PNG at the correct dimensions is what a capture taken during a font
race looks like. Legacy had to ship it with a warning, because throwing there
dropped the whole slide. Here a failed layer costs only that layer — the slide
around it keeps playing — so the second blank is an error, and the layer simply
stays pending instead of an invisible rectangle being uploaded and hung on a
wall.

The three guards that are not about a browser live in `runner.go` behind the
`Renderer` interface, so all of them are tested with no Chromium installed.

## Why one browser per job

Legacy kept a long-lived browser with a pool of reused pages, and spent a large
fraction of its render worker recovering from the shared state that implies:
context poisoning, a consecutive-inner-timeout detector, a pool-starvation
detector, a context-rebuild path.

This tool launches a browser per job and kills the tree afterwards. It is the
right trade *here* and would have been the wrong one *there*: legacy re-rendered
widgets on a timer, on the appliance, forever, so startup was a per-second cost.
This runs out of band over a handful of layers when somebody asks it to, and
~half a second per layer buys the complete removal of every shared-state failure
mode above.
