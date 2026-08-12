import { useState } from "react";
import { describe, expect, it } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Combobox, type ComboboxOption } from "./combobox";
import { FormField } from "./form-field";

// The 446-zone list is the reason this exists, so the tests are sized like it:
// a long option set, driven by typing, with the assertions on what the caller
// was told and on what an operator can still reach.

const ZONES: ComboboxOption[] = [
  "UTC",
  "Africa/Cairo",
  "America/Chicago",
  "America/New_York",
  "Asia/Tokyo",
  "Australia/Sydney",
  "Europe/London",
  "Europe/Paris",
  "Pacific/Auckland",
].map((z) => ({ value: z, label: z }));

function Subject({
  options = ZONES,
  initial = "",
  onPick,
}: {
  options?: ComboboxOption[];
  initial?: string;
  onPick?: (v: string) => void;
}) {
  const [value, setValue] = useState(initial);
  return (
    <Combobox
      aria-label="Time zone"
      options={options}
      value={value}
      onChange={(v) => {
        setValue(v);
        onPick?.(v);
      }}
      placeholder="Choose an IANA time zone"
      searchPlaceholder="Search time zone"
    />
  );
}

const open = async (user: ReturnType<typeof userEvent.setup>) =>
  user.click(screen.getByRole("combobox", { name: "Time zone" }));

describe("Combobox", () => {
  it("shows the placeholder while empty and the selected label once chosen", async () => {
    const user = userEvent.setup();
    render(<Subject />);
    const trigger = screen.getByRole("combobox", { name: "Time zone" });
    expect(trigger).toHaveTextContent("Choose an IANA time zone");

    await open(user);
    await user.click(await screen.findByRole("option", { name: "Asia/Tokyo" }));
    expect(trigger).toHaveTextContent("Asia/Tokyo");
  });

  it("reports the chosen VALUE, never the string it was matched by", async () => {
    // cmdk hands `onSelect` the item's own `value` — which is the SEARCH KEY,
    // not the option's value. Where the two differ (a record named "Lobby
    // south" and valued `01HXY…`) taking that argument would store a display
    // string where an id belongs, and the record would resolve against nothing.
    // The component closes over the option instead. Both shapes are driven here
    // because only the second one can tell the two apart — a picker whose label
    // IS its value (the time-zone case) passes either way.
    const user = userEvent.setup();
    const picked: string[] = [];

    const view = render(<Subject onPick={(v) => picked.push(v)} />);
    await open(user);
    await user.click(await screen.findByRole("option", { name: "Asia/Tokyo" }));
    expect(picked).toEqual(["Asia/Tokyo"]);
    view.unmount();

    render(
      <Subject
        options={[
          { value: "01J8Z3K4N5P6Q7R8S9T0V1W2A1", label: "Lobby north" },
          { value: "01HXYQ9M4B7D2F6G8J0K3N5P7R", label: "Lobby south" },
        ]}
        onPick={(v) => picked.push(v)}
      />,
    );
    await open(user);
    await user.click(await screen.findByRole("option", { name: "Lobby south" }));
    expect(picked[1]).toBe("01HXYQ9M4B7D2F6G8J0K3N5P7R");
  });

  it("narrows the list as the operator types — anywhere in the name, not just the start", async () => {
    // The native-select failure this replaces: type-ahead there matches only the
    // leading characters, so "tokyo" finds nothing in a list of `Asia/Tokyo`.
    const user = userEvent.setup();
    render(<Subject />);
    await open(user);
    await user.type(screen.getByPlaceholderText("Search time zone"), "tokyo");
    await waitFor(() => expect(screen.getAllByRole("option")).toHaveLength(1));
    expect(screen.getByRole("option", { name: "Asia/Tokyo" })).toBeInTheDocument();
  });

  it("says so when nothing matches instead of showing an empty box", async () => {
    const user = userEvent.setup();
    render(<Subject />);
    await open(user);
    await user.type(screen.getByPlaceholderText("Search time zone"), "zzzz");
    expect(await screen.findByText("Nothing matches.")).toBeInTheDocument();
    expect(screen.queryAllByRole("option")).toHaveLength(0);
  });

  it("matches on the underlying value too, when it differs from the label", async () => {
    // A `data`-kind option source names candidates by one field and values them
    // by another (UIS-133): an operator holding an id from a log line must be
    // able to paste it.
    const user = userEvent.setup();
    render(
      <Subject
        options={[
          { value: "01J8Z3K4N5P6Q7R8S9T0V1W2A1", label: "Lobby north" },
          { value: "01HXYQ9M4B7D2F6G8J0K3N5P7R", label: "Lobby south" },
        ]}
      />,
    );
    await open(user);
    await user.type(screen.getByPlaceholderText("Search time zone"), "HXYQ9M");
    await waitFor(() => expect(screen.getAllByRole("option")).toHaveLength(1));
    expect(screen.getByRole("option", { name: "Lobby south" })).toBeInTheDocument();
  });

  it("does not repeat the label in the search key when it IS the value", async () => {
    // cmdk scores a candidate against its `value`, and a doubled term scores
    // higher — so `Asia/Tokyo Asia/Tokyo` would outrank a genuinely better match
    // for the same query. The key is the label alone when the two agree, and
    // both only when they differ (so an id can be pasted).
    const user = userEvent.setup();
    render(
      <Subject
        options={[
          { value: "Asia/Tokyo", label: "Asia/Tokyo" },
          { value: "01HXYQ9M4B7D2F6G8J0K3N5P7R", label: "Lobby south" },
        ]}
      />,
    );
    await open(user);
    expect(await screen.findByRole("option", { name: "Asia/Tokyo" })).toHaveAttribute(
      "data-value",
      "Asia/Tokyo",
    );
    expect(screen.getByRole("option", { name: "Lobby south" })).toHaveAttribute(
      "data-value",
      "Lobby south 01HXYQ9M4B7D2F6G8J0K3N5P7R",
    );
  });

  it("is operable from the keyboard end to end", async () => {
    const user = userEvent.setup();
    const picked: string[] = [];
    render(<Subject onPick={(v) => picked.push(v)} />);
    await user.tab();
    expect(screen.getByRole("combobox", { name: "Time zone" })).toHaveFocus();
    await user.keyboard("{Enter}");
    await screen.findByPlaceholderText("Search time zone");
    await user.keyboard("paris");
    await waitFor(() => expect(screen.getAllByRole("option")).toHaveLength(1));
    await user.keyboard("{Enter}");
    expect(picked).toEqual(["Europe/Paris"]);
  });

  it("closes after a pick, and reopening starts from an unfiltered list", async () => {
    const user = userEvent.setup();
    render(<Subject />);
    await open(user);
    await user.type(screen.getByPlaceholderText("Search time zone"), "tokyo");
    await user.click(await screen.findByRole("option", { name: "Asia/Tokyo" }));
    await waitFor(() => expect(screen.queryByRole("option")).toBeNull());

    await open(user);
    await waitFor(() => expect(screen.getAllByRole("option")).toHaveLength(ZONES.length));
  });

  it("opens ON the stored value rather than at the top of the alphabet", async () => {
    // Caught by LOOKING at it: a site on `America/Chicago` opened the picker
    // showing `Africa/Abidjan`, 400 rows above its own value. cmdk scrolls its
    // highlighted row into view, so the highlight is seeded from what is stored.
    const user = userEvent.setup();
    render(<Subject initial="Pacific/Auckland" />);
    await open(user);
    const stored = await screen.findByRole("option", { name: "Pacific/Auckland" });
    expect(stored).toHaveAttribute("aria-selected", "true");
    // …and nothing else is holding the highlight.
    expect(
      screen.getAllByRole("option").filter((o) => o.getAttribute("aria-selected") === "true"),
    ).toHaveLength(1);
  });

  it("marks the STORED option as current, and marks only that one", async () => {
    // `aria-current`, not `aria-selected`: cmdk owns aria-selected for the
    // keyboard highlight, which moves on every keystroke. If the stored value
    // were announced that way, a screen-reader user typing to search would hear
    // their zone change repeatedly without anything having been saved.
    const user = userEvent.setup();
    render(<Subject initial="Europe/London" />);
    await open(user);
    const stored = await screen.findByRole("option", { name: "Europe/London" });
    expect(stored).toHaveAttribute("aria-current", "true");
    expect(screen.getAllByRole("option").filter((o) => o.hasAttribute("aria-current"))).toHaveLength(
      1,
    );
    expect(screen.getByRole("option", { name: "Asia/Tokyo" })).not.toHaveAttribute("aria-current");
  });

  it("wires up to FormField's label, help and error like every other control", async () => {
    render(
      <FormField label="Time zone" help="Where local time comes from." error="Pick one.">
        {(field) => (
          <Combobox
            {...field}
            aria-label="Time zone"
            options={ZONES}
            value=""
            onChange={() => {}}
          />
        )}
      </FormField>,
    );
    const trigger = screen.getByRole("combobox", { name: "Time zone" });
    expect(trigger).toHaveAttribute("aria-invalid", "true");
    const described = (trigger.getAttribute("aria-describedby") ?? "")
      .split(" ")
      .map((id) => document.getElementById(id)?.textContent)
      .join(" ");
    expect(described).toContain("Pick one.");
    expect(described).toContain("Where local time comes from.");
  });

  it("does not open when disabled", async () => {
    const user = userEvent.setup();
    render(
      <Combobox
        aria-label="Time zone"
        options={ZONES}
        value=""
        onChange={() => {}}
        disabled
      />,
    );
    await user.click(screen.getByRole("combobox", { name: "Time zone" }));
    expect(screen.queryByRole("option")).toBeNull();
  });

  it("stays usable at the size that motivated it — 446 candidates", async () => {
    // Not a performance claim: a smoke test that the real zone count renders,
    // filters, and picks. The flat <select> this replaced also "worked" at 446.
    const many: ComboboxOption[] = Intl.supportedValuesOf("timeZone").map((z) => ({
      value: z,
      label: z,
    }));
    expect(many.length).toBeGreaterThan(400);

    const user = userEvent.setup();
    const picked: string[] = [];
    render(<Subject options={many} onPick={(v) => picked.push(v)} />);
    await open(user);
    const list = screen.getByRole("listbox");
    expect(within(list).getAllByRole("option").length).toBe(many.length);

    await user.type(screen.getByPlaceholderText("Search time zone"), "auckland");
    await waitFor(() => expect(screen.getAllByRole("option")).toHaveLength(1));
    await user.click(screen.getByRole("option", { name: "Pacific/Auckland" }));
    expect(picked).toEqual(["Pacific/Auckland"]);
  });
});
