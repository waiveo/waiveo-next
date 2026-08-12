import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter } from "react-router";
import { vi } from "vitest";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { SessionGate } from "@/auth/session-gate";
import type { Role, SessionSummary, WaiveoApi } from "@/api";
import ApiKeysRoute from "./api-keys-route";
import { TRACE_ID } from "@/api/test-support";

// The API-keys page (security-model SEC-003a–e, SEC-020).
//
// The register's line for this family was "production automation has no
// credential path". The endpoints closed the engine half; this page is the half
// an operator reaches, and the tests below pin the ONE interaction that cannot
// be got wrong: a plaintext returned exactly once.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

function jsonBody(body: Parameters<typeof HttpResponse.json>[0]) {
  return HttpResponse.json(body, { headers: { "Trace-Id": TRACE_ID } });
}

const KEY = {
  id: "01J8Z9AVTHTEST0F1XTVRE0001",
  label: "ci-runner",
  created_at: 1_753_000_000_000,
  last_used_at: 0,
};

function renderRoute() {
  return render(
    <ThemeProvider>
      <MemoryRouter>
        <ApiKeysRoute />
      </MemoryRouter>
    </ThemeProvider>,
  );
}

/** The page inside a real SessionGate, so the role gate is exercised rather
 * than assumed absent. */
function renderAs(role: Role) {
  const session: SessionSummary = {
    principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
    kind: "user",
    role,
    aal: "standard",
    session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
  };
  const api = {
    auth: { login: vi.fn(), logout: vi.fn(), session: vi.fn(async () => session), claim: vi.fn() },
  } as unknown as WaiveoApi;
  return render(
    <ThemeProvider>
      <MemoryRouter>
        <SessionGate api={api}>
          <ApiKeysRoute />
        </SessionGate>
      </MemoryRouter>
    </ThemeProvider>,
  );
}

describe("API keys", () => {
  it("lists a key by label, and says plainly when one has never been used", async () => {
    server.use(http.get("*/api/v1/auth/api-keys", () => jsonBody({ items: [KEY], cursor: null })));
    renderRoute();

    const table = await screen.findByRole("table", { name: "API keys" });
    expect(within(table).getByText("ci-runner")).toBeInTheDocument();
    // A blank would read as "unknown". A key nobody has ever presented is the
    // one an operator can revoke without wondering what breaks.
    expect(within(table).getByText("never used")).toBeInTheDocument();
  });

  // The interaction the whole page exists for. SEC-003e returns the plaintext
  // exactly once and makes it unrecoverable; the reveal has to SAY that, and it
  // must not be swept away by the list refresh that follows the mint.
  it("shows the plaintext once, says it will not be shown again, and survives the refresh", async () => {
    let minted = false;
    server.use(
      http.get("*/api/v1/auth/api-keys", () =>
        jsonBody({ items: minted ? [KEY] : [], cursor: null }),
      ),
      http.post("*/api/v1/auth/api-keys", async () => {
        minted = true;
        return HttpResponse.json(
          { id: KEY.id, label: "ci-runner", key: "wv_secret_plaintext_value", created_at: KEY.created_at },
          { status: 201, headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );
    renderRoute();

    await userEvent.type(await screen.findByLabelText(/label/i), "ci-runner");
    await userEvent.click(screen.getByRole("button", { name: /mint an api key/i }));

    const reveal = await screen.findByRole("region", { name: /copy this key now/i });
    expect(within(reveal).getByText("wv_secret_plaintext_value")).toBeInTheDocument();
    expect(within(reveal).getByText(/only time/i)).toBeInTheDocument();
    expect(within(reveal).getByText(/cannot be recovered/i)).toBeInTheDocument();

    // The mint triggers a list reload. The secret must still be on screen after
    // it — a refresh taking away the one showing of a plaintext would lose the
    // credential outright.
    await waitFor(() => expect(screen.getByRole("table", { name: "API keys" })).toBeInTheDocument());
    expect(within(reveal).getByText("wv_secret_plaintext_value")).toBeInTheDocument();

    // And it goes only when the operator says so.
    await userEvent.click(within(reveal).getByRole("button", { name: /i have copied it/i }));
    expect(screen.queryByText("wv_secret_plaintext_value")).not.toBeInTheDocument();
  });

  // Revoking is not undoable and the confirmation has to name that, not ask
  // "are you sure".
  it("confirms a revoke by naming the consequence, then revokes", async () => {
    let revoked: string | null = null;
    server.use(
      http.get("*/api/v1/auth/api-keys", () =>
        jsonBody({ items: revoked ? [] : [KEY], cursor: null }),
      ),
      http.delete("*/api/v1/auth/api-keys/:id", ({ params }) => {
        revoked = String(params.id);
        return new HttpResponse(null, { status: 204 });
      }),
    );
    renderRoute();

    const table = await screen.findByRole("table", { name: "API keys" });
    await userEvent.click(within(table).getByRole("button", { name: /revoke ci-runner/i }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/stops working immediately/i)).toBeInTheDocument();
    expect(within(dialog).getByText(/cannot be restored/i)).toBeInTheDocument();

    await userEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(revoked).toBe(KEY.id));
  });

  // SEC-003b needs admin to mint. The gate is a COURTESY — the server refuses
  // regardless — and its value is that an operator is told before composing a
  // label, rather than after pressing a button.
  it("tells a viewer that minting needs admin, instead of letting them try", async () => {
    server.use(http.get("*/api/v1/auth/api-keys", () => jsonBody({ items: [], cursor: null })));
    renderAs("viewer");

    expect(await screen.findByText(/needs the/i)).toHaveTextContent(/admin/);
    expect(screen.getByRole("button", { name: /mint an api key/i })).toBeDisabled();
  });

  it("leaves the mint control usable for an admin", async () => {
    server.use(http.get("*/api/v1/auth/api-keys", () => jsonBody({ items: [], cursor: null })));
    renderAs("admin");

    await screen.findByRole("heading", { name: /mint a key/i });
    expect(screen.queryByText(/needs the/i)).not.toBeInTheDocument();
    // Disabled only because the label is empty, which is the ordinary state.
    await userEvent.type(screen.getByLabelText(/label/i), "ci-runner");
    expect(screen.getByRole("button", { name: /mint an api key/i })).toBeEnabled();
  });

  it("reports a listing that could not be read, rather than an empty, healthy-looking page", async () => {
    server.use(
      http.get("*/api/v1/auth/api-keys", () =>
        HttpResponse.json(
          { type: "about:blank", title: "Internal Server Error", status: 500, code: "INTERNAL", detail: "The store is unavailable.", trace_id: TRACE_ID },
          { status: 500, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    renderRoute();

    expect(await screen.findByRole("alert")).toHaveTextContent(/could not be read/i);
    expect(screen.queryByText("No API keys")).not.toBeInTheDocument();
  });
});

// Self-service password change (SEC-054). The consequence copy is the part
// under test as much as the request: an operator who guesses wrong about what
// a password change does either leaves a threat live or takes down an
// integration.
describe("change your password", () => {
  it("states both consequences — other sessions out, API keys kept", async () => {
    server.use(http.get("*/api/v1/auth/api-keys", () => jsonBody({ items: [], cursor: null })));
    renderAs("admin");

    const section = await screen.findByRole("region", { name: /change your password/i });
    expect(within(section).getByText(/other sessions are signed out/i)).toBeInTheDocument();
    expect(within(section).getByText(/api keys keep working/i)).toBeInTheDocument();
  });

  it("sends the current password alongside the new one", async () => {
    let sent: Record<string, unknown> | null = null;
    server.use(
      http.get("*/api/v1/auth/api-keys", () => jsonBody({ items: [], cursor: null })),
      http.put("*/api/v1/auth/password", async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    renderAs("admin");

    const section = await screen.findByRole("region", { name: /change your password/i });
    await userEvent.type(within(section).getByLabelText(/current password/i), "old-one");
    await userEvent.type(within(section).getByLabelText(/new password/i), "new-one");
    await userEvent.click(within(section).getByRole("button", { name: /change your password/i }));

    await waitFor(() =>
      expect(sent).toEqual({ current_password: "old-one", new_password: "new-one" }),
    );
  });

  // A wrong current password is a FIELD error, and the form must show it as
  // one. The server deliberately does not answer 401 here — that would tell
  // this client the session had ended and bounce the operator out of the form
  // over a typo.
  it("reports a wrong current password against the field, staying on the page", async () => {
    server.use(
      http.get("*/api/v1/auth/api-keys", () => jsonBody({ items: [], cursor: null })),
      http.put("*/api/v1/auth/password", () =>
        HttpResponse.json(
          {
            type: "about:blank",
            title: "Unprocessable Content",
            status: 422,
            code: "VALIDATION_FAILED",
            detail: "the current password is incorrect",
            trace_id: TRACE_ID,
          },
          { status: 422, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    renderAs("admin");

    const section = await screen.findByRole("region", { name: /change your password/i });
    await userEvent.type(within(section).getByLabelText(/current password/i), "wrong");
    await userEvent.type(within(section).getByLabelText(/new password/i), "new-one");
    await userEvent.click(within(section).getByRole("button", { name: /change your password/i }));

    expect(await within(section).findByText(/current password is incorrect/i)).toBeInTheDocument();
    // Still on the page, with the form intact.
    expect(within(section).getByLabelText(/new password/i)).toBeInTheDocument();
  });
});
