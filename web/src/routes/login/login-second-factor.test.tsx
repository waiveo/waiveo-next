import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import LoginRoute from "./login-route";
import { ApiError, type Problem, type WaiveoApi } from "@/api";

// The console's half of the second-factor floor (security-model/1 SEC-004),
// driven by CLICKING and TYPING through the real form rather than by asserting
// that a component rendered. Every case below completes or fails a sign-in.

function LocationEcho() {
  const loc = useLocation();
  return <span data-testid="where">{loc.pathname + loc.search}</span>;
}

function apiStub(auth: Partial<WaiveoApi["auth"]>): WaiveoApi {
  return {
    auth: {
      login: vi.fn(),
      logout: vi.fn(),
      session: vi.fn(),
      claim: vi.fn(),
      enrollTotp: vi.fn(),
      confirmTotp: vi.fn(),
      ...auth,
    },
  } as unknown as WaiveoApi;
}

function renderLogin(api: WaiveoApi) {
  return render(
    <MemoryRouter initialEntries={["/login"]}>
      <Routes>
        <Route path="/login" element={<LoginRoute api={api} />} />
        <Route path="*" element={<LocationEcho />} />
      </Routes>
    </MemoryRouter>,
  );
}

/** The server's refusal for a login whose password was accepted but whose
 * second factor was not supplied: a 401 whose Problem NAMES the factor. */
function secondFactorDue(): ApiError {
  const problem = {
    type: "about:blank",
    title: "Unauthorized",
    status: 401,
    code: "UNAUTHENTICATED",
    trace_id: "01J8Z3K4N5P6Q7R8S9T0V1W2T1",
    second_factor: "totp",
  } as unknown as Problem;
  return new ApiError(401, problem, "01J8Z3K4N5P6Q7R8S9T0V1W2T1");
}

/** The server's refusal for a wrong password, a wrong code and a replayed code
 * alike — byte-identical, and carrying NO second-factor member. */
function genericRefusal(): ApiError {
  const problem = {
    type: "about:blank",
    title: "Unauthorized",
    status: 401,
    code: "UNAUTHENTICATED",
    detail: "The identifier or password is incorrect.",
    trace_id: "01J8Z3K4N5P6Q7R8S9T0V1W2T2",
  } as unknown as Problem;
  return new ApiError(401, problem, "01J8Z3K4N5P6Q7R8S9T0V1W2T2");
}

const goodSession = {
  principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
  kind: "user" as const,
  role: "owner" as const,
  aal: "standard" as const,
  session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
};

async function submitCredentials() {
  await userEvent.type(screen.getByLabelText(/identifier/i), "owner@example.test");
  await userEvent.type(screen.getByLabelText(/^password/i), "pw");
  await userEvent.click(screen.getByRole("button", { name: /sign in/i }));
}

describe("LoginRoute — second factor (SEC-004)", () => {
  it("does not offer a code field until the server says one is due", () => {
    renderLogin(apiStub({ login: vi.fn() }));
    expect(screen.queryByLabelText(/authentication code/i)).toBeNull();
  });

  it("reveals the code field on the server's second-factor refusal and completes the SAME operation with everything", async () => {
    const login = vi
      .fn()
      .mockRejectedValueOnce(secondFactorDue())
      .mockResolvedValueOnce(goodSession);
    renderLogin(apiStub({ login }));

    await submitCredentials();

    // First call carried no code, and was refused.
    await waitFor(() =>
      expect(login).toHaveBeenNthCalledWith(1, { identifier: "owner@example.test", password: "pw" }),
    );
    const codeField = await screen.findByLabelText(/authentication code/i);

    await userEvent.type(codeField, "123456");
    await userEvent.click(screen.getByRole("button", { name: /sign in/i }));

    // Second call is the SAME operation, carrying both factors.
    await waitFor(() =>
      expect(login).toHaveBeenNthCalledWith(2, {
        identifier: "owner@example.test",
        password: "pw",
        totp_code: "123456",
      }),
    );
    expect(await screen.findByTestId("where")).toHaveTextContent("/");
  });

  it("does not treat the second-factor prompt as an error", async () => {
    const login = vi.fn().mockRejectedValueOnce(secondFactorDue());
    renderLogin(apiStub({ login }));
    await submitCredentials();
    await screen.findByLabelText(/authentication code/i);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("reports a wrong code without confirming that the password was right", async () => {
    const login = vi
      .fn()
      .mockRejectedValueOnce(secondFactorDue())
      .mockRejectedValueOnce(genericRefusal());
    renderLogin(apiStub({ login }));

    await submitCredentials();
    await userEvent.type(await screen.findByLabelText(/authentication code/i), "000000");
    await userEvent.click(screen.getByRole("button", { name: /sign in/i }));

    const alert = await screen.findByRole("alert");
    // The message must not name the code as the thing that was wrong — that
    // would tell someone working through a password list that the password
    // was right, which is exactly what the server's identical refusal withholds.
    expect(alert.textContent ?? "").not.toMatch(/code (was|is) (wrong|incorrect|invalid)/i);
    expect(alert.textContent ?? "").not.toMatch(/password (was|is) (correct|right|accepted)/i);
  });

  it("keeps the code field after a refusal, and clears it so the next attempt carries a fresh code", async () => {
    const login = vi
      .fn()
      .mockRejectedValueOnce(secondFactorDue())
      .mockRejectedValueOnce(genericRefusal())
      .mockResolvedValueOnce(goodSession);
    renderLogin(apiStub({ login }));

    await submitCredentials();
    const codeField = await screen.findByLabelText(/authentication code/i);
    await userEvent.type(codeField, "000000");
    await userEvent.click(screen.getByRole("button", { name: /sign in/i }));

    await screen.findByRole("alert");
    const again = screen.getByLabelText(/authentication code/i) as HTMLInputElement;
    expect(again).toBeInTheDocument();
    expect(again.value).toBe("");

    await userEvent.type(again, "654321");
    await userEvent.click(screen.getByRole("button", { name: /sign in/i }));
    await waitFor(() =>
      expect(login).toHaveBeenNthCalledWith(3, {
        identifier: "owner@example.test",
        password: "pw",
        totp_code: "654321",
      }),
    );
  });

  it("still reports a lockout distinctly once the second factor is in play", async () => {
    const locked = new ApiError(
      429,
      {
        type: "about:blank",
        title: "Too Many Requests",
        status: 429,
        code: "CREDENTIAL_LOCKED",
        trace_id: "t",
      } as unknown as Problem,
      "t",
    );
    const login = vi.fn().mockRejectedValueOnce(secondFactorDue()).mockRejectedValueOnce(locked);
    renderLogin(apiStub({ login }));

    await submitCredentials();
    await userEvent.type(await screen.findByLabelText(/authentication code/i), "000000");
    await userEvent.click(screen.getByRole("button", { name: /sign in/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/too many failed attempts/i);
  });

  it("accepts only digits into the code field", async () => {
    const login = vi.fn().mockRejectedValueOnce(secondFactorDue());
    renderLogin(apiStub({ login }));
    await submitCredentials();
    const codeField = (await screen.findByLabelText(/authentication code/i)) as HTMLInputElement;
    await userEvent.type(codeField, "12ab34");
    expect(codeField.value).toBe("1234");
  });
});
