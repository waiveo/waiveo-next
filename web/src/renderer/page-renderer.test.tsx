import { describe, expect, it, vi } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PageRenderer } from "./PageRenderer";
import type { ActionHandler } from "./types";

// RTL over the corpus: the four valid page-type documents are loaded straight
// from the frozen ui-schema/1 corpus and rendered through the real renderer. A
// message catalog supplies human labels so the assertions read against real
// copy; the renderer's own fallback humanizer covers anything unlisted.
interface CorpusCase {
  case_id: string;
  input: Record<string, unknown>;
}
const modules = import.meta.glob<CorpusCase>("../../../conformance/corpora/ui-schema-1/*.json", {
  eager: true,
  import: "default",
});
const corpus = Object.fromEntries(Object.values(modules).map((c) => [c.case_id, c])) as Record<string, CorpusCase>;
const docFor = (id: string) => corpus[id].input;

const messages: Record<string, string> = {
  "msg:presets.list.name": "Name",
  "msg:presets.detail.empty": "Select a preset to edit it.",
  "msg:presets.detail.nameLabel": "Preset name",
  "msg:settings.general.title": "General",
  "msg:settings.general.displayName": "Display name",
  "msg:settings.general.quietHours": "Quiet hours",
  "msg:settings.save": "Save",
  "msg:dashboard.onlineScreens": "Online screens",
  "msg:dashboard.activeAutomations": "Active automations",
  "msg:wizard.step.name": "Name",
  "msg:wizard.step.nameLabel": "Automation name",
  "msg:wizard.step.review": "Review",
};

describe("PageRenderer — list-detail (UIS-020)", () => {
  const presets = [
    { id: "01J8Z3K4N5P6Q7R8S9T0V1W2A1", name: "Morning open" },
    { id: "01J8Z3K4N5P6Q7R8S9T0V1W2A2", name: "Evening wind-down" },
  ];

  it("renders the list table with its rows and shows the empty detail until a row is pressed", async () => {
    const user = userEvent.setup();
    render(<PageRenderer doc={docFor("UIS-020-valid-list-detail-presets")} data={{ presets }} messages={messages} />);

    // The list table renders a Name column and both preset rows.
    expect(screen.getByText("Morning open")).toBeInTheDocument();
    expect(screen.getByText("Evening wind-down")).toBeInTheDocument();
    // Detail is empty until a selection (detail.source predicate matches nothing).
    expect(screen.getByText("Select a preset to edit it.")).toBeInTheDocument();

    // Press a row → rowPress `set $ui.selected` → the detail resolves the record
    // and its text-input shows that preset's name (write-back path proven).
    await user.click(screen.getByRole("button", { name: /Evening wind-down/ }));
    const detail = screen.getByRole("region", { name: "Detail" });
    await waitFor(() =>
      expect(within(detail).getByLabelText("Preset name")).toHaveValue("Evening wind-down"),
    );
  });

  it("edits the selected record's field through the input's write-back (UIS-065)", async () => {
    const user = userEvent.setup();
    render(<PageRenderer doc={docFor("UIS-020-valid-list-detail-presets")} data={{ presets }} messages={messages} />);
    await user.click(screen.getByRole("button", { name: /Morning open/ }));
    const input = await screen.findByLabelText("Preset name");
    await user.clear(input);
    await user.type(input, "Sunrise");
    expect(input).toHaveValue("Sunrise");
  });
});

describe("PageRenderer — settings-form (UIS-030)", () => {
  it("renders the section, its fields, and a submit-wired save button", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = { submit: vi.fn() };
    render(
      <PageRenderer
        doc={docFor("UIS-030-valid-settings-form")}
        data={{ site: { displayName: "The Hangar", quietHours: true } }}
        messages={messages}
        handler={handler}
      />,
    );

    expect(screen.getByRole("heading", { name: "General" })).toBeInTheDocument();
    expect(screen.getByLabelText("Display name")).toHaveValue("The Hangar");
    expect(screen.getByRole("switch", { name: "Quiet hours" })).toHaveAttribute("aria-checked", "true");

    // The save button's on.press submit dispatches to the api-client seam.
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(handler.submit).toHaveBeenCalledWith("site", { site: { displayName: "The Hangar", quietHours: true } });
  });
});

describe("PageRenderer — dashboard (UIS-040)", () => {
  it("renders stat-tiles with computed counts and a host-provided slot", () => {
    render(
      <PageRenderer
        doc={docFor("UIS-040-valid-dashboard")}
        data={{ screens: [{}, {}, {}], automations: [{}, {}] }}
        messages={messages}
        slots={{ "dashboard-cards": <div data-testid="slot-content">cards</div> }}
      />,
    );
    // count(screens) === 3, count(automations) === 2 (each tile a root binding).
    expect(screen.getByText("Online screens")).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
    expect(screen.getByText("Active automations")).toBeInTheDocument();
    expect(screen.getByText("2")).toBeInTheDocument();
    // The slot renders the host-supplied fragment content (UIS-185).
    expect(screen.getByTestId("slot-content")).toBeInTheDocument();
  });
});

describe("PageRenderer — wizard (UIS-050)", () => {
  it("gates Next on canAdvanceIf, advances, and Finish invokes onFinish", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = { callAction: vi.fn() };
    render(<PageRenderer doc={docFor("UIS-050-valid-wizard")} messages={messages} handler={handler} />);

    // Step 1: Next is disabled until the shared draft's `name` is truthy.
    const next = screen.getByRole("button", { name: "Next" });
    expect(next).toBeDisabled();
    await user.type(screen.getByLabelText("Automation name"), "Lobby wind-down");
    expect(next).toBeEnabled();

    // Advance → the review step reads the name the first step wrote (UIS-052).
    await user.click(next);
    expect(screen.getByRole("heading", { name: "Review" })).toBeInTheDocument();
    expect(screen.getByText("Lobby wind-down")).toBeInTheDocument();

    // Finish → onFinish call-action dispatches to the handler.
    await user.click(screen.getByRole("button", { name: "Finish" }));
    expect(handler.callAction).toHaveBeenCalledWith("createFromDraft", {});

    // Back returns to the first step (its input retains the shared draft value).
    await user.click(screen.getByRole("button", { name: "Back" }));
    expect(screen.getByLabelText("Automation name")).toHaveValue("Lobby wind-down");
  });
});

describe("PageRenderer — a non-conformant document never renders (UIS-200)", () => {
  it("refuses an invalid document and reports its taxonomy code instead of painting it", () => {
    const onValidationError = vi.fn();
    render(
      <PageRenderer doc={docFor("UIS-060-invalid-unknown-widget-rejected")} onValidationError={onValidationError} />,
    );
    // The rejection panel shows, the page does not.
    expect(screen.getByRole("alert")).toHaveTextContent("UNKNOWN_WIDGET_TYPE");
    expect(onValidationError).toHaveBeenCalled();
    const [errors] = onValidationError.mock.calls[0];
    expect(errors[0].code).toBe("UNKNOWN_WIDGET_TYPE");
  });
});
