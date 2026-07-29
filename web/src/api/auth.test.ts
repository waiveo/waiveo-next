import { describe, expect, it, vi } from "vitest";
import { ApiClient, ApiError, CSRF_COOKIE, CSRF_HEADER, createApi, readCookie } from "./index";

const BASE = "http://192.0.2.10/api/v1";

/** A fetch stub returning a canned response and recording every call. */
function stubFetch(responses: Array<{ status: number; body?: unknown; headers?: Record<string, string> }>) {
  const calls: Array<{ url: string; init: RequestInit }> = [];
  let n = 0;
  const fetchImpl = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(input), init: init ?? {} });
    const r = responses[Math.min(n, responses.length - 1)];
    n += 1;
    const headers = new Headers({ "Content-Type": "application/json", ...(r?.headers ?? {}) });
    return new Response(r?.body === undefined ? null : JSON.stringify(r.body), {
      status: r?.status ?? 200,
      headers,
    });
  });
  return { fetchImpl: fetchImpl as unknown as typeof fetch, calls };
}

describe("readCookie", () => {
  it("reads a named cookie out of a document.cookie string", () => {
    expect(readCookie("csrf_token", "theme=dusk; csrf_token=abc123; other=x")).toBe("abc123");
  });
  it("returns null when the cookie is absent", () => {
    expect(readCookie("csrf_token", "theme=dusk")).toBeNull();
  });
});

describe("CSRF double-submit (security-model/1 SEC-024)", () => {
  it("echoes the CSRF cookie in the X-CSRF-Token header on a MUTATING request", async () => {
    const { fetchImpl, calls } = stubFetch([{ status: 201, body: { id: "x" } }]);
    const client = new ApiClient({
      baseUrl: BASE,
      fetch: fetchImpl,
      readCsrfToken: () => "csrf-value-1",
      newIdempotencyKey: () => "k",
    });
    await client.create("/playlists", { name: "p" });

    const headers = new Headers(calls[0]?.init.headers);
    expect(headers.get(CSRF_HEADER)).toBe("csrf-value-1");
  });

  it("does NOT send the CSRF header on a safe request — there is nothing to forge", async () => {
    const { fetchImpl, calls } = stubFetch([{ status: 200, body: { items: [], cursor: null } }]);
    const client = new ApiClient({ baseUrl: BASE, fetch: fetchImpl, readCsrfToken: () => "csrf-value-1" });
    await client.list("/playlists");

    const headers = new Headers(calls[0]?.init.headers);
    expect(headers.get(CSRF_HEADER)).toBeNull();
  });

  it("omits the header when no CSRF cookie is present, rather than sending an empty one", async () => {
    const { fetchImpl, calls } = stubFetch([{ status: 201, body: { id: "x" } }]);
    const client = new ApiClient({
      baseUrl: BASE,
      fetch: fetchImpl,
      readCsrfToken: () => null,
      newIdempotencyKey: () => "k",
    });
    await client.create("/playlists", { name: "p" });

    const headers = new Headers(calls[0]?.init.headers);
    expect(headers.has(CSRF_HEADER)).toBe(false);
  });

  it("names the cookie the server names", () => {
    expect(CSRF_COOKIE).toBe("csrf_token");
    expect(CSRF_HEADER).toBe("X-CSRF-Token");
  });
});

describe("401 handling", () => {
  it("raises the unauthenticated hook once and still throws the ApiError", async () => {
    const { fetchImpl } = stubFetch([
      { status: 401, body: { code: "UNAUTHENTICATED", title: "Unauthorized", status: 401 } },
    ]);
    const onUnauthenticated = vi.fn();
    const client = new ApiClient({ baseUrl: BASE, fetch: fetchImpl, onUnauthenticated });

    await expect(client.read("/playlists/x")).rejects.toBeInstanceOf(ApiError);
    expect(onUnauthenticated).toHaveBeenCalledTimes(1);
  });

  it("fires the hook only ONCE across several failing requests, so a page issuing parallel reads redirects once", async () => {
    const { fetchImpl } = stubFetch([
      { status: 401, body: { code: "UNAUTHENTICATED", title: "Unauthorized", status: 401 } },
    ]);
    const onUnauthenticated = vi.fn();
    const client = new ApiClient({ baseUrl: BASE, fetch: fetchImpl, onUnauthenticated });

    await Promise.allSettled([
      client.read("/playlists/a"),
      client.read("/playlists/b"),
      client.read("/playlists/c"),
    ]);
    expect(onUnauthenticated).toHaveBeenCalledTimes(1);
  });

  it("does NOT fire the hook for the session probe — a 401 there is the ANSWER, not a failure", async () => {
    const { fetchImpl } = stubFetch([
      { status: 401, body: { code: "UNAUTHENTICATED", title: "Unauthorized", status: 401 } },
    ]);
    const onUnauthenticated = vi.fn();
    const api = createApi({ baseUrl: BASE, fetch: fetchImpl, onUnauthenticated });

    await expect(api.auth.session()).resolves.toBeNull();
    // Firing here would send the sign-in page to the sign-in page, forever.
    expect(onUnauthenticated).not.toHaveBeenCalled();
  });

  it("does NOT fire the hook for a credential-exchange refusal — the page collecting the credential must stay put", async () => {
    const { fetchImpl } = stubFetch([
      { status: 401, body: { code: "UNAUTHENTICATED", title: "Unauthorized", status: 401 } },
    ]);
    const onUnauthenticated = vi.fn();
    const api = createApi({ baseUrl: BASE, fetch: fetchImpl, onUnauthenticated });

    // A refused sign-in and a refused first-boot claim are the SAME kind of
    // answer: the credentials just presented were not accepted. Neither is the
    // loss of a session, because neither operation ever had one. Redirecting
    // would reload the form that is mid-conversation with the operator and throw
    // away the message explaining what to fix.
    await expect(api.auth.login({ identifier: "a", password: "b" })).rejects.toBeInstanceOf(ApiError);
    expect(onUnauthenticated).not.toHaveBeenCalled();

    await expect(
      api.auth.claim({ code: "wrong", identifier: "a", password: "b" }),
    ).rejects.toBeInstanceOf(ApiError);
    expect(onUnauthenticated).not.toHaveBeenCalled();
  });
});

describe("auth module", () => {
  it("login POSTs the credentials in the BODY and carries no Idempotency-Key (API-051/091)", async () => {
    const session = {
      principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
      kind: "user",
      role: "owner",
      aal: "standard",
      session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
      csrf_token: "csrf-1",
    };
    const { fetchImpl, calls } = stubFetch([{ status: 200, body: session }]);
    const api = createApi({ baseUrl: BASE, fetch: fetchImpl });

    await expect(api.auth.login({ identifier: "owner@example.test", password: "pw" })).resolves.toEqual(session);

    const call = calls[0];
    expect(call?.url).toBe(`${BASE}/auth/login`);
    expect(call?.init.method).toBe("POST");
    expect(JSON.parse(String(call?.init.body))).toEqual({ identifier: "owner@example.test", password: "pw" });
    // An Idempotency-Key is scoped to the AUTHENTICATED principal (API-051), and
    // a caller minting its first session has none for a key to be scoped by.
    expect(new Headers(call?.init.headers).has("Idempotency-Key")).toBe(false);
  });

  it("surfaces CREDENTIAL_LOCKED distinctly, since it is the one failure with a different remedy", async () => {
    const { fetchImpl } = stubFetch([
      { status: 429, body: { code: "CREDENTIAL_LOCKED", title: "Too Many Requests", status: 429 } },
    ]);
    const api = createApi({ baseUrl: BASE, fetch: fetchImpl });

    await expect(api.auth.login({ identifier: "a", password: "b" })).rejects.toMatchObject({
      code: "CREDENTIAL_LOCKED",
      status: 429,
    });
  });

  it("logout POSTs and resolves on a 204", async () => {
    const { fetchImpl, calls } = stubFetch([{ status: 204 }]);
    const api = createApi({ baseUrl: BASE, fetch: fetchImpl });

    await expect(api.auth.logout()).resolves.toBeUndefined();
    expect(calls[0]?.url).toBe(`${BASE}/auth/logout`);
    expect(calls[0]?.init.method).toBe("POST");
  });

  it("claim POSTs the setup code and the chosen credential", async () => {
    const { fetchImpl, calls } = stubFetch([
      {
        status: 201,
        body: {
          principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
          kind: "user",
          role: "owner",
          aal: "standard",
          session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
        },
      },
    ]);
    const api = createApi({ baseUrl: BASE, fetch: fetchImpl });

    await api.auth.claim({ code: "deadbeef", identifier: "first", password: "pw" });
    expect(calls[0]?.url).toBe(`${BASE}/auth/setup`);
    expect(JSON.parse(String(calls[0]?.init.body))).toEqual({
      code: "deadbeef",
      identifier: "first",
      password: "pw",
    });
  });

  it("never exposes a session token — the browser holds it in an HttpOnly cookie (SEC-023)", async () => {
    const { fetchImpl } = stubFetch([
      {
        status: 200,
        body: {
          principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
          kind: "user",
          role: "owner",
          aal: "standard",
          session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
        },
      },
    ]);
    const api = createApi({ baseUrl: BASE, fetch: fetchImpl });
    const s = await api.auth.session();
    expect(s).not.toBeNull();
    expect(Object.keys(s ?? {})).not.toContain("token");
  });
});
