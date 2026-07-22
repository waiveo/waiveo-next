import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

// cn merges conditional class names and resolves Tailwind conflicts (later
// classes win). The vendored shadcn base and the widget kit compose classes
// through it.
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
