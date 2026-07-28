ALTER TABLE app.project_circuit_state
  ADD COLUMN IF NOT EXISTS probe_workflow_run_id UUID;

ALTER TABLE app.provider_circuit_state
  ADD COLUMN IF NOT EXISTS probe_workflow_run_id UUID;

CREATE INDEX IF NOT EXISTS project_circuit_half_open_probe_idx
  ON app.project_circuit_state (probe_workflow_run_id)
  WHERE state = 'half_open';

CREATE INDEX IF NOT EXISTS provider_circuit_half_open_probe_idx
  ON app.provider_circuit_state (probe_workflow_run_id)
  WHERE state = 'half_open';
