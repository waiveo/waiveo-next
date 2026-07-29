import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import LoginRoute from "./login-route";
import { ApiError, type WaiveoApi } from "@/api";

/** Renders the path the router is currently on, so a test can assert where a
 * successful sign-in navigated to. */
function LocationEcho() {
  const loc = useLocation();
  return <span data-testid="where">{loc.pathname + loc.search}</span>;
}

function apiStub(auth: Partial<WaiveoApi["auth"]>): WaiveoApi {
  return {
    auth: { login: vi.fn(), logout: vi.fn(), session: vi.fn(), claim: vi.fn(), ...auth },
  } as unknown as WaiveoApi;
}

function renderLogin(api: WaiveoApi, entry = "/login") {
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <Routes>
        <Route path="/login" element={<LoginRoute api={api} />} />
        <Route path="*" element={<LocationEcho />} />
      </Routes>
    </MemoryRouter>,
  );
}

async function signIn(identifier = "owner@example.test", password = "pw") {
  await userEvent.type(screen.getByLabelText(/identifier/i), identifier);
  await userEvent.type(screen.getByLabelText(/password/i), password);
  await userEvent.click(screen.getByRole("button", { name: /sign in/i }));
}

function problem(status: number, code: string): ApiError {
  return new ApiError(status, { type: "about:blank", title: "x", status, code, trace_id: "t" } as never, "t");
}

describe("LoginRoute", () => {
  it("authenticates from the form BODY and navigates on success (API-091)", async () => {
    const login = vi.fn(async () => ({
      principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
      kind: "user" as const,
      role: "owner" as const,
      aal: "standard" as const,
      session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
    }));
    renderLogin(apiStub({ login }));

    await signIn();

    await waitFor(() => expect(login).toHaveBeenCalledWith({ identifier: "owner@example.test", password: "pw" }));
    expect(await screen.findByTestId("where")).toHaveTextContent("/");
  });

  it("returns the operator to the page that bounced them here", async () => {
    const login = vi.fn(async () => ({
      principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
      kind: "user" as const,
      role: "owner" as const,
      aal: "standard" as const,
      session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
    }));
    renderLogin(apiStub({ login }), "/login?next=%2Fschedules%3Ffilter%3Dtoday");

    await signIn();
    expect(await screen.findByTestId("where")).toHaveTextContent("/schedules?filter=today");
  });

  it("refuses an absolute `next`, so the sign-in page is not an open redirect", async () => {
    const login = vi.fn(async () => ({
      principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
      kind: "user" as const,
      role: "owner" as const,
      aal: "standard" as const,
      session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
    }));
    renderLogin(apiStub({ login }), "/login?next=https%3A%2F%2Fevil.example%2Fsteal");

    await signIn();
    // Falls back to the overview rather than following an attacker-supplied
    // destination a phishing link could have planted in the query.
    expect(await screen.findByTestId("where")).toHaveTextContent("/");
  });

  it("does not disclose whether the identifier exists", async () => {
    const login = vi.fn(async () => {
      throw problem(401, "UNAUTHENTICATED");
    });
    renderLogin(apiStub({ login }));

    await signIn("nobody@example.test", "wrong");

    const alert = await screen.findByRole("alert");
    // The server already refuses a wrong password and an unknown identifier
    // identically so the endpoint cannot enumerate accounts; the page must not
    // hand back the distinction the server withheld.
    expect(alert).toHaveTextContent(/did not match/i);
    expect(alert.textContent ?? "").not.toMatch(/no such|unknown|not found|exist/i);
  });

  it("calls out a lockout distinctly, because retrying immediately cannot work (SEC-090)", async () => {
    const login = vi.fn(async () => {
      throw problem(429, "CREDENTIAL_LOCKED");
    });
    renderLogin(apiStub({ login }));

    await signIn();
    expect(await screen.findByRole("alert")).toHaveTextContent(/too many failed attempts/i);
  });

  it("stays on the page after a failure, so the operator can correct and retry", async () => {
    const login = vi.fn(async () => {
      throw problem(401, "UNAUTHENTICATED");
    });
    renderLogin(apiStub({ login }));

    await signIn();
    await screen.findByRole("alert");
    expect(screen.queryByTestId("where")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /sign in/i })).toBeEnabled();
  });

  it("offers the first-boot setup path unconditionally (SEC-120)", () => {
    renderLogin(apiStub({}));
    // An operator standing at a box nobody has claimed yet arrives HERE — the
    // console has no way to know the box is unclaimed, and giving it one would
    // mean the box answering "am I unclaimed?" to any anonymous caller that
    // asked. So the way through is always on offer, and says nothing about this
    // particular box because every box says it.
    expect(screen.getByRole("link", { name: /setup code/i })).toHaveAttribute("href", "/setup");
  });

  it("labels both fields, so the form is operable by assistive tech", () => {
    renderLogin(apiStub({}));
    expect(screen.getByLabelText(/identifier/i)).toHaveAttribute("autocomplete", "username");
    expect(screen.getByLabelText(/password/i)).toHaveAttribute("type", "password");
  });
});
