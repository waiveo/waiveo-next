import { useEffect, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router";
import { ArrowLeft, Compass } from "lucide-react";
import { ApiError, createApi, type Cast, type WaiveoApi } from "@/api";
import { Button, EmptyState } from "@/components/kit";
import { useContentLibrary } from "@/routes/media/media-library";
import { CastPlayer } from "./cast-player";

/**
 * `/preview?id=…` — watch a SAVED cast play.
 *
 * # Why a route, and not only a mode of the Studio
 *
 * It is both, and the split is the whole design.
 *
 * The Studio hands its LIVE document to the same `CastPlayer` in an overlay
 * (`studio-route.tsx`), because previewing unsaved edits is the verification
 * loop this whole surface exists for. A route cannot do that job: navigating
 * away unmounts the editor, and the editor's own door already asks before
 * discarding — a Preview button that raised "discard your changes?" would be
 * absurd. The overlay also keeps the undo history alive, so "watch it, come
 * back, fix the third slide" is one continuous session.
 *
 * But a route is the job the overlay cannot do. An operator asking "what is this
 * cast, and is it any good" should not have to open an EDITOR on it — legacy
 * gave preview its own page for the same reason, and the Casts library is where
 * that question is asked. A route is also linkable, which is how one person
 * sends another the thing they are looking at.
 *
 * # Why it is outside the shell
 *
 * Same reason the Studio is, stated in App.tsx: a 1920×1080 stage, a transport
 * and a panel want the viewport, and every rail destination abandons what you
 * are watching. It carries its own door back, and it is declared in
 * OFF_RAIL_ROUTES with that door — a rail entry would open it on nothing, the
 * way a rail entry to the Studio would.
 */
export default function PreviewRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [params] = useSearchParams();
  const castId = params.get("id");

  const [cast, setCast] = useState<Cast | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!castId) return;
    let cancelled = false;
    void (async () => {
      try {
        const read = await client.casts.get(castId);
        if (!cancelled) {
          setCast(read.data);
          setError(null);
        }
      } catch (err) {
        if (!cancelled) setError(err instanceof ApiError ? (err.detail ?? err.code) : "The service is unreachable.");
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [client, castId]);

  // The content origin's listing, for the layers whose substance is bytes. Read
  // the same way the Studio reads it, and degraded the same way: a FAILED read
  // leaves the map null so nothing can be reported missing on the strength of an
  // answer nobody has. The player draws a grey placeholder for bytes it could
  // not fetch, and a preview that drew that placeholder because its own listing
  // request timed out would be accusing the content origin of something it did
  // not do.
  const { assets, error: originError } = useContentLibrary(client);
  const assetUrls = originError !== null || assets === null ? null : new Map(assets.map((a) => [a.asset_ref, a.url]));

  if (!castId) {
    return (
      <Frame>
        <EmptyState
          icon={Compass}
          title="No cast to preview"
          description="Preview opens on a cast. Pick one from the library and press Preview."
          action={
            <Button variant="secondary" asChild>
              <Link to="/casts">Back to casts</Link>
            </Button>
          }
        />
      </Frame>
    );
  }

  if (error) {
    return (
      <Frame>
        <EmptyState
          icon={Compass}
          title="That cast could not be opened"
          description={error}
          action={
            <Button variant="secondary" asChild>
              <Link to="/casts">Back to casts</Link>
            </Button>
          }
        />
      </Frame>
    );
  }

  if (!cast) {
    return (
      <Frame>
        <p className="text-sm text-muted-foreground">Opening the cast…</p>
      </Frame>
    );
  }

  return (
    <CastPlayer
      cast={cast}
      assetUrls={assetUrls}
      originError={originError}
      door={
        <Button variant="ghost" size="sm" icon={ArrowLeft} asChild>
          <Link to="/casts">Back to casts</Link>
        </Button>
      }
    />
  );
}

/** The full-viewport frame for the states that have no player to show. Matches
 * the player's own shell so the page does not jump when the cast lands. */
function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="fixed inset-0 z-40 flex h-[100dvh] w-screen items-center justify-center bg-background p-8 text-foreground">
      {children}
    </div>
  );
}
