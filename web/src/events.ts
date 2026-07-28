import { useEffect } from "react";
import type { ControlPlaneEvent } from "./api";

export function useControlPlaneEvents(onEvent: (event: ControlPlaneEvent) => void): void {
  useEffect(() => {
    const source = new EventSource("/api/v1/events");
    const receive = (event: MessageEvent<string>) => {
      try { onEvent(JSON.parse(event.data) as ControlPlaneEvent); } catch { return; }
    };
    source.addEventListener("control-plane", receive);
    return () => { source.removeEventListener("control-plane", receive); source.close(); };
  }, [onEvent]);
}
