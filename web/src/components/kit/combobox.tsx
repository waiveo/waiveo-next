import { useState } from "react";
import { Check, ChevronsUpDown } from "lucide-react";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";
import { KitIcon } from "./kit-icon";

/**
 * Combobox — a picker with a search box, for a candidate set too long to scan.
 *
 * # The list this exists for
 *
 * The Settings page offers a site's IANA time zone. `Intl.supportedValuesOf`
 * enumerates **446** of them, and a `<select>` over 446 options is a control an
 * operator scrolls rather than uses: there is no way to get from the top of the
 * list to `Pacific/Auckland` except by dragging, and type-ahead on a native
 * select matches only the leading characters of the option — typing "tokyo"
 * finds nothing, because the option is called `Asia/Tokyo`.
 *
 * Search fixes that in a way grouping does not. Region `<optgroup>`s (what the
 * legacy console did) shorten the scroll and still leave the operator scrolling;
 * typing `tokyo`, `+13`, or `euro` here narrows 446 rows to a handful, matching
 * anywhere in the string. Grouping is the weaker half of the same fix and would
 * additionally need a declared group key per candidate — which, for a picker
 * driven by a ui-schema/1 document, is a contract change (`groupPath` alongside
 * UIS-133's `valuePath`/`labelPath`). Search needs nothing declared, because the
 * labels are already there.
 *
 * # Below the threshold, a native select is better
 *
 * This is not the replacement for `<select>`. A native select on a phone opens
 * the platform's own wheel, costs no JavaScript, and cannot be got wrong; for
 * five options this is strictly worse. It is for the long tail — see the
 * renderer's own `SELECT_SEARCH_THRESHOLD`, which decides between them from the
 * size of the candidate set.
 *
 * # The name is required at the type level
 *
 * The kit's standing discipline (`Button`, `Checkbox`, `TabList`). The trigger is
 * a `<button role="combobox">`, and the accessible name of a button comes from
 * its CONTENT — which here is the selected value, or the placeholder, and
 * changes as the operator picks. A control whose name is "Choose an IANA time
 * zone" until you use it and "Europe/London" afterwards is a control a screen
 * reader cannot describe twice the same way, so the name is passed in and the
 * content is left to say what is selected.
 */
export interface ComboboxOption {
  /** The value written when this candidate is chosen. */
  value: string;
  /** What the operator reads, and what the search matches against. */
  label: string;
}

type ComboboxBase = {
  options: readonly ComboboxOption[];
  /** The currently selected value, or `""` for none. */
  value: string;
  onChange: (value: string) => void;
  /** Shown on the trigger while nothing is selected. */
  placeholder?: string;
  /** Placeholder inside the search box. */
  searchPlaceholder?: string;
  /** Shown when the search matches nothing. */
  emptyText?: string;
  /** Set by `FormField` so the visible `<label>` points at this control. */
  id?: string;
  "aria-describedby"?: string;
  "aria-invalid"?: boolean;
  "aria-required"?: boolean;
  disabled?: boolean;
  className?: string;
};

export type ComboboxProps = ComboboxBase &
  (
    | { "aria-label": string; "aria-labelledby"?: string }
    | { "aria-labelledby": string; "aria-label"?: string }
  );

/** The search string cmdk scores a candidate against.
 *
 * Both halves, when they differ. A `data`-kind option source names its
 * candidates by one field and values them by another (UIS-133), so a screen
 * listed as "Lobby north" may be valued `01J8Z…`; an operator who has the id in
 * front of them — from a URL, a log line, an error — must be able to paste it.
 * When the two are the same string (the time-zone case, where the label IS the
 * value) it is not repeated, because cmdk scores a doubled term higher and would
 * order those candidates above better matches. */
function searchKey(option: ComboboxOption): string {
  return option.label === option.value ? option.label : `${option.label} ${option.value}`;
}

export function Combobox({
  options,
  value,
  onChange,
  placeholder = "Select…",
  searchPlaceholder = "Search…",
  emptyText = "Nothing matches.",
  id,
  disabled,
  className,
  ...aria
}: ComboboxProps) {
  const [open, setOpen] = useState(false);
  const selected = options.find((o) => o.value === value);

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          id={id}
          role="combobox"
          aria-expanded={open}
          disabled={disabled}
          className={cn(
            "wv-touch flex h-9 w-full min-w-0 items-center justify-between gap-2 rounded-input border border-border",
            "bg-transparent px-3 py-1 text-sm text-foreground outline-none",
            "transition-[box-shadow,border-color] focus-visible:ring-2 focus-visible:ring-ring",
            "disabled:cursor-not-allowed disabled:opacity-50",
            "aria-invalid:border-[color:var(--wv-err)]",
            className,
          )}
          {...aria}
        >
          {/* The selected value, or the placeholder in muted ink — never an empty
              box, which reads as "this field has no value" even when it does. */}
          <span className={cn("truncate", selected ? "" : "text-muted-foreground")}>
            {selected ? selected.label : placeholder}
          </span>
          <KitIcon icon={ChevronsUpDown} decorative className="size-4 shrink-0 opacity-60" />
        </button>
      </PopoverTrigger>
      <PopoverContent
        align="start"
        className="w-(--radix-popover-trigger-width) min-w-[16rem] p-0"
      >
        {/* Open ON the stored value, not at the top of the alphabet.
            `defaultValue` seeds cmdk's own highlight, and cmdk scrolls its
            highlighted row into view — so a site set to `America/Chicago`
            opens showing America/Chicago with its tick, instead of opening on
            `Africa/Abidjan` and leaving the operator to wonder where their
            value went. The content is unmounted while closed, so this re-seeds
            on every open rather than sticking at the first one. */}
        {/* Spread rather than a ternary yielding `undefined`: under
            exactOptionalPropertyTypes an optional prop holding undefined is NOT
            the same as an absent one, and cmdk types defaultValue as a bare
            string. Absent is what "no stored value to open on" means. */}
        <Command {...(selected ? { defaultValue: searchKey(selected) } : {})}>
          <CommandInput placeholder={searchPlaceholder} />
          <CommandList>
            <CommandEmpty>{emptyText}</CommandEmpty>
            <CommandGroup>
              {options.map((option) => (
                <CommandItem
                  key={option.value}
                  value={searchKey(option)}
                  // WHICH option is the stored one is `aria-current`, not
                  // `aria-selected`: cmdk owns `aria-selected` for the keyboard
                  // HIGHLIGHT (the row the arrow keys are on), which moves as
                  // the operator types and has nothing to do with what is
                  // saved. Announcing the highlight as the selection would tell
                  // a screen-reader user their zone had changed on every
                  // keystroke.
                  {...(option.value === value ? { "aria-current": true } : {})}
                  // The option is CLOSED OVER rather than read from `onSelect`'s
                  // argument, and the difference is not cosmetic: cmdk hands the
                  // handler the item's own `value` — which here is `searchKey`,
                  // the string built for MATCHING. For a picker whose label and
                  // value differ (`Lobby south` / `01HXY…`) that argument is
                  // "Lobby south 01HXY…", and writing it back would store a
                  // display string where a record id belongs.
                  onSelect={() => {
                    onChange(option.value);
                    setOpen(false);
                  }}
                >
                  <KitIcon
                    icon={Check}
                    decorative
                    className={cn("size-4 shrink-0", option.value === value ? "" : "opacity-0")}
                  />
                  <span className="truncate">{option.label}</span>
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
