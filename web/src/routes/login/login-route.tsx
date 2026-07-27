import { useState, type FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { Button, FormField } from "@/components/kit";
import { ApiError, createApi, type WaiveoApi } from "@/api";

/**
 * The sign-in route ("/login") — the console's one credential-exchange page.
 *
 * It is deliberately outside the AppShell: the shell's nav, live event stream
 * and pack pages all assume an authenticated session, so rendering them around
 * a sign-in form would mean mounting components that immediately 401 behind the
 * form the operator is trying to use.
 *
 * Two behaviours here are security requirements rather than UX choices:
 *
 *   - The failure message never distinguishes "no such account" from "wrong
 *     password". The server already refuses both identically so the endpoint
 *     cannot be used to enumerate accounts; echoing a more specific message here
 *     would hand back exactly what the server withheld.
 *
 *   - A lockout (`CREDENTIAL_LOCKED`, security-model/1 SEC-090) IS called out
 *     distinctly, because it is the one failure where the operator's next action
 *     differs: retrying immediately cannot work, and a person who knows their
 *     password is right otherwise has no way to tell why it stopped working.
 *
 * On success the browser holds a host-only session cookie (SEC-023) and its
 * companion CSRF cookie (SEC-024); the client picks both up automatically, so
 * there is no token for this page to store or thread anywhere.
 */

// Native form control styling, straight from the design tokens — the same idiom
// the design-system route uses. The widget kit exports no Input of its own and
// the import boundary forbids reaching for the vendored one directly, so a
// native control wearing the token classes is the sanctioned way to render a
// text field until the kit grows one.
const fieldClass =
  "flex h-9 w-full rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none transition-[box-shadow,border-color] focus-visible:ring-2 focus-visible:ring-ring aria-invalid:border-[color:var(--wv-err)] placeholder:text-muted-foreground";
export interface LoginRouteProps {
  /** Injected api client (tests); defaults to the same-origin client. */
  api?: WaiveoApi;
}

export default function LoginRoute({ api }: LoginRouteProps = {}) {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const [identifier, setIdentifier] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [traceId, setTraceId] = useState<string | null>(null);

  // Where to land after signing in: whatever page bounced us here, or the
  // overview. Only a same-origin PATH is honoured — an absolute URL in `next`
  // would turn this into an open redirect a phishing link could aim anywhere.
  const requested = params.get("next") ?? "/";
  const next = requested.startsWith("/") && !requested.startsWith("//") ? requested : "/";

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setError(null);
    setTraceId(null);
    const client = api ?? createApi();
    try {
      await client.auth.login({ identifier, password });
      navigate(next, { replace: true });
    } catch (err) {
      if (err instanceof ApiError) {
        setTraceId(err.traceId);
        setError(
          err.code === "CREDENTIAL_LOCKED"
            ? "Too many failed attempts from this network. Wait for the lockout to lift, then try again."
            : err.code === "VALIDATION_FAILED"
              ? "Enter both an identifier and a password."
              : "That identifier and password did not match.",
        );
      } else {
        setError("Could not reach the server.");
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="flex min-h-dvh items-center justify-center bg-background px-4 py-12">
      <div className="w-full max-w-sm">
        <header className="mb-8 text-center">
          <h1 className="text-2xl font-semibold tracking-tight text-foreground">Waiveo</h1>
          <p className="mt-1 text-sm text-muted-foreground">Sign in to the console.</p>
        </header>

        <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
          <FormField label="Identifier" required>
            {(control) => (
              <input
                {...control}
                className={fieldClass}
                name="identifier"
                type="text"
                autoComplete="username"
                autoFocus
                value={identifier}
                onChange={(e) => setIdentifier(e.target.value)}
              />
            )}
          </FormField>

          <FormField label="Password" required>
            {(control) => (
              <input
                {...control}
                className={fieldClass}
                name="password"
                type="password"
                autoComplete="current-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            )}
          </FormField>

          {error ? (
            <p role="alert" className="text-sm text-destructive">
              {error}
              {traceId ? <span className="ml-1 text-muted-foreground">({traceId})</span> : null}
            </p>
          ) : null}

          <Button type="submit" disabled={busy} className="w-full">
            {busy ? "Signing in…" : "Sign in"}
          </Button>
        </form>
      </div>
    </main>
  );
}
