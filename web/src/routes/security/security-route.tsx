import { useState, type FormEvent } from "react";
import { Button, FormField, PageHeader } from "@/components/kit";
import { useSession } from "@/auth/session-gate";
import { ApiError, createApi, type TotpEnrollment, type WaiveoApi } from "@/api";

/**
 * The account-security route ("/security") — where an operator adds the second
 * factor to their OWN account (`security-model.md` SEC-004).
 *
 * It enrolls for the signed-in principal and no other, and there is no field
 * here that could name a different one. That is the contract's own boundary
 * rather than a simplification: an operator who can see another principal's TOTP
 * secret holds that principal's second factor, and clearing or re-enrolling
 * someone else's TOTP is explicitly outside plain `admin` authority (SEC-052).
 *
 * The flow is two steps because arming on the first would be a lie: handing out
 * a secret proves nothing about whether it reached an authenticator, and a
 * credential armed by an enrollment the operator abandoned halfway locks them
 * out of their own account. The code they type back is the only evidence
 * available, so it is the thing that arms it.
 *
 * The secret is shown as a typeable key rather than only as a QR image. A QR
 * code needs a second device with a camera pointed at this screen; the key works
 * from a password manager, a desktop authenticator, or a phone that cannot see
 * the monitor. The `otpauth://` URI is offered alongside it for the apps that
 * take one directly.
 *
 * Arming signs out every OTHER session this principal holds — the server does
 * that, and this page says so before the operator commits, because "you will be
 * signed out on your other devices" is not something to discover afterwards.
 */

// Same native-control styling the sign-in route uses; the widget kit exports no
// Input of its own yet.
const fieldClass =
  "flex h-9 w-full rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none transition-[box-shadow,border-color] focus-visible:ring-2 focus-visible:ring-ring aria-invalid:border-[color:var(--wv-err)] placeholder:text-muted-foreground";

export interface SecurityRouteProps {
  /** Injected api client (tests); defaults to the same-origin client. */
  api?: WaiveoApi;
}

type Stage =
  | { step: "idle" }
  | { step: "pending"; enrollment: TotpEnrollment }
  | { step: "armed"; revokedSessions: number };

export default function SecurityRoute({ api }: SecurityRouteProps = {}) {
  const { session } = useSession();
  const [stage, setStage] = useState<Stage>({ step: "idle" });
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [traceId, setTraceId] = useState<string | null>(null);

  const client = api ?? createApi();

  function reportError(err: unknown, alreadyEnrolled: string) {
    if (err instanceof ApiError) {
      setTraceId(err.traceId);
      setError(
        err.status === 403
          ? alreadyEnrolled
          : err.status === 503
            ? "This deployment cannot protect a second-factor secret yet; its workspace key has not been established."
            : err.status === 404
              ? "That enrollment is no longer in progress. Start a new one."
              : err.detail ?? "That did not work.",
      );
    } else {
      setError("Could not reach the server.");
    }
  }

  async function onStart() {
    if (busy) return;
    setBusy(true);
    setError(null);
    setTraceId(null);
    try {
      const enrollment = await client.auth.enrollTotp();
      setStage({ step: "pending", enrollment });
      setCode("");
    } catch (err) {
      reportError(
        err,
        "This account already has an authenticator app enrolled. Replacing it is a recovery operation, not a self-service one.",
      );
    } finally {
      setBusy(false);
    }
  }

  async function onConfirm(e: FormEvent) {
    e.preventDefault();
    if (busy || stage.step !== "pending") return;
    setBusy(true);
    setError(null);
    setTraceId(null);
    try {
      const armed = await client.auth.confirmTotp(code);
      setStage({ step: "armed", revokedSessions: armed.revoked_sessions });
    } catch (err) {
      setCode("");
      reportError(err, "This account already has an authenticator app enrolled.");
      if (err instanceof ApiError && err.status === 401) {
        setError(
          "That code does not match this enrollment. Check the clock on the device generating it, then try the next code.",
        );
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="mx-auto w-full max-w-2xl px-4 py-8">
      <PageHeader
        title="Security"
        description="Add a second factor to this account. Signed in as this principal only — it cannot be enrolled on anyone else's behalf."
      />

      <section className="mt-6 rounded-card border border-border p-5">
        <h2 className="text-base font-semibold text-foreground">Authenticator app</h2>
        <p className="mt-1 text-sm text-muted-foreground">
          A time-based code from an authenticator app, required alongside your password every time
          you sign in.
        </p>
        <p className="mt-1 text-xs text-muted-foreground">Principal {session.principal_id}</p>

        {stage.step === "idle" ? (
          <div className="mt-4">
            <p className="text-sm text-muted-foreground">
              Turning this on signs you out everywhere else. This browser stays signed in.
            </p>
            <Button className="mt-3" disabled={busy} onClick={() => void onStart()}>
              {busy ? "Starting…" : "Set up authenticator app"}
            </Button>
          </div>
        ) : null}

        {stage.step === "pending" ? (
          <form onSubmit={onConfirm} className="mt-4 flex flex-col gap-4" noValidate>
            <div>
              <p className="text-sm text-foreground">
                Add this key to your authenticator app, then enter the code it shows.
              </p>
              <code
                data-testid="totp-secret"
                className="mt-2 block break-all rounded-input border border-border px-3 py-2 font-mono text-sm text-foreground"
              >
                {stage.enrollment.secret}
              </code>
              <p className="mt-2 break-all text-xs text-muted-foreground" data-testid="totp-uri">
                {stage.enrollment.otpauth_uri}
              </p>
            </div>

            <FormField label="Code from your app" required>
              {(control) => (
                <input
                  {...control}
                  className={fieldClass}
                  name="code"
                  type="text"
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  maxLength={6}
                  value={code}
                  onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
                />
              )}
            </FormField>

            <Button type="submit" disabled={busy}>
              {busy ? "Verifying…" : "Turn on"}
            </Button>
          </form>
        ) : null}

        {stage.step === "armed" ? (
          <p role="status" className="mt-4 text-sm text-foreground">
            Your authenticator app is now required to sign in.
            {stage.revokedSessions > 0
              ? ` ${stage.revokedSessions} other session${stage.revokedSessions === 1 ? "" : "s"} signed out.`
              : ""}
          </p>
        ) : null}

        {error ? (
          <p role="alert" className="mt-4 text-sm text-destructive">
            {error}
            {traceId ? <span className="ml-1 text-muted-foreground">({traceId})</span> : null}
          </p>
        ) : null}
      </section>
    </main>
  );
}
