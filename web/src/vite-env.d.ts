/// <reference types="vite/client" />

interface ImportMetaEnv {
  /**
   * Build-time override for the heartbeat interval the runners page assumes
   * (`LOOP_RUNNER_HEARTBEAT_INTERVAL`, milliseconds). Set it when the fleet
   * does not run the runner's 10s default, otherwise every runner is reported
   * stale. Removed once `GET /api/v1/runners` reports the interval itself
   * (docs/design/web-console/tasks.md A12).
   */
  readonly VITE_RUNNER_HEARTBEAT_INTERVAL_MS?: string;
}
