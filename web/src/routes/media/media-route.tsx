import { useMemo, useState } from "react";
import { Check, Copy, RefreshCw } from "lucide-react";
import { Button, PageHeader, Toaster, toast } from "@/components/kit";
import { createApi, type WaiveoApi } from "@/api";
import { MediaGrid, formatBytes, shortRef, useContentLibrary } from "./media-library";

/**
 * The Media route — browse everything the content origin is serving.
 *
 * The Content page uploads and shows what THIS SESSION uploaded; nothing showed
 * an operator what was already on the box, so a ref uploaded yesterday could
 * only be recovered by uploading the same bytes again. This is the read half:
 * the origin's own listing (GET /api/v1/content), with each asset's ref ready to
 * copy onto a playlist item and the same tiles the Studio's image picker offers.
 *
 * A row's "name" is its digest, because a content-addressed origin has no other
 * identity to show — no filename survives the upload. The list is digest-ordered
 * server-side, which is stable but arbitrary, so the newest-first sort is applied
 * here: an operator looking for what they just uploaded is the common case.
 */
export default function MediaRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const { assets, error, reload } = useContentLibrary(client);
  const [copied, setCopied] = useState<string | null>(null);

  const newestFirst = useMemo(
    () => (assets === null ? null : [...assets].sort((a, b) => b.stored_at - a.stored_at)),
    [assets],
  );

  const copyRef = async (assetRef: string) => {
    try {
      await navigator.clipboard.writeText(assetRef);
      setCopied(assetRef);
      window.setTimeout(() => setCopied((c) => (c === assetRef ? null : c)), 1500);
    } catch {
      toast.error("Couldn't copy to the clipboard.");
    }
  };

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Media"
          description="Every asset stored on this box, addressed by its content hash. Place one on a slide in the Studio, or copy its reference into a playlist."
          actions={
            <Button variant="secondary" icon={RefreshCw} onClick={() => void reload()}>
              Refresh
            </Button>
          }
        />

        <MediaGrid assets={newestFirst} error={error} />

        {newestFirst && newestFirst.length > 0 ? (
          <section className="flex flex-col gap-2">
            <h2 className="text-lg font-semibold">References</h2>
            <p className="text-sm text-muted-foreground">
              The `sha256:` reference a playlist item or an image layer names.
            </p>
            <ul className="flex flex-col divide-y divide-[color:var(--wv-row-border)] rounded-card border border-border bg-card">
              {newestFirst.map((asset) => (
                <li key={asset.asset_ref} className="flex min-w-0 flex-wrap items-center gap-3 p-3">
                  <code className="min-w-0 flex-1 truncate font-mono text-[12px]" title={asset.asset_ref}>
                    {shortRef(asset.asset_ref)}
                  </code>
                  <span className="shrink-0 text-[12px] text-muted-foreground">
                    {formatBytes(asset.size_bytes)}
                  </span>
                  <Button
                    variant="secondary"
                    size="sm"
                    icon={copied === asset.asset_ref ? Check : Copy}
                    className="wv-touch shrink-0"
                    aria-label={`Copy content reference ${asset.asset_ref}`}
                    onClick={() => void copyRef(asset.asset_ref)}
                  >
                    {copied === asset.asset_ref ? "Copied" : "Copy"}
                  </Button>
                </li>
              ))}
            </ul>
          </section>
        ) : null}
      </div>
      <Toaster />
    </div>
  );
}
