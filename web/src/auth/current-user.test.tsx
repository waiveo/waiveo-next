import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import type { SessionSummary, WaiveoApi } from "@/api";
import { SessionGate } from "./session-gate";
import { CurrentUser } from "./current-user";

// The console fetched the session on every load and never showed it. These pin
// both halves of the fix: it names the ACCOUNT, and it names the AUTHORITY —
// the part that changes what the console will let you do, and the part an
// operator otherwise learns from a 403.

const SESSION: SessionSummary = {
  principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
  kind: "user",
  role: "viewer",
  aal: "standard",
  session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
};

function apiStub(session: SessionSummary | null): WaiveoApi {
  return {
    auth: {
      login: vi.fn(),
      logout: vi.fn(),
      session: vi.fn(async () => session),
      claim: vi.fn(),
    },
  } as unknown as WaiveoApi;
}

describe("CurrentUser", () => {
  it("renders nothing outside a SessionGate, rather than inventing an identity", () => {
    const { container } = render(<CurrentUser />);
    expect(container).toBeEmptyDOMElement();
  });

  it("names the signed-in principal and the authority it holds", async () => {
    render(
      <MemoryRouter>
        <SessionGate api={apiStub(SESSION)}>
          <CurrentUser />
        </SessionGate>
      </MemoryRouter>,
    );

    expect(await screen.findByText(SESSION.principal_id)).toBeInTheDocument();
    // The role is announced AS authority: "viewer" alone beside an id reads as
    // a name to a screen reader.
    expect(await screen.findByText(/signed in with the role/i)).toBeInTheDocument();
    expect(screen.getByText("viewer")).toBeInTheDocument();
  });
});
