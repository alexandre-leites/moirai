// Split out of ui/index.tsx so that file exports only components
// (react-refresh/only-export-components: useFocusTrap is a plain hook, not a
// component).
import { useEffect } from "react";

const FOCUSABLE = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';

/**
 * Keeps keyboard focus inside `panel` while it is open, moves focus into it on
 * mount, returns focus to whatever opened it, and closes on Escape
 * (specification.md §6). Used by both the modal and the mobile nav drawer.
 */
export function useFocusTrap(panel: { current: HTMLElement | null }, onClose: () => void): void {
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    const focusable = () =>
      Array.from(panel.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? [])
        .filter((node) => !node.hasAttribute("disabled"));

    focusable()[0]?.focus();

    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.stopPropagation();
        onClose();
        return;
      }
      if (event.key !== "Tab") return;
      const nodes = focusable();
      if (nodes.length === 0) return;
      const first = nodes[0];
      const last = nodes[nodes.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };

    document.addEventListener("keydown", onKeyDown, true);
    return () => {
      document.removeEventListener("keydown", onKeyDown, true);
      opener?.focus?.();
    };
  }, [panel, onClose]);
}
