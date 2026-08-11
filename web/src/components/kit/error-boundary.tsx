import { Component, type ErrorInfo, type ReactNode } from "react";
import { TriangleAlert, RotateCcw } from "lucide-react";
import { Button } from "./button";
import { EmptyState } from "./empty-state";

/**
 * ErrorBoundary — the console's last line of defence against a thrown render.
 *
 * # Why this exists
 *
 * There was none. A single component throwing during render unmounted the WHOLE
 * React tree, so the operator got a blank white page: no nav, no way back, no
 * indication anything had gone wrong rather than merely finished loading. On an
 * appliance console that is indistinguishable from the box being down, and the
 * natural next move — reboot the box — is both drastic and useless, because the
 * fault is in the browser.
 *
 * `routes/packs/catalog.ts:45` had already noticed the absence in a comment
 * while reasoning about untrusted pack data reaching React as a child. That is
 * the sharpest case: a pack is third-party content, its manifest is data this
 * box did not author, and a value that is valid JSON but not a valid React child
 * throws at render time. Without a boundary, an extension can white-screen the
 * entire console.
 *
 * # Why a class
 *
 * React exposes error catching ONLY through the class lifecycle
 * (`getDerivedStateFromError` / `componentDidCatch`). There is no hook, and this
 * is the one place in this codebase where a class is the correct answer rather
 * than a legacy one.
 *
 * # What it deliberately does NOT do
 *
 * It does not retry automatically, and it does not reload the page. A render
 * that threw once will usually throw again on the same state, so an automatic
 * retry produces a flicker loop that is harder to diagnose than a stopped page.
 * The operator gets an explicit control instead.
 *
 * It also does not swallow the error: `componentDidCatch` re-reports to the
 * console so the stack survives for whoever is looking, and the message is shown
 * verbatim rather than replaced with a generic apology. This console's house
 * style is to surface the system's own words — a refused install shows the box's
 * error code, and a crashed render should show what actually threw.
 */
interface ErrorBoundaryProps {
  children: ReactNode;
  /** Names the region that failed, so the message can say WHERE. */
  label?: string;
}

interface ErrorBoundaryState {
  error: Error | null;
}

export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  override state: ErrorBoundaryState = { error: null };

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { error };
  }

  override componentDidCatch(error: Error, info: ErrorInfo) {
    // Re-reported, never swallowed: the boundary changes what the OPERATOR sees,
    // and must not change what a developer with the console open can find.
    console.error("[waiveo] a render threw and was caught by the error boundary", error, info);
  }

  private readonly reset = () => this.setState({ error: null });

  override render() {
    const { error } = this.state;
    if (!error) return this.props.children;

    const where = this.props.label ? `${this.props.label} could not be shown` : "This page could not be shown";

    return (
      <div role="alert" data-slot="error-boundary">
        <EmptyState
          icon={TriangleAlert}
          title={where}
          description={
            // The thrown message verbatim. An operator reporting a fault can
            // read this out; "something went wrong" cannot be acted on by
            // anyone, and it is the failure mode this console avoids elsewhere.
            error.message || "The error carried no message."
          }
          action={
            <Button variant="secondary" icon={RotateCcw} onClick={this.reset}>
              Try again
            </Button>
          }
        />
      </div>
    );
  }
}
