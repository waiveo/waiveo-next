import * as React from "react";
import { Toaster as Sonner, type ToasterProps } from "sonner";
import { useTheme } from "@/components/theme/theme-context";

// Toaster (sonner) — styled from HORIZON tokens, following the active Dusk/
// Daybreak theme. Flat content-layer surface; a toast is never a gradient.
function Toaster({ ...props }: ToasterProps) {
  const { theme } = useTheme();

  return (
    <Sonner
      theme={theme}
      className="toaster group"
      style={
        {
          "--normal-bg": "var(--wv-surface-2)",
          "--normal-text": "var(--wv-text)",
          "--normal-border": "var(--wv-border)",
          "--success-bg": "var(--wv-ok-bg)",
          "--success-text": "var(--wv-ok)",
          "--error-bg": "var(--wv-err-bg)",
          "--error-text": "var(--wv-err)",
          "--warning-bg": "var(--wv-warn-bg)",
          "--warning-text": "var(--wv-warn)",
          "--border-radius": "var(--radius-card)",
        } as React.CSSProperties
      }
      {...props}
    />
  );
}

export { Toaster };
