import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ErrorBoundary } from "./error-boundary";

// A boundary is only worth having if it CATCHES. These drive a real throw
// through React's render phase rather than asserting on the fallback markup in
// isolation — a fallback that renders when handed an error prop proves nothing
// about whether the boundary is wired into the tree.

function Boom({ message }: { message: string }): React.ReactNode {
  throw new Error(message);
}

describe("ErrorBoundary", () => {
  beforeEach(() => {
    // React logs caught errors itself; silence only that noise, and assert
    // separately that OUR reporting still happens.
    vi.spyOn(console, "error").mockImplementation(() => {});
  });
  afterEach(() => vi.restoreAllMocks());

  it("catches a thrown render and keeps the rest of the app alive", () => {
    render(
      <div>
        <p>the navigation</p>
        <ErrorBoundary>
          <Boom message="layer kind 'sparkle' is not renderable" />
        </ErrorBoundary>
      </div>,
    );
    // The sibling survives — this is the whole point: a broken region must not
    // take down what surrounds it.
    expect(screen.getByText("the navigation")).toBeInTheDocument();
    expect(screen.getByRole("alert")).toBeInTheDocument();
  });

  it("shows the thrown message VERBATIM, not a generic apology", () => {
    render(
      <ErrorBoundary>
        <Boom message="layer kind 'sparkle' is not renderable" />
      </ErrorBoundary>,
    );
    // An operator reporting a fault reads this out. "Something went wrong"
    // cannot be acted on by anyone, which is why the console surfaces the
    // system's own words everywhere else too.
    expect(screen.getByText("layer kind 'sparkle' is not renderable")).toBeInTheDocument();
  });

  it("names the region when given a label", () => {
    render(
      <ErrorBoundary label="The cast library">
        <Boom message="nope" />
      </ErrorBoundary>,
    );
    expect(screen.getByText("The cast library could not be shown")).toBeInTheDocument();
  });

  it("re-reports the error rather than swallowing it", () => {
    const spy = console.error as unknown as ReturnType<typeof vi.fn>;
    render(
      <ErrorBoundary>
        <Boom message="nope" />
      </ErrorBoundary>,
    );
    // The boundary changes what the OPERATOR sees; it must not change what a
    // developer with the console open can find.
    const ours = spy.mock.calls.filter(
      (c: unknown[]) => typeof c[0] === "string" && c[0].includes("caught by the error boundary"),
    );
    expect(ours.length).toBeGreaterThan(0);
  });

  it("recovers when Try again is CLICKED and the child no longer throws", async () => {
    const user = userEvent.setup();
    let shouldThrow = true;
    function Sometimes(): React.ReactNode {
      if (shouldThrow) throw new Error("transient");
      return <p>recovered content</p>;
    }
    render(
      <ErrorBoundary>
        <Sometimes />
      </ErrorBoundary>,
    );
    expect(screen.getByRole("alert")).toBeInTheDocument();

    shouldThrow = false;
    await user.click(screen.getByRole("button", { name: /try again/i }));

    expect(screen.getByText("recovered content")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders children untouched when nothing throws", () => {
    render(
      <ErrorBoundary>
        <p>ordinary page</p>
      </ErrorBoundary>,
    );
    expect(screen.getByText("ordinary page")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
