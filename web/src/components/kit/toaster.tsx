import { toast } from "sonner";

// Toaster — the one toast idiom. The visual host is the themed sonner Toaster
// (styled from HORIZON tokens, following the active Dusk/Daybreak theme); `toast`
// is the imperative call every page uses. sonner's `toast.success` reads as the
// reserved ok/green — an allowed use of that lane (success == ok).
export { Toaster } from "@/components/ui/sonner";
export { toast };
