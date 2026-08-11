import { useCallback, useEffect, useState } from "react";
import { ImageOff, Images } from "lucide-react";
import { Button, EmptyState, KitIcon, Modal } from "@/components/kit";
import { ApiError, type ContentAsset, type WaiveoApi } from "@/api";
import { cn } from "@/lib/utils";

/**
 * The media library — one browsable view of the content origin, shared by the
 * standalone /media route and the Studio's image picker.
 *
 * It exists as a shared piece rather than twice because the two surfaces answer
 * the same question ("what bytes does this box already have?") and the ANSWER
 * has a subtlety worth writing once: the origin is content-addressed, so an
 * asset has no filename, no MIME type and no thumbnail — only a `sha256:` ref, a
 * fetch URL, a size and a store time (internal/app/api/content.go). The grid
 * therefore renders the asset by FETCHING it as an image and falling back to a
 * placeholder when that fails, which is also how it distinguishes an image from
 * a video or a PDF: by whether the browser could decode it. There is no metadata
 * to ask.
 */

/**
 * Load the content origin's listing: the assets, the failure (as a human
 * string), and a reload for after an upload.
 *
 * `enabled` exists because the Studio's picker is a MODAL that stays mounted
 * while closed — a hook that fetched on mount would pull the whole library on
 * every Studio page load, for a dialog the operator may never open. Gating the
 * fetch also means the listing is FRESH when it is opened, rather than a
 * snapshot from whenever the editor happened to mount.
 */
export function useContentLibrary(
  api: WaiveoApi,
  enabled = true,
): {
  assets: ContentAsset[] | null;
  error: string | null;
  reload: () => Promise<void>;
} {
  const [assets, setAssets] = useState<ContentAsset[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(async () => {
    try {
      setAssets(await api.content.list());
      setError(null);
    } catch (err) {
      setAssets([]);
      setError(err instanceof ApiError ? (err.detail ?? err.code) : "The service is unreachable.");
    }
  }, [api]);

  useEffect(() => {
    if (!enabled) return;
    void reload();
  }, [enabled, reload]);

  return { assets, error, reload };
}

/** Human-readable byte size — the only thing the origin knows about an asset
 * besides when it landed, so it is worth rendering legibly. */
export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/** The short form of a content ref, for a card caption. The digest's leading
 * hex is what an operator eyeballs against a playlist item. */
export function shortRef(assetRef: string): string {
  const hex = assetRef.replace(/^sha256:/, "");
  return hex.length > 12 ? `${hex.slice(0, 12)}…` : hex;
}

/** One asset tile. The thumbnail is the asset itself; if the browser cannot
 * decode it (a video, a font, a PDF) the tile shows a plain placeholder rather
 * than a broken-image glyph. */
function AssetTile({
  asset,
  selected,
  onSelect,
}: {
  asset: ContentAsset;
  selected: boolean;
  onSelect?: (asset: ContentAsset) => void;
}) {
  const [undecodable, setUndecodable] = useState(false);
  const interactive = onSelect !== undefined;
  const body = (
    <>
      <div className="flex h-28 w-full items-center justify-center overflow-hidden rounded-input bg-[color:var(--wv-surface-2)]">
        {undecodable ? (
          <KitIcon icon={ImageOff} decorative className="size-6 text-muted-foreground" />
        ) : (
          <img
            src={asset.url}
            alt=""
            className="max-h-full max-w-full object-contain"
            onError={() => setUndecodable(true)}
          />
        )}
      </div>
      <div className="flex min-w-0 flex-col gap-0.5">
        <code className="min-w-0 truncate font-mono text-[12px]" title={asset.asset_ref}>
          {shortRef(asset.asset_ref)}
        </code>
        <span className="text-[12px] text-muted-foreground">{formatBytes(asset.size_bytes)}</span>
      </div>
    </>
  );

  const shell = cn(
    "flex min-w-0 flex-col gap-2 rounded-card border p-3 text-left",
    selected ? "border-[color:var(--wv-accent-text)] bg-accent" : "border-border bg-card",
  );

  if (!interactive) {
    return (
      <li data-slot="media-asset" className={shell}>
        {body}
      </li>
    );
  }
  return (
    <li className="min-w-0">
      <button
        type="button"
        data-slot="media-asset"
        aria-pressed={selected}
        aria-label={`Use ${asset.asset_ref}`}
        onClick={() => onSelect(asset)}
        className={cn(
          shell,
          "wv-touch w-full cursor-pointer outline-none transition-colors hover:border-[color:var(--wv-accent-text)] focus-visible:ring-2 focus-visible:ring-ring",
        )}
      >
        {body}
      </button>
    </li>
  );
}

/** The grid itself. Pass `onSelect` to make the tiles pickable (the Studio's
 * image layer); omit it for a read-only browse (the /media route). */
export function MediaGrid({
  assets,
  error,
  selectedRef,
  onSelect,
  emptyHint,
}: {
  assets: ContentAsset[] | null;
  error: string | null;
  selectedRef?: string | undefined;
  onSelect?: ((asset: ContentAsset) => void) | undefined;
  emptyHint?: string;
}) {
  if (error) {
    return (
      <p role="alert" className="text-sm text-[color:var(--wv-err)]">
        Couldn't load the media library — {error}
      </p>
    );
  }
  if (assets === null) {
    return <p className="text-sm text-muted-foreground">Loading the media library…</p>;
  }
  if (assets.length === 0) {
    return (
      <EmptyState
        icon={Images}
        title="The media library is empty"
        description={emptyHint ?? "Upload a file on the Content page and it appears here, ready to place on a slide."}
      />
    );
  }
  return (
    <ul className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4">
      {assets.map((asset) => (
        <AssetTile
          key={asset.asset_ref}
          asset={asset}
          selected={asset.asset_ref === selectedRef}
          {...(onSelect ? { onSelect } : {})}
        />
      ))}
    </ul>
  );
}

/**
 * The content picker: the same grid in a modal, resolving `{asset_ref, url}`
 * onto a content-bearing slide layer. Both fields travel together and that is a
 * wire requirement, not a convenience — `wire.ValidateSlideLayers` refuses an
 * image or video layer holding one without the other, and the projector would
 * drop the whole slide.
 *
 * `kind` changes the WORDING and nothing else, deliberately: the origin is
 * content-addressed and knows no MIME type (see this file's header), so there is
 * nothing to filter the grid by. Saying "video" while offering every asset is
 * honest about that; pretending to filter would not be.
 */
export function MediaPickerModal({
  open,
  onOpenChange,
  api,
  kind = "image",
  selectedRef,
  onPick,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  api: WaiveoApi;
  kind?: "image" | "video";
  selectedRef?: string | undefined;
  onPick: (asset: ContentAsset) => void;
}) {
  const { assets, error, reload } = useContentLibrary(api, open);
  return (
    <Modal
      title={kind === "video" ? "Choose a video" : "Choose an image"}
      description="Everything the content origin is serving — it stores bytes by digest, not by file type, so every asset is listed. Uploads land on the Content page."
      size="xl"
      open={open}
      onOpenChange={onOpenChange}
      footer={
        <Button variant="secondary" onClick={() => void reload()}>
          Refresh
        </Button>
      }
    >
      <div className="max-h-[60vh] overflow-y-auto">
        <MediaGrid
          assets={assets}
          error={error}
          selectedRef={selectedRef}
          onSelect={(asset) => {
            onPick(asset);
            onOpenChange(false);
          }}
        />
      </div>
    </Modal>
  );
}
