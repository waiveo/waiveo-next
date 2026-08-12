import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Download } from "lucide-react";
import { DownloadLink } from "./link";

// The point of this component is that it stays an ANCHOR. Every assertion below
// is about a property a scripted button would have destroyed.

describe("DownloadLink", () => {
  it("is a link with an href, not a button", () => {
    render(
      <DownloadLink href="/api/v1/backup/archives/w.waiveobk" download="w.waiveobk">
        Download
      </DownloadLink>,
    );
    const link = screen.getByRole("link", { name: "Download" });
    expect(link).toHaveAttribute("href", "/api/v1/backup/archives/w.waiveobk");
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("names the saved file, so the browser does not invent one from the URL", () => {
    render(
      <DownloadLink href="/api/v1/casts/01J/export" download="morning-loop.cast">
        Export
      </DownloadLink>,
    );
    expect(screen.getByRole("link", { name: "Export" })).toHaveAttribute(
      "download",
      "morning-loop.cast",
    );
  });

  it("accepts the server's own filename with `download`, and still sets the attribute", () => {
    // `download={true}` must render the BARE attribute. Rendered as
    // `download="true"` the browser would save every cast bundle as a file
    // literally called `true`.
    render(
      <DownloadLink href="/api/v1/casts/01J/export" download aria-label="Export Morning Loop" />,
    );
    const link = screen.getByRole("link", { name: "Export Morning Loop" });
    expect(link).toHaveAttribute("download", "");
    expect(link.getAttribute("download")).not.toBe("true");
  });

  it("names an icon-only link from aria-label, since it has no text at all", () => {
    render(
      <DownloadLink
        variant="icon"
        href="/x"
        download
        icon={Download}
        aria-label="Export Morning Loop as a .cast bundle"
      />,
    );
    expect(
      screen.getByRole("link", { name: "Export Morning Loop as a .cast bundle" }),
    ).toBeInTheDocument();
  });

  it("renders the icon variant as a link wearing the kit button's own chrome", () => {
    // The reason the casts row had a hand-rolled copy of the button's utility
    // string: an export sitting in a row of icon buttons has to be the same
    // square. `asChild` means it IS the button's class list, not a lookalike.
    render(<DownloadLink variant="icon" href="/x" download icon={Download} aria-label="Export" />);
    const link = screen.getByRole("link", { name: "Export" });
    expect(link).toHaveAttribute("data-slot", "button");
  });

  it("passes a click handler through — the pressable-row case depends on it", async () => {
    // In the cast library the whole ROW is pressable, so the export link must be
    // able to stop the click bubbling or downloading would also navigate away.
    const user = userEvent.setup();
    let stopped = 0;
    render(
      <div
        onClick={() => {
          stopped += 1;
        }}
      >
        <DownloadLink
          variant="icon"
          href="/x"
          download
          icon={Download}
          aria-label="Export"
          onClick={(e) => e.stopPropagation()}
        />
      </div>,
    );
    await user.click(screen.getByRole("link", { name: "Export" }));
    expect(stopped).toBe(0);
  });
});
