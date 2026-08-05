import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { ToastContext } from "./toast-context";

/** Announces one message at a time, bottom-centre, auto-dismissed. */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [message, setMessage] = useState<string | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const show = useCallback((text: string) => {
    setMessage(text);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setMessage(null), 2600);
  }, []);

  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current);
  }, []);

  return (
    <ToastContext.Provider value={show}>
      {children}
      <div role="status" aria-live="polite">{message && <div id="toast">{message}</div>}</div>
    </ToastContext.Provider>
  );
}
