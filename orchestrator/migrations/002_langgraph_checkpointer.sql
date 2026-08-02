-- LangGraph checkpoint tables (PostgresSaver schema)
-- https://langchain-ai.github.io/langgraph/how-to/persistence/
--
-- Dead schema, kept only because an applied migration cannot be rewritten: #247
-- replaced the Python/LangGraph orchestrator with the Go state machine, which
-- persists workflow state in app.workflow_runs and writes nothing here. Nothing
-- in the codebase reads or writes these tables — do not build against them.

CREATE TABLE IF NOT EXISTS langgraph.checkpoints (
    thread_id TEXT NOT NULL,
    checkpoint_id TEXT NOT NULL,
    parent_checkpoint_id TEXT,
    checkpoint JSONB NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (thread_id, checkpoint_id)
);

CREATE TABLE IF NOT EXISTS langgraph.checkpoint_writes (
    thread_id TEXT NOT NULL,
    checkpoint_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    idx INTEGER NOT NULL,
    channel TEXT NOT NULL,
    type TEXT,
    value JSONB,
    PRIMARY KEY (thread_id, checkpoint_id, task_id, idx)
);

CREATE INDEX IF NOT EXISTS checkpoints_thread_id_idx ON langgraph.checkpoints (thread_id, created_at DESC);
CREATE INDEX IF NOT EXISTS checkpoint_writes_thread_id_idx ON langgraph.checkpoint_writes (thread_id, checkpoint_id);
