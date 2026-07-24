import type { ApiClient, HealthStatus } from "./api";

export type HealthViewState = "checking" | HealthStatus | "unavailable";

export async function loadHealth(client: ApiClient, signal?: AbortSignal): Promise<HealthViewState> {
  try {
    return await client.health(signal);
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError") {
      throw error;
    }
    return "unavailable";
  }
}
