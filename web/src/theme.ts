// Theme selection. The OS preference is the default signal; an explicit choice
// is written to <html data-theme> and persisted (specification.md §6).
import { useCallback, useEffect, useState } from "react";

export type Theme = "light" | "dark" | "system";

const STORAGE_KEY = "moirai.theme";

function read(): Theme {
  try {
    const stored = window.localStorage.getItem(STORAGE_KEY);
    return stored === "light" || stored === "dark" ? stored : "system";
  } catch {
    // Private-mode browsers throw on localStorage access. Following the OS
    // preference is the correct fallback, and the toggle still works for the
    // life of the page.
    return "system";
  }
}

function apply(theme: Theme): void {
  const root = document.documentElement;
  if (theme === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", theme);
}

/** Reads the stored preference before first paint, from index.html's entry point. */
export function initTheme(): void {
  apply(read());
}

export function useTheme(): { theme: Theme; toggle: () => void } {
  const [theme, setTheme] = useState<Theme>(read);

  useEffect(() => {
    apply(theme);
    try {
      if (theme === "system") window.localStorage.removeItem(STORAGE_KEY);
      else window.localStorage.setItem(STORAGE_KEY, theme);
    } catch {
      // Non-persistent is still usable; see read().
    }
  }, [theme]);

  // Two-state toggle against what is currently on screen: from "system" it
  // flips to the opposite of whatever the OS is asking for, which is what a
  // user reaching for the control wants.
  const toggle = useCallback(() => {
    setTheme((current) => {
      if (current === "system") {
        return window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "light" : "dark";
      }
      return current === "dark" ? "light" : "dark";
    });
  }, []);

  return { theme, toggle };
}

/** The theme actually on screen, for the toggle's label. */
export function resolvedTheme(theme: Theme): "light" | "dark" {
  if (theme !== "system") return theme;
  return window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}
