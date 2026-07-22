import { useCallback, useMemo, useRef, useState, type DragEvent } from "react";
import { Copy, Upload, Check } from "lucide-react";
import { PageHeader, EmptyState, KitIcon, Button, Toaster, toast } from "@/components/kit";
import { ApiError, createApi, type ContentUploadResult, type WaiveoApi } from "@/api";
import { cn } from "@/lib/utils";

/**
 * The Content route — upload media, then see it in a library with its
 * content-addressed reference ready to drop into a playlist.
 *
 * // grammar-gap: file upload widget. ui-schema/1 has no input widget that takes
 * bytes from a drag-drop / file-picker gesture (its input catalog is
 * text/number/duration/toggle/select/entity/time — UIS-070). Byte upload is one
 * of the three sanctioned first-party gaps (alongside the live stream and the raw
 * code editor); the drop zone + picker below is therefore hand-built React over
 * the kit. Everything downstream — the api/1 POST, the Problem handling, the
 * library grid — stays inside the shared conventions. Future path: a `file-input`
 * widget would be a ui-schema/1 minor once the upload transfer shape is specced.
 *
 * api/1 content is content-addressed and write-only: POST /content returns
 * `{asset_ref, url}` (the sha256 ref a playlist item references, and the
 * same-origin url a screen fetches the bytes from). There is no list endpoint, so
 * the library is the session's own uploads — each accumulates as it lands. A
 * failed upload surfaces its Problem inline (and as a toast); a 422 quotes the
 * server's field message.
 */

interface LibraryAsset extends ContentUploadResult {
  /** The original filename, for a human-legible card (not part of the ref). */
  name: string;
  /** Deck-gradient index (1–4) for the placeholder poster thumb. */
  deck: number;
}

export default function ContentRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const inputRef = useRef<HTMLInputElement>(null);
  const [assets, setAssets] = useState<LibraryAsset[]>([]);
  const [dragActive, setDragActive] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState<string | null>(null);

  const uploadFiles = useCallback(
    async (files: File[]) => {
      if (files.length === 0) return;
      setBusy(true);
      setError(null);
      let firstError: string | null = null;
      const landed: LibraryAsset[] = [];
      for (const file of files) {
        try {
          const result = await client.content.upload(file, file.type || "application/octet-stream");
          landed.push({ ...result, name: file.name, deck: 0 });
        } catch (err) {
          if (firstError === null) {
            if (err instanceof ApiError) {
              firstError = Object.values(err.fieldErrors)[0] ?? err.detail ?? err.code;
            } else {
              firstError = "The service is unreachable.";
            }
          }
        }
      }
      if (landed.length > 0) {
        setAssets((prev) => {
          const next = [...landed.map((a, i) => ({ ...a, deck: ((prev.length + i) % 4) + 1 })), ...prev];
          return next;
        });
        toast.success(landed.length === 1 ? `Uploaded ${landed[0].name}` : `Uploaded ${landed.length} files`);
      }
      if (firstError) {
        setError(firstError);
        toast.error(`Upload failed: ${firstError}`);
      }
      setBusy(false);
    },
    [client],
  );

  const openPicker = useCallback(() => inputRef.current?.click(), []);

  const onDrop = useCallback(
    (e: DragEvent<HTMLDivElement>) => {
      e.preventDefault();
      setDragActive(false);
      const files = Array.from(e.dataTransfer?.files ?? []);
      void uploadFiles(files);
    },
    [uploadFiles],
  );

  const copyRef = useCallback(async (assetRef: string) => {
    try {
      await navigator.clipboard.writeText(assetRef);
      setCopied(assetRef);
      window.setTimeout(() => setCopied((c) => (c === assetRef ? null : c)), 1500);
    } catch {
      toast.error("Couldn't copy to the clipboard.");
    }
  }, []);

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Content"
          description="Upload the media your screens play. Each file is stored by its content hash — copy the reference into a playlist to schedule it."
        />

        {/* The drop zone + picker — the sanctioned first-party upload affordance. */}
        <div
          role="button"
          tabIndex={0}
          aria-label="Upload content — drop files here or browse"
          onClick={openPicker}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === " ") {
              e.preventDefault();
              openPicker();
            }
          }}
          onDragOver={(e) => {
            e.preventDefault();
            setDragActive(true);
          }}
          onDragLeave={() => setDragActive(false)}
          onDrop={onDrop}
          data-slot="content-dropzone"
          data-drag-active={dragActive}
          className={cn(
            "wv-touch flex min-h-[9rem] cursor-pointer flex-col items-center justify-center gap-2 rounded-card border-2 border-dashed border-border bg-card p-6 text-center outline-none transition-colors",
            "hover:border-[color:var(--wv-accent-text)] focus-visible:ring-2 focus-visible:ring-ring",
            dragActive && "border-[color:var(--wv-accent-text)] bg-accent",
          )}
        >
          <div className="flex size-11 items-center justify-center rounded-panel bg-[color:var(--wv-surface-2)]">
            <KitIcon icon={Upload} decorative className="size-5 text-muted-foreground" />
          </div>
          <p className="font-display text-[15px] font-semibold">
            {busy ? "Uploading…" : "Drop files to upload"}
          </p>
          <p className="text-[13px] text-muted-foreground">or click to browse — images, video, and more</p>
          <input
            ref={inputRef}
            type="file"
            multiple
            aria-label="Choose files"
            className="sr-only"
            onChange={(e) => {
              const files = Array.from(e.target.files ?? []);
              void uploadFiles(files);
              e.target.value = "";
            }}
          />
        </div>

        {error ? (
          <p role="alert" className="text-sm text-[color:var(--wv-err)]">
            {error}
          </p>
        ) : null}

        {/* The library grid — the session's uploads with their content-address refs. */}
        {assets.length === 0 ? (
          <EmptyState
            icon={Upload}
            title="Nothing uploaded yet"
            description="Drop a file above to add it to the library. Its content reference appears here, ready to copy into a playlist."
          />
        ) : (
          <ul className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {assets.map((asset) => (
              <li
                key={asset.asset_ref}
                className="flex min-w-0 flex-col overflow-hidden rounded-card border border-border bg-card wv-elevation"
              >
                {/* Deck-gradient placeholder poster — content decks are one of the
                    sanctioned gradient surfaces (HORIZON). No text on the raw ramp. */}
                <div
                  data-slot="content-thumb"
                  aria-hidden="true"
                  className="wv-grain h-28 w-full"
                  style={{ backgroundImage: `var(--grad-deck-${asset.deck})` }}
                />
                <div className="flex min-w-0 flex-col gap-2 p-4">
                  <p className="min-w-0 truncate font-display text-[15px] font-semibold" title={asset.name}>
                    {asset.name}
                  </p>
                  <code className="min-w-0 truncate rounded-input bg-[color:var(--wv-surface-2)] px-2 py-1 font-mono text-[12px] text-muted-foreground">
                    {asset.asset_ref}
                  </code>
                  <Button
                    variant="secondary"
                    size="sm"
                    icon={copied === asset.asset_ref ? Check : Copy}
                    className="wv-touch w-fit"
                    aria-label={`Copy content reference for ${asset.name}`}
                    onClick={() => void copyRef(asset.asset_ref)}
                  >
                    {copied === asset.asset_ref ? "Copied" : "Copy reference"}
                  </Button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </div>
      <Toaster />
    </div>
  );
}
