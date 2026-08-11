import { cn } from "@/lib/utils";
import { historyRows, type StudioHistory } from "./edit-history";

/**
 * The History panel — every step of this editing session, with the operator's
 * position in it, and a click to go anywhere on the list.
 *
 * It exists because undo is otherwise blind. `⌘Z` and its label say what ONE
 * press will take back; they do not say how far back the stack goes, whether the
 * thing an operator is looking for is three steps or thirty, or that a redo
 * branch is still there after they undid too far. A cast is an hour of small
 * edits, so "how do I get back to before I started fiddling with the countdown"
 * is the question this answers.
 *
 * The list is the SAME stack the shortcuts drive (`edit-history.ts`) projected
 * through `historyRows`, and a click dispatches `jumpTo`, which is defined as a
 * replay of undo/redo. So there is no second idea anywhere of what a step is,
 * what order the steps are in, or where the operator currently stands.
 *
 * ── Presentation ────────────────────────────────────────────────────────────
 * Legacy's HistoryPanel marks the current row with an accent left-edge and fades
 * the undone tail to 40%, which is the convention every editor with this palette
 * uses; both are reproduced. Legacy also prints a wall-clock time per row. This
 * stack carries no timestamps — steps are `{state, label}` and nothing more —
 * and inventing one at render time would print the time the PANEL drew rather
 * than the time the edit happened, which is a worse answer than no column at
 * all. The step NUMBER is shown instead: it is what the operator is counting
 * when they think "two more".
 */
export function HistoryPanel({
  history,
  onJump,
}: {
  history: StudioHistory;
  onJump: (step: number) => void;
}) {
  const rows = historyRows(history);
  return (
    <ol aria-label="Edit history" className="flex flex-col">
      {rows.map((row) => {
        const current = row.position === "current";
        return (
          <li key={row.step}>
            <button
              type="button"
              data-slot="history-row"
              data-step={row.step}
              data-position={row.position}
              aria-current={current ? "step" : undefined}
              // The name carries the position, because the styling that conveys
              // it (an accent edge, a fade) conveys nothing to a screen reader —
              // and "which of these am I on" is the panel's entire question.
              aria-label={`Step ${row.step}: ${row.label}${
                current ? " — current" : row.position === "future" ? " — undone" : ""
              }`}
              onClick={() => onJump(row.step)}
              className={cn(
                "flex w-full items-center gap-2 border-l-2 px-2 py-1 text-left text-[12px] outline-none transition-colors",
                "focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring",
                current
                  ? "border-l-[color:var(--wv-accent)] bg-[color:var(--wv-nav-active-bg)] text-foreground"
                  : "border-l-transparent hover:bg-accent",
                row.position === "future" ? "opacity-40" : null,
              )}
            >
              <span
                aria-hidden="true"
                className={cn(
                  "size-1.5 shrink-0 rounded-full",
                  current ? "bg-[color:var(--wv-accent)]" : "bg-muted-foreground",
                )}
              />
              <span className="min-w-0 flex-1 truncate first-letter:uppercase">{row.label}</span>
              <span aria-hidden="true" className="shrink-0 tabular-nums text-[11px] text-muted-foreground">
                {row.step}
              </span>
            </button>
          </li>
        );
      })}
    </ol>
  );
}
