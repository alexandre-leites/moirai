// The toast context and the hook that reads it, split out from toast.tsx so
// that file can export only the ToastProvider component (react-refresh/
// only-export-components: useToast is a plain hook, not a component).
import { createContext, useContext } from "react";

export type ToastContextValue = (message: string) => void;

export const ToastContext = createContext<ToastContextValue>(() => undefined);

export function useToast(): ToastContextValue {
  return useContext(ToastContext);
}
