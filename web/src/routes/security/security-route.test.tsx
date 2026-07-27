import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import SecurityRoute from "./security-route";
import { SessionGate } from "@/auth/session-gate";
import { ApiError, type Problem, type WaiveoApi } from "@/api";

// The enrollment flow, driven by clicking the real controls. "The page rendered"
// is not evidence that an enrollment can be completed, so every case here starts
// an enrollment and types a code into the form.

const session = {
  principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
  kind: "user" as const,
  role: "owner" as const,
  aal: "standard" as const,
  session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
};

function apiStub(auth: Partial<WaiveoApi["auth"]>): WaiveoApi {
  return {
    auth: {
      login: vi.fn(),
      logout: vi.fn(),
      session: vi.fn(async () => session),
      claim: vi.fn(),
      enrollTotp: vi.fn(),
      confirmTotp: vi.fn(),
      ...auth,
    },
  } as unknown as WaiveoApi;
}

function renderSecurity(api: WaiveoApi) {
  return render(
    <MemoryRouter initialEntries={["/security"]}>
      <SessionGate api={api}>
        <SecurityRoute api={api} />
      </SessionGate>
    </MemoryRouter>,
  );
}

function problem(status: number, code: string, detail?: string): ApiError {
  return new ApiError(
    status,
    { type: "about:blank", title: "x", status, code, detail, trace_id: "t" } as unknown as Problem,
    "t",
  );
}

const enrollment = {
  secret: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP",
  otpauth_uri:
    "otpauth://totp/Waiveo:owner%40example.test?algorithm=SHA1&digits=6&issuer=Waiveo&period=30&secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP",
};

describe("SecurityRoute — second-factor enrollment (SEC-004)", () => {
  it("does not show a secret until the operator starts an enrollment", async () => {
    const enrollTotp = vi.fn();
    renderSecurity(apiStub({ enrollTotp }));
    expect(await screen.findByRole("button", { name: /set up authenticator app/i })).toBeInTheDocument();
    expect(screen.queryByTestId("totp-secret")).toBeNull();
    expect(enrollTotp).not.toHaveBeenCalled();
  });

  it("shows the key and the otpauth URI, then arms the credential with a code typed back", async () => {
    const enrollTotp = vi.fn(async () => enrollment);
    const confirmTotp = vi.fn(async () => ({
      credential_id: "01J8Z3K4N5P6Q7R8S9T0V1W2C1",
      principal_id: session.principal_id,
      kind: "totp" as const,
      created_at: 1752537600000,
      revoked_sessions: 2,
    }));
    renderSecurity(apiStub({ enrollTotp, confirmTotp }));

    await userEvent.click(await screen.findByRole("button", { name: /set up authenticator app/i }));

    expect(await screen.findByTestId("totp-secret")).toHaveTextContent(enrollment.secret);
    expect(screen.getByTestId("totp-uri")).toHaveTextContent("otpauth://totp/");

    await userEvent.type(screen.getByLabelText(/code from your app/i), "123456");
    await userEvent.click(screen.getByRole("button", { name: /turn on/i }));

    await waitFor(() => expect(confirmTotp).toHaveBeenCalledWith("123456"));
    const done = await screen.findByRole("status");
    expect(done).toHaveTextContent(/required to sign in/i);
    // The eviction is stated, not left to be discovered on the other device.
    expect(done).toHaveTextContent(/2 other sessions signed out/i);
  });

  it("warns before committing that other sessions will be signed out", async () => {
    renderSecurity(apiStub({ enrollTotp: vi.fn() }));
    expect(await screen.findByText(/signs you out everywhere else/i)).toBeInTheDocument();
  });

  it("keeps the enrollment open and clears the field when a code does not match", async () => {
    const enrollTotp = vi.fn(async () => enrollment);
    const confirmTotp = vi
      .fn()
      .mockRejectedValueOnce(problem(401, "UNAUTHENTICATED"))
      .mockResolvedValueOnce({
        credential_id: "01J8Z3K4N5P6Q7R8S9T0V1W2C1",
        principal_id: session.principal_id,
        kind: "totp" as const,
        created_at: 1752537600000,
        revoked_sessions: 0,
      });
    renderSecurity(apiStub({ enrollTotp, confirmTotp }));

    await userEvent.click(await screen.findByRole("button", { name: /set up authenticator app/i }));
    await userEvent.type(await screen.findByLabelText(/code from your app/i), "000000");
    await userEvent.click(screen.getByRole("button", { name: /turn on/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/does not match/i);
    const field = screen.getByLabelText(/code from your app/i) as HTMLInputElement;
    expect(field.value).toBe("");

    await userEvent.type(field, "654321");
    await userEvent.click(screen.getByRole("button", { name: /turn on/i }));
    await waitFor(() => expect(confirmTotp).toHaveBeenNthCalledWith(2, "654321"));
    expect(await screen.findByRole("status")).toBeInTheDocument();
  });

  it("explains a refusal to replace an existing second factor rather than looping the operator", async () => {
    const enrollTotp = vi.fn().mockRejectedValue(problem(403, "FORBIDDEN"));
    renderSecurity(apiStub({ enrollTotp }));
    await userEvent.click(await screen.findByRole("button", { name: /set up authenticator app/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/already has an authenticator app/i);
    expect(screen.queryByTestId("totp-secret")).toBeNull();
  });

  it("says so when the deployment holds no key to protect the secret with", async () => {
    const enrollTotp = vi.fn().mockRejectedValue(problem(503, "UNAVAILABLE"));
    renderSecurity(apiStub({ enrollTotp }));
    await userEvent.click(await screen.findByRole("button", { name: /set up authenticator app/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/workspace key/i);
  });

  it("accepts only digits into the code field", async () => {
    renderSecurity(apiStub({ enrollTotp: vi.fn(async () => enrollment) }));
    await userEvent.click(await screen.findByRole("button", { name: /set up authenticator app/i }));
    const field = (await screen.findByLabelText(/code from your app/i)) as HTMLInputElement;
    await userEvent.type(field, "9a8b7c");
    expect(field.value).toBe("987");
  });
});
