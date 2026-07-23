# Waiveo console (`web/`)

The Waiveo admin console — a single-page React app. It is built with Vite and
**embedded into the feeder binary** (`internal/app/webui`, via `go:embed`), so the
single-binary deployment carries its own web UI with no separate asset server to
run or mount. The feeder serves it at `/` for every non-API path; `/api/v1`,
`/events/v1`, `/content/` and `/telemetry/` keep priority.

## Stack

| Concern | Choice |
|---|---|
| Framework | React 19 |
| Build / dev server | Vite |
| Language | TypeScript (strict) |
| Styling | Tailwind CSS v4 (CSS-first, `@import "tailwindcss"`) |
| Component base | shadcn/ui — vendored into `src/components/ui/` (CLI config: `components.json`) |
| Routing | react-router |
| Tests | Vitest + Testing Library (jsdom) |
| Lint | ESLint (flat config) + typescript-eslint |

Node **≥ 24** is required (see `.node-version`).

## Commands

Run from the repo root (they wrap the npm scripts here):

| Command | What it does |
|---|---|
| `cd web && npm ci` | Install dependencies from the committed lockfile. |
| `make web-dev` | Vite dev server; `/api`, `/events` and `/content` proxy to the running Go feeder (`https://127.0.0.1:7420`, self-signed TLS accepted). |
| `make web-check` | The web gate: `tsc --noEmit` + `eslint` + `vitest run`. |
| `make web-build` | Production build into `web/dist`, then synced into `internal/app/webui/dist` (the `go:embed` source). |
| `make web-e2e` | The **click-through gate** (Playwright, dev-only): builds the SPA into the feeder embed, brings the full stack up, drives Chromium headless through the real console — New→fill→Save (a row appears **and** lands in the pack-data API), select→edit→Save (persists), Delete (gone), and every core nav item's heading — then tears the stack down. A dead control (renders but does nothing) fails it where a render-only test stays green. |

### One-time Playwright browser install

`make web-e2e` drives a real Chromium. Playwright is a **dev-only** dependency
(Apache-2.0) and never enters the production bundle, and the browser binary is
**not** committed — it lives in the OS cache (`~/Library/Caches/ms-playwright` on
macOS). Install it once after `npm ci`:

```sh
cd web && npx playwright install chromium
```

`web/dist/index.html` and `internal/app/webui/dist/index.html` are committed
**placeholder** shells (everything else the build emits is git-ignored). They keep
`go build ./...` green without a Node build; `make web-build` overwrites them with
the real bundle before a production run.

> `web/go.mod` is a module boundary with no Go code of its own. It exists only so
> the Go toolchain skips `web/node_modules` when expanding `./...` at the repo
> root (an npm dependency ships a Go package). Nothing imports it.

## The kit is the only sanctioned component surface

Pages, routes and the (later) ui-schema renderer import **only** from the Waiveo
widget kit at `src/components/kit/`. They must never reach into the vendored
shadcn base at `src/components/ui/` directly — the kit is the single wrapping
layer that makes the console (and every extension page rendered through it)
structurally uniform and on-brand.

This is enforced, not just documented: the ESLint `no-restricted-imports` rule in
`eslint.config.js` fails any import of `components/ui` outside `src/components/kit/`
and `src/components/ui/` themselves. A lint failure is the signal that a page
tried to bypass the kit.

## Theme

The console theme **is** the Waiveo **HORIZON** brand system — a dual-mode identity
(**Dusk**, dark, the default; **Daybreak**, light), driven entirely by CSS-variable
design tokens. The canonical source of every token value is the brand file
**`brand/HORIZON.md`** in the Waiveo brand/design workspace; the token set is
transcribed into `src/theme.css`. Any color, gradient, radius or type value that
deviates from that file is a defect. Fonts (Bricolage Grotesque, Inter, JetBrains
Mono) are self-hosted via `@fontsource` — no external font, CDN, or network
requests in the built bundle.

The vendored shadcn base in `src/components/ui/` is restyled so **all** styling
flows from those tokens (the primary Button carries `--grad-accent`; there are no
stock-shadcn palette remnants). `src/components/theme/` holds the `ThemeProvider`
(restores the last theme from `localStorage`, defaults to Dusk, reflects it as
`data-theme` on `<html>`) and the Dusk/Daybreak toggle. Brand build-rules are
enforced by tests: a token-equality test asserts `theme.css` matches
`brand/HORIZON.md` verbatim, and a rule test forbids text on a raw `--grad-hero`
(the `.wv-hero` utility always lays a scrim between the ramp and its content).

## Dependency licenses

Every **direct** dependency is permissively licensed (MIT / Apache-2.0 / ISC /
OFL only) — this ships inside a commercial-capable AGPL product, so no
source-available or paid-tier components are used.

### Runtime dependencies

| Package | License |
|---|---|
| react | MIT |
| react-dom | MIT |
| react-router | MIT |
| @tanstack/react-table | MIT |
| clsx | MIT |
| tailwind-merge | MIT |
| class-variance-authority | Apache-2.0 |
| lucide-react | ISC |
| sonner | MIT |
| cmdk | MIT |
| @radix-ui/react-slot | MIT |
| @radix-ui/react-dialog | MIT |
| @radix-ui/react-dropdown-menu | MIT |
| @radix-ui/react-popover | MIT |
| @radix-ui/react-tooltip | MIT |
| @radix-ui/react-tabs | MIT |
| @radix-ui/react-checkbox | MIT |
| @radix-ui/react-switch | MIT |
| @radix-ui/react-radio-group | MIT |
| @radix-ui/react-select | MIT |
| @radix-ui/react-label | MIT |
| @radix-ui/react-separator | MIT |
| @radix-ui/react-scroll-area | MIT |
| @fontsource-variable/bricolage-grotesque | OFL-1.1 (font) + MIT (package) |
| @fontsource-variable/inter | OFL-1.1 (font) + MIT (package) |
| @fontsource-variable/jetbrains-mono | OFL-1.1 (font) + MIT (package) |

### Development dependencies

| Package | License |
|---|---|
| @eslint/js | MIT |
| @tailwindcss/vite | MIT |
| tailwindcss | MIT |
| tw-animate-css | MIT |
| @testing-library/dom | MIT |
| @testing-library/jest-dom | MIT |
| @testing-library/react | MIT |
| @testing-library/user-event | MIT |
| @types/react | MIT |
| @types/react-dom | MIT |
| @vitejs/plugin-react | MIT |
| eslint | MIT |
| eslint-plugin-react-hooks | MIT |
| eslint-plugin-react-refresh | MIT |
| globals | MIT |
| jsdom | MIT |
| typescript | Apache-2.0 |
| typescript-eslint | MIT |
| vite | MIT |
| vitest | MIT |

The three brand faces — **Bricolage Grotesque** (display), **Inter** (UI) and
**JetBrains Mono** (telemetry) — are self-hosted via `@fontsource` (their font
files are OFL-1.1). They are imported in `src/main.tsx`, so Vite bundles the
`.woff2` files locally and the built app makes **zero external font/CDN/network
requests**.
