import type { AnchorHTMLAttributes, ReactNode } from "react";
import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";
import { Button } from "./button";
import { KitIcon } from "./kit-icon";

/**
 * DownloadLink — a real anchor for a URL the browser streams to disk.
 *
 * # Why a download is not a button
 *
 * Two surfaces already reached this conclusion independently and each hand-rolled
 * its own `<a href download>` with its own class string: the cast library's
 * export action, and the backup page — which had to open a ui-schema `slot`
 * specifically because the widget catalog has no link kind, and recorded in its
 * own docstring that a `button` + `navigate` "would have thrown away what an
 * anchor is for". This is that anchor, once.
 *
 * What a `<button onClick={fetch → Blob → click()}>` throws away, concretely:
 * the URL stops being right-clickable ("Save link as…", "Copy link address"),
 * the transfer stops appearing in the browser's own download manager, a
 * multi-hundred-megabyte workspace archive is buffered through the tab's memory
 * instead of streamed to disk, and the whole thing stops working if scripting is
 * blocked or the JS throws. None of that is recoverable by styling.
 *
 * # Two shapes, because there are two real call sites
 *
 * `inline` is a text link in prose or beside a panel. `icon` renders through the
 * kit `Button` with `asChild`, so an export action sitting in a row of icon
 * buttons is byte-for-byte the same ghost square as its neighbours — the reason
 * the casts row had a hand-rolled copy of the button's utility string in the
 * first place.
 *
 * # The name is required at the type level
 *
 * Same discipline as `Button` and `Checkbox`. An `icon` link has no text at all,
 * so `aria-label` is the only place its name can come from, and "Download"
 * repeated on twelve rows names none of them — pass the thing being downloaded
 * (`Export Morning Loop as a .cast bundle`).
 *
 * # `download` is not optional
 *
 * A same-origin `<a download>` is what makes the browser save rather than
 * navigate. Omitting it on a URL the server returns inline (a JSON export, an
 * image) replaces the console with the file, and the operator's way back is the
 * Back button. `true` accepts the server's own `Content-Disposition` filename;
 * a string overrides it.
 */
type DownloadLinkBase = Omit<
  AnchorHTMLAttributes<HTMLAnchorElement>,
  "href" | "download" | "children" | "aria-label"
> & {
  href: string;
  /** Filename to save as, or `true` to accept the server's own. */
  download: string | true;
  /** Optional leading glyph — decorative; the accessible name carries the meaning. */
  icon?: LucideIcon;
  /** `inline` for a text link, `icon` for a glyph square in a row of actions. */
  variant?: "inline" | "icon";
};

export type DownloadLinkProps = DownloadLinkBase &
  (
    | { children: ReactNode; "aria-label"?: string }
    | { "aria-label": string; children?: ReactNode }
  );

export function DownloadLink({
  href,
  download,
  icon,
  variant = "inline",
  className,
  children,
  ...rest
}: DownloadLinkProps) {
  // `download` passes straight through. It is `string | true`, and React already
  // renders `download={true}` as the BARE attribute — the "server names it"
  // form — rather than as `download="true"`, which would save every cast bundle
  // as a file literally called `true`. This carried a `=== true ? "" :` ternary
  // to force that; a mutation proved the ternary changed nothing, so it is gone
  // and `link.test.tsx` holds the DOM contract instead.
  if (variant === "icon") {
    return (
      <Button asChild size="icon" variant="ghost" className={className}>
        <a href={href} download={download} {...rest}>
          {icon ? <KitIcon icon={icon} decorative /> : null}
          {children}
        </a>
      </Button>
    );
  }

  return (
    <a
      href={href}
      download={download}
      className={cn(
        "wv-touch inline-flex w-fit items-center gap-1.5 rounded-input text-sm underline underline-offset-2",
        "outline-none hover:text-[color:var(--wv-accent-text)] focus-visible:ring-2 focus-visible:ring-ring",
        className,
      )}
      {...rest}
    >
      {icon ? <KitIcon icon={icon} decorative className="size-4" /> : null}
      {children}
    </a>
  );
}
