import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import SetupRoute from "./setup-route";
import { ApiError, type SessionSummary, type WaiveoApi } from "@/api";

/**
 * Every test here PRESSES the button. The console has shipped a control that
 * rendered perfectly and did nothing, so a test that asserts this page paints
 * four fields proves nothing about whether an operator can claim a box with it:
 * each case fills the real inputs, submits the real form, and asserts on what
 * reached the api client or on what the operator is told afterwards.
 */

/** Renders the path the router is currently on, so a test can assert that a
 * successful claim actually opened the console. */
function LocationEcho() {
  const loc = useLocation();
  return <span data-testid="where">{loc.pathname}</span>;
}

const SESSION: SessionSummary = {
  principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
  kind: "user",
  role: "owner",
  aal: "standard",
  session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
  csrf_token: "csrf-1",
};

function apiStub(auth: Partial<WaiveoApi["auth"]>): WaiveoApi {
  return {
    auth: { login: vi.fn(), logout: vi.fn(), session: vi.fn(), claim: vi.fn(), ...auth },
  } as unknown as WaiveoApi;
}

function renderSetup(api: WaiveoApi) {
  return render(
    <MemoryRouter initialEntries={["/setup"]}>
      <Routes>
        <Route path="/setup" element={<SetupRoute api={api} />} />
        <Route path="*" element={<LocationEcho />} />
      </Routes>
    </MemoryRouter>,
  );
}

/** Fill in the whole form and press "Set up this box" — the real interaction. */
async function claim({
  code = "0a1b2c3d",
  identifier = "owner",
  password = "first-owner-password",
  confirm = password,
}: { code?: string; identifier?: string; password?: string; confirm?: string } = {}) {
  await userEvent.type(screen.getByLabelText(/setup code/i), code);
  await userEvent.type(screen.getByLabelText(/^identifier/i), identifier);
  await userEvent.type(screen.getByLabelText(/^password/i), password);
  await userEvent.type(screen.getByLabelText(/confirm password/i), confirm);
  await userEvent.click(screen.getByRole("button", { name: /set up this box/i }));
}

function problem(status: number, code: string): ApiError {
  return new ApiError(status, { type: "about:blank", title: "x", status, code, trace_id: "t" } as never, "t");
}

describe("SetupRoute", () => {
  it("redeems the setup code from the form BODY and opens the console (SEC-120)", async () => {
    const claimFn = vi.fn(async () => SESSION);
    renderSetup(apiStub({ claim: claimFn }));

    await claim({ code: "0a1b2c3d", identifier: "owner", password: "first-owner-password" });

    await waitFor(() =>
      expect(claimFn).toHaveBeenCalledWith({
        code: "0a1b2c3d",
        identifier: "owner",
        password: "first-owner-password",
      }),
    );
    // The claim minted the session, so the console opens — the whole point of
    // the page is that this is the LAST step, not a handoff to another form.
    expect(await screen.findByTestId("where")).toHaveTextContent("/");
  });

  it("is ready to claim a second time, because the PAGE reads no state first (SEC-121)", async () => {
    const first = vi.fn(async () => SESSION);
    const session = vi.fn();
    const { unmount } = renderSetup(apiStub({ claim: first, session }));
    await claim();
    await waitFor(() => expect(first).toHaveBeenCalledTimes(1));
    unmount();

    // A factory reset destroys the workspace and re-opens the claim window. This
    // is a claim about the PAGE only: it must be usable again with no ceremony,
    // which it is precisely because it never asked anything about the box before
    // letting the operator submit. A precondition read is what would have made
    // "already used once" a state something could get stuck on, so assert there
    // is none. Whether the BOX accepts the second claim is a separate question
    // with a separate answer — it does not until it restarts
    // (TestFactoryResetReopensClaimOnlyAtTheNextBoot), which is why the refusal
    // test below insists the 401 message names a restart.
    const second = vi.fn(async () => SESSION);
    renderSetup(apiStub({ claim: second, session }));
    await claim({ code: "9f8e7d6c" });

    await waitFor(() => expect(second).toHaveBeenCalledWith(expect.objectContaining({ code: "9f8e7d6c" })));
    expect(session).not.toHaveBeenCalled();
  });

  it("strips the whitespace a pasted code carries, instead of refusing it as wrong", async () => {
    const claimFn = vi.fn(async () => SESSION);
    renderSetup(apiStub({ claim: claimFn }));

    // The box writes the code to a file ending in a newline and prints it on an
    // indented log line, so a paste routinely arrives padded. The server matches
    // exactly, so without this the operator gets "that code was not accepted"
    // for a code that is character-for-character correct.
    await claim({ code: "  0a1b2c3d  " });

    await waitFor(() => expect(claimFn).toHaveBeenCalledWith(expect.objectContaining({ code: "0a1b2c3d" })));
  });

  it("strips whitespace from INSIDE the code too, not merely from its ends", async () => {
    const claimFn = vi.fn(async () => SESSION);
    renderSetup(apiStub({ claim: claimFn }));

    // The interior is where the realistic damage is. The code is long enough to
    // wrap, and every browser turns the newline in a wrapped copy into an
    // interior space (or drops it) when it lands in a single-line input — so a
    // paste off a wrapped terminal line or a printout arrives split. Trimming
    // the ends alone leaves that code refused, with the same invisible cause
    // this normalization exists to remove.
    await claim({ code: " 0a1b 2c3d " });

    await waitFor(() => expect(claimFn).toHaveBeenCalledWith(expect.objectContaining({ code: "0a1b2c3d" })));
  });

  it("passes the code's case through untouched, since the box matches it exactly", async () => {
    const claimFn = vi.fn(async () => SESSION);
    renderSetup(apiStub({ claim: claimFn }));

    await claim({ code: "  0A1B2C3D  " });

    // Case is deliberately NOT folded. Stripping whitespace assumes only that
    // the alphabet contains no spaces; folding case assumes what the alphabet
    // IS, and today's codes being lowercase hex is a fact about today's mint,
    // not a promise. A client that quietly rewrites a credential on that guess
    // is how a claim starts failing for reasons nobody can see the day the
    // format changes — so the guess is pinned out rather than left to the doc.
    await waitFor(() => expect(claimFn).toHaveBeenCalledWith(expect.objectContaining({ code: "0A1B2C3D" })));
  });

  it("catches mismatched passwords BEFORE anything is sent, since the code burns on success", async () => {
    const claimFn = vi.fn(async () => SESSION);
    renderSetup(apiStub({ claim: claimFn }));

    await claim({ password: "first-owner-password", confirm: "first-owner-passwrod" });

    expect(await screen.findByRole("alert")).toHaveTextContent(/do not match/i);
    // Nothing was sent: a typo here would have claimed the box permanently with
    // a credential nobody knows, and the one-time code cannot be re-spent.
    expect(claimFn).not.toHaveBeenCalled();
    expect(screen.queryByTestId("where")).not.toBeInTheDocument();
  });

  it("reports a refused code so it reads the same for a wrong code and an already-claimed box", async () => {
    const claimFn = vi.fn(async () => {
      throw problem(401, "UNAUTHENTICATED");
    });
    renderSetup(apiStub({ claim: claimFn }));

    await claim({ code: "not-the-code" });

    // 401 is the box's answer to THREE situations: a wrong code; any code at all
    // once the box is claimed and has dropped its setup grants; and any code at
    // all on a box that was factory-reset and has not rebooted, which holds no
    // live grant yet (TestFactoryResetReopensClaimOnlyAtTheNextBoot). The page
    // must not pick one — it names a remedy for each, so whichever is true the
    // operator's next move is on screen.
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/check the code/i);
    expect(alert).toHaveTextContent(/sign in/i);
    // The restart is the ONLY remedy for the reset case: "check the code" is
    // useless (the code on disk is the dead one) and "sign in" is useless (the
    // reset destroyed every credential). Leaving it out walks an operator who
    // has just reset a box into a dead end with two wrong exits.
    expect(alert).toHaveTextContent(/restart/i);
    expect(screen.getByRole("link", { name: /sign in/i })).toHaveAttribute("href", "/login");
  });

  it("tells the loser of a concurrent claim that the box is claimed and to sign in (SEC-036)", async () => {
    const claimFn = vi.fn(async () => {
      throw problem(403, "GRANT_ALREADY_REDEEMED");
    });
    renderSetup(apiStub({ claim: claimFn }));

    await claim();

    // Reachable only by presenting the GENUINE code, so it may say more than the
    // 401 does: of two claims racing with one code exactly one wins, and the
    // other needs to know that retrying is pointless and signing in is not.
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/already been used/i);
    expect(alert).toHaveTextContent(/sign in/i);
  });

  it("names the remedy for an expired code — a fresh one comes from a restart", async () => {
    renderSetup(
      apiStub({
        claim: vi.fn(async () => {
          throw problem(403, "GRANT_EXPIRED");
        }),
      }),
    );

    await claim();
    expect(await screen.findByRole("alert")).toHaveTextContent(/expired.*restart the box/i);
  });

  it("says a pairing or reset code is the wrong kind of code, not a wrong code", async () => {
    renderSetup(
      apiStub({
        claim: vi.fn(async () => {
          throw problem(403, "GRANT_PURPOSE_MISMATCH");
        }),
      }),
    );

    await claim();
    expect(await screen.findByRole("alert")).toHaveTextContent(/not a setup code/i);
  });

  it("explains the attempt budget rather than looking broken (SEC-033)", async () => {
    renderSetup(
      apiStub({
        claim: vi.fn(async () => {
          throw problem(429, "RATE_LIMITED");
        }),
      }),
    );

    await claim();
    expect(await screen.findByRole("alert")).toHaveTextContent(/too many setup attempts/i);
  });

  it("quotes the trace id, so a failure at a box can be correlated in its log", async () => {
    renderSetup(
      apiStub({
        claim: vi.fn(async () => {
          throw problem(401, "UNAUTHENTICATED");
        }),
      }),
    );

    await claim();
    expect(await screen.findByRole("alert")).toHaveTextContent("(t)");
  });

  it("stays on the page after a failure, so the operator can correct the code and retry", async () => {
    const claimFn = vi
      .fn<() => Promise<SessionSummary>>()
      .mockRejectedValueOnce(problem(401, "UNAUTHENTICATED"))
      .mockResolvedValueOnce(SESSION);
    renderSetup(apiStub({ claim: claimFn }));

    await claim({ code: "wrong" });
    await screen.findByRole("alert");
    expect(screen.queryByTestId("where")).not.toBeInTheDocument();

    // The retry is a real one: correct the code in the SAME form and press again.
    const codeField = screen.getByLabelText(/setup code/i);
    await userEvent.clear(codeField);
    await userEvent.type(codeField, "0a1b2c3d");
    await userEvent.click(screen.getByRole("button", { name: /set up this box/i }));

    await waitFor(() => expect(claimFn).toHaveBeenLastCalledWith(expect.objectContaining({ code: "0a1b2c3d" })));
    expect(await screen.findByTestId("where")).toHaveTextContent("/");
  });

  it("reports an unreachable box instead of failing silently", async () => {
    renderSetup(
      apiStub({
        claim: vi.fn(async () => {
          throw new TypeError("network down");
        }),
      }),
    );

    await claim();
    expect(await screen.findByRole("alert")).toHaveTextContent(/could not reach the box/i);
  });

  it("labels every field, so the form is operable by assistive tech", () => {
    renderSetup(apiStub({}));
    expect(screen.getByLabelText(/setup code/i)).toHaveAttribute("type", "text");
    expect(screen.getByLabelText(/^identifier/i)).toHaveAttribute("autocomplete", "username");
    expect(screen.getByLabelText(/^password/i)).toHaveAttribute("autocomplete", "new-password");
    expect(screen.getByLabelText(/confirm password/i)).toHaveAttribute("type", "password");
  });
});
