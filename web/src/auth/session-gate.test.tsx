import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useSearchParams } from "react-router";
import { SessionGate } from "./session-gate";
import { SignOutButton } from "./sign-out-button";
import type { SessionSummary, WaiveoApi } from "@/api";

/** Renders the `next` search parameter the gate redirected with, so a test can
 * assert on it without reaching into router internals. */
function NextEcho() {
  const [params] = useSearchParams();
  return <span data-testid="next">{params.get("next") ?? ""}</span>;
}

const SESSION: SessionSummary = {
  principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
  kind: "user",
  role: "owner",
  aal: "standard",
  session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
};

/** A minimal WaiveoApi stub carrying only the auth module the gate touches. */
function apiStub(auth: Partial<WaiveoApi["auth"]>): WaiveoApi {
  return {
    auth: {
      login: vi.fn(),
      logout: vi.fn(),
      session: vi.fn(async () => null),
      claim: vi.fn(),
      ...auth,
    },
  } as unknown as WaiveoApi;
}

function renderGate(api: WaiveoApi, initial = "/screens") {
  return render(
    <MemoryRouter initialEntries={[initial]}>
      <Routes>
        <Route path="/login" element={<p>sign in page</p>} />
        <Route
          path="*"
          element={
            <SessionGate api={api}>
              <p>console</p>
              <SignOutButton />
            </SessionGate>
          }
        />
      </Routes>
    </MemoryRouter>,
  );
}

describe("SessionGate", () => {
  it("renders the console when a session exists", async () => {
    const api = apiStub({ session: vi.fn(async () => SESSION) });
    renderGate(api);
    expect(await screen.findByText("console")).toBeInTheDocument();
  });

  it("redirects to the sign-in page when there is no session (SEC-005: refuse, never default-permit)", async () => {
    const api = apiStub({ session: vi.fn(async () => null) });
    renderGate(api);
    expect(await screen.findByText("sign in page")).toBeInTheDocument();
    expect(screen.queryByText("console")).not.toBeInTheDocument();
  });

  it("carries where the operator was trying to go, so signing in lands them back there", async () => {
    const api = apiStub({ session: vi.fn(async () => null) });
    render(
      <MemoryRouter initialEntries={["/schedules?filter=today"]}>
        <Routes>
          <Route path="/login" element={<NextEcho />} />
          <Route
            path="*"
            element={
              <SessionGate api={api}>
                <p>console</p>
              </SessionGate>
            }
          />
        </Routes>
      </MemoryRouter>,
    );
    // The redirect preserves the ORIGINAL path and query verbatim, so the
    // sign-in page can send the operator straight back to it.
    expect(await screen.findByTestId("next")).toHaveTextContent("/schedules?filter=today");
  });

  it("treats a transport failure as anonymous rather than rendering a console with no data", async () => {
    const api = apiStub({
      session: vi.fn(async () => {
        throw new Error("network down");
      }),
    });
    renderGate(api);
    expect(await screen.findByText("sign in page")).toBeInTheDocument();
  });

  it("probes exactly once per mount", async () => {
    const session = vi.fn(async () => SESSION);
    renderGate(apiStub({ session }));
    await screen.findByText("console");
    expect(session).toHaveBeenCalledTimes(1);
  });
});

describe("SignOutButton", () => {
  it("revokes the session SERVER-SIDE, then drops the console", async () => {
    const logout = vi.fn(async () => {});
    const api = apiStub({ session: vi.fn(async () => SESSION), logout });
    renderGate(api);
    await screen.findByText("console");

    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));

    // The server-side revoke is what makes the session row dead everywhere,
    // including any open events/1 stream it authenticated (EVT-114). Merely
    // forgetting the cookie locally would leave it live.
    await waitFor(() => expect(logout).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("sign in page")).toBeInTheDocument();
  });

  it("still signs out locally when the revoke call fails — the cookie may already be dead", async () => {
    const logout = vi.fn(async () => {
      throw new Error("401");
    });
    const api = apiStub({ session: vi.fn(async () => SESSION), logout });
    renderGate(api);
    await screen.findByText("console");

    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));
    expect(await screen.findByText("sign in page")).toBeInTheDocument();
  });

  it("renders nothing outside a SessionGate, so the shell can be mounted standalone", () => {
    render(
      <MemoryRouter>
        <SignOutButton />
      </MemoryRouter>,
    );
    expect(screen.queryByRole("button", { name: "Sign out" })).not.toBeInTheDocument();
  });
});
