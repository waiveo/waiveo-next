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
| clsx | MIT |
| tailwind-merge | MIT |

### Development dependencies

| Package | License |
|---|---|
| @eslint/js | MIT |
| @tailwindcss/vite | MIT |
| tailwindcss | MIT |
| @testing-library/dom | MIT |
| @testing-library/jest-dom | MIT |
| @testing-library/react | MIT |
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

Self-hosted OFL fonts (via `@fontsource`) are added with the design-system theme.
