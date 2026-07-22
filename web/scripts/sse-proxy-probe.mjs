// web/scripts/sse-proxy-probe.mjs — the Task 2 regression guard for the live
// event stream reaching the SPA THROUGH the Vite dev proxy (not just direct to
// the feeder). It boots the REAL Vite dev server (web/vite.config.ts, so the
// /events proxy and its SSE header-flush hook are exactly what production dev
// uses) against an already-running feeder, then probes /events/v1 over the
// proxy's plain-HTTP port and asserts two things the browser depends on:
//
//   A. FRESH subscribe (no resume_from) → 200 + `text/event-stream` headers
//      arrive within 3s. This is the precise path the bug hung on: the feeder
//      opens an SSE response with headers then stays silent until an event, and
//      http-proxy-3 buffered those headers until a first body byte that never
//      came, so EventSource never fired `open`. Headers-before-body IS the fix.
//   B. BACKLOG subscribe (resume_from=genesis) → at least one SSE body byte
//      streams within 3s: the relay's boot-recorded automation.run event
//      (WAIVEO_RELAY_DEMO_OBSERVE) flowing through the proxy pipe end to end.
//
// B runs FIRST, as a barrier: it blocks until that one demo event is ingested,
// draining it into the past so A's fresh subscribe is genuinely quiescent (no
// heartbeat, no further events) and thus a true test of the header-flush hook —
// otherwise a live boot event could flush A's headers and mask a missing fix.
//
// It requires a feeder on 127.0.0.1:7420; the `make web-sse-check` target brings
// the feeder+relay stack up first and tears it down after. The probe hits the
// proxy over plain HTTP (localhost) — the HTTPS-to-feeder leg with `secure:false`
// is the proxy's job — so no TLS trust decision is made here. Prints SSE PROXY OK
// on success, or SSE PROXY FAIL: <reason> and exits non-zero otherwise.

import { createServer } from "vite";
import { fileURLToPath } from "node:url";
import net from "node:net";
import path from "node:path";

const WEB_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

// The feeder listener the proxy targets (mirrors web/vite.config.ts FEEDER).
// Readiness polls this port directly with a bare TCP connect — never through the
// proxy — so the backgrounded feeder's bind race never makes Vite log a
// (transient, handled) ECONNREFUSED proxy error that reads like a failure.
const FEEDER_HOST = "127.0.0.1";
const FEEDER_PORT = 7420;

// A valid-but-minimal ULID (26 zeros) strictly below every real event id, so the
// resume resolves to a retention gap that streams the WHOLE retained backlog —
// the boot-fired automation.run included — exactly as scripts/observabilityprobe
// reads it back. An empty log answers this with 400 RESUME_FROM_INVALID; retry.
const GENESIS = "00000000000000000000000000";

const HEADER_BUDGET_MS = 3000; // the assertion the task pins: headers within 3s
const BODY_BUDGET_MS = 3000; // one backlog read attempt
const BODY_RETRY_BUDGET_MS = 20000; // the boot event may lag Vite's startup briefly
const READY_BUDGET_MS = 20000; // `make dev-up` backgrounds the feeder; wait for its bind

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failed = false;
function fail(msg) {
  console.error(`SSE PROXY FAIL: ${msg}`);
  failed = true;
}

async function main() {
  // Wait for the backgrounded feeder to bind BEFORE starting Vite's proxy, so the
  // proxy's first connection already succeeds and Vite logs no startup error.
  if (!(await waitForFeeder())) {
    process.exitCode = 1;
    return;
  }

  const server = await createServer({
    root: WEB_DIR, // auto-loads web/vite.config.ts — the real /events proxy + hook
    server: { port: 0, strictPort: false, host: "127.0.0.1" },
    logLevel: "warn",
    clearScreen: false,
  });
  await server.listen();
  const addr = server.httpServer?.address();
  const port = addr && typeof addr === "object" ? addr.port : server.config.server.port;
  const base = `http://127.0.0.1:${port}`;

  try {
    // Order matters. BACKLOG runs FIRST and blocks until the relay's single boot
    // automation.run has been ingested (it streams that event's bytes). That both
    // proves body bytes pipe through the proxy AND drains the one demo event into
    // the past, so the subsequent FRESH subscribe is guaranteed quiescent: the
    // feeder sends no heartbeat and no further events fire, so the ONLY thing that
    // can deliver its headers is the flush hook. Run FRESH first and a live boot
    // event could flush the headers as a side effect and mask a missing fix.
    await assertBacklogBytes(base);
    await assertFreshHeaders(base);
  } finally {
    await server.close();
  }

  if (failed) process.exitCode = 1;
  else console.log("SSE PROXY OK (fresh headers <3s + a recorded event's bytes stream through the Vite proxy)");
}

// The feeder binds FEEDER_PORT only once it is fully initialized and serving
// (its "listening (HTTPS)" line is the last boot step), so a successful bare TCP
// connect is a sufficient, TLS-free readiness signal. Polling the port directly
// (not the proxy) keeps the proxy's first real connection clean, so Vite logs no
// startup error. Returns true when ready, false (after failing) if it never binds.
async function waitForFeeder() {
  const deadline = Date.now() + READY_BUDGET_MS;
  while (Date.now() < deadline) {
    if (await tcpOpen(FEEDER_HOST, FEEDER_PORT, 1000)) return true;
    await sleep(250);
  }
  fail(`feeder never bound ${FEEDER_HOST}:${FEEDER_PORT} within ${READY_BUDGET_MS}ms`);
  return false;
}

// tcpOpen resolves true if a TCP connection to host:port succeeds within
// timeoutMs, false on refusal/timeout/error. It always tears the socket down.
function tcpOpen(host, port, timeoutMs) {
  return new Promise((resolve) => {
    const socket = net.connect({ host, port });
    const settle = (ok) => {
      socket.destroy();
      resolve(ok);
    };
    socket.setTimeout(timeoutMs);
    socket.once("connect", () => settle(true));
    socket.once("timeout", () => settle(false));
    socket.once("error", () => settle(false));
  });
}

// A. A fresh subscribe must open its 200 + text/event-stream headers within the
// budget. It runs after the BACKLOG barrier, so the stream is quiescent and the
// flush hook is the only thing that can deliver the headers. `fetch` resolves as
// soon as response headers arrive, so a hang shows up as the AbortController
// firing — the exact failure the header-flush fix removes.
async function assertFreshHeaders(base) {
  const ac = new AbortController();
  const timer = setTimeout(() => ac.abort(), HEADER_BUDGET_MS);
  const t0 = Date.now();
  try {
    const res = await fetch(`${base}/events/v1`, {
      headers: { Accept: "text/event-stream" },
      signal: ac.signal,
    });
    const ms = Date.now() - t0;
    const ct = res.headers.get("content-type") ?? "";
    await res.body?.cancel(); // headers are all we needed; close the idle stream
    if (res.status !== 200) return fail(`FRESH: expected status 200, got ${res.status}`);
    if (!ct.includes("text/event-stream")) return fail(`FRESH: expected text/event-stream, got '${ct}'`);
    console.log(`  FRESH ok: 200 ${ct} opened in ${ms}ms (< ${HEADER_BUDGET_MS}ms)`);
  } catch (e) {
    if (e.name === "AbortError") fail(`FRESH: no SSE headers within ${HEADER_BUDGET_MS}ms (the proxy hang)`);
    else fail(`FRESH: ${e.message}`);
  } finally {
    clearTimeout(timer);
  }
}

// B. A backlog subscribe must stream at least one body byte through the proxy.
// The boot event may not be ingested the instant Vite is up, so retry the
// genesis-resume read (400 RESUME_FROM_INVALID = log still empty) within budget.
async function assertBacklogBytes(base) {
  const deadline = Date.now() + BODY_RETRY_BUDGET_MS;
  while (Date.now() < deadline) {
    const r = await readFirstBacklogByte(base);
    if (r.ok) {
      console.log(`  BACKLOG ok: ${r.bytes} body byte(s) streamed through the proxy in ${r.ms}ms`);
      return;
    }
    if (!r.retry) return fail(r.msg);
    await sleep(500);
  }
  fail(`BACKLOG: no recorded event streamed through the proxy within ${BODY_RETRY_BUDGET_MS}ms`);
}

async function readFirstBacklogByte(base) {
  const ac = new AbortController();
  const timer = setTimeout(() => ac.abort(), BODY_BUDGET_MS);
  const t0 = Date.now();
  try {
    const res = await fetch(`${base}/events/v1?resume_from=${GENESIS}`, {
      headers: { Accept: "text/event-stream" },
      signal: ac.signal,
    });
    if (res.status === 400) {
      await res.body?.cancel(); // RESUME_FROM_INVALID: log still empty — retry
      return { ok: false, retry: true };
    }
    if (res.status !== 200) {
      await res.body?.cancel();
      return { ok: false, retry: false, msg: `BACKLOG: expected status 200, got ${res.status}` };
    }
    const reader = res.body.getReader();
    const { value, done } = await reader.read();
    const ms = Date.now() - t0;
    await reader.cancel();
    if (done || !value || value.length === 0) {
      return { ok: false, retry: false, msg: "BACKLOG: stream closed with no body byte" };
    }
    return { ok: true, bytes: value.length, ms };
  } catch (e) {
    if (e.name === "AbortError") return { ok: false, retry: false, msg: `BACKLOG: no body byte within ${BODY_BUDGET_MS}ms` };
    return { ok: false, retry: false, msg: `BACKLOG: ${e.message}` };
  } finally {
    clearTimeout(timer);
  }
}

main().catch((e) => {
  console.error(`SSE PROXY FAIL: ${e.stack || e.message}`);
  process.exit(1);
});
