// Split out of ui/index.tsx so that file exports only components
// (react-refresh/only-export-components: useConfirm is a plain hook, not a
// component).
import { useCallback, useMemo, useState, type ReactNode } from "react";
import { ConfirmDialog, type Confirmation } from "./confirm";

/** Opens a confirm dialog and returns the element to render plus the opener. */
export function useConfirm(): { confirm: (confirmation: Confirmation) => void; dialog: ReactNode } {
  const [pending, setPending] = useState<Confirmation | null>(null);
  const close = useCallback(() => setPending(null), []);
  const dialog = useMemo(
    () => (pending ? <ConfirmDialog confirmation={pending} onClose={close} /> : null),
    [pending, close]
  );
  return { confirm: setPending, dialog };
}
