import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { Toaster, toast } from "./toaster";

describe("Toaster", () => {
  it("mounts a labelled notifications region", () => {
    render(
      <ThemeProvider>
        <Toaster />
      </ThemeProvider>,
    );
    expect(screen.getByRole("region", { name: /notifications/i })).toBeInTheDocument();
  });

  it("exposes the imperative toast idiom", () => {
    expect(typeof toast).toBe("function");
    expect(typeof toast.success).toBe("function");
    expect(typeof toast.error).toBe("function");
  });
});
