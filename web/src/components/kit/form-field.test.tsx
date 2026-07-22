import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { FormField } from "./form-field";

describe("FormField", () => {
  it("associates the label with the control and describes it with help text", () => {
    render(
      <FormField label="Screen name" help="Shown on the device list.">
        {(field) => <input {...field} placeholder="e.g. The Hangar TV" />}
      </FormField>,
    );
    const input = screen.getByLabelText("Screen name");
    expect(input).toBeInTheDocument();

    const help = screen.getByText("Shown on the device list.");
    expect(input.getAttribute("aria-describedby")).toContain(help.id);
    expect(input).not.toHaveAttribute("aria-invalid");
  });

  it("flags the control invalid and exposes the error via role=alert + aria-describedby", () => {
    render(
      <FormField label="Daypart" error="Pick a daypart.">
        {(field) => <input {...field} />}
      </FormField>,
    );
    const input = screen.getByLabelText("Daypart");
    expect(input).toHaveAttribute("aria-invalid", "true");

    const error = screen.getByRole("alert");
    expect(error).toHaveTextContent("Pick a daypart.");
    expect(input.getAttribute("aria-describedby")).toContain(error.id);
  });

  it("surfaces the required flag to assistive tech (aria-required) with a decorative asterisk", () => {
    render(
      <FormField label="Screen name" required>
        {(field) => <input {...field} />}
      </FormField>,
    );
    const input = screen.getByRole("textbox");
    expect(input).toHaveAttribute("aria-required", "true");

    // The magenta asterisk is purely visual and must be hidden from AT.
    const asterisk = screen.getByText("*");
    expect(asterisk).toHaveAttribute("aria-hidden", "true");
  });

  it("omits aria-required when the field is not required", () => {
    render(
      <FormField label="Notes">{(field) => <input {...field} />}</FormField>,
    );
    expect(screen.getByRole("textbox")).not.toHaveAttribute("aria-required");
  });

  it("honors an explicit htmlFor id", () => {
    render(
      <FormField label="Notes" htmlFor="notes-field">
        {(field) => <textarea {...field} />}
      </FormField>,
    );
    expect(screen.getByLabelText("Notes").id).toBe("notes-field");
  });
});
