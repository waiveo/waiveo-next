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
  `POST /api/v1/content` for the bytes, then `PATCH /api/v1/casts/{id}` or
  `PATCH /api/v1/playlists/{id}` for the reference — so there is no second
  content path to keep in step with the first.

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
It renders `-concurrency` layers at a time across the WHOLE pass — one pool sized
by the Runner's own clamp, not one per row, so a workspace of single-layer casts
parallelises exactly as one many-layered cast does.

Each **distinct picture** is rendered once, not each layer: `DeriveDigest` is the
identity of a layer's pixels, so layers sharing one are one render whose answer
every one of them receives. That is not only a saved browser launch — the digest
is also the circuit breaker's key, so N concurrent renders of one key charged the
backoff N times per pass and made the report depend on which browser finished
first.

A per-layer failure never costs another layer. That includes a **panic**: the
pass holds every finished PNG in memory until its serial upload/apply/write-back
phase, so a crash in one unit used to discard the completed work of every other
row. Each unit renders under a `recover()` and a panic becomes that layer's
error.

### Both authored shapes

A `derive` layer lives in either shape that carries an authored layer stack: a
**cast slide**, or a **`source: "slide"` playlist item's inline slide**. Both are
rewritten into an ordinary `image` by the same projection, so both really do
reach a screen — and the queue reports both. A job says which with `source`
(`cast`/`playlist`) plus `resource_id`, then locates the layer with `slide_id`
(a cast slide's document-local id) or `item_index` (an inline slide has no id of
its own). `store.RowLayerStacks` is the single enumeration behind the queue AND
behind the retention/write-time projection, and `store.LayerStackKinds` — which
the queue *aliases* rather than copies — is the single list of kinds either one
scans, so the two cannot know about different shapes.

Both shapes are also held to the **same authoring gate**: an inline slide's
layers pass `wire.ValidateAuthoredSlideLayers` exactly as a cast slide's do
(`PLAYLIST_ITEM_SLIDE_LAYERS_INVALID`). They once did not, and the gap was not a
looser rule but the absence of one — a `derive` layer with no spec was a 422 in a
cast and a 201 inline, and this tool then dereferenced the nil mid-pass. A job
whose layer carries no spec is refused by the renderer with a reason, and the
queue does not emit one at all: `DerivePendingLayer.spec` is declared required
and non-nullable, and a work order with nothing to draw is not a work order.

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

Every member is refused on a kind that would IGNORE it, never quietly dropped:
`align`/`valign` on a `qr` or `rect`, `font_px`/`font_family`/`font_asset_ref` on
anything but `text`, `color` on a `rect`, a second stop on a `solid` fill. A
control an operator sets, saves and re-renders for, and never sees applied, is
the defect shape this codebase keeps producing.

### Custom typography

`font_asset_ref` is a content-addressed reference to a font file (TTF/OTF/WOFF2)
that the renderer fetches and embeds as an `@font-face` before drawing — the
member that makes "a font the device does not ship" possible at all. Upload the
file on the Content page, then attach it in the Studio: select a rasterized text
layer and use **Custom font file** in the properties panel. `font_family` is the
name the embedded face registers under.

Because it is a real content reference, it is checked to exist when the row is
written and it is held against the content retention sweep for as long as a row
names it.

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
content-addressing dedupe, what makes a second `sync` pass over a rendered
workspace cost nothing, and what lets duplicate layers collapse to one render. It
is held up by:

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
| **bounded concurrency clamp** (default 2, legacy's pool size) | `runner.go` (the clamp) + `sync.go` (the pool that pushes against it) | each concurrent job is a whole browser; unbounded is how a render host is taken down by its own queue. The clamp is only a clamp if something concurrent meets it, so `sync` runs the whole pass through a pool sized by `Runner.Concurrency()` — a clamp nothing ever reaches is a flag that reads as a setting and changes nothing |

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
