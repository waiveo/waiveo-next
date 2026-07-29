import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router";
import { Button, FormField } from "@/components/kit";
import { ApiError, createApi, type WaiveoApi } from "@/api";

/**
 * The first-boot setup route ("/setup") — the console's half of the claim
 * (`security-model.md` SEC-120).
 *
 * The box mints a one-time `setup`-purpose grant on every unclaimed boot and
 * presents its code (printed, as a QR code, or on its startup screen). This page
 * is where that code is redeemed: it is exchanged, in one request, for the first
 * `owner` principal and the session that principal signs in with. Without it the
 * only way from "setup code in hand" to "signed in" was a hand-rolled POST.
 *
 * # Why this page is offered unconditionally
 *
 * Nothing tells the console whether the box is claimed, and that is a deliberate
 * choice rather than a gap this page works around. The alternative — an
 * unauthenticated endpoint reporting claim state, so a fresh box could land
 * straight here — would publish, to anyone who can reach the box, exactly the
 * fact SEC-120's threat model is about: that this box is inside its claim window
 * right now. SEC-121 re-opens that window on factory reset, so the answer is not
 * even a static installation fact; it is a live signal that a box has just been
 * reset. The code requirement still holds either way, so publishing it would not
 * BREAK anything — it would just hand a scanner on a shared network a target
 * list, in exchange for saving the operator one click.
 *
 * Being offered unconditionally is a property of the ROUTE, and it is the only
 * "works on any box" claim this file makes. It is not a claim that any box will
 * accept a code whenever this page is reached: see the reset case below.
 *
 * So the routing signal stays with the artifact SEC-120 already requires to
 * exist: the printed/on-screen presentation, which names this page. The console
 * itself says the same thing to everybody — the sign-in page always offers this
 * link, on a fresh box and on a claimed one alike — and reveals nothing by it.
 *
 * # What the refusals may and may not say
 *
 * A wrong code and an already-claimed box are INDISTINGUISHABLE here, because
 * they are indistinguishable on the wire: claiming shuts the window and the box
 * drops every `setup` grant it holds, so on a claimed box no code resolves at
 * all and every attempt draws the same 401 a wrong code draws on an unclaimed
 * one. The one message covering that 401 therefore names both possibilities and
 * asserts neither.
 *
 * A third box answers the same 401, and it is the one worth naming here because
 * neither of the other two remedies fixes it: a box that has been factory-reset
 * but not restarted. SEC-121 re-opens the claim window by destroying the last
 * `owner` binding, and destroying rows is all it does — the fresh `setup` grant
 * is minted by the next BOOT's EnsureClaimWindow (internal/app/auth/destroy.go,
 * bootstrap.go), not by the reset. In between, the box holds no live grant, so
 * every code is refused; worse, the reset does not remove the previous window's
 * `setup-code.txt`, so an operator can be holding a code that reads exactly like
 * a live one and is dead. The only way out is a restart, so the 401 message says
 * so — it is the same remedy the GRANT_EXPIRED branch names, for the same reason
 * (only the person at the box can perform it), and it is the one an operator
 * who reset the box five minutes ago would never guess.
 *
 * The 403s ARE reported distinctly, and that is not an inconsistency: a grant
 * that was expired, already redeemed, or of the wrong purpose can only be
 * REACHED by presenting its genuine code. Only a holder of the code the box
 * printed ever sees those answers, and telling that person which of the three
 * happened tells them nothing they could not have worked out from the piece of
 * paper in their hand — while withholding it would leave them with a dead form
 * and no idea whether to look for a fresher code or to go and sign in.
 *
 * On success the browser holds the session cookie (SEC-023) and its CSRF
 * companion (SEC-024), exactly as after a sign-in, and the console opens on the
 * overview.
 */

// Native form control styling, straight from the design tokens — the same idiom
// the sign-in route uses, and for the same reason: the widget kit exports no
// Input of its own and the import boundary forbids reaching for the vendored one
// directly, so a native control wearing the token classes is the sanctioned way
// to render a text field until the kit grows one.
const fieldClass =
  "flex h-9 w-full rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none transition-[box-shadow,border-color] focus-visible:ring-2 focus-visible:ring-ring aria-invalid:border-[color:var(--wv-err)] placeholder:text-muted-foreground";

/** Strip every whitespace character out of a pasted setup code.
 *
 * The code is a long random string with no whitespace anywhere in its alphabet,
 * and the ways an operator gets it into this field all tend to bring some along:
 * the box persists it to a file that ends in a newline, prints it on its own
 * indented log line, and a QR scan or a terminal copy picks up the surrounding
 * blanks. The server matches the code exactly, so that paste is refused as a
 * wrong code — a failure with a completely invisible cause. Removing whitespace
 * can only turn a paste that would certainly have failed into the code that was
 * actually issued; it can never turn one valid code into a different one.
 *
 * Case is deliberately NOT folded. Trimming makes no assumption about the code's
 * alphabet beyond "it contains no spaces"; case-folding would assume one, and a
 * client quietly rewriting a credential on a guess is how a claim starts failing
 * for reasons nobody can see the day the format changes.
 */
function normalizeCode(raw: string): string {
  return raw.replace(/\s+/g, "");
}

export interface SetupRouteProps {
  /** Injected api client (tests); defaults to the same-origin client. */
  api?: WaiveoApi;
}

export default function SetupRoute({ api }: SetupRouteProps = {}) {
  const navigate = useNavigate();
  const [code, setCode] = useState("");
  const [identifier, setIdentifier] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirmError, setConfirmError] = useState<string | null>(null);
  const [traceId, setTraceId] = useState<string | null>(null);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (busy) return;
    setError(null);
    setConfirmError(null);
    setTraceId(null);

    // Confirmed locally, before anything is sent. The setup code is one-time and
    // burns on success, so a mistyped password does not merely fail — it claims
    // the box, permanently, with a credential nobody knows. The second field is
    // the only thing standing between a typo and a box whose only remaining way
    // in is the root-only console binding.
    if (password !== confirm) {
      setConfirmError("The two passwords do not match.");
      return;
    }

    setBusy(true);
    const client = api ?? createApi();
    try {
      await client.auth.claim({ code: normalizeCode(code), identifier, password });
      navigate("/", { replace: true });
    } catch (err) {
      if (err instanceof ApiError) {
        setTraceId(err.traceId);
        setError(claimFailureMessage(err));
      } else {
        setError("Could not reach the box.");
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
          <p className="mt-1 text-sm text-muted-foreground">
            Set up this box: redeem its one-time setup code and choose the first owner
            credential.
          </p>
        </header>

        <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
          <FormField
            label="Setup code"
            required
            help="The one-time code this box presented when it was installed."
          >
            {(control) => (
              <input
                {...control}
                className={`${fieldClass} font-mono`}
                name="code"
                type="text"
                autoComplete="off"
                spellCheck={false}
                autoFocus
                value={code}
                onChange={(e) => setCode(e.target.value)}
              />
            )}
          </FormField>

          <FormField label="Identifier" required help="What you will sign in with from now on.">
            {(control) => (
              <input
                {...control}
                className={fieldClass}
                name="identifier"
                type="text"
                autoComplete="username"
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
                autoComplete="new-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            )}
          </FormField>

          <FormField label="Confirm password" required {...(confirmError ? { error: confirmError } : {})}>
            {(control) => (
              <input
                {...control}
                className={fieldClass}
                name="confirm_password"
                type="password"
                autoComplete="new-password"
                value={confirm}
                onChange={(e) => setConfirm(e.target.value)}
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
            {busy ? "Setting up…" : "Set up this box"}
          </Button>
        </form>

        <p className="mt-6 text-center text-sm text-muted-foreground">
          Already set up?{" "}
          <Link to="/login" className="text-[color:var(--wv-accent-text)] underline-offset-4 hover:underline">
            Sign in
          </Link>
        </p>
      </div>
    </main>
  );
}

/** The operator-facing line for a refused claim.
 *
 * Every branch answers the operator's only real question — what do I do next —
 * and the default is the non-disclosing one, so an error code this page has never
 * heard of degrades to "check the code, or go and sign in" rather than to
 * silence. See the file doc for why the 401 must stay ambiguous and why the 403s
 * need not.
 */
function claimFailureMessage(err: ApiError): string {
  switch (err.code) {
    case "GRANT_ALREADY_REDEEMED":
      // Reachable only with the genuine code. Two people racing to claim one box
      // with one code land here too (SEC-036 lets exactly one of them win), and
      // the remedy is the same for both readings: the box is claimed now, so
      // sign in with the credential that claimed it.
      return "This setup code has already been used, so this box is already set up. Sign in with the credential it was set up with.";
    case "GRANT_EXPIRED":
      // The box mints a fresh code on every unclaimed boot, so a restart is the
      // remedy — and it is a remedy only the person at the box can perform,
      // which is the right shape for this control.
      return "This setup code has expired. Restart the box to have it present a fresh one.";
    case "GRANT_PURPOSE_MISMATCH":
      return "That is not a setup code. Use the code this box presented when it was installed, not a pairing or reset code.";
    case "RATE_LIMITED":
      return "Too many setup attempts from this network. Wait for the limit to lift, then try again.";
    case "VALIDATION_FAILED":
      return "Fill in the setup code, an identifier and a password.";
    default:
      // UNAUTHENTICATED, and anything unrecognized. Deliberately covers all
      // THREE boxes that answer this way — a wrong code, an already-claimed box,
      // and a box whose claim window re-opened but which has not rebooted since
      // — without asserting which: the box itself cannot tell them apart, and a
      // page that guessed would be handing out an answer the server withheld on
      // purpose. Each clause is a remedy, so whichever is true the operator's
      // next move is on screen; the restart clause is the ONLY remedy for the
      // third, and it is the one an operator holding a code that worked
      // yesterday will not think of.
      return "That setup code was not accepted. Check the code this box presented. If the box was reset, restart it so it presents a fresh code. If it has already been set up, sign in instead.";
  }
}
