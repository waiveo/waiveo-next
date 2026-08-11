import {
  CalendarDays,
  Clock,
  CloudSun,
  Image as ImageIcon,
  ListTree,
  MousePointerClick,
  Sparkles,
  Square,
  Timer,
  ToggleLeft,
  Type,
  Video,
  type LucideIcon,
} from "lucide-react";
import type { DeriveKind, LayerKind } from "@/api";

/**
 * The LAYER CATALOG — what a slide can carry, described once.
 *
 * This was private to the Studio's insert toolbar, which was fine while the
 * insert menu was the only place a widget was ever named. It is not the only
 * place any more: `/widgets` browses the same set (legacy had a Widgets area
 * under Slidecast and waiveo-next had none — parity row 8.4). Two lists of the
 * same twelve kinds would drift on the first kind that got added to one of them,
 * and the drift would be silent: both surfaces would still render.
 *
 * So the glyph, the name and the sentence that explains where a kind's value
 * comes from live here, and both surfaces read them. `layer-catalog.test.ts`
 * derives its expectations from `LAYER_KINDS`/`DERIVE_KINDS`, so a thirteenth
 * kind fails the build until it is described here — which is the property that
 * makes one shared catalog worth more than two convenient ones.
 */

export const KIND_ICON: Record<LayerKind, LucideIcon> = {
  text: Type,
  rect: Square,
  image: ImageIcon,
  clock: Clock,
  date: CalendarDays,
  countdown: Timer,
  weather: CloudSun,
  entity: ToggleLeft,
  video: Video,
  ping: MousePointerClick,
  nav: ListTree,
  derive: Sparkles,
};

export const KIND_LABEL: Record<LayerKind, string> = {
  text: "Text",
  rect: "Rectangle",
  image: "Image",
  clock: "Clock",
  date: "Date",
  countdown: "Countdown",
  weather: "Weather",
  entity: "Entity state",
  video: "Video",
  ping: "Button",
  nav: "Menu",
  derive: "Rasterized",
};

/** What each live widget IS, in the terms that decide whether it is the one you
 * want. Every line says where the value comes from, because that is the whole
 * difference between these four and a text layer — and, for the two the box
 * resolves, the reason the canvas can only ever show a stand-in. */
export const WIDGET_BLURB: Record<"date" | "countdown" | "weather" | "entity", string> = {
  date: "Today's date, drawn by the screen from its own clock and re-rendered as the day turns.",
  countdown:
    "Time remaining until an instant you choose. The screen counts it down itself, so it keeps ticking even if the box is unreachable.",
  weather: "Current conditions for this site, fetched by the box and refreshed on every screen poll.",
  entity: "The live state of one device — on, off, playing — as the box last observed it.",
};

/** What each rasterized kind IS, and — because a derive layer costs a render an
 * operator has to go and run — WHEN NOT to reach for it. Two of the three have a
 * native equivalent that stays live, needs no content and re-styles instantly;
 * choosing the rasterized one for a flat panel is a real, silent cost. */
export const DERIVE_LABEL: Record<DeriveKind, string> = {
  qr: "QR code",
  text: "Styled text",
  rect: "Styled panel",
};

export const DERIVE_BLURB: Record<DeriveKind, string> = {
  qr: "A scannable QR code. No screen can draw one itself, so this is the only way to put a code on a slide.",
  text: "Text with a gradient, a drop shadow, a rounded backing or a font the screen does not ship. Plain text should stay a Text layer — it needs no render.",
  rect: "A panel with a gradient fill, rounded corners or a drop shadow. A flat block of colour should stay a Rectangle — it needs no render.",
};

/** The kinds that are NOT live widgets and NOT rasterized — the ones an operator
 * types or picks the content of. `derive` is excluded because it is the header of
 * its own family, described by DERIVE_LABEL/DERIVE_BLURB instead. */
export const STATIC_LAYER_KINDS = ["text", "rect", "image", "video"] as const;

/** What each static kind is, for the browse surface. The Studio toolbar does not
 * need these — a button labelled "Text" is self-evident at the moment of
 * inserting one — but a page that answers "what can a slide carry?" does. */
export const STATIC_BLURB: Record<(typeof STATIC_LAYER_KINDS)[number], string> = {
  text: "Words you type, drawn by the screen in a font it already ships. The cheapest layer there is — nothing to fetch, nothing to render.",
  rect: "A flat block of colour. Backing for text, a divider, a band of brand colour.",
  image: "A picture from the media library, addressed by its content hash. The screen caches it and re-fetches only when it changes.",
  video: "A video file from the media library, played by the screen's own decoder.",
};

/** What each interactive kind is. These are the two an operator places like a
 * rectangle but that DO something when the remote's OK button lands on them. */
export const INTERACTIVE_BLURB: Record<"ping" | "nav", string> = {
  ping: "A button the viewer can focus with the remote's D-pad. Pressing OK reports an interaction to the box, which an automation can act on.",
  nav: "A menu entry that jumps the deck to another slide when OK is pressed. This is how a cast becomes something a viewer steers.",
};
