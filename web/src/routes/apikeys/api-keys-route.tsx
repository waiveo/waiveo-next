import { useCallback, useEffect, useMemo, useState } from "react";
import { Plus, Trash2 } from "lucide-react";
import {
  Button,
  ConfirmModal,
  DataTable,
  EmptyState,
  FormField,
  PageHeader,
  StatusBadge,
  toast,
  type ColumnDef,
} from "@/components/kit";
import { ApiError, createApi, type ApiKey, type WaiveoApi } from "@/api";
import { useOptionalSession } from "@/auth/session-gate";
import { can } from "@/auth/can";

/**
 * API keys — the credentials that can drive this platform (security-model
 * SEC-003a–e, SEC-020).
 *
 * # Why this page is TSX rather than a ui-schema/1 document
 *
 * The standing rule is that a console page is authored as a `.uis.json`
 * document unless there is a stated reason it cannot be, and the reason behind
 * that rule is a measured redo bill: 81% of the parity register's rows were
 * extension-owned in legacy, so a page hand-written here is thrown away when
 * its capability moves into a pack.
 *
 * This capability never moves. The ownership map puts identity and API keys in
 * the one bucket marked "CORE, permanently" — "the thing that grants a pack
 * trust cannot be granted trust by a pack" — and scores the redo waste for this
 * family at ZERO. The rule's own justification therefore does not reach it.
 *
 * The grammar would also have to grow for it: the reveal below needs a
 * copy-to-clipboard affordance on a value returned by an action, and the closed
 * 18-kind catalog has no copy widget. That gap is recorded rather than worked
 * around, but it is not what decided the format — the ownership map is.
 *
 * # The one interaction that matters
 *
 * SEC-003e returns a key's plaintext EXACTLY ONCE, from the mint, and makes it
 * unrecoverable afterwards by any operation. Everything about the reveal below
 * follows from that: it is the loudest thing on the page, it says plainly that
 * it will not be shown again, and it does not disappear on a re-render or a
 * background refresh. An operator who misses it has lost the key — the row will
 * still be listed, still be revocable, and never be usable, which is a
 * credential they must revoke and re-mint rather than recover.
 */
export default function ApiKeysRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [keys, setKeys] = useState<ApiKey[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [label, setLabel] = useState("");
  const [minting, setMinting] = useState(false);
  /** The plaintext of the key just minted. Held in page state and cleared only
   * by an explicit dismissal — never by a refresh, because a list reload must
   * not be able to take away the one showing of a secret. */
  const [revealed, setRevealed] = useState<{ label: string; key: string } | null>(null);
  const [confirming, setConfirming] = useState<ApiKey | null>(null);
  // SEC-003b: minting needs admin at the workspace root. Gating the control
  // here does not enforce that — the server does, on every request — it stops
  // an operator composing a label and learning their authority from a 403.
  // The mint path still handles its own refusal; both halves stay.
  const session = useOptionalSession();
  const mayMint = can(session?.session.role, "admin");

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const items = await client.auth.apiKeys();
      setKeys(items);
      setError(null);
    } catch (err: unknown) {
      setError(err instanceof ApiError ? (err.detail ?? err.title ?? err.code) : String(err));
    } finally {
      setLoading(false);
    }
  }, [client]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const mint = useCallback(async () => {
    setMinting(true);
    try {
      const created = await client.auth.mintApiKey({ label });
      setRevealed({ label: created.label, key: created.key });
      setLabel("");
      await refresh();
    } catch (err: unknown) {
      toast.error(err instanceof ApiError ? (err.detail ?? err.title ?? err.code) : String(err));
    } finally {
      setMinting(false);
    }
  }, [client, label, refresh]);

  const revoke = useCallback(
    async (key: ApiKey) => {
      setConfirming(null);
      try {
        await client.auth.revokeApiKey(key.id);
        toast.success(`${key.label} revoked. It is refused from the next request onward.`);
        await refresh();
      } catch (err: unknown) {
        toast.error(err instanceof ApiError ? (err.detail ?? err.title ?? err.code) : String(err));
      }
    },
    [client, refresh],
  );

  const columns: ColumnDef<ApiKey>[] = [
    { accessorKey: "label", header: "Label" },
    {
      id: "created",
      header: "Created",
      cell: ({ row }) => new Date(row.original.created_at).toLocaleString(),
    },
    {
      id: "last_used",
      header: "Last used",
      // Never used is a FACT worth stating, not a blank: a key nobody has ever
      // presented is the one an operator can revoke without wondering what
      // breaks.
      cell: ({ row }) =>
        row.original.last_used_at
          ? new Date(row.original.last_used_at).toLocaleString()
          : "never used",
    },
    {
      id: "expires",
      header: "Expires",
      cell: ({ row }) =>
        row.original.expires_at ? (
          new Date(row.original.expires_at).toLocaleString()
        ) : (
          <StatusBadge status="off">no expiry</StatusBadge>
        ),
    },
    {
      id: "revoke",
      header: "",
      cell: ({ row }) => (
        <Button
          variant="ghost"
          icon={Trash2}
          onClick={() => setConfirming(row.original)}
          aria-label={`Revoke ${row.original.label}`}
        >
          Revoke
        </Button>
      ),
    },
  ];

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        variant="hero"
        title="API keys"
        description="The credentials that can drive this platform from a script, an integration or a device. A key acts as you — it carries exactly the roles you already hold."
      />

      {revealed ? (
        <section
          aria-labelledby="revealed-heading"
          className="flex flex-col gap-2 rounded-input border border-[color:var(--wv-warn)] p-4"
        >
          <h2 id="revealed-heading" className="text-lg font-semibold">
            Copy this key now
          </h2>
          <p className="text-sm">
            This is the only time <strong>{revealed.label}</strong> will be shown. It cannot be
            recovered afterwards — if you lose it, revoke the key and mint another.
          </p>
          {/* Selectable, wrapping, monospace. There is no copy button because
              the widget kit has none; recorded as a gap rather than reached
              around with a bare DOM call. */}
          <code className="rounded-input bg-[color:var(--wv-surface-2)] p-3 font-mono text-sm break-all select-all">
            {revealed.key}
          </code>
          <div>
            <Button variant="secondary" onClick={() => setRevealed(null)}>
              I have copied it
            </Button>
          </div>
        </section>
      ) : null}

      <section aria-labelledby="mint-heading" className="flex flex-col gap-3">
        <h2 id="mint-heading" className="text-lg font-semibold">
          Mint a key
        </h2>
        {session && !mayMint ? (
          // Said, not silently omitted. A missing control reads as a broken
          // page; naming the authority it needs is the difference between "this
          // console is missing something" and "this account cannot do that".
          <p className="text-sm text-muted-foreground">
            Minting an API key needs the <strong>admin</strong> role. You are signed in as{" "}
            <strong>{session.session.role}</strong>, so this box will refuse it.
          </p>
        ) : null}
        <div className="flex flex-wrap items-end gap-3">
          <FormField label="Label" help="What this key is for, so the list below stays readable.">
            {(control) => (
              <input
                {...control}
                type="text"
                autoComplete="off"
                placeholder="ci-runner"
                className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
                value={label}
                onChange={(e) => setLabel(e.target.value)}
              />
            )}
          </FormField>
          <Button
            icon={Plus}
            disabled={label.trim() === "" || minting || (session !== null && !mayMint)}
            onClick={() => void mint()}
            aria-label="Mint an API key"
          >
            Mint key
          </Button>
        </div>
      </section>

      <section aria-labelledby="keys-heading" className="flex flex-col gap-3">
        <h2 id="keys-heading" className="text-lg font-semibold">
          Your keys
        </h2>
        {error ? (
          <p className="text-sm text-[color:var(--wv-err)]" role="alert">
            Your API keys could not be read: {error}
          </p>
        ) : !loading && keys.length === 0 ? (
          <EmptyState
            title="No API keys"
            description="Mint one above to drive this box from a script, an integration or a device."
          />
        ) : (
          <DataTable label="API keys" columns={columns} data={keys} loading={loading} />
        )}
      </section>

      <ConfirmModal
        open={confirming !== null}
        onOpenChange={(open) => !open && setConfirming(null)}
        title={confirming ? `Revoke ${confirming.label}?` : "Revoke"}
        // Names the consequence, not the action: anything still presenting this
        // key stops working at once, and there is no undo — the plaintext is
        // gone, so it cannot be re-issued, only replaced.
        description="Anything still using this key stops working immediately. It cannot be restored — mint a new key instead."
        confirmLabel="Revoke"
        onConfirm={() => confirming && void revoke(confirming)}
      />
    </div>
  );
}
